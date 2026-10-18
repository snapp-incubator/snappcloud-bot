package brain

import (
	"context"
	"strings"
	"testing"

	"github.com/snapp-incubator/snappcloud-bot/internal/agent"
)

type staticMCP struct {
	tools  []string
	called string
}

func (m *staticMCP) ListTools(context.Context) ([]agent.Tool, error) {
	out := make([]agent.Tool, 0, len(m.tools))
	for _, t := range m.tools {
		out = append(out, agent.Tool{Name: t})
	}
	return out, nil
}

func (m *staticMCP) CallTool(_ context.Context, name string, _ map[string]any) (string, error) {
	m.called = name
	return `{"ok":true}`, nil
}

func TestCallSystemTool(t *testing.T) {
	audit := &staticMCP{tools: []string{"list_pods", "disruption_audit"}}
	old := &staticMCP{tools: []string{"list_pods"}}
	b := &Brain{clusters: map[string]*clusterMCP{"teh-1": {mcp: audit}, "teh-2": {mcp: old}}}

	if got, err := b.CallSystemTool(context.Background(), "teh-1", "disruption_audit", nil); err != nil || got != `{"ok":true}` {
		t.Fatalf("got %q, %v", got, err)
	}
	if audit.called != "disruption_audit" {
		t.Errorf("called %q", audit.called)
	}

	_, err := b.CallSystemTool(context.Background(), "teh-2", "disruption_audit", nil)
	if err == nil || !strings.Contains(err.Error(), "no MCP server on this cluster serves disruption_audit") {
		t.Errorf("a cluster without the tool must say so, got %v", err)
	}
	if old.called != "" {
		t.Error("must not call a tool the cluster does not list")
	}
	if _, err := b.CallSystemTool(context.Background(), "nope", "disruption_audit", nil); err == nil {
		t.Error("unknown cluster must fail")
	}
	if got := b.ClusterNames(); len(got) != 2 || got[0] != "teh-1" {
		t.Errorf("ClusterNames = %v", got)
	}
}
