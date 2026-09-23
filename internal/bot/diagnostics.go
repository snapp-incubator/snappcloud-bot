package bot

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/snapp-incubator/snappcloud-bot/internal/brain"
	"github.com/snapp-incubator/snappcloud-bot/internal/mattermost"
	"github.com/snapp-incubator/snappcloud-bot/internal/version"
)

// Every complaint about a thin or wrong answer so far has been diagnosed by
// reading the answer's wording and guessing: a server that is down, one
// refusing the bot's credentials, one whose tools were trimmed to fit the
// request, and one that genuinely lacks a tool all look the same from chat.
// They look completely different here. `diagnostics` puts that in front of the
// person asking, in the same place they noticed the problem.

var diagnosticVerbs = map[string]bool{
	"diagnostics": true, "diagnostic": true, "tools": true,
	"tool status": true, "mcp status": true, "server status": true, "self test": true,
}

// diagnosticsCommand reports every MCP server the caller's clusters have, with
// its tool count or the error that stopped it, plus the limits that shape an
// answer. Cluster-admins only: it exposes server URLs and the bot's own
// configuration, neither of which is a tenant's business.
func (s *Service) diagnosticsCommand(ctx context.Context, identity string, p mattermost.Post, query string) (bool, string) {
	if !diagnosticVerbs[normalizeCommand(query)] {
		return false, ""
	}
	reporter, ok := s.brain.(diagnoser)
	if !ok {
		return true, "Diagnostics are not available in this build."
	}
	scope, err := s.resolver.Resolve(ctx, identity)
	if err != nil {
		return true, msgBackendError
	}
	if !scope.HasClusterWide() {
		return true, "Diagnostics are for cluster-admins — they show the bot's MCP servers and its own configuration."
	}
	clusters := scope.Clusters()
	sort.Strings(clusters)
	return true, diagnosticsReport(reporter.Status(ctx, clusters), reporter.Limits)
}

// diagnoser is what the Brain provides for diagnostics, as an interface so the
// report can be tested without MCP servers.
type diagnoser interface {
	Status(ctx context.Context, clusters []string) []brain.ClusterStatus
	Limits() (model, backup string, maxIter, maxTools int)
}

func diagnosticsReport(st []brain.ClusterStatus, limits func() (string, string, int, int)) string {
	var b strings.Builder
	model, backup, maxIter, maxTools := limits()
	b.WriteString("**MCP servers, right now**\n\n")
	b.WriteString("| cluster | server | tools | status |\n|---|---|---:|---|\n")

	total, failing := 0, 0
	for _, c := range st {
		for _, srv := range c.Servers {
			status := "ok"
			if srv.Err != "" {
				failing++
				status = "**" + firstLine(srv.Err, 90) + "**"
			}
			total += srv.Tools
			fmt.Fprintf(&b, "| %s | %s | %d | %s |\n", c.Cluster, hostOf(srv.URL), srv.Tools, status)
		}
	}
	if len(st) == 0 {
		b.WriteString("| _none_ | | | |\n")
	}

	fmt.Fprintf(&b, "\n%d tools across %d clusters", total, len(st))
	if failing > 0 {
		fmt.Fprintf(&b, "; **%d server(s) not answering** — their tools are missing from every answer until they recover", failing)
	}
	fmt.Fprintf(&b, ".\n\nModel `%s`", model)
	if backup != "" {
		fmt.Fprintf(&b, ", backup `%s`", backup)
	}
	fmt.Fprintf(&b, ". Up to %d tool calls per question, %d tools per request", maxIter, maxTools)
	b.WriteString(" — a question that names its cluster gets that cluster's tools in full, so the limit applies to the others.")
	// The build, because the image tag is republished in place: "1.7.0" does
	// not say which 1.7.0, and a pod that has not been restarted since a fix
	// answers exactly like one that has.
	fmt.Fprintf(&b, "\n\nBuild %s.", version.String())
	return b.String()
}

// hostOf keeps the table readable: the host says which server and which
// cluster, and the scheme and path never differ.
func hostOf(url string) string {
	u := strings.TrimPrefix(strings.TrimPrefix(url, "https://"), "http://")
	if i := strings.IndexByte(u, '/'); i > 0 {
		u = u[:i]
	}
	return u
}

func firstLine(s string, max int) string {
	if i := strings.IndexByte(s, '\n'); i > 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	if len([]rune(s)) > max {
		s = string([]rune(s)[:max-1]) + "…"
	}
	return s
}
