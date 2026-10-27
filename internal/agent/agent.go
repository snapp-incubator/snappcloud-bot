package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/snapp-incubator/snappcloud-bot/internal/identity"
	"github.com/snapp-incubator/snappcloud-bot/internal/metrics"
)

// Tool is an MCP tool exposed to the model.
type Tool struct {
	Name        string
	Description string
	InputSchema map[string]any // JSON Schema object
	// SelfAuthorized marks a tool from an identity-aware server (e.g. argocd-mcp):
	// the server authorizes the caller itself from the forwarded identity, so the
	// agent skips namespace enforcement and returns the result unfiltered.
	SelfAuthorized bool
	// Unscoped marks a tool whose results are shared by every user authorized
	// on the cluster: the agent skips namespace enforcement, PromQL pinning and
	// result filtering. Cluster-admin-only rules still apply.
	Unscoped bool
	// Server names the MCP server that advertised the tool, so a long tool list
	// can be trimmed evenly across servers rather than by configuration order.
	Server string
}

// ToolCall is the model's request to invoke a tool.
type ToolCall struct {
	ID   string
	Name string
	Args map[string]any
}

// ToolResult is the outcome of a tool call fed back to the model.
type ToolResult struct {
	CallID  string
	Content string
	IsError bool
}

// Turn is one message in the conversation. A user turn carries either free text
// (the query) or tool results; an assistant turn carries text and/or tool calls.
type Turn struct {
	Role    string // "user" | "assistant"
	Text    string
	Calls   []ToolCall
	Results []ToolResult
}

// Request is one LLM invocation.
type Request struct {
	System   string
	Messages []Turn
	Tools    []Tool
}

// Response is the assistant's turn: final text when Calls is empty, otherwise
// the tool calls to run.
type Response struct {
	Text  string
	Calls []ToolCall
	// Truncated reports that the model stopped because it hit its output
	// limit, not because it had finished. The text is a sentence cut in half,
	// which reads as a complete answer to everyone but the model.
	Truncated bool
}

// LLM is the reasoning model (Anthropic-style tool use).
type LLM interface {
	Complete(ctx context.Context, req Request) (Response, error)
}

// MCP executes tool calls against one cluster's MCP servers.
type MCP interface {
	ListTools(ctx context.Context) ([]Tool, error)
	CallTool(ctx context.Context, name string, args map[string]any) (string, error)
}

// Resolver maps bare resource references (IPs) in a tool result to the
// namespace(s) they live in, so records naming only an IP can still be gated.
type Resolver interface {
	ResolveIPs(ctx context.Context, cluster string, ips []string) (map[string][]string, error)
}

// Agent runs the enforced tool-calling loop.
type Agent struct {
	llm          LLM
	enforcer     *Enforcer
	resolver     Resolver // may be nil (no IP resolution)
	maxIter      int
	budgets      Budgets
	log          *slog.Logger
	reviewPrompt string
}

// New builds an Agent. maxIter bounds the tool-calling loop (default 6).
// resolver may be nil.
func New(llm LLM, enforcer *Enforcer, resolver Resolver, maxIter int, budgets Budgets, log *slog.Logger) *Agent {
	if maxIter <= 0 {
		maxIter = 6
	}
	budgets.applyDefaults()
	return &Agent{llm: llm, enforcer: enforcer, resolver: resolver, maxIter: maxIter, budgets: budgets, log: log}
}

// ClusterTools binds one authorized cluster: the user's scope on it, its MCP
// executor, and a short alias it also answers to. Passing several lets the agent
// answer cross-cluster queries in a single loop — a call names its cluster and
// is enforced against that cluster's scope.
type ClusterTools struct {
	Cluster string   // full cluster name (resolver + logs)
	Alias   string   // short tool-name prefix (defaults to Cluster)
	Allowed []string // namespaces the user may access on this cluster
	// ClusterAdmin marks callers with cluster-wide access; required for
	// cluster-infrastructure tools (ToolRule.ClusterAdminOnly).
	ClusterAdmin bool
	MCP          MCP // executor for this cluster's tools
	// NoEnforce marks a namespace-agnostic, non-cluster tool source (e.g. general
	// docs): its results are passed through unfiltered and it is not tied to any
	// region's scope. Use only for trusted, tenant-independent content.
	NoEnforce bool
	// Preferred marks a cluster the question is actually about, so it keeps its
	// whole tool list when the list has to be trimmed. A question that names one
	// cluster should not lose that cluster's tools to five it did not mention.
	Preferred bool
}

