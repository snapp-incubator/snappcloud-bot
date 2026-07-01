// Package mcp is a minimal MCP client over the streamable-http transport
// (JSON-RPC POSTed to a `/mcp` endpoint; responses are JSON or SSE). It supports
// exactly what the agent needs: initialize, tools/list, tools/call.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const protocolVersion = "2025-03-26"

// Client talks to one MCP server.
type Client struct {
	url        string
	authHeader string // full Authorization header value ("" = none)
	http       *http.Client

	mu        sync.Mutex
	sessionID string
	initDone  bool
	nextID    int
}

// New builds a client for one MCP server URL. authHeader is the full value of
// the Authorization header (e.g. "Basic abc..."), or "" for no auth.
func New(url, authHeader string, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	return &Client{url: url, authHeader: authHeader, http: &http.Client{Timeout: timeout}}
}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcResponse struct {
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("mcp error %d: %s", e.Code, e.Message) }

// ensureInit performs the initialize handshake once.
func (c *Client) ensureInit(ctx context.Context) error {
	c.mu.Lock()
	done := c.initDone
	c.mu.Unlock()
	if done {
		return nil
	}
	_, err := c.call(ctx, "initialize", map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "snappcloud-bot", "version": "1"},
	})
	if err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	// Fire-and-forget the initialized notification (no id, no response expected).
	_ = c.notify(ctx, "notifications/initialized")
	c.mu.Lock()
	c.initDone = true
	c.mu.Unlock()
	return nil
}

// Tool is one tool advertised by the server.
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// ListTools returns the server's tools.
func (c *Client) ListTools(ctx context.Context) ([]Tool, error) {
	if err := c.ensureInit(ctx); err != nil {
		return nil, err
	}
	raw, err := c.call(ctx, "tools/list", map[string]any{})
	if err != nil {
		return nil, err
	}
	var out struct {
		Tools []Tool `json:"tools"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode tools: %w", err)
	}
	return out.Tools, nil
}

// CallTool invokes a tool and returns its concatenated text content.
func (c *Client) CallTool(ctx context.Context, name string, args map[string]any) (string, error) {
	if err := c.ensureInit(ctx); err != nil {
		return "", err
	}
	raw, err := c.call(ctx, "tools/call", map[string]any{"name": name, "arguments": args})
	if err != nil {
		return "", err
	}
	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("decode tool result: %w", err)
	}
	var b strings.Builder
	for _, part := range out.Content {
		if part.Type == "text" {
			b.WriteString(part.Text)
		}
	}
	if out.IsError {
		return "", fmt.Errorf("tool reported error: %s", b.String())
	}
	return b.String(), nil
}

// notify sends a notification (no id, no result).
func (c *Client) notify(ctx context.Context, method string) error {
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method})
	req, err := c.newRequest(ctx, body)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	return nil
}

// call sends a JSON-RPC request and returns its result, handling JSON or SSE.
func (c *Client) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	c.mu.Unlock()

	body, _ := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params})
	req, err := c.newRequest(ctx, body)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("post %s: %w", method, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		c.mu.Lock()
		c.sessionID = sid
		c.mu.Unlock()
	}
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("%s: http %d: %s", method, resp.StatusCode, strings.TrimSpace(string(b)))
	}

	rpc, err := decodeResponse(resp, id)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", method, err)
	}
	if rpc.Error != nil {
		return nil, rpc.Error
	}
	return rpc.Result, nil
}

func (c *Client) newRequest(ctx context.Context, body []byte) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if c.authHeader != "" {
		req.Header.Set("Authorization", c.authHeader)
	}
	c.mu.Lock()
	sid := c.sessionID
	c.mu.Unlock()
	if sid != "" {
		req.Header.Set("Mcp-Session-Id", sid)
	}
	return req, nil
}

// decodeResponse reads either a single JSON response or an SSE stream, returning
// the JSON-RPC response whose id matches.
func decodeResponse(resp *http.Response, id int) (*rpcResponse, error) {
	ct := resp.Header.Get("Content-Type")
	if strings.Contains(ct, "text/event-stream") {
		return decodeSSE(resp.Body, id)
	}
	var rpc rpcResponse
	if err := json.NewDecoder(resp.Body).Decode(&rpc); err != nil {
		return nil, fmt.Errorf("decode json: %w", err)
	}
	return &rpc, nil
}

// decodeSSE scans `data:` events for the JSON-RPC response with the given id.
func decodeSSE(r io.Reader, id int) (*rpcResponse, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}
		var probe struct {
			ID *int `json:"id"`
		}
		if json.Unmarshal([]byte(payload), &probe) != nil || probe.ID == nil || *probe.ID != id {
			continue // a notification or another message
		}
		var rpc rpcResponse
		if err := json.Unmarshal([]byte(payload), &rpc); err != nil {
			return nil, fmt.Errorf("decode sse json: %w", err)
		}
		return &rpc, nil
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read sse: %w", err)
	}
	return nil, fmt.Errorf("no response for id %d in stream", id)
}
