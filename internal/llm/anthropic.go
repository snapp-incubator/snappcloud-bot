// Package llm is an Anthropic Messages API client (tool use, streaming) that
// implements the agent.LLM interface. It streams the response over SSE — so no
// part of a long answer is lost to a cut body, periodic pings keep the
// connection alive — and retries transient failures (network errors, 429, 5xx,
// or a stream that ended before the model finished).
package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strings"
	"time"

	"github.com/snapp-incubator/snappcloud-bot/internal/agent"
)

// maxAttempts is the total number of tries per Complete (1 + retries).
const maxAttempts = 4

// Client calls the Anthropic Messages API.
type Client struct {
	baseURL   string
	apiKey    string
	model     string
	maxTokens int
	version   string
	http      *http.Client
}

// Options configures the client.
type Options struct {
	BaseURL   string // e.g. https://llm.snapp.tech/anthropic
	APIKey    string
	Model     string // e.g. zai/glm-5.2
	MaxTokens int    // default 8192
	Version   string // anthropic-version, default 2023-06-01
	Timeout   time.Duration
}

// New builds a client.
func New(o Options) *Client {
	if o.MaxTokens <= 0 {
		o.MaxTokens = 8192
	}
	if o.Version == "" {
		o.Version = "2023-06-01"
	}
	if o.Timeout <= 0 {
		o.Timeout = 10 * time.Minute
	}
	// Keep-alive tuned transport: reuse connections between the agent loop's many
	// requests; HTTP/2 so the long streamed response rides one multiplexed conn.
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	return &Client{
		baseURL:   strings.TrimRight(o.BaseURL, "/"),
		apiKey:    o.APIKey,
		model:     o.Model,
		maxTokens: o.MaxTokens,
		version:   o.Version,
		http:      &http.Client{Timeout: o.Timeout, Transport: transport},
	}
}

// --- wire types ---

type message struct {
	Role    string  `json:"role"`
	Content []block `json:"content"`
}

type block struct {
	Type string `json:"type"`
	// text
	Text string `json:"text,omitempty"`
	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// tool_result
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
}

type tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

type request struct {
	Model     string    `json:"model"`
	MaxTokens int       `json:"max_tokens"`
	System    string    `json:"system,omitempty"`
	Messages  []message `json:"messages"`
	Tools     []tool    `json:"tools,omitempty"`
	Stream    bool      `json:"stream"`
}

// retryable marks a failure worth retrying (transient network/stream/5xx/429).
type retryable struct{ err error }

func (e *retryable) Error() string { return e.err.Error() }
func (e *retryable) Unwrap() error { return e.err }

// Complete implements agent.LLM: send the conversation, stream the reply, retry
// transient failures with exponential backoff + jitter.
func (c *Client) Complete(ctx context.Context, req agent.Request) (agent.Response, error) {
	body, err := json.Marshal(request{
		Model:     c.model,
		MaxTokens: c.maxTokens,
		System:    req.System,
		Messages:  toMessages(req.Messages),
		Tools:     toTools(req.Tools),
		Stream:    true,
	})
	if err != nil {
		return agent.Response{}, err
	}

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		resp, err := c.stream(ctx, body)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		var r *retryable
		if !errors.As(err, &r) || attempt == maxAttempts {
			break
		}
		// Exponential backoff with jitter: ~0.5s, 1s, 2s (+ up to 250ms).
		wait := time.Duration(1<<(attempt-1)) * 500 * time.Millisecond
		wait += time.Duration(rand.Int63n(int64(250 * time.Millisecond)))
		select {
		case <-ctx.Done():
			return agent.Response{}, ctx.Err()
		case <-time.After(wait):
		}
	}
	return agent.Response{}, fmt.Errorf("llm after %d attempts: %w", maxAttempts, lastErr)
}

// stream performs one request and accumulates the full streamed response.
func (c *Client) stream(ctx context.Context, body []byte) (agent.Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return agent.Response{}, err
	}
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("accept", "text/event-stream")
	httpReq.Header.Set("x-api-key", c.apiKey)
	httpReq.Header.Set("anthropic-version", c.version)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return agent.Response{}, &retryable{fmt.Errorf("request: %w", err)} // network — retry
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		e := fmt.Errorf("http %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			return agent.Response{}, &retryable{e}
		}
		return agent.Response{}, e // 4xx (bad request/auth) — permanent
	}
	// Fallback for proxies that ignore stream:true and return one JSON body.
	if ct := resp.Header.Get("Content-Type"); strings.Contains(ct, "application/json") {
		return parseJSON(resp.Body)
	}
	return parseStream(resp.Body)
}