// Input is one user query, evaluated across every cluster the user can access.
type Input struct {
	System string
	// User is the caller's identity, forwarded to identity-aware MCP servers so
	// self-authorized tools can resolve the caller's own access.
	User     string
	Query    string
	Clusters []ClusterTools
	// ReqID correlates all log lines for one user message (empty = none).
	ReqID string
	// Review asks for the finished answer to be checked against the tool
	// results before it is returned. Meant for answers nobody can question
	// back — a scheduled report, an alert investigation — where a figure that
	// contradicts its own table is read and acted on as it stands.
	Review bool
}

// binding maps a cluster-qualified tool name back to its cluster + real name.
type binding struct {
	ct      ClusterTools
	real    string
	allowed map[string]bool
	// selfAuthorized: the tool's server authorizes the caller itself (identity-
	// aware), so the agent skips namespace enforcement and returns it unfiltered.
	selfAuthorized bool
	// unscoped: the tool's results are shared by every authorized caller, so
	// the agent neither scopes the call nor filters the result.
	unscoped bool
	// schema is the tool's JSON Schema, kept so a refusal can name the
	// arguments that would have narrowed the call.
	schema map[string]any
}

// capResult truncates a result to the rune budget (after filtering, so JSON
// stays filterable), cutting on runes, and logs what was dropped.
func (a *Agent) capResult(tool, s string) string {
	r := []rune(s)
	if len(r) <= a.budgets.ResultRunes {
		return s
	}
	a.log.Info("tool result truncated to the result budget",
		"tool", tool, "runes", len(r), "kept", a.budgets.ResultRunes,
		"dropped", len(r)-a.budgets.ResultRunes)
	metrics.ResultsTruncated.WithLabelValues(tool).Inc()
	return string(r[:a.budgets.ResultRunes]) +
		"\n[output truncated — narrow the query (a namespace, a node, or a filter)]"
}

// Run drives the LLM ↔ MCP loop across every authorized cluster, enforcing each
// tool call against that cluster's scope, and returns the final answer text.
func (a *Agent) Run(ctx context.Context, in Input) (string, error) {
	var t transcript
	text, err := a.run(ctx, in, &t)
	if err != nil || !in.Review || strings.TrimSpace(text) == "" {
		return text, err
	}
	return a.review(ctx, in.ReqID, t, text), nil
}

// transcript is what run leaves behind for review: the system prompt it used
// and every turn, tool results included — the evidence a draft is checked against.
type transcript struct {
	system string
	msgs   []Turn
}

