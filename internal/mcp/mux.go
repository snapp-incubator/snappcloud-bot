package mcp

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/snapp-incubator/snappcloud-bot/internal/metrics"
)

// Mux fans a cluster's several MCP servers (cilium, envoy, docs) into one tool
// surface: it aggregates their tools and routes each tool call to the server
// that owns it. A server that fails to list is skipped (best-effort) so one dead
// server doesn't sink the whole cluster.
type Mux struct {
	servers []named
	log     *slog.Logger
	mu      sync.Mutex
	owner   map[string]*Client // tool name -> owning client
}

type named struct {
	name   string
	client *Client
}

// NewMux builds a mux over named servers. log may be nil.
func NewMux(log *slog.Logger) *Mux {
	if log == nil {
		log = slog.Default()
	}
	return &Mux{owner: map[string]*Client{}, log: log}
}

// Add registers a server under a name (for logs).
func (m *Mux) Add(name string, c *Client) { m.servers = append(m.servers, named{name, c}) }

// ListTools aggregates tools from every server, first-writer-wins on name
// collisions, and records ownership for routing.
func (m *Mux) ListTools(ctx context.Context) ([]Tool, error) {
	owner := map[string]*Client{}
	var tools []Tool
	var firstErr error
	ok := false
	for _, s := range m.servers {
		ts, err := s.client.ListTools(ctx)
		if err != nil {
			// A server that fails here simply has no tools this turn, and the
			// answer will say "no such tool for this cluster" as if that were
			// the configuration. That is indistinguishable from a real gap
			// unless it is recorded somewhere, so record it: the name says
			// which cluster and which server, the metric makes it alertable.
			m.log.Warn("mcp server unavailable; its tools are missing this turn",
				"server", s.name, "url", s.client.URL(), "err", err)
			metrics.MCPListFailures.WithLabelValues(s.name).Inc()
			if firstErr == nil {
				firstErr = fmt.Errorf("%s: %w", s.name, err)
			}
			continue
		}
		ok = true
		m.log.Debug("mcp server listed", "server", s.name, "url", s.client.URL(), "tools", len(ts))
		for _, t := range ts {
			if _, dup := owner[t.Name]; dup {
				continue
			}
			owner[t.Name] = s.client
			// Stamp the tool with its owning server's identity-aware flag so the
			// agent can skip namespace enforcement for self-authorized servers.
			t.SelfAuthorized = s.client.SelfAuthorized()
			t.Unscoped = s.client.Unscoped()
			tools = append(tools, t)
		}
	}
	if !ok && firstErr != nil {
		return nil, firstErr // every server failed
	}
	m.mu.Lock()
	m.owner = owner
	m.mu.Unlock()
	return tools, nil
}

// CallTool routes to the server that advertised the tool.
func (m *Mux) CallTool(ctx context.Context, name string, args map[string]any) (string, error) {
	m.mu.Lock()
	c := m.owner[name]
	m.mu.Unlock()
	if c == nil {
		return "", fmt.Errorf("unknown tool %q", name)
	}
	return c.CallTool(ctx, name, args)
}