// parseJSON handles a non-streamed Messages response. A read error (truncated
// body) is retryable so no content is lost.
func parseJSON(r io.Reader) (agent.Response, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return agent.Response{}, &retryable{fmt.Errorf("read body: %w", err)}
	}
	var out struct {
		Content []block `json:"content"`
		Error   *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return agent.Response{}, &retryable{fmt.Errorf("decode body: %w", err)}
	}
	if out.Error != nil {
		return agent.Response{}, &retryable{errors.New(out.Error.Message)}
	}
	var r2 agent.Response
	for _, b := range out.Content {
		switch b.Type {
		case "text":
			r2.Text += b.Text
		case "tool_use":
			args := map[string]any{}
			if len(b.Input) > 0 {
				_ = json.Unmarshal(b.Input, &args)
			}
			r2.Calls = append(r2.Calls, agent.ToolCall{ID: b.ID, Name: b.Name, Args: args})
		}
	}
	return r2, nil
}

// blockAcc accumulates one streamed content block.
type blockAcc struct {
	typ     string
	text    strings.Builder
	id      string
	name    string
	partial strings.Builder // tool_use input JSON, streamed as fragments
}

// sseEvent is the subset of Anthropic stream events we consume.
type sseEvent struct {
	Type         string `json:"type"`
	Index        int    `json:"index"`
	ContentBlock *struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"content_block"`
	Delta *struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// parseStream reads the SSE body, accumulating every text/tool_use delta into a
// complete response. If the stream ends before the model signals completion, it
// returns a retryable error so the whole request is retried (nothing partial is
// ever returned to the caller).
func parseStream(r io.Reader) (agent.Response, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

	blocks := map[int]*blockAcc{}
	var order []int
	done := false

	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var ev sseEvent
		if json.Unmarshal([]byte(payload), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "content_block_start":
			b := &blockAcc{}
			if ev.ContentBlock != nil {
				b.typ = ev.ContentBlock.Type
				b.id = ev.ContentBlock.ID
				b.name = ev.ContentBlock.Name
			}
			blocks[ev.Index] = b
			order = append(order, ev.Index)
		case "content_block_delta":
			b := blocks[ev.Index]
			if b == nil || ev.Delta == nil {
				continue
			}
			switch ev.Delta.Type {
			case "text_delta":
				b.text.WriteString(ev.Delta.Text)
			case "input_json_delta":
				b.partial.WriteString(ev.Delta.PartialJSON)
			}
		case "message_delta":
			if ev.Delta != nil && ev.Delta.StopReason != "" {
				done = true
			}
		case "message_stop":
			done = true
		case "error":
			msg := "stream error"
			if ev.Error != nil {
				msg = ev.Error.Message
			}
			// Overloaded/temporary upstream errors are worth retrying.
			return agent.Response{}, &retryable{errors.New(msg)}
		}
	}
	if err := sc.Err(); err != nil {
		return agent.Response{}, &retryable{fmt.Errorf("read stream: %w", err)}
	}
	if !done {
		return agent.Response{}, &retryable{errors.New("stream ended before completion")}
	}

	var out agent.Response
	for _, idx := range order {
		b := blocks[idx]
		switch b.typ {
		case "text":
			out.Text += b.text.String()
		case "tool_use":
			args := map[string]any{}
			if s := b.partial.String(); s != "" {
				_ = json.Unmarshal([]byte(s), &args)
			}
			out.Calls = append(out.Calls, agent.ToolCall{ID: b.id, Name: b.name, Args: args})
		}
	}
	return out, nil
}

func toMessages(turns []agent.Turn) []message {
	msgs := make([]message, 0, len(turns))
	for _, t := range turns {
		switch t.Role {
		case "assistant":
			var blocks []block
			if t.Text != "" {
				blocks = append(blocks, block{Type: "text", Text: t.Text})
			}
			for _, call := range t.Calls {
				in, _ := json.Marshal(call.Args)
				blocks = append(blocks, block{Type: "tool_use", ID: call.ID, Name: call.Name, Input: in})
			}
			msgs = append(msgs, message{Role: "assistant", Content: blocks})
		default: // user
			if len(t.Results) > 0 {
				blocks := make([]block, 0, len(t.Results))
				for _, res := range t.Results {
					blocks = append(blocks, block{
						Type:      "tool_result",
						ToolUseID: res.CallID,
						Content:   res.Content,
						IsError:   res.IsError,
					})
				}
				msgs = append(msgs, message{Role: "user", Content: blocks})
			} else {
				msgs = append(msgs, message{Role: "user", Content: []block{{Type: "text", Text: t.Text}}})
			}
		}
	}
	return msgs
}

func toTools(ts []agent.Tool) []tool {
	if len(ts) == 0 {
		return nil
	}
	out := make([]tool, 0, len(ts))
	for _, t := range ts {
		schema := t.InputSchema
		if schema == nil {
			schema = map[string]any{"type": "object"}
		}
		out = append(out, tool{Name: t.Name, Description: t.Description, InputSchema: schema})
	}
	return out
}
