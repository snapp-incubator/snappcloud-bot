package llm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/snapp-incubator/snappcloud-bot/internal/agent"
)

// sse builds a minimal Anthropic event stream.
func sse(events ...string) string {
	var b strings.Builder
	for _, e := range events {
		fmt.Fprintf(&b, "event: x\ndata: %s\n\n", e)
	}
	return b.String()
}

func TestParseStreamAccumulatesTextAndToolUse(t *testing.T) {
	body := sse(
		`{"type":"message_start"}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello "}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"world"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"t1","name":"get_flows"}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"namespace\":"}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"team-a\"}"}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"}}`,
		`{"type":"message_stop"}`,
	)
	r, err := parseStream(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if r.Text != "Hello world" {
		t.Fatalf("text not fully accumulated: %q", r.Text)
	}
	if len(r.Calls) != 1 || r.Calls[0].Name != "get_flows" || r.Calls[0].Args["namespace"] != "team-a" {
		t.Fatalf("tool_use not reassembled: %+v", r.Calls)
	}
}

func TestParseStreamIncompleteIsRetryable(t *testing.T) {
	// No message_stop / stop_reason → treated as cut → retryable.
	body := sse(
		`{"type":"content_block_start","index":0,"content_block":{"type":"text"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}`,
	)
	_, err := parseStream(strings.NewReader(body))
	var r *retryable
	if !errors.As(err, &r) {
		t.Fatalf("incomplete stream should be retryable, got %v", err)
	}
}

func TestCompleteRetriesThenSucceeds(t *testing.T) {
	var calls int32
	full := sse(
		`{"type":"content_block_start","index":0,"content_block":{"type":"text"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_stop"}`,
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			http.Error(w, "overloaded", http.StatusTooManyRequests) // retryable
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(full))
	}))
	defer srv.Close()

	c := New(Options{BaseURL: srv.URL, APIKey: "k", Model: "m", Timeout: 5 * time.Second})
	r, err := c.Complete(context.Background(), agent.Request{Messages: []agent.Turn{{Role: "user", Text: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	if r.Text != "ok" {
		t.Fatalf("answer: %q", r.Text)
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("expected 1 retry (2 calls), got %d", calls)
	}
}

func TestCompleteDoesNotRetry4xx(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		http.Error(w, "bad request", http.StatusBadRequest)
	}))
	defer srv.Close()

	c := New(Options{BaseURL: srv.URL, APIKey: "k", Model: "m", Timeout: 5 * time.Second})
	_, err := c.Complete(context.Background(), agent.Request{Messages: []agent.Turn{{Role: "user", Text: "hi"}}})
	if err == nil {
		t.Fatal("expected error on 400")
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("4xx must not retry, got %d calls", calls)
	}
}
