package agent

import "strings"

import "testing"

func dashSchema() map[string]any {
	return map[string]any{"properties": map[string]any{
		"uid":       map[string]any{"type": "string"},
		"panel_ids": map[string]any{"type": "string"},
	}}
}

func TestUnseenIDsCatchesTheInventedDashboardUID(t *testing.T) {
	seen := map[string]bool{}
	noteQuestionWords(seen, "Daily reliability report for cluster okd4-teh-1 only.")
	got := unseenIDs(dashSchema(), map[string]any{"uid": "test"}, seen)
	if len(got) != 1 || !strings.Contains(got[0], `uid="test"`) {
		t.Fatalf("want the invented uid refused, got %v", got)
	}
	msg := unseenIDMessage("get_dashboard_panel_queries", got)
	for _, want := range []string{"was NOT called", "search_dashboards", "list_datasources", "not looked it up"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q:\n%s", want, msg)
		}
	}
}

func TestUnseenIDsAllowsAUIDASearchReturned(t *testing.T) {
	seen := map[string]bool{}
	noteIDs(seen, `{"dashboards":[{"uid":"aK3n-9xZr","title":"Ingress"}]}`)
	if got := unseenIDs(dashSchema(), map[string]any{"uid": "aK3n-9xZr"}, seen); len(got) != 0 {
		t.Fatalf("a uid from a result must pass, got %v", got)
	}
}

func TestUnseenIDsAllowsADatasourceUIDFromAList(t *testing.T) {
	seen := map[string]bool{}
	noteIDs(seen, `[{"uid":"P5DCFC7561CCDE821","type":"prometheus"}]`)
	schema := map[string]any{"properties": map[string]any{
		"datasourceUid": map[string]any{"type": "string"},
		"expr":          map[string]any{"type": "string"},
	}}
	args := map[string]any{"datasourceUid": "P5DCFC7561CCDE821", "expr": "count(kube_node_info)"}
	if got := unseenIDs(schema, args, seen); len(got) != 0 {
		t.Fatalf("a datasource uid from list_datasources must pass, got %v", got)
	}
}

func TestUnseenIDsIgnoresArgumentsThatAreNotIdentifiers(t *testing.T) {
	schema := map[string]any{"properties": map[string]any{
		"namespace": map[string]any{"type": "string"},
		"expr":      map[string]any{"type": "string"},
	}}
	args := map[string]any{"namespace": "baly-ode-central", "expr": "up"}
	if got := unseenIDs(schema, args, map[string]bool{}); len(got) != 0 {
		t.Fatalf("only uid arguments are checked, got %v", got)
	}
}

func TestNoteConfiguredIDsTakesUIDsNotEnglish(t *testing.T) {
	seen := map[string]bool{}
	noteConfiguredIDs(seen, "Use the Thanos datasource efoosukv4zthcf for anything over time; "+
		"the latest reading is not a trend and unmeasurable is a finding.")
	if !seen["efoosukv4zthcf"] {
		t.Error("a uid pinned in the prompt must be allowed")
	}
	for _, w := range []string{"test", "latest", "unmeasurable", "datasource"} {
		if seen[w] {
			t.Errorf("%q is a word, not an identifier", w)
		}
	}
}

func TestIdArgsMatchesIdentifierNamesOnly(t *testing.T) {
	schema := map[string]any{"properties": map[string]any{
		"uid": map[string]any{}, "datasourceUid": map[string]any{}, "dashboard_uid": map[string]any{},
		"expr": map[string]any{}, "liquid": map[string]any{}, "uidmap": map[string]any{},
	}}
	got := idArgs(schema)
	want := map[string]bool{"uid": true, "datasourceUid": true, "dashboard_uid": true}
	if len(got) != len(want) {
		t.Fatalf("got %v, want exactly %v", got, want)
	}
	for _, g := range got {
		if !want[g] {
			t.Errorf("%q is not an identifier argument", g)
		}
	}
}
