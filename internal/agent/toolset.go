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

// gatewayToolCeiling is how many tool definitions the LLM endpoint carries.
// Past it the tail of the array is dropped and the response says nothing: the
// model is simply never told those tools exist. Measured against
// ai.snapp.tech by asking a model whether it holds a sentinel tool placed last
// — visible at 128 definitions, gone at 129, on minimax-m3, glm-5.3-flash and
// kimi-k2 alike, and independent of their size (16 KB of definitions truncates
// at the same count as 100 KB). glm-4.6 answers 400 instead.
//
// This is a measured property of the endpoint, not a preference, so it bounds
// maxTools rather than being configured next to it: a budget set above it does
// not buy tools, it loses the last server's.
const gatewayToolCeiling = 128

// interleave fits the tool list into max by taking one tool from each SERVER in
// turn: a server with 64 tools cannot crowd out one with 15, and what is lost
// is the deep tail of the largest servers rather than everything one server
// offers. Servers of a cluster the question NAMED go first in each round, so
// the named cluster keeps the most of its own tools, but it is not exempt from
// the budget — a named cluster used to be appended whole on the grounds that an
// oversized list was the endpoint's problem and a loud one. It is not loud.
// Offering 138 tools for teh-1 under a budget of 150 put grafana-mcp, the
// server configured last, past the ceiling: the bot logged 15 metrics tools
// offered, the model was never sent them, and a daily report explained at
// length that the cluster had no Prometheus tool.
//
// A server's own order is its priority, so what each keeps is its head.
//
// It returns the list and, per cluster, how many tools were left out.
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
func toolNotice(present, unreachable, degraded []string, dropped map[string]int, inventory map[string][]string) string {
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

// countFor totals one cluster's tools across its servers.
func countFor(groups []clusterTools, cluster string) int {
	n := 0
	for _, g := range groups {
		if g.cluster == cluster {
			n += len(g.tools)
		}
	}
	return n
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
