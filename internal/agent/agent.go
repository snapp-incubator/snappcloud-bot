package agent

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
)

// Tool is an MCP tool exposed to the model.
type Tool struct {
	Name        string
	Description string
	InputSchema map[string]any // JSON Schema object
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
	llm      LLM
	enforcer *Enforcer
	resolver Resolver // may be nil (no IP resolution)
	maxIter  int
	log      *slog.Logger
}

// New builds an Agent. maxIter bounds the tool-calling loop (default 6).
// resolver may be nil.
func New(llm LLM, enforcer *Enforcer, resolver Resolver, maxIter int, log *slog.Logger) *Agent {
	if maxIter <= 0 {
		maxIter = 6
	}
	return &Agent{llm: llm, enforcer: enforcer, resolver: resolver, maxIter: maxIter, log: log}
}

// ClusterTools binds one authorized cluster: the user's scope on it, its MCP
// executor, and a short alias used to prefix tool names so the model can target
// this cluster explicitly. Passing several lets the agent answer cross-cluster
// queries in a single loop — each tool is tagged with its cluster and enforced
// against that cluster's scope.
type ClusterTools struct {
	Cluster string   // full cluster name (resolver + logs)
	Alias   string   // short tool-name prefix (defaults to Cluster)
	Allowed []string // namespaces the user may access on this cluster
	MCP     MCP      // executor for this cluster's tools
	// NoEnforce marks a namespace-agnostic, non-cluster tool source (e.g. general
	// docs): its results are passed through unfiltered and it is not tied to any
	// region's scope. Use only for trusted, tenant-independent content.
	NoEnforce bool
}

// Input is one user query, evaluated across every cluster the user can access.
type Input struct {
	System   string
	Query    string
	Clusters []ClusterTools
}

// binding maps a cluster-qualified tool name back to its cluster + real name.
type binding struct {
	ct      ClusterTools
	real    string
	allowed map[string]bool
}

// Run drives the LLM ↔ MCP loop across every authorized cluster, enforcing each
// tool call against that cluster's scope, and returns the final answer text.
func (a *Agent) Run(ctx context.Context, in Input) (string, error) {
	tools, reg, err := a.buildTools(ctx, in.Clusters)
	if err != nil {
		return "", fmt.Errorf("list tools: %w", err)
	}
	msgs := []Turn{{Role: "user", Text: in.Query}}

	for iter := 0; iter < a.maxIter; iter++ {
		resp, err := a.llm.Complete(ctx, Request{System: in.System, Messages: msgs, Tools: tools})
		if err != nil {
			return "", fmt.Errorf("llm: %w", err)
		}
		msgs = append(msgs, Turn{Role: "assistant", Text: resp.Text, Calls: resp.Calls})
		if len(resp.Calls) == 0 {
			return resp.Text, nil // final answer
		}

		results := make([]ToolResult, 0, len(resp.Calls))
		for _, call := range resp.Calls {
			b, ok := reg[call.Name]
			if !ok {
				results = append(results, errResult(call.ID, "unknown tool "+call.Name))
				continue
			}
			if !b.ct.NoEnforce {
				if err := a.enforcer.Check(b.real, call.Args, b.ct.Allowed); err != nil {
					a.log.Warn("tool call denied", "cluster", b.ct.Cluster, "tool", b.real, "err", err)
					results = append(results, errResult(call.ID, "authorization denied: "+err.Error()))
					continue
				}
			}
			out, cerr := b.ct.MCP.CallTool(ctx, b.real, call.Args)
			if cerr != nil {
				a.log.Error("tool call failed", "cluster", b.ct.Cluster, "tool", b.real, "err", cerr)
				results = append(results, errResult(call.ID, "tool error: "+cerr.Error()))
				continue
			}
			if b.ct.NoEnforce {
				// Trusted namespace-agnostic source (docs) — no filtering.
				results = append(results, ToolResult{CallID: call.ID, Content: out})
			} else {
				results = append(results, a.filtered(ctx, b, call.ID, out))
			}
		}
		msgs = append(msgs, Turn{Role: "user", Results: results})
	}

	// Ran out of iterations — ask the model for a final answer with no tools.
	resp, err := a.llm.Complete(ctx, Request{
		System:   in.System + "\n\nYou have reached the tool-call limit. Answer now with what you have.",
		Messages: msgs,
	})
	if err != nil {
		return "", fmt.Errorf("llm (final): %w", err)
	}
	return resp.Text, nil
}

// buildTools lists every authorized cluster's tools, cluster-qualifying their
// names and tagging descriptions, and returns the LLM tool set plus a registry
// mapping each qualified name back to its cluster + real tool. A cluster whose
// MCP servers all fail to list is skipped; only if EVERY cluster fails is an
// error returned.
func (a *Agent) buildTools(ctx context.Context, clusters []ClusterTools) ([]Tool, map[string]binding, error) {
	var tools []Tool
	reg := make(map[string]binding)
	anyOK := false
	var firstErr error
	for _, ct := range clusters {
		ts, err := ct.MCP.ListTools(ctx)
		if err != nil {
			a.log.Error("list cluster tools", "cluster", ct.Cluster, "err", err)
			if firstErr == nil {
				firstErr = fmt.Errorf("%s: %w", ct.Cluster, err)
			}
			continue
		}
		anyOK = true
		allowed := make(map[string]bool, len(ct.Allowed))
		for _, n := range ct.Allowed {
			allowed[n] = true
		}
		alias := ct.Alias
		if alias == "" {
			alias = ct.Cluster
		}
		for _, t := range ts {
			q := qualify(alias, t.Name, reg)
			reg[q] = binding{ct: ct, real: t.Name, allowed: allowed}
			tools = append(tools, Tool{
				Name:        q,
				Description: fmt.Sprintf("[cluster %s] %s", ct.Cluster, t.Description),
				InputSchema: t.InputSchema,
			})
		}
	}
	if !anyOK && firstErr != nil {
		return nil, nil, firstErr
	}
	return tools, reg, nil
}

// filtered enforces namespace scope on a raw tool result before the model sees
// it. Resolves any IPs to namespaces (fail-closed: if IPs are present and
// resolution fails, the whole result is withheld), then drops records touching
// namespaces outside the tool's cluster scope.
func (a *Agent) filtered(ctx context.Context, b binding, callID, out string) ToolResult {
	_, ips := ExtractRefs(out)
	var resolved map[string][]string
	if len(ips) > 0 {
		if a.resolver == nil {
			return a.withheld(callID, b, "cannot verify IP ownership")
		}
		r, err := a.resolver.ResolveIPs(ctx, b.ct.Cluster, ips)
		if err != nil {
			a.log.Error("resolve ips", "cluster", b.ct.Cluster, "err", err)
			return a.withheld(callID, b, "resource resolution unavailable")
		}
		resolved = r
	}

	body, removed, blocked := FilterResult(out, b.allowed, resolved)
	if blocked {
		return a.withheld(callID, b, "result references namespaces you cannot access")
	}
	if removed > 0 {
		a.log.Info("filtered tool result", "cluster", b.ct.Cluster, "removed", removed)
		body += fmt.Sprintf("\n\n[authorization: %d record(s) in namespaces you cannot access were withheld]", removed)
	}
	return ToolResult{CallID: callID, Content: body}
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
