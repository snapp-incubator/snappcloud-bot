package brain

import (
	"strings"
	"testing"

	"github.com/snapp-incubator/snappcloud-bot/internal/authzclient"
)

func TestSystemPromptIncludesGuidanceAndScope(t *testing.T) {
	b := &Brain{persona: "PERSONA", system: "SYSTEM", guidance: "TOOL_SKILLS"}
	out := b.systemPrompt(authzclient.Scope{"okd4-teh-1": {"team-a", "team-b"}}, "")
	if !strings.Contains(out, "PERSONA") || !strings.Contains(out, "SYSTEM") || !strings.Contains(out, "TOOL_SKILLS") {
		t.Fatal("persona + system + guidance must be present")
	}
	if !strings.Contains(out, "okd4-teh-1: team-a, team-b") {
		t.Fatalf("scope not listed: %s", out)
	}
}
