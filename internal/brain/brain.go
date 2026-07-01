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
	system   string
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
	Clusters     []Cluster
	Rules        map[string]agent.ToolRule
	MCPTimeout   time.Duration
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

	ag := agent.New(llm.New(o.LLM), agent.NewEnforcer(o.Rules), o.Resolver, o.MaxIter, log)
	system := o.SystemPrompt
	if strings.TrimSpace(system) == "" {
		system = defaultSystem
	}
	return &Brain{agent: ag, clusters: clusters, system: system, log: log}
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
	if len(cts) == 0 {
		return "", ErrNoClusterTools
	}
	return b.agent.Run(ctx, agent.Input{
		System:   b.systemPrompt(scope, history),
		Query:    query,
		Clusters: cts,
	})
}

// systemPrompt appends the caller's per-cluster scope (so the model knows which
// clusters/namespaces it may reason about) and any prior transcript (memory).
func (b *Brain) systemPrompt(scope authzclient.Scope, history string) string {
	var sb strings.Builder
	sb.WriteString(b.system)
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

const defaultSystem = `You are the SnappCloud network assistant. You answer questions about cluster networking, ingress, and traffic using the provided MCP tools (Cilium/Hubble flows, Envoy/Contour config). Investigate with the tools — resolve the resources a question is about even when the user does not name a namespace. Be concise and factual; do not narrate your reasoning or restate the question. If a result is withheld for authorization, tell the user which namespaces they may query instead.`

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