func (a *Agent) run(ctx context.Context, in Input, out *transcript) (string, error) {
	lg := a.log.With("req", in.ReqID)
	start := time.Now()
	var toolCalls, toolErrs, denied, filtered, nudges int
	iters := 0
	// summary logs one line per user message — the anchor for troubleshooting.
	summary := func(outcome string, extra ...any) {
		args := append([]any{
			"outcome", outcome, "iters", iters, "toolCalls", toolCalls,
			"toolErrs", toolErrs, "denied", denied, "filtered", filtered,
			"dur", time.Since(start).Round(time.Millisecond),
		}, extra...)
		lg.Info("turn done", args...)
		metrics.TurnIterations.Observe(float64(iters))
	}

	// Carry the caller identity on the context; identity-aware MCP clients
	// forward it as X-Remote-User. Never sourced from a tool argument.
	ctx = identity.WithUser(ctx, in.User)

	tools, cat, notice, err := a.buildTools(ctx, in.Clusters)
	if err != nil {
		summary("build-tools-failed", "err", err)
		return "", fmt.Errorf("list tools: %w", err)
	}
	reg := cat.routes
	system := in.System + notice
	msgs := []Turn{{Role: "user", Text: in.Query}}
	defer func() { out.system, out.msgs = system, msgs }()
	// Identifiers the model is allowed to pass: what the question carried, plus
	// whatever the results add as the turn goes on.
	seenIDs := map[string]bool{}
	noteQuestionWords(seenIDs, in.Query)
	noteConfiguredIDs(seenIDs, system)

	for iter := 0; iter < a.maxIter; iter++ {
		iters = iter + 1
		llmStart := time.Now()
		resp, err := a.llm.Complete(ctx, Request{System: system, Messages: msgs, Tools: tools})
		metrics.LLMDuration.Observe(time.Since(llmStart).Seconds())
		if err != nil {
			metrics.LLMRequests.WithLabelValues("error").Inc()
			summary("llm-error", "err", err)
			return "", fmt.Errorf("llm: %w", err)
		}
		metrics.LLMRequests.WithLabelValues("ok").Inc()
		msgs = append(msgs, Turn{Role: "assistant", Text: resp.Text, Calls: resp.Calls})
		if len(resp.Calls) == 0 {
			if resp.Truncated {
				// Output limit hit mid-answer: ask for the remainder rather than post a cut answer.
				text, err := a.finish(ctx, system, msgs, tools, resp.Text)
				if err != nil {
					summary("answered-truncated", "err", err)
					return resp.Text + "\n\n_[answer cut off at the model's output limit]_", nil
				}
				summary("answered", "continued", true)
				return text, nil
			}
			// A turn that ends on "let me check X" and calls nothing is not an
			// answer; it is an investigation stopped one sentence short. Send
			// it back rather than posting it.
			if nudges < maxNudges && iter < a.maxIter-1 && announcesMoreWork(resp.Text) {
				nudges++
				lg.Info("model announced work it did not do; asking it to continue",
					"nudge", nudges, "iteration", iters)
				metrics.AnswerNudges.Inc()
				msgs = append(msgs, Turn{Role: "user", Text: nudge})
				continue
			}
			// An answer that is mostly unmeasured cells, or only the sentence before the
			// work, while rounds remain: ask for the work.
			if nudges < maxNudges && iter < a.maxIter-1 && looksLikePreamble(resp.Text, toolCalls) {
				nudges++
				lg.Info("answer is a preamble; asking for the work itself",
					"nudge", nudges, "iteration", iters, "runes", len([]rune(resp.Text)))
				metrics.AnswerNudges.Inc()
				msgs = append(msgs, Turn{Role: "user", Text: finishTheThought})
				continue
			}
			if nudges < maxNudges && iter < a.maxIter-2 && mostlyUnmeasured(resp.Text) {
				nudges++
				lg.Info("answer is mostly unmeasured with rounds left; asking for the queries",
					"nudge", nudges, "iteration", iters)
				metrics.AnswerNudges.Inc()
				msgs = append(msgs, Turn{Role: "user", Text: runTheQueries})
				continue
			}
			// Still narrating after every nudge: ask once more, without tools, for the answer.
			if nudges >= maxNudges && announcesMoreWork(resp.Text) {
				lg.Info("model still announcing work after its nudges; demanding the answer", "iteration", iters)
				metrics.TurnOutcomes.WithLabelValues("demanded").Inc()
				text, derr := a.demandAnswer(ctx, system, msgs)
				if derr != nil {
					summary("answered", "demanded", true, "err", derr)
					return resp.Text, nil
				}
				summary("answered", "demanded", true)
				return text, nil
			}
			summary("answered")
			metrics.TurnOutcomes.WithLabelValues("answered").Inc()
			return resp.Text, nil // final answer
		}

		results := make([]ToolResult, 0, len(resp.Calls))
		for _, call := range resp.Calls {
			toolCalls++
			switch call.Name {
			case toolSearchName:
				results = append(results, ToolResult{CallID: call.ID, Content: cat.search(call.Args)})
				continue
			case toolDescribeName:
				results = append(results, ToolResult{CallID: call.ID, Content: cat.describe(call.Args)})
				continue
			case toolCallName:
				inner, msg := cat.unwrap(call)
				if msg != "" {
					results = append(results, errResult(call.ID, msg))
					continue
				}
				call = inner
			}
			r, legacyCluster := reg[call.Name], ""
			if r == nil {
				r, legacyCluster = legacyRoute(reg, call.Name)
			}
			if r == nil {
				results = append(results, errResult(call.ID, "unknown tool "+call.Name))
				continue
			}
			if legacyCluster != "" {
				if call.Args == nil {
					call.Args = map[string]any{}
				}
				if _, set := call.Args[clusterArg]; !set {
					call.Args[clusterArg] = legacyCluster
				}
			}
			b, args, refusal := r.resolve(call.Args)
			if refusal != "" {
				denied++
				results = append(results, errResult(call.ID, refusal))
				continue
			}
			call.Args = args
			// Check required arguments before spending the call: servers report a missing
			// argument in their own terms, which reads as an empty result.
			if missing := missingRequired(b.schema, call.Args); len(missing) > 0 {
				denied++
				metrics.ToolCalls.WithLabelValues(b.ct.Cluster, b.real, "invalid").Inc()
				lg.Info("tool call rejected before it was sent", "tool", b.real,
					"missing", missing, "sent", argNames(call.Args), "unknown", unknownArgs(b.schema, call.Args))
				results = append(results, errResult(call.ID, missingRequiredMessage(b.real, b.schema, missing, call.Args)))
				continue
			}
			if refusal := a.needsApproval(ctx, b, call.Args); refusal != "" {
				denied++
				metrics.ToolCalls.WithLabelValues(b.ct.Cluster, b.real, "needs-approval").Inc()
				lg.Info("tool call awaits the user's approval", "tool", b.real, "cluster", b.ct.Cluster)
				results = append(results, errResult(call.ID, refusal))
				continue
			}
			// An identifier the model made up is a 404 the model reads as "this
			// does not exist", so refuse it here and name the lookup instead.
			if unseen := unseenIDs(b.schema, call.Args, seenIDs); len(unseen) > 0 {
				denied++
				metrics.ToolCalls.WithLabelValues(b.ct.Cluster, b.real, "invalid").Inc()
				lg.Info("tool call rejected before it was sent", "tool", b.real, "invented", unseen)
				results = append(results, errResult(call.ID, unseenIDMessage(b.real, unseen)))
				continue
			}
			if !b.ct.NoEnforce {
				if b.selfAuthorized {
					// Identity-aware server: it scopes results itself from the forwarded
					// X-Remote-User, so skip enforcement and filtering.
					if in.User == "" {
						denied++
						metrics.ToolCalls.WithLabelValues(b.ct.Cluster, b.real, "denied").Inc()
						results = append(results, errResult(call.ID,
							"authorization denied: this tool needs your identity, which is unavailable"))
						continue
					}
					callStart := time.Now()
					out, cerr := b.ct.MCP.CallTool(ctx, b.real, call.Args)
					metrics.ToolDuration.WithLabelValues(b.ct.Cluster).Observe(time.Since(callStart).Seconds())
					if cerr != nil {
						toolErrs++
						metrics.ToolCalls.WithLabelValues(b.ct.Cluster, b.real, "error").Inc()
						metrics.ToolErrors.WithLabelValues(b.ct.Cluster, b.real, metrics.ClassifyToolError(cerr.Error())).Inc()
						lg.Warn("tool failed", "cluster", b.ct.Cluster, "tool", b.real, "err", cerr)
						results = append(results, errResult(call.ID, "tool error: "+cerr.Error()))
						continue
					}
					metrics.ToolCalls.WithLabelValues(b.ct.Cluster, b.real, "ok").Inc()
					results = append(results, ToolResult{CallID: call.ID, Content: a.capResult(b.real, out)})
					continue
				}
				if a.enforcer.ClusterAdminOnly(b.real) {
					if !b.ct.ClusterAdmin {
						denied++
						metrics.ToolCalls.WithLabelValues(b.ct.Cluster, b.real, "denied").Inc()
						results = append(results, errResult(call.ID,
							"authorization denied: this tool exposes cluster infrastructure and requires cluster-admin access on "+b.ct.Cluster))
						continue
					}
					callStart := time.Now()
					out, cerr := b.ct.MCP.CallTool(ctx, b.real, call.Args)
					metrics.ToolDuration.WithLabelValues(b.ct.Cluster).Observe(time.Since(callStart).Seconds())
					if cerr != nil {
						toolErrs++
						metrics.ToolCalls.WithLabelValues(b.ct.Cluster, b.real, "error").Inc()
						metrics.ToolErrors.WithLabelValues(b.ct.Cluster, b.real, metrics.ClassifyToolError(cerr.Error())).Inc()
						lg.Warn("tool failed", "cluster", b.ct.Cluster, "tool", b.real, "err", cerr)
						results = append(results, errResult(call.ID, "tool error: "+cerr.Error()))
						continue
					}
					// Cluster-admin caller: infrastructure output, returned
					// unfiltered — but still capped. Unfiltered is about
					// authorization, not size: an uncapped 30 MiB config dump
					// takes the whole round's budget with it, and the results
					// of every tool called beside it are dropped to make room.
					metrics.ToolCalls.WithLabelValues(b.ct.Cluster, b.real, "ok").Inc()
					results = append(results, ToolResult{CallID: call.ID, Content: a.capResult(b.real, out)})
					continue
				}
				// An unscoped server's results are shared by everyone authorized
				// on the cluster, so there is nothing to scope the call to.
				if !b.unscoped {
					if err := a.enforcer.Check(b.real, call.Args, b.ct.Allowed); err != nil {
						denied++
						metrics.ToolCalls.WithLabelValues(b.ct.Cluster, b.real, "denied").Inc()
						results = append(results, errResult(call.ID, "authorization denied: "+err.Error()))
						continue
					}
					// A metrics query is restricted before it runs: its result
					// cannot be filtered afterwards, because an aggregating query
					// returns numbers carrying no namespace at all.
					if err := a.pinPromQL(b.real, call.Args, b.ct.Allowed); err != nil {
						denied++
						metrics.ToolCalls.WithLabelValues(b.ct.Cluster, b.real, "denied").Inc()
						results = append(results, errResult(call.ID, "authorization denied: "+err.Error()))
						continue
					}
				}
			}
			callStart := time.Now()
			out, cerr := b.ct.MCP.CallTool(ctx, b.real, call.Args)
			metrics.ToolDuration.WithLabelValues(b.ct.Cluster).Observe(time.Since(callStart).Seconds())
			if cerr != nil {
				toolErrs++
				metrics.ToolCalls.WithLabelValues(b.ct.Cluster, b.real, "error").Inc()
				metrics.ToolErrors.WithLabelValues(b.ct.Cluster, b.real, metrics.ClassifyToolError(cerr.Error())).Inc()
				lg.Warn("tool failed", "cluster", b.ct.Cluster, "tool", b.real, "err", cerr)
				msg := "tool error: " + cerr.Error()
				if tooLarge(cerr) {
					msg += narrowingHint(b.schema, call.Args)
				}
				msg += retryHint(metrics.ClassifyToolError(cerr.Error()))
				results = append(results, errResult(call.ID, msg))
				continue
			}
			if b.ct.NoEnforce {
				// Trusted namespace-agnostic source (docs) — no filtering, and
				// capped like everything else for the same reason.
				metrics.ToolCalls.WithLabelValues(b.ct.Cluster, b.real, "ok").Inc()
				results = append(results, ToolResult{CallID: call.ID, Content: a.capResult(b.real, out)})
			} else if b.unscoped {
				// Shared by every authorized caller — no filtering, but still
				// capped like any other result.
				metrics.ToolCalls.WithLabelValues(b.ct.Cluster, b.real, "ok").Inc()
				results = append(results, ToolResult{CallID: call.ID, Content: a.capResult(b.real, out)})
			} else {
				r, dropped := a.filtered(ctx, b, call.ID, out)
				if dropped {
					filtered++
					metrics.ToolCalls.WithLabelValues(b.ct.Cluster, b.real, "filtered").Inc()
				} else {
					metrics.ToolCalls.WithLabelValues(b.ct.Cluster, b.real, "ok").Inc()
				}
				results = append(results, r)
			}
		}
		round := capRound(results, a.budgets.RoundRunes)
		for _, r := range round {
			if !r.IsError {
				noteIDs(seenIDs, r.Content)
			}
		}
		msgs = append(msgs, Turn{Role: "user", Results: round})
		if n := trimConversation(msgs, a.budgets.ConversationRunes); n > 0 {
			// Better a model that has forgotten an early dump than a request the
			// model refuses outright, which fails the same way on every retry.
			a.log.Info("trimmed oldest tool output to fit the context budget",
				"req", in.ReqID, "results_dropped", n, "iteration", iter+1)
			metrics.ConversationTrims.Inc()
		}
	}
	summary("max-iters")
	metrics.TurnOutcomes.WithLabelValues("max-iters").Inc()
	text, err := a.demandAnswer(ctx, system, msgs)
	if err != nil {
		return "", fmt.Errorf("llm (final): %w", err)
	}
	// Say it in the answer, not only in a log line. An investigation that
	// stopped at the limit reads exactly like one that finished, and the person
	// who can do something about it is the one reading the message.
	return text + fmt.Sprintf("\n\n_Stopped after %d tool calls — the per-question limit._", a.maxIter), nil
}

