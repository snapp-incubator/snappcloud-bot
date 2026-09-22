package llm

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/snapp-incubator/snappcloud-bot/internal/agent"
)

func TestLeakedToolCallRecognisesGatewayFormats(t *testing.T) {
	leaks := []string{
		`Datasources mapped. Now checking.<tools><call tool="snappgroup__list_prometheus_metric_names" index="1"><argument key="limit" type="number">30</argument></call></tools>`,
		"<tool_call>\n{\"name\":\"x\"}\n</tool_call>",
		`<minimax:tool_call><invoke name="query_prometheus">`,
		`<|tool_calls_section_begin|><|tool_call_begin|>functions.query:0`,
		`[TOOL_CALLS][{"name":"x"}]`,
		`<function=get_pods>{"namespace":"a"}</function>`,
	}
	for _, s := range leaks {
		if !leakedToolCall(s, 0) {
			t.Errorf("not recognised as a leaked tool call: %q", s)
		}
	}
	for _, s := range []string{
		"The pod is OOMKilled; raise the limit.",
		"Use `<call>` in your YAML? No — use `oc get pods`.",
		"CPU usage table:\n| ns | cores |\n| a | 1.0 |",
	} {
		if leakedToolCall(s, 0) {
			t.Errorf("false positive: %q", s)
		}
	}
	// Markup beside real tool_use blocks is the model's problem, not a leak.
	if leakedToolCall("<tools><call tool=\"x\">", 1) {
		t.Error("a response with parsed calls must not be treated as a leak")
	}
}

// A leaked tool call is an error from the client, so the failover serves the
// request from the backup instead of the agent posting the markup.
func TestCompleteReportsLeakedToolCallAsError(t *testing.T) {
	full := sse(
		`{"type":"content_block_start","index":0,"content_block":{"type":"text"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Checking.<tools><call tool=\"a__b\" index=\"1\"></call></tools>"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_stop"}`,
	)
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(full))
	}))
	defer srv.Close()

	c := New(Options{BaseURL: srv.URL, APIKey: "k", Model: "m", Timeout: 5 * time.Second})
	_, err := c.Complete(context.Background(), agent.Request{Messages: []agent.Turn{{Role: "user", Text: "hi"}}})
	if !errors.Is(err, ErrLeakedToolCall) {
		t.Fatalf("want ErrLeakedToolCall, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("a leak is not retryable on the same model; got %d calls", calls)
	}
}
