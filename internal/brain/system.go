package brain

import (
	"context"
	"fmt"
	"sort"
)

// ClusterNames returns every configured cluster, sorted.
func (b *Brain) ClusterNames() []string {
	out := make([]string, 0, len(b.clusters))
	for c := range b.clusters {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// CallSystemTool calls a tool on a cluster with the bot's OWN authority: there
// is no caller, so no scope, no namespace enforcement and no result filtering.
// It exists for the best-practice report, which reads cluster-wide data and
// gates what it releases itself (cloud-admin preview, then a per-recipient
// access check). Answer never reaches it, so neither can the model.
func (b *Brain) CallSystemTool(ctx context.Context, cluster, tool string, args map[string]any) (string, error) {
	cm, ok := b.clusters[cluster]
	if !ok {
		return "", fmt.Errorf("unknown cluster %q", cluster)
	}
	// Listing first routes the call, and turns "the server that has it is down
	// or too old" into an error that says so, instead of "unknown tool".
	tools, err := cm.mcp.ListTools(ctx)
	if err != nil {
		return "", fmt.Errorf("list tools: %w", err)
	}
	found := false
	for _, t := range tools {
		found = found || t.Name == tool
	}
	if !found {
		return "", fmt.Errorf("no MCP server on this cluster serves %s (openshift-mcp down, or older than the tool)", tool)
	}
	return cm.mcp.CallTool(ctx, tool, args)
}