// demandAnswer asks for the final answer with NO tools attached. Told only
// that it had run out of calls, a model writes what it would do next — a plan,
// posted to the user as an answer — or narrates its own budget. So the
// instruction says there are no tools, and says what the reply must contain.
func (a *Agent) demandAnswer(ctx context.Context, system string, msgs []Turn) (string, error) {
	final := append(append([]Turn(nil), msgs...), Turn{
		Role: "user",
		Text: "Stop investigating. You have NO tools for this message — nothing you say you will check can " +
			"happen, and there is nothing left to decide about how to spend calls. Write the finished answer " +
			"now, from what you already have: the findings with their numbers, what they mean, the most " +
			"likely cause and how to confirm it, and what to do. If the question asked for a report with " +
			"sections, produce every section from what you have; leave a cell you could not measure empty and " +
			"give the reason once under its table, never 'n/a' in the cell. Then one last line, 'Not checked:', " +
			"listing what you would have looked at next. Do not describe your own progress, your budget, or what you were about to do: none of " +
			"that is an answer, and the reader sees only this message.",
	})
	resp, err := a.llm.Complete(ctx, Request{System: system, Messages: final})
	if err != nil {
		return "", err
	}
	text := resp.Text
	if resp.Truncated {
		if t, ferr := a.finish(ctx, system, final, nil, text); ferr == nil {
			text = t
		}
	}
	return text, nil
}

