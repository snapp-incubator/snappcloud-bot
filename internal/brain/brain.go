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
	global   agent.MCP              // namespace-agnostic tools (docs); nil if none
	persona  string                 // who the bot is + how to greet/help (leads the prompt)
	system   string
	guidance string // MCP tool-usage guidance ("skills"), appended to every prompt
	log      *slog.Logger
}

type clusterMCP struct {
	alias string
	mcp   agent.MCP
}

// Server describes one MCP endpoint for a cluster.
type Server struct {
	URL        string
	AuthHeader string
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
			mux.Add(name, mcp.New(s.URL, s.AuthHeader, o.MCPTimeout))
		}
		alias := c.Alias
		if alias == "" {
			alias = c.Name
		}
		clusters[c.Name] = &clusterMCP{alias: alias, mcp: muxAdapter{mux}}
	}

	var global agent.MCP
	if len(o.GlobalServers) > 0 {
		mux := mcp.NewMux()
		for i, s := range o.GlobalServers {
			mux.Add(fmt.Sprintf("global-%d", i), mcp.New(s.URL, s.AuthHeader, o.MCPTimeout))
		}
		global = muxAdapter{mux}
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

// Answer runs the agent over every authorized cluster that has MCP tools and
// returns the final text. history is a prior-conversation transcript ("" for a
// fresh thread) used for memory.
func (b *Brain) Answer(ctx context.Context, scope authzclient.Scope, query, history string) (string, error) {
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
	// Global (docs) tools are available to any authorized user, unfiltered.
	if b.global != nil {
		cts = append(cts, agent.ClusterTools{Cluster: "docs", Alias: "docs", MCP: b.global, NoEnforce: true})
	}
	if len(cts) == 0 {
		return "", ErrNoClusterTools
	}
	return b.agent.Run(ctx, agent.Input{
		System:   b.systemPrompt(scope, history),
		Query:    query,
		Clusters: cts,
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
	sb.WriteString("\n\nThe user is authorized on these clusters and namespaces:\n")
	for _, c := range scope.Clusters() {
		ns := append([]string(nil), scope[c].Namespaces...)
		sort.Strings(ns)
		fmt.Fprintf(&sb, "- %s: %s\n", c, strings.Join(ns, ", "))
	}
	sb.WriteString("\nEach tool is tagged [cluster X]; call tools on the correct cluster. " +
		"For a cross-cluster question, call the relevant tools on each cluster and combine the results. " +
		"Results for namespaces the user cannot access are withheld automatically — do not mention other namespaces. " +
		"The namespaces listed above are the complete set the user may access; if asked which namespaces they have, answer from this list directly — never call a tool to enumerate namespaces, and never imply the cluster has only these namespaces.")
	if strings.TrimSpace(history) != "" {
		sb.WriteString("\n\nConversation so far (for context):\n")
		sb.WriteString(history)
	}
	return sb.String()
}

const defaultPersona = `You are SnappCloud Bot — the assistant for SnappCloud, Snapp's internal cloud platform (OpenShift/OKD across several clusters). You help engineers on Mattermost investigate their workloads and the cluster: pods and crashes, rollouts, quotas, services and routes, logs and events, and networking — connectivity, traffic and packet drops, ingress and routing, network policy — all scoped to the namespaces and clusters they are authorized for. Cluster-infrastructure views (nodes, BGP, agent status) are available to cluster-admins.

When a user greets you (e.g. "hi"), thanks you, or asks what you can do, respond briefly and warmly: introduce yourself in one line, say what you can help with, and offer a few concrete example questions tailored to what they can access, e.g.:
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

Answer concisely and factually. Do not narrate your reasoning or restate the question.`

// muxAdapter converts an *mcp.Mux (returning mcp.Tool) to agent.MCP.
type muxAdapter struct{ mux *mcp.Mux }

func (m muxAdapter) ListTools(ctx context.Context) ([]agent.Tool, error) {
	ts, err := m.mux.ListTools(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]agent.Tool, 0, len(ts))
	for _, t := range ts {
		out = append(out, agent.Tool{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema})
	}
	return out, nil
}

func (m muxAdapter) CallTool(ctx context.Context, name string, args map[string]any) (string, error) {
	return m.mux.CallTool(ctx, name, args)
}
