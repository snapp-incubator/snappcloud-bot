package agent

import (
	"fmt"
	"sort"
	"strings"
)

// The tool list grows with the number of clusters and is re-sent every round;
// an endpoint that cannot carry it all drops the tail silently. The bot trims
// it itself, fairly, and tells the model what was trimmed.

// clusterTools is one server's tools in its advertised order. Trimming per
// server, not per cluster, keeps the last-configured server from always losing.
type clusterTools struct {
	cluster   string
	preferred bool // the question named this cluster: keep its tools whole
	tools     []Tool
}

// interleave fits the list into max, one tool per server per round, so each
// server keeps its head. Servers of named clusters go first in each round; the
// budget is absolute for everyone. Returns the list and per-cluster drop counts.
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

	var preferred, rest []clusterTools
	for _, g := range groups {
		if g.preferred {
			preferred = append(preferred, g)
		} else {
			rest = append(rest, g)
		}
	}

	// Every server of a cluster the question did not name is held one slot, so
	// a question about teh-1 does not make the docs server disappear — but no
	// more than a quarter of the budget goes to clusters nobody asked about.
	reserve := len(rest)
	if len(preferred) > 0 && reserve > max/4 {
		reserve = max / 4
	}

	var out []Tool
	taken := make(map[string]int, len(groups))
	// One tool per server per round, so a server with 64 tools cannot crowd out
	// one with 15, and each server keeps its head rather than a random slice.
	fill := func(order []clusterTools, limit int) {
		for round := 0; len(out) < limit; round++ {
			progressed := false
			for _, g := range order {
				if round >= len(g.tools) {
					continue
				}
				progressed = true
				out = append(out, g.tools[round])
				taken[g.cluster]++
				if len(out) == limit {
					break
				}
			}
			if !progressed {
				return
			}
		}
	}
	fill(preferred, max-reserve)
	fill(rest, max)

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
func toolNotice(present, unreachable, degraded []string, dropped map[string]int, inventory map[string][]string, clusterArgUsed bool) string {
	if len(present) == 0 && len(unreachable) == 0 && len(degraded) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\nTools in this turn: ")
	if len(present) > 0 {
		b.WriteString("you have tools for " + strings.Join(present, ", ") + ".")
		// Named and counted per server, because a model that reasons about
		// its tools from memory rather than from the list will report a
		// capability missing while holding it.
		for _, c := range present {
			if srv := inventory[c]; len(srv) > 0 {
				fmt.Fprintf(&b, "\n- %s answered with %s.", c, strings.Join(srv, ", "))
			}
		}
		b.WriteString("\nThose servers' tools are in your tool list for this message. Before you report a " +
			"capability as unavailable, look for it there under the name it actually has — a name you " +
			"remember from elsewhere may not be the name here, and a tool you do not find under a guessed " +
			"name is not a tool you do not have.")
		if clusterArgUsed {
			b.WriteString(" A tool that runs on more than one of these clusters is listed once and takes a \"cluster\" " +
				"argument; its schema lists the clusters it runs on. For a cross-cluster question, call it once per cluster.")
		}
	} else {
		b.WriteString("no cluster's tools could be listed.")
	}
	if len(unreachable) > 0 {
		b.WriteString(" The MCP servers for " + strings.Join(unreachable, ", ") +
			" did not respond this turn, so their tools are missing. That is an outage on the bot's side, " +
			"NOT a limit on the user's access and NOT a cluster that lacks tooling: say the cluster could not " +
			"be reached right now and that it is being retried, and never suggest the user lacks access to it.")
	}
	if len(degraded) > 0 {
		sort.Strings(degraded)
		b.WriteString(" Part of a cluster is missing this turn: the servers " + strings.Join(degraded, ", ") +
			" did not answer, so the tools they provide — which may be the metrics tools, the network tools or " +
			"any other group — are not in your list. Those clusters still HAVE those tools; they are unreachable " +
			"right now. Say that a tool was unavailable this run and which part of the answer it would have " +
			"covered. Do not say the cluster has no such tool, has no metrics, or is not configured for it.")
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

// lastName is the final tool definition in the array — the one an endpoint that
// truncates drops first. Logging it turns "the model says it has no metrics
// tool" into a question with an answer: if the list ends where it should, the
// tools were sent.
func lastName(tools []Tool) string {
	if len(tools) == 0 {
		return ""
	}
	return tools[len(tools)-1].Name
}