// partialLister is implemented by a tool source that can report which of its
// own servers failed. Optional: a source without it is simply assumed whole.
type partialLister interface {
	ListFailures() []string
}

// finish asks for the remainder of a cut-off answer rather than a fresh one.
// maxContinuations bounds it; a model that keeps running out needs a larger maxTokens.
const maxContinuations = 3

func (a *Agent) finish(ctx context.Context, system string, msgs []Turn, tools []Tool, text string) (string, error) {
	for i := 0; i < maxContinuations; i++ {
		a.log.Info("answer hit the model's output limit; asking for the rest",
			"continuation", i+1, "runes", len([]rune(text)))
		metrics.AnswerContinuations.Inc()
		next := append(append([]Turn(nil), msgs...), Turn{
			Role: "user",
			Text: "Your answer stopped at the output limit, mid-sentence. Continue it from exactly where it " +
				"broke off. Do not repeat what you already wrote, do not restate the question, and do not " +
				"start over — write only the remainder.",
		})
		resp, err := a.llm.Complete(ctx, Request{System: system, Messages: next, Tools: tools})
		if err != nil {
			return "", err
		}
		text += resp.Text
		if !resp.Truncated {
			return text, nil
		}
		msgs = append(next, Turn{Role: "assistant", Text: resp.Text})
	}
	return text + "\n\n_[answer cut off at the model's output limit]_", nil
}

