// Package brain wires the reasoning model, the per-cluster MCP servers, and the
// namespace enforcer into one thing the bot can call: Answer. The agent loop,
// tool calling, and authorization filtering all run in-process, so the bot can
// investigate across clusters in a single loop.
package brain

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/snapp-incubator/snappcloud-bot/internal/agent"
	"github.com/snapp-incubator/snappcloud-bot/internal/authzclient"
	"github.com/snapp-incubator/snappcloud-bot/internal/llm"
	"github.com/snapp-incubator/snappcloud-bot/internal/mcp"
)

// ErrNoClusterTools means the user is authorized somewhere, but no MCP servers
// are configured for any cluster they can access.
var ErrNoClusterTools = errors.New("no MCP tools available for the user's authorized clusters")

// Brain answers user queries via the enforced agent loop.
type Brain struct {
	agent    *agent.Agent
	clusters map[string]*clusterMCP // cluster name -> its tools
	global   map[string]agent.MCP   // alias -> namespace-agnostic tools (e.g. docs); empty if none
	persona  string                 // who the bot is + how to greet/help (leads the prompt)
	system   string
	guidance string // MCP tool-usage guidance ("skills"), appended to every prompt
	log      *slog.Logger
}

type clusterMCP struct {
	alias string
	mcp   agent.MCP
}

// Server describes one MCP endpoint for a cluster or a global group.
type Server struct {
	URL        string
	AuthHeader string
	// Alias groups global servers under a tool tag (default "docs"). Ignored for
	// per-cluster servers.
	Alias string
	// SelfAuthorized marks a trusted, identity-aware server (argocd-mcp): its
	// caller identity is forwarded as X-Remote-User and its tools are trusted to
	// self-authorize, so the agent returns their results unfiltered.
	SelfAuthorized bool
}

// Cluster describes one cluster's MCP servers.
type Cluster struct {
	Name    string
	Alias   string
	Servers []Server
}

// Options builds a Brain.
type Options struct {
	LLM     llm.Options
	MaxIter int
	// Persona is the bot's identity + greeting/help behavior, leading the prompt.
	// A SnappCloud default is used when empty.
	Persona      string
	SystemPrompt string // optional; a sensible default is used when empty
	// ToolGuidance is MCP tool-usage guidance ("skills") appended to every prompt.
	ToolGuidance string
	Clusters     []Cluster
	// GlobalServers are namespace-agnostic MCP servers (e.g. general docs)
	// available to every authorized user regardless of cluster, and NOT
	// scope-filtered.
	GlobalServers []Server
	Rules         map[string]agent.ToolRule
	MCPTimeout    time.Duration
	// Resolver maps IPs -> namespaces for result filtering (the authz client).
	Resolver agent.Resolver
}

// New builds the Brain: the LLM client, the enforcer, and one MCP mux per cluster.
func New(o Options, log *slog.Logger) *Brain {
	clusters := make(map[string]*clusterMCP, len(o.Clusters))
	for _, c := range o.Clusters {
		mux := mcp.NewMux()
		for i, s := range c.Servers {
			name := fmt.Sprintf("%s-%d", c.Name, i)
			mux.Add(name, mcp.New(s.URL, s.AuthHeader, s.SelfAuthorized, o.MCPTimeout))
		}
		alias := c.Alias
		if alias == "" {
			alias = c.Name
		}
		clusters[c.Name] = &clusterMCP{alias: alias, mcp: muxAdapter{mux}}
	}

	// Group global servers by alias (default "docs"): each alias becomes its own
	// namespace-agnostic tool group tagged [alias].
	muxes := make(map[string]*mcp.Mux)
	for i, s := range o.GlobalServers {
		alias := s.Alias
		if alias == "" {
			alias = "docs"
		}
		m, ok := muxes[alias]
		if !ok {
			m = mcp.NewMux()
			muxes[alias] = m
		}
		// Global servers are tenant-independent; never send identity.
		m.Add(fmt.Sprintf("%s-%d", alias, i), mcp.New(s.URL, s.AuthHeader, false, o.MCPTimeout))
	}
	global := make(map[string]agent.MCP, len(muxes))
	for alias, m := range muxes {
		global[alias] = muxAdapter{m}
	}

	ag := agent.New(llm.New(o.LLM), agent.NewEnforcer(o.Rules), o.Resolver, o.MaxIter, log)
	system := o.SystemPrompt
	if strings.TrimSpace(system) == "" {
		system = defaultSystem
	}
	persona := o.Persona
	if strings.TrimSpace(persona) == "" {
		persona = defaultPersona
	}
	return &Brain{agent: ag, clusters: clusters, global: global, persona: persona, system: system, guidance: o.ToolGuidance, log: log}
}

