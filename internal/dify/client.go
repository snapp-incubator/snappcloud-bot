// Package dify is a thin client for invoking the SnappCloud Bot Dify workflow,
// called only after a query is authorized.
package dify

import (
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

type chatResponse struct {
	Answer string `json:"answer"`
}

// Chat sends a query to the workflow as user, with inputs supplying the
// authorized namespace scope, and returns the agent's answer.
func (c *Client) Chat(ctx context.Context, user, query string, inputs map[string]any) (string, error) {
	body, err := json.Marshal(chatRequest{
		Inputs:       inputs,
		Query:        query,
		ResponseMode: "blocking",
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

	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return "", fmt.Errorf("dify chat-messages: %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	var out chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("decode dify response: %w", err)
	}
	return out.Answer, nil
}