// buildTools lists every authorized cluster's tools, merged by name (see
// route), and returns them, the routes back to each cluster's binding, and a
// notice for the prompt. It errors only when every cluster fails.
func (a *Agent) buildTools(ctx context.Context, clusters []ClusterTools) ([]Tool, *catalog, string, error) {
	var lists []clusterList
	var present, unreachable, degraded []string
	namedClusters := map[string]bool{}
	inventory := map[string][]string{}
	var firstErr error
	for _, ct := range clusters {
		ts, err := ct.MCP.ListTools(ctx)
		if err != nil {
			a.log.Error("list cluster tools", "cluster", ct.Cluster, "err", err)
			unreachable = append(unreachable, ct.Cluster)
			if firstErr == nil {
				firstErr = fmt.Errorf("%s: %w", ct.Cluster, err)
			}
			continue
		}
		if ct.Alias == "" {
			ct.Alias = ct.Cluster
		}
		present = append(present, ct.Cluster)
		// A cluster can answer with only some of its servers. The tools of the
		// ones that did not answer are simply absent, and absent is what a
		// cluster that never had them looks like too.
		if pl, ok := ct.MCP.(partialLister); ok {
			for _, srv := range pl.ListFailures() {
				degraded = append(degraded, ct.Cluster+" ("+srv+")")
			}
		}
		namedClusters[ct.Cluster] = ct.Preferred
		lists = append(lists, clusterList{ct: ct, tools: ts})
		// Name and count each cluster's servers, so the model reasons from the
		// list it holds rather than from memory.
		perServer := map[string]int{}
		var order []string
		for _, t := range ts {
			if !ct.ClusterAdmin && a.enforcer.ClusterAdminOnly(t.Name) {
				continue
			}
			if perServer[t.Server] == 0 {
				order = append(order, t.Server)
			}
			perServer[t.Server]++
		}
		for _, srv := range order {
			inventory[ct.Cluster] = append(inventory[ct.Cluster], fmt.Sprintf("%s (%d)", srv, perServer[srv]))
		}
	}
	if len(lists) == 0 && firstErr != nil {
		return nil, nil, "", firstErr
	}

	routes, order := a.buildRoutes(lists)
	var groups []clusterTools
	idx := map[string]int{}
	for _, r := range order {
		if len(r.clusters) == 0 {
			continue
		}
		i, ok := idx[r.server]
		if !ok {
			i = len(groups)
			idx[r.server] = i
			groups = append(groups, clusterTools{cluster: r.server})
		}
		groups[i].tools = append(groups[i].tools, r.tool)
		groups[i].preferred = groups[i].preferred || r.preferred
	}
	// Within a server, configured priority decides what stays direct when the
	// list does not fit; ties keep the server's order.
	for _, g := range groups {
		sort.SliceStable(g.tools, func(i, j int) bool {
			return a.priority(routes[g.tools[i].Name]) > a.priority(routes[g.tools[j].Name])
		})
	}
	max := a.budgets.MaxTools
	total := 0
	for _, g := range groups {
		total += len(g.tools)
	}
	direct, _ := interleave(groups, max)
	if max > 0 && total > max {
		direct = nil
		if room := max - bridgeTools; room > 0 {
			direct, _ = interleave(groups, room)
		}
	}
	cat := newCatalog(routes, order, direct)
	tools := append(direct, cat.bridge()...)

	offered := make(map[string]bool, len(direct))
	for _, t := range direct {
		offered[t.Name] = true
	}
	perCluster := map[string]int{}
	dropped := map[string]int{}
	for _, r := range order {
		for _, c := range r.clusters {
			if offered[r.name] {
				perCluster[c]++
			} else {
				dropped[c]++
			}
		}
	}
	// Per-cluster counts, every turn: the only way to tell from outside whether
	// a cluster's servers are all advertising what they should.
	for c, n := range perCluster {
		metrics.ToolsPerCluster.WithLabelValues(c).Set(float64(n))
	}
	for cluster, n := range dropped {
		a.log.Info("tools deferred to tool_search to fit the request budget",
			"cluster", cluster, "deferred", n, "maxTools", max, "named", namedClusters[cluster])
		metrics.ToolsDropped.WithLabelValues(cluster).Add(float64(n))
	}
	for _, c := range present {
		a.log.Info("cluster tools offered", "cluster", c, "named", namedClusters[c],
			"tools", perCluster[c], "servers", strings.Join(inventory[c], " "))
	}
	// defBytes is what the tool array weighs on the wire: the count is what the
	// endpoint limits, the size is what makes a turn expensive.
	defBytes := 0
	if b, err := json.Marshal(tools); err == nil {
		defBytes = len(b)
	}
	a.log.Info("tools offered", "tools", len(tools), "clusters", len(present),
		"unreachable", len(unreachable), "maxTools", max,
		"defBytes", defBytes, "last", lastName(tools))
	metrics.ToolsOffered.Set(float64(len(tools)))
	if len(degraded) > 0 {
		a.log.Warn("some of a cluster's MCP servers did not answer; its tool list is incomplete this turn",
			"servers", degraded)
	}
	return tools, cat, toolNotice(present, unreachable, degraded, cat.directory(), inventory, clusterArgUsed(tools)), nil
}

