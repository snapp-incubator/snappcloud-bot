// Package dify is a thin client for invoking the SnappCloud Bot Dify workflow,
// called only after a query is authorized.
package dify

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client invokes a Dify advanced-chat app via /chat-messages.
type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

// NewClient builds a Dify client. baseURL must include the /v1 suffix.
func NewClient(baseURL, apiKey string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		http:    &http.Client{Timeout: 5 * time.Minute},
	}
}

type chatRequest struct {
	Inputs       map[string]any `json:"inputs"`
	Query        string         `json:"query"`
	ResponseMode string         `json:"response_mode"`
	User         string         `json:"user"`
}

// Chat sends a query to the workflow as user, with inputs supplying the
// authorized namespace scope, and returns the full agent answer.
//
// It uses streaming mode and concatenates every answer chunk. An agent run emits
// the answer in several parts (reasoning, post-tool synthesis); blocking mode
// returns only the first part, so the bot would otherwise drop the rest.
func (c *Client) Chat(ctx context.Context, user, query string, inputs map[string]any) (string, error) {
	body, err := json.Marshal(chatRequest{
		Inputs:       inputs,
		Query:        query,
		ResponseMode: "streaming",
		User:         user,
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat-messages", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return "", fmt.Errorf("dify chat-messages: %s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	return readStream(resp.Body)
}

// streamEvent is the subset of a Dify SSE event we read.
type streamEvent struct {
	Event   string `json:"event"`
	Answer  string `json:"answer"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// readStream parses the SSE body and returns the concatenated answer. Text
// arrives in "message"/"agent_message" events; "message_replace" replaces the
// whole answer (moderation); "error" aborts.
func readStream(r io.Reader) (string, error) {
	var b strings.Builder
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadString('\n')
		if data, ok := strings.CutPrefix(strings.TrimSpace(line), "data:"); ok {
			data = strings.TrimSpace(data)
			if data != "" && data != "[DONE]" {
				var ev streamEvent
				if json.Unmarshal([]byte(data), &ev) == nil {
					switch ev.Event {
					case "message", "agent_message":
						b.WriteString(ev.Answer)
					case "message_replace":
						b.Reset()
						b.WriteString(ev.Answer)
					case "error":
						return "", fmt.Errorf("dify stream error: %s %s", ev.Code, ev.Message)
					}
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return "", fmt.Errorf("read dify stream: %w", err)
		}
	}
	answer := strings.TrimSpace(b.String())
	if answer == "" {
		return "", fmt.Errorf("dify returned an empty answer")
	}
	return answer, nil
}
