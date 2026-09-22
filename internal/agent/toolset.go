package agent

import (
	"fmt"
	"sort"
	"strings"
)

// The tool list is the one part of a request that grows with the number of
// clusters rather than with the question, and it is sent again on every round.
// An endpoint that will not carry them all drops the tail without saying so:
// the clusters listed last simply have no tools, and the model — which cannot
// tell a trimmed list from a short one — reports that the cluster has none.
// That is how a report for teh-1 came back saying every tool it could see
// belonged to box.
//
// So the bot trims the list itself. Taking one tool from each cluster in turn
// until the budget is spent means a cluster with 140 tools cannot crowd out a
// cluster with 8, and what is lost is the deep tail of the largest servers
// rather than every tool of the last cluster. What was trimmed is then stated
// to the model, because a silently short list is exactly the failure being
// fixed.

// clusterTools is one server's share of the tool list, in the order that
// server advertised them (a server's own order is its priority). Grouping by
// server matters: a cluster's servers are configured in some order, and
// trimming a flat per-cluster list would always take from whichever server was
// configured last — which is how the metrics server, added most recently,
// would be the first thing to disappear from a report about metrics.
type clusterTools struct {
	cluster   string
	preferred bool // the question named this cluster: keep its tools whole
	tools     []Tool
}

// interleave fits the tool list into max. Clusters the question actually named
// are served first and in full — a report about one cluster must not lose that
// cluster's tools to five it never mentioned — and whatever budget is left is
// shared among the rest by taking one tool from each group in turn, so a
// cluster with a large server cannot crowd out a small one. It returns the
// list and, per cluster, how many tools were left out.
func interleave(groups []clusterTools, max int) ([]Tool, map[string]int) {
	total := 0
	for _, g := range groups {
		total += len(g.tools)
	}
	if max <= 0 || total <= max {
		var out []Tool
		for _, g := range groups {
			out = append(out, g.tools...)
		}
		return out, nil
	}

	out := make([]Tool, 0, max)
	taken := make(map[string]int, len(groups))
	var rest []clusterTools
	for _, g := range groups {
		if !g.preferred {
			rest = append(rest, g)
			continue
		}
		for _, t := range g.tools {
			if len(out) == max {
				break
			}
			out = append(out, t)
			taken[g.cluster]++
		}
	}

	for round := 0; len(out) < max; round++ {
		progressed := false
		for _, g := range rest {
			if round >= len(g.tools) {
				continue
			}
			progressed = true
			out = append(out, g.tools[round])
			taken[g.cluster]++
			if len(out) == max {
				break
			}
		}
		if !progressed {
			break
		}
	}

	dropped := make(map[string]int)
	per := make(map[string]int, len(groups))
	for _, g := range groups {
		per[g.cluster] += len(g.tools)
	}
	for cluster, n := range per {
		if d := n - taken[cluster]; d > 0 {
			dropped[cluster] = d
		}
	}
	return out, dropped
}

// toolNotice tells the model which clusters it has tools for this turn, which
// are unreachable, and which had tools trimmed — so it never has to infer any
// of that from the shape of its own tool list, and never reports a transient
// outage as an absence of access.
func toolNotice(present, unreachable []string, dropped map[string]int) string {
	if len(present) == 0 && len(unreachable) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\nTools in this turn: ")
	if len(present) > 0 {
		b.WriteString("you have tools for " + strings.Join(present, ", ") + ".")
	} else {
		b.WriteString("no cluster's tools could be listed.")
	}
	if len(unreachable) > 0 {
		b.WriteString(" The MCP servers for " + strings.Join(unreachable, ", ") +
			" did not respond this turn, so their tools are missing. That is an outage on the bot's side, " +
			"NOT a limit on the user's access and NOT a cluster that lacks tooling: say the cluster could not " +
			"be reached right now and that it is being retried, and never suggest the user lacks access to it.")
	}
	if len(dropped) > 0 {
		keys := make([]string, 0, len(dropped))
		for k := range dropped {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%s (%d)", k, dropped[k]))
		}
		b.WriteString(" Some tools were left out to fit the request, so a cluster's list here is not its full " +
			"set: " + strings.Join(parts, ", ") + ". If the tool you need is missing for a cluster you do have, " +
			"say so plainly rather than answering from another cluster.")
	}
	return b.String()
}
