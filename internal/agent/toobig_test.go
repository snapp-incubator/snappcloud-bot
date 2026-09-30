package agent

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/snapp-incubator/snappcloud-bot/internal/mcp"
)

func TestNarrowingHintNamesUnsetArguments(t *testing.T) {
	schema := map[string]any{
		"properties": map[string]any{
			"resource_type": map[string]any{"type": "string"},
			"ingress_class": map[string]any{"type": "string"},
			"name_regex":    map[string]any{"type": "string"},
			"pod":           map[string]any{"type": "string"},
		},
		"required": []any{"pod"},
	}
	got := narrowingHint(schema, map[string]any{"pod": "envoy-1", "ingress_class": ""})
	for _, want := range []string{"resource_type", "ingress_class", "name_regex", "call it again"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %q", want, got)
		}
	}
	if strings.Contains(got, "pod") {
		t.Fatalf("a required argument is not a way to narrow: %q", got)
	}
	// Everything already set, or no arguments at all: nothing useful to add.
	if h := narrowingHint(schema, map[string]any{"resource_type": "listeners", "ingress_class": "private", "name_regex": "x"}); h != "" {
		t.Fatalf("expected no hint, got %q", h)
	}
	if h := narrowingHint(map[string]any{}, nil); h != "" {
		t.Fatalf("expected no hint, got %q", h)
	}
}

type tooBigMCP struct{ fakeMCP }

func (t *tooBigMCP) CallTool(_ context.Context, name string, _ map[string]any) (string, error) {
	t.called = append(t.called, name)
	return "", fmt.Errorf("tools/call: tool response exceeded 32 MiB and was refused: %w", mcp.ErrTooLarge)
}

