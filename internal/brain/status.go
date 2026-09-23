package brain

import (
	"context"
	"sort"

	"github.com/snapp-incubator/snappcloud-bot/internal/mcp"
)

// ClusterStatus is one cluster's MCP servers as the bot sees them now.
type ClusterStatus struct {
	Cluster string
	Servers []mcp.ServerStatus
}

// Status reports, for every cluster the caller can use, which of its MCP
// servers answer and how many tools each advertises. It is the ground truth
// behind "the bot says it has no tool for that": a server that is down, one
// that is refusing the bot's credentials, and one that genuinely does not
// offer a tool look identical from a chat answer and completely different
// here.
func (b *Brain) Status(ctx context.Context, clusters []string) []ClusterStatus {
	out := make([]ClusterStatus, 0, len(clusters))
	for _, c := range clusters {
		cm, ok := b.clusters[c]
		if !ok {
			continue
		}
		mx, ok := cm.mcp.(muxAdapter)
		if !ok {
			continue
		}
		out = append(out, ClusterStatus{Cluster: c, Servers: mx.mux.Status(ctx)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Cluster < out[j].Cluster })
	return out
}

// Limits reports the settings that decide how much of a question the bot can
// actually do, for the same reason: they explain a thin answer.
func (b *Brain) Limits() (model, backup string, maxIter, maxTools int) {
	return b.model, b.backup, b.maxIter, b.maxTools
}
