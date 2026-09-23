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
