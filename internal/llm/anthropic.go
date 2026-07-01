// Package llm is an Anthropic Messages API client (tool use) that implements the
// agent.LLM interface. Points at any Anthropic-style endpoint (e.g.
// https://llm.snapp.tech/anthropic).
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/snapp-incubator/snappcloud-bot/internal/agent"
)

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
	return &Client{
		baseURL:   strings.TrimRight(o.BaseURL, "/"),
		apiKey:    o.APIKey,
		model:     o.Model,
		maxTokens: o.MaxTokens,
		version:   o.Version,
		http:      &http.Client{Timeout: o.Timeout},
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
}

type response struct {
	Content []block `json:"content"`
	Error   *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// Complete implements agent.LLM.
func (c *Client) Complete(ctx context.Context, req agent.Request) (agent.Response, error) {
	body, err := json.Marshal(request{
		Model:     c.model,
		MaxTokens: c.maxTokens,
		System:    req.System,
		Messages:  toMessages(req.Messages),
		Tools:     toTools(req.Tools),
	})
	if err != nil {
		return agent.Response{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return agent.Response{}, err
	}
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("x-api-key", c.apiKey)
	httpReq.Header.Set("anthropic-version", c.version)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return agent.Response{}, fmt.Errorf("messages request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return agent.Response{}, fmt.Errorf("messages http %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out response
	if err := json.Unmarshal(raw, &out); err != nil {
		return agent.Response{}, fmt.Errorf("decode response: %w", err)
	}
	if out.Error != nil {
		return agent.Response{}, fmt.Errorf("llm error: %s", out.Error.Message)
	}

	var r agent.Response
	for _, b := range out.Content {
		switch b.Type {
		case "text":
			r.Text += b.Text
		case "tool_use":
			args := map[string]any{}
			if len(b.Input) > 0 {
				_ = json.Unmarshal(b.Input, &args)
			}
			r.Calls = append(r.Calls, agent.ToolCall{ID: b.ID, Name: b.Name, Args: args})
		}
	}
	return r, nil
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
