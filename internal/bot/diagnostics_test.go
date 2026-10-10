package bot

import (
	"strings"
	"testing"

	"github.com/snapp-incubator/snappcloud-bot/internal/brain"
	"github.com/snapp-incubator/snappcloud-bot/internal/mcp"
)

func TestDiagnosticsReportShowsFailingServersAndCounts(t *testing.T) {
	st := []brain.ClusterStatus{
		{Cluster: "okd4-teh-1", Servers: []mcp.ServerStatus{
			{Name: "okd4-teh-1-0", URL: "https://openshift-mcp.apps.private.okd4.teh-1.snappcloud.io/mcp", Tools: 31},
			{Name: "okd4-teh-1-5", URL: "https://cloud-grafana-mcp.apps.private.okd4.teh-1.snappcloud.io/mcp", Tools: 8},
		}},
		{Cluster: "okd4-snappgroup", Servers: []mcp.ServerStatus{
			{Name: "okd4-snappgroup-0", URL: "https://cilium-mcp.apps.inter-venture.snappgroup.teh-1.snappcloud.io/mcp",
				Err: "initialize: initialize: http 403: \nsecond line"},
		}},
	}
	out := diagnosticsReport(st, func() (string, string, int, int) {
		return "minimax/MiniMax-M3", "zai/glm-5.3-flash", 25, 150
	})

	for _, want := range []string{
		"openshift-mcp.apps.private.okd4.teh-1.snappcloud.io | 31",
		"cloud-grafana-mcp.apps.private.okd4.teh-1.snappcloud.io | 8",
		"http 403",
		"39 tools across 2 clusters",
		"1 server(s) not answering",
		"minimax/MiniMax-M3", "zai/glm-5.3-flash", "25 tool calls", "150 tools",
		"Build ", "commit",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "second line") {
		t.Error("multi-line errors must be trimmed to one line")
	}
}

func TestDiagnosticsVerbsRecognised(t *testing.T) {
	for _, v := range []string{"diagnostics", "tools", "TOOLS", "mcp status", "self test"} {
		if !diagnosticVerbs[normalizeCommand(v)] {
			t.Errorf("%q not recognised as a diagnostics command", v)
		}
	}
	for _, v := range []string{"why are my pods crashing", "alerts on", "help"} {
		if diagnosticVerbs[normalizeCommand(v)] {
			t.Errorf("%q must not be a diagnostics command", v)
		}
	}
}

// The report that sent someone to look at five healthy servers. "http 403" with
// an empty body is the same text whether the bot sent a bad credential or none,
// and the two want opposite fixes, so the status cell has to say which.
func TestDiagnosticsNamesTheCredentialCause(t *testing.T) {
	st := []brain.ClusterStatus{{Cluster: "okd4-teh-1", Servers: []mcp.ServerStatus{
		{Name: "a", URL: "https://cilium-mcp.apps.private.okd4.teh-1.snappcloud.io/mcp",
			Err: "initialize: initialize: http 403: ", Cause: "no-credential"},
		{Name: "b", URL: "https://hubble-mcp.apps.private.okd4.teh-1.snappcloud.io/mcp",
			Err: "initialize: initialize: http 401: ", Cause: "credential-refused"},
		{Name: "c", URL: "https://cloud-grafana-mcp.apps.private.okd4.teh-1.snappcloud.io/mcp", Tools: 15},
		// A genuine outage alongside the credential problems, so the report has
		// to describe a mixed set rather than claim nothing is down.
		{Name: "d", URL: "https://argocd-mcp.apps.private.okd4.teh-1.snappcloud.io/mcp",
			Err: "post initialize: dial tcp: i/o timeout"},
	}}}
	out := diagnosticsReport(st, func() (string, string, int, int) { return "m", "b", 40, 128 })

	for _, want := range []string{
		"403 — no credential sent",
		"add this server's auth env var to the bot's Secret",
		"401 — credential refused",
		"rotate this server's token",
		"credential problem rather than an outage",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report is missing %q:\n%s", want, out)
		}
	}
	// "until they recover" is what sent the reader to the servers: nothing here
	// recovers without someone editing a Secret.
	if strings.Contains(out, "until they recover") {
		t.Error("a credential problem does not recover on its own")
	}
}

// When every failure is a credential problem — the real teh-1 case — the report
// says so outright rather than leaving the reader to count rows.
func TestDiagnosticsSaysWhenNothingIsActuallyDown(t *testing.T) {
	servers := make([]mcp.ServerStatus, 0, 5)
	for i := 0; i < 5; i++ {
		servers = append(servers, mcp.ServerStatus{
			Name: string(rune('a' + i)), URL: "https://x.example.com/mcp",
			Err: "initialize: initialize: http 403: ", Cause: "no-credential"})
	}
	out := diagnosticsReport([]brain.ClusterStatus{{Cluster: "okd4-teh-1", Servers: servers}},
		func() (string, string, int, int) { return "m", "", 40, 128 })
	if !strings.Contains(out, "Every one of them is a CREDENTIAL problem, not an outage") {
		t.Errorf("report does not say the servers are healthy:\n%s", out)
	}
}

// An unclassified failure keeps the raw error: a 404 is a missing route and a
// timeout is a network problem, and neither is a Secret to edit.
func TestDiagnosticsKeepsAnUnclassifiedError(t *testing.T) {
	st := []brain.ClusterStatus{{Cluster: "okd4-ts-2", Servers: []mcp.ServerStatus{
		{Name: "a", URL: "https://openshift-mcp.apps.private.okd4.ts-2.staging-snappcloud.io/mcp",
			Err: "initialize: initialize: http 404: "},
	}}}
	out := diagnosticsReport(st, func() (string, string, int, int) { return "m", "", 40, 128 })
	if !strings.Contains(out, "http 404") {
		t.Errorf("the raw error was dropped:\n%s", out)
	}
	for _, unwanted := range []string{"auth env var", "rotate this server's token"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("a 404 was reported as a credential problem (%q):\n%s", unwanted, out)
		}
	}
	if !strings.Contains(out, "until they recover") {
		t.Error("an unclassified outage may still recover on its own")
	}
}