// priority is a route's configured trim priority.
func (a *Agent) priority(r *route) int {
	if r == nil {
		return 0
	}
	p := 0
	for _, b := range r.byKey {
		if v := a.enforcer.Priority(b.real); v > p {
			p = v
		}
	}
	return p
}

// clusterList is one cluster's listed tools.
type clusterList struct {
	ct    ClusterTools
	tools []Tool
}

func clusterArgUsed(tools []Tool) bool {
	for _, t := range tools {
		if props, ok := t.InputSchema["properties"].(map[string]any); ok && props[clusterArg] != nil {
			return true
		}
	}
	return false
}

// filtered enforces namespace scope on a result before the model sees it,
// failing closed when IPs cannot be resolved. Reports whether anything was dropped.
func (a *Agent) filtered(ctx context.Context, b binding, callID, out string) (ToolResult, bool) {
	// Too large to filter safely (parsing multiplies memory). Withhold rather than
	// truncate: truncated JSON cannot be filtered.
	if len(out) > a.budgets.FilterBytes {
		metrics.ToolCalls.WithLabelValues(b.ct.Cluster, b.real, "filtered").Inc()
		return errResult(callID, fmt.Sprintf(
			"result was %d MB, too large to authorize safely and withheld. "+
				"Narrow the query: a namespace, a node, a selector, or a smaller limit.",
			len(out)>>20)), true
	}

	_, ips := ExtractRefs(out)
	var resolved map[string][]string
	if len(ips) > 0 {
		if a.resolver == nil {
			return a.withheld(callID, b, "cannot verify IP ownership"), true
		}
		r, err := a.resolver.ResolveIPs(ctx, b.ct.Cluster, ips)
		if err != nil {
			// Resolver down = fail-closed withholding: WARN (handled), not ERROR.
			a.log.Warn("ip resolve failed; withholding", "cluster", b.ct.Cluster, "err", err)
			return a.withheld(callID, b, "resource resolution unavailable"), true
		}
		resolved = r
	}

	body, removed, blocked := FilterResult(out, b.allowed, resolved)
	if blocked {
		return a.withheld(callID, b, "result references namespaces you cannot access"), true
	}
	if removed > 0 {
		body += fmt.Sprintf("\n\n[authorization: %d record(s) in namespaces you cannot access were withheld]", removed)
	}
	return ToolResult{CallID: callID, Content: a.capResult(b.real, body)}, removed > 0
}