// sortedKeys returns a map's keys sorted, for stable ordering.
func sortedKeys(m map[string]agent.MCP) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Answer runs the agent over every authorized cluster that has MCP tools and
// returns the final text. user is the caller's identity (forwarded to identity-
// aware servers); history is a prior-conversation transcript ("" for a fresh
// thread) used for memory; reqID correlates the turn's log lines.
func (b *Brain) Answer(ctx context.Context, scope authzclient.Scope, user, query, history, reqID string) (string, error) {
	var cts []agent.ClusterTools
	for _, c := range scope.Clusters() {
		cm, ok := b.clusters[c]
		if !ok {
			b.log.Debug("no MCP servers configured for cluster", "cluster", c)
			continue
		}
		cts = append(cts, agent.ClusterTools{
			Cluster:      c,
			Alias:        cm.alias,
			Allowed:      scope[c].Namespaces,
			ClusterAdmin: scope[c].ClusterWide,
			MCP:          cm.mcp,
		})
	}
	// Global tools are available to any authorized user, unfiltered, grouped by
	// alias (e.g. docs). Sorted for stable tool ordering.
	for _, alias := range sortedKeys(b.global) {
		cts = append(cts, agent.ClusterTools{Cluster: alias, Alias: alias, MCP: b.global[alias], NoEnforce: true})
	}
	if len(cts) == 0 {
		return "", ErrNoClusterTools
	}
	return b.agent.Run(ctx, agent.Input{
		System:   b.systemPrompt(scope, history),
		User:     user,
		Query:    query,
		Clusters: cts,
		ReqID:    reqID,
	})
}

// systemPrompt appends the tool guidance, the caller's per-cluster scope, and
// any prior transcript (memory).
func (b *Brain) systemPrompt(scope authzclient.Scope, history string) string {
	var sb strings.Builder
	sb.WriteString(b.persona)
	sb.WriteString("\n\n")
	sb.WriteString(b.system)
	if strings.TrimSpace(b.guidance) != "" {
		sb.WriteString("\n\n")
		sb.WriteString(b.guidance)
	}
	sb.WriteString("\n\nThe user's access (the ONLY clusters and namespaces they can use):\n")
	for _, c := range scope.Clusters() {
		ns := append([]string(nil), scope[c].Namespaces...)
		sort.Strings(ns)
		fmt.Fprintf(&sb, "- %s: %s\n", c, strings.Join(ns, ", "))
	}
	sb.WriteString("\nThis list is exhaustive and per-user: it contains every cluster and namespace this user may access, and NOTHING else. " +
		"If the user asks what access / which clusters / which namespaces they have, answer directly from this list — never call a tool to enumerate, and never mention or imply any cluster or namespace not listed here (other clusters exist but are not this user's business). " +
		"Each cluster tool is tagged [cluster X]; call tools on the correct cluster. " +
		"For a cross-cluster question, call the relevant tools on each cluster and combine the results. " +
		"Results for namespaces the user cannot access are withheld automatically; never imply a cluster has only these namespaces.")
	if len(b.global) > 0 {
		tags := sortedKeys(b.global)
		for i := range tags {
			tags[i] = "[" + tags[i] + "]"
		}
		fmt.Fprintf(&sb, "\n\nGlobal tools tagged %s are NOT tied to any cluster; they cover general SnappCloud/platform how-to and concepts (how to do X, what is Y, where to configure Z). "+
			"ANSWER PRECEDENCE — follow strictly: (1) for a question about a specific running workload, use the cluster tools; (2) if the cluster tools can't answer, or the question is general SnappCloud/platform knowledge, CALL these global %s tools; (3) only if the global tools also have nothing relevant, say you don't have that information and suggest where to look. "+
			"NEVER answer a SnappCloud/platform question from your own training data or general internet knowledge — SnappCloud is internal and your training is stale/wrong about it. If it's about SnappCloud and not covered by cluster data, the global docs tools are the source of truth; consult them before answering, and do not invent an answer when they lack it.", strings.Join(tags, ", "), strings.Join(tags, ", "))
	}
	if strings.TrimSpace(history) != "" {
		sb.WriteString("\n\nConversation so far (for context):\n")
		sb.WriteString(history)
	}
	return sb.String()
}

