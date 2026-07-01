// Package brain wires the reasoning model, the per-cluster MCP servers, and the
// namespace enforcer into one thing the bot can call: Answer(scope, query). It
// replaces the Dify workflow — the agent loop, tool calling, and authorization
// filtering all run in-process, so the bot does the hard work itself and can
// answer cross-cluster queries in a single loop.
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
	LLM          llm.Options
	MaxIter      int
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
	return &Brain{agent: ag, clusters: clusters, global: global, system: system, guidance: o.ToolGuidance, log: log}
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
			Cluster: c,
			Alias:   cm.alias,
			Allowed: scope[c],
			MCP:     cm.mcp,
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
	sb.WriteString(b.system)
	if strings.TrimSpace(b.guidance) != "" {
		sb.WriteString("\n\n")
		sb.WriteString(b.guidance)
	}
	sb.WriteString("\n\nThe user is authorized on these clusters and namespaces:\n")
	for _, c := range scope.Clusters() {
		ns := append([]string(nil), scope[c]...)
		sort.Strings(ns)
		fmt.Fprintf(&sb, "- %s: %s\n", c, strings.Join(ns, ", "))
	}
	sb.WriteString("\nEach tool is tagged [cluster X]; call tools on the correct cluster. " +
		"For a cross-cluster question, call the relevant tools on each cluster and combine the results. " +
		"Results for namespaces the user cannot access are withheld automatically — do not mention other namespaces.")
	if strings.TrimSpace(history) != "" {
		sb.WriteString("\n\nConversation so far (for context):\n")
		sb.WriteString(history)
	}
	return sb.String()
}

const defaultSystem = `You are the SnappCloud network assistant. You answer questions about cluster networking, connectivity, ingress, and traffic using the provided MCP tools: Cilium/Hubble (observed flows, drops, network policy, endpoints), Envoy/Contour (ingress, routes, upstream clusters), and the docs.

Be thorough and accurate — a single tool rarely gives the full picture:
- Investigate before answering. For a connectivity or packet-drop question, look at the actual flows (Hubble), the relevant network policies and endpoints (Cilium), and the ingress/route config (Envoy) as applicable, then reconcile them into one answer.
- Call every tool that could be relevant, in parallel when you can. Do not stop at the first result if another tool would confirm, explain the cause, or complete the picture.
- Resolve the resources a question is about even when the user does not name a namespace or exact object: find the pod/service/IP, then query around it.
- Do not ask the user for something you can obtain yourself. When a node- or agent-scoped tool needs a pod to resolve the node (e.g. Cilium BGP/status/datapath tools), pick any pod from a namespace the user is authorized for and use it — do not ask the user to supply one.
- Do not refuse based on assumptions about access. Call the tool; the platform enforces authorization and automatically withholds anything the user may not see. Only report an authorization limit AFTER a tool actually returns a withheld/denied result — then say which namespaces they may query.
- If tools disagree or data is missing, gather more rather than guessing; state any uncertainty.
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
