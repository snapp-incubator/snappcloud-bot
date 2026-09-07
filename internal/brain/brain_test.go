package brain

import (
	"strings"
	"testing"

	"github.com/snapp-incubator/snappcloud-bot/internal/agent"
	"github.com/snapp-incubator/snappcloud-bot/internal/authzclient"
)

func TestSystemPromptIncludesGuidanceAndScope(t *testing.T) {
	b := &Brain{persona: "PERSONA", system: "SYSTEM", guidance: "TOOL_SKILLS"}
	out := b.systemPrompt(authzclient.Scope{"okd4-teh-1": {Namespaces: []string{"team-a", "team-b"}}}, "")
	if !strings.Contains(out, "PERSONA") || !strings.Contains(out, "SYSTEM") || !strings.Contains(out, "TOOL_SKILLS") {
		t.Fatal("persona + system + guidance must be present")
	}
	if !strings.Contains(out, "okd4-teh-1: team-a, team-b") {
		t.Fatalf("scope not listed: %s", out)
	}
}

// An admin-only global group must be invisible to non-admins: not denied at
// call time, absent from the tool list entirely.
func TestAdminOnlyGlobalGroupHiddenFromNonAdmins(t *testing.T) {
	b := &Brain{
		global:          map[string]agent.MCP{"docs": nil, "platform-docs": nil},
		globalAdminOnly: map[string]bool{"platform-docs": true},
	}

	if got := b.visibleGlobal(false); len(got) != 1 || got[0] != "docs" {
		t.Errorf("non-admin sees %v, want [docs]", got)
	}
	if got := b.visibleGlobal(true); len(got) != 2 {
		t.Errorf("admin sees %v, want both groups", got)
	}
}

func TestHasClusterWide(t *testing.T) {
	none := authzclient.Scope{"a": {Namespaces: []string{"x"}}, "b": {Namespaces: []string{"y"}}}
	if none.HasClusterWide() {
		t.Error("no cluster-wide grant reported as admin")
	}
	some := authzclient.Scope{"a": {Namespaces: []string{"x"}}, "b": {ClusterWide: true}}
	if !some.HasClusterWide() {
		t.Error("cluster-wide grant on one cluster not reported")
	}
}