func (a *Agent) withheld(callID string, b binding, why string) ToolResult {
	return ToolResult{
		CallID: callID,
		Content: fmt.Sprintf("authorization: result withheld (%s). On cluster %s you may only access namespaces: %s. Narrow your query to those.",
			why, b.ct.Cluster, strings.Join(b.ct.Allowed, ", ")),
		IsError: true,
	}
}

func errResult(callID, msg string) ToolResult {
	return ToolResult{CallID: callID, Content: msg, IsError: true}
}

var nameUnsafe = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

// qualify builds a unique, valid (<=64 char, [a-zA-Z0-9_-]) tool name of the
// form "<alias>__<tool>", disambiguating collisions with a numeric suffix.
func qualify(alias, name string, reg map[string]binding) string {
	base := nameUnsafe.ReplaceAllString(alias, "_") + "__" + nameUnsafe.ReplaceAllString(name, "_")
	if len(base) > 64 {
		base = base[:64]
	}
	q := base
	for i := 1; ; i++ {
		if _, exists := reg[q]; !exists {
			return q
		}
		suffix := fmt.Sprintf("_%d", i)
		cut := 64 - len(suffix)
		if cut > len(base) {
			cut = len(base)
		}
		q = base[:cut] + suffix
	}
}

// pinPromQL rewrites every PromQL argument of a tool call so its selectors are
// restricted to the caller's namespaces. Cluster-admins are not rewritten: they
// reach this code only for tools that are not ClusterAdminOnly, and their scope
// is the whole cluster anyway.
func (a *Agent) pinPromQL(tool string, args map[string]any, allowed []string) error {
	rule, _ := a.enforcer.ruleFor(tool)
	for _, name := range rule.PromQLArgs {
		raw, ok := args[name].(string)
		if !ok || strings.TrimSpace(raw) == "" {
			continue
		}
		pinned, err := PinNamespaces(raw, allowed)
		if err != nil {
			return err
		}
		if pinned != raw {
			a.log.Debug("pinned a metrics query to the caller's namespaces",
				"tool", tool, "arg", name, "before", raw, "after", pinned)
		}
		args[name] = pinned
	}
	return nil
}