// A refused-for-size result must reach the model as its own remedy: which
// arguments of that tool would have narrowed it. Told only to "narrow the
// query", the model repeated the same call and lost the round.
func TestRunTellsTheModelHowToNarrowARefusedTool(t *testing.T) {
	llm := &fakeLLM{turns: []Response{
		{Calls: []ToolCall{{ID: "1", Name: "c__envoy_config_dump", Args: map[string]any{}}}},
		{Text: "done"},
	}}
	m := &tooBigMCP{}
	m.tools = []string{"envoy_config_dump"}
	m.schema = map[string]any{"properties": map[string]any{
		"resource_type": map[string]any{"type": "string"},
		"ingress_class": map[string]any{"type": "string"},
	}}
	ag := New(llm, NewEnforcer(nil), nil, 4, DefaultBudgets(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := ag.Run(context.Background(), Input{
		Query:    "dump the config",
		Clusters: []ClusterTools{{Cluster: "c", Allowed: []string{"team-a"}, MCP: m}},
	}); err != nil {
		t.Fatal(err)
	}
	res := lastResults(llm)
	if len(res) != 1 || !res[0].IsError {
		t.Fatalf("unexpected result: %+v", res)
	}
	for _, want := range []string{"exceeded 32 MiB", "resource_type", "ingress_class", "do not give up"} {
		if !strings.Contains(res[0].Content, want) {
			t.Fatalf("missing %q in %q", want, res[0].Content)
		}
	}
}

func TestMissingRequiredNamesArgumentsAndDescriptions(t *testing.T) {
	schema := map[string]any{
		"required": []any{"datasourceUid", "expr", "endTime"},
		"properties": map[string]any{
			"datasourceUid": map[string]any{"description": "The UID of the datasource to query"},
			"expr":          map[string]any{"description": "The PromQL expression to query"},
			"endTime":       map[string]any{"description": "The end time. RFC3339 or relative to now (e.g. 'now', 'now-2h')."},
		},
	}
	got := missingRequired(schema, map[string]any{"datasourceUid": "P1", "expr": "up"})
	if len(got) != 1 || got[0] != "endTime" {
		t.Fatalf("missing = %v", got)
	}
	msg := missingRequiredMessage("query_prometheus", schema, got, map[string]any{"datasourceUid": "P1", "expr": "up"})
	for _, want := range []string{
		"query_prometheus was NOT called",
		"the tool itself is fine",
		"You sent: datasourceUid, expr",
		"It requires endTime",
		"relative to now",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("missing %q in %q", want, msg)
		}
	}
	if len(missingRequired(schema, map[string]any{"datasourceUid": "P1", "expr": "up", "endTime": "now"})) != 0 {
		t.Fatal("a complete call must not be reported as missing anything")
	}
	if len(missingRequired(map[string]any{}, nil)) != 0 {
		t.Fatal("a tool with no required arguments never misses any")
	}
}

// The call is not spent: the server never sees a request it would only reject
// in its own vocabulary.
func TestRunRefusesAToolCallMissingRequiredArgumentsWithoutCallingIt(t *testing.T) {
	llm := &fakeLLM{turns: []Response{
		{Calls: []ToolCall{{ID: "1", Name: "c__query_prometheus",
			Args: map[string]any{"datasourceUid": "P1", "expr": "count(kube_node_info)"}}}},
		{Text: "done"},
	}}
	m := &fakeMCP{tools: []string{"query_prometheus"}}
	m.schema = map[string]any{
		"required": []any{"datasourceUid", "expr", "endTime"},
		"properties": map[string]any{
			"endTime": map[string]any{"description": "The end time, e.g. 'now'."},
		},
	}
	ag := New(llm, NewEnforcer(nil), nil, 4, DefaultBudgets(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := ag.Run(context.Background(), Input{
		Query:    "how many nodes?",
		Clusters: []ClusterTools{{Cluster: "c", Allowed: []string{"team-a"}, MCP: m}},
	}); err != nil {
		t.Fatal(err)
	}
	if len(m.called) != 0 {
		t.Fatalf("an incomplete call must not reach the server: %v", m.called)
	}
	res := lastResults(llm)
	if len(res) != 1 || !res[0].IsError || !strings.Contains(res[0].Content, "endTime") ||
		!strings.Contains(res[0].Content, "the tool itself is fine") {
		t.Fatalf("unhelpful result: %+v", res)
	}
}

func TestRetryHintTellsTheModelWhatToDoNext(t *testing.T) {
	if h := retryHint("not_found"); !strings.Contains(h, "Do NOT try another spelling") ||
		!strings.Contains(h, "take the identifier from what it returns") {
		t.Fatalf("not_found hint: %q", h)
	}
	if h := retryHint("auth"); !strings.Contains(h, "not a missing thing") {
		t.Fatalf("auth hint: %q", h)
	}
	if h := retryHint("timeout"); !strings.Contains(h, "never report its subject") {
		t.Fatalf("timeout hint: %q", h)
	}
	if retryHint("bad_args") != "" || retryHint("other") != "" {
		t.Fatal("a failure with no useful next move must add nothing")
	}
}

// A 404 must reach the model as an instruction to look the identifier up.
func TestRunTurnsNotFoundIntoALookupInstruction(t *testing.T) {
	llm := &fakeLLM{turns: []Response{
		{Calls: []ToolCall{{ID: "1", Name: "c__get_dashboard_summary", Args: map[string]any{"uid": "cluster-capacity"}}}},
		{Text: "done"},
	}}
	m := &notFoundMCP{}
	m.tools = []string{"get_dashboard_summary"}
	ag := New(llm, NewEnforcer(nil), nil, 4, DefaultBudgets(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := ag.Run(context.Background(), Input{
		Query:    "capacity",
		Clusters: []ClusterTools{{Cluster: "c", Allowed: []string{"team-a"}, MCP: m}},
	}); err != nil {
		t.Fatal(err)
	}
	res := lastResults(llm)
	if len(res) != 1 || !res[0].IsError {
		t.Fatalf("unexpected result: %+v", res)
	}
	if !strings.Contains(res[0].Content, "Do NOT try another spelling") {
		t.Fatalf("no lookup instruction: %q", res[0].Content)
	}
}

type notFoundMCP struct{ fakeMCP }

func (n *notFoundMCP) CallTool(_ context.Context, name string, _ map[string]any) (string, error) {
	n.called = append(n.called, name)
	return "", fmt.Errorf(`tool reported error: get dashboard "cluster-capacity": 404 Not Found`)
}

// A model reaching for argument names it remembers from another tool is told
// so: the first version of this message was read as a description of the tool
// ("it exposes a datasource uid, not a PromQL endpoint") rather than as an
// instruction, and the capability was reported absent.
func TestMissingRequiredMessageNamesWrongArguments(t *testing.T) {
	schema := map[string]any{
		"required":   []any{"datasourceUid", "expr", "endTime"},
		"properties": map[string]any{"datasourceUid": map[string]any{}, "expr": map[string]any{}, "endTime": map[string]any{}},
	}
	msg := missingRequiredMessage("query_prometheus", schema, []string{"datasourceUid", "endTime", "expr"},
		map[string]any{"query": "up", "time": "now"})
	for _, want := range []string{
		"You sent: query, time",
		"not arguments of this tool at all: query, time",
		"names from a different tool",
		"Call query_prometheus again with exactly those names",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("missing %q in %q", want, msg)
		}
	}
}