const defaultPersona = `You are SnappCloud Bot — the assistant for SnappCloud, Snapp's internal cloud platform (OpenShift/OKD across several clusters). You help engineers on Mattermost investigate their workloads and the cluster: pods and crashes, rollouts, quotas, services and routes, logs and events, and networking — connectivity, traffic and packet drops, ingress and routing, network policy — all scoped to the namespaces and clusters they are authorized for. Cluster-infrastructure views (nodes, BGP, agent status) are available to cluster-admins.

When a user greets you (e.g. "hi"), thanks you, or asks what you can do or what access they have, respond briefly and warmly: introduce yourself in one line, then show THEIR specific access — the exact clusters (and namespaces) they can use, taken from the access list in this prompt — and offer a few concrete example questions tailored to those, e.g.:
- "Why is my app crashing in <namespace> on <cluster>?"
- "Why are packets dropping for <pod/namespace> on <cluster>?"
- "Why is <service>'s route returning 503 on <cluster>?"
- "Is my namespace hitting its quota on <cluster>?"
Do not run tools for a plain greeting — just introduce yourself and invite a question. Keep it short.`

const defaultSystem = `You are the SnappCloud cluster assistant. You answer questions about workloads and networking using the provided MCP tools: Kubernetes/OpenShift (pods, workloads, services, routes, events, logs, quotas, nodes), Cilium/Hubble (observed flows, drops, network policy, endpoints), Envoy/Contour (ingress, routes, upstream clusters), and the docs.

Be thorough and accurate — a single tool rarely gives the full picture:
- Investigate before answering. For a connectivity or packet-drop question, look at the actual flows (Hubble), the relevant network policies and endpoints (Cilium), and the ingress/route config (Envoy) as applicable, then reconcile them into one answer.
- Call every tool that could be relevant, in parallel when you can. Do not stop at the first result if another tool would confirm, explain the cause, or complete the picture.
- Resolve the resources a question is about even when the user does not name a namespace or exact object: find the pod/service/IP, then query around it.
- Do not ask the user for something you can obtain yourself. When a node- or agent-scoped tool needs a pod to resolve the node (e.g. Cilium BGP/status/datapath tools), pick any pod from a namespace the user is authorized for and use it — do not ask the user to supply one.
- Do not refuse based on assumptions about access. Call the tool; the platform enforces authorization and automatically withholds anything the user may not see. Only report an authorization limit AFTER a tool actually returns a withheld/denied result — then say which namespaces they may query.
- If tools disagree or data is missing, gather more rather than guessing; state any uncertainty.
- When a result is withheld or a call is denied for authorization, report that as an access limitation on that specific data — NEVER conclude that the thing does not exist or is not configured. Absence of evidence you were not allowed to see is not evidence of absence.
- Cluster-infrastructure tools (nodes, BGP state, agent status) require cluster-admin access; if denied, tell the user those need cluster-admin rather than trying workarounds.
- Each tool is tagged [cluster X]; use the correct cluster's tools. For a cross-cluster question, query each cluster and combine.
- "Project" is ambiguous on this platform: users almost always mean an OpenShift project, which IS a Kubernetes namespace. Treat it as a namespace (OpenShift/Kubernetes tools; pass it as namespace=). An ArgoCD AppProject is a different object (GitOps policy: source repos, destinations, roles) — use ArgoCD project tools/arguments ONLY when the user explicitly means ArgoCD. If a project= lookup finds nothing, retry it as a namespace before concluding it does not exist, and state which meaning you used.

Answer concisely and factually. Do not narrate your reasoning or restate the question.

OUTPUT FORMAT — always: reply with the final answer for the user as plain, human-readable Markdown (short paragraphs, bullet lists, or a small table when it helps). Never output your chain-of-thought, tool-call syntax, function names, raw tool JSON, or control tokens — the user sees your message verbatim in chat. Do NOT wrap the whole reply in a code block; use code fences only for actual commands, code, or log snippets. Every reply must contain a real answer or a clear statement that you couldn't find the information — never send an empty or markup-only message.`

// muxAdapter converts an *mcp.Mux (returning mcp.Tool) to agent.MCP.
type muxAdapter struct{ mux *mcp.Mux }

func (m muxAdapter) ListTools(ctx context.Context) ([]agent.Tool, error) {
	ts, err := m.mux.ListTools(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]agent.Tool, 0, len(ts))
	for _, t := range ts {
		out = append(out, agent.Tool{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema, SelfAuthorized: t.SelfAuthorized})
	}
	return out, nil
}

func (m muxAdapter) CallTool(ctx context.Context, name string, args map[string]any) (string, error) {
	return m.mux.CallTool(ctx, name, args)
}
