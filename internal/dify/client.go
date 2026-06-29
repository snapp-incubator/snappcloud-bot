// Package dify is a thin client for invoking the SnappCloud Bot Dify workflow,
// called only after a query is authorized.
package dify

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ErrEmptyAnswer means the workflow produced no answer text — usually a stream
// cut before any content (upstream timeout) or an agent that emitted nothing.
// It is retryable.
var ErrEmptyAnswer = errors.New("dify returned no answer")

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
		// Long: a multi-tool agent run streams for a while. The total cap must
		// exceed any single run; the upstream ingress idle/response timeouts are
		// the real backstop.
		http: &http.Client{Timeout: 15 * time.Minute},
	}
}

type chatRequest struct {
	Inputs         map[string]any `json:"inputs"`
	Query          string         `json:"query"`
	ResponseMode   string         `json:"response_mode"`
	User           string         `json:"user"`
	ConversationID string         `json:"conversation_id,omitempty"`
}

// Reply is the result of a Chat call: the answer plus the Dify conversation id
// to reuse on the next message for memory.
type Reply struct {
	Answer         string
	ConversationID string
}

// Chat sends a query to the workflow as user. conversationID continues an
// existing Dify conversation (memory); pass "" to start a new one. inputs supply
// the authorized namespace scope. It streams and concatenates every answer chunk
// (an agent run emits the answer in several parts), and returns the answer plus
// the conversation id to reuse next time.
//
// On an empty/cut answer the error is ErrEmptyAnswer (retryable); the returned
// Reply.ConversationID is still set when Dify provided one, so a retry continues
// the same conversation.
func (c *Client) Chat(ctx context.Context, user, query, conversationID string, inputs map[string]any) (Reply, error) {
	body, err := json.Marshal(chatRequest{
		Inputs:         inputs,
		Query:          query,
		ResponseMode:   "streaming",
		User:           user,
		ConversationID: conversationID,
	})
	if err != nil {
		return Reply{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat-messages", bytes.NewReader(body))
	if err != nil {
		return Reply{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	resp, err := c.http.Do(req)
	if err != nil {
		return Reply{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		// A stale/invalid conversation id is a common 4xx; surface it so the
		// caller can drop the id and retry fresh.
		return Reply{}, fmt.Errorf("dify chat-messages: %s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	return readStream(resp.Body)
}

// streamEvent is the subset of a Dify SSE event we read.
type streamEvent struct {
	Event          string `json:"event"`
	Answer         string `json:"answer"`
	ConversationID string `json:"conversation_id"`
	Code           string `json:"code"`
	Message        string `json:"message"`
}

// readStream parses the SSE body and returns the concatenated answer. Text
// arrives in "message"/"agent_message" events; "message_replace" replaces the
// whole answer (moderation); "error" aborts.
// truncatedNote is appended when the stream ends before Dify signalled a clean
// finish, so the user knows the answer is incomplete (vs. silently partial).
const truncatedNote = "\n\n_⚠️ The response was cut off before Dify finished (the workflow ended the stream early). Try again, or simplify the query._"

func readStream(r io.Reader) (Reply, error) {
	var b strings.Builder
	br := bufio.NewReader(r)
	var readErr error
	var convID string
	finished := false
	for {
		// ReadString returns the (possibly final, newline-less) line *and* the
		// error together — process the line before acting on the error.
		line, err := br.ReadString('\n')
		if data, ok := strings.CutPrefix(strings.TrimSpace(line), "data:"); ok {
			data = strings.TrimSpace(data)
			if data != "" && data != "[DONE]" {
				var ev streamEvent
				if json.Unmarshal([]byte(data), &ev) == nil {
					if ev.ConversationID != "" {
						convID = ev.ConversationID
					}
					switch ev.Event {
					case "message", "agent_message":
						b.WriteString(ev.Answer)
					case "message_replace":
						b.Reset()
						b.WriteString(ev.Answer)
					case "message_end", "workflow_finished":
						finished = true
					case "error":
						return Reply{ConversationID: convID}, fmt.Errorf("dify stream error: %s %s", ev.Code, ev.Message)
					}
				}
			}
		}
		if err != nil {
			if err != io.EOF {
				readErr = err // e.g. unexpected EOF: stream cut mid/after answer
			}
			break
		}
	}

	answer := strings.TrimSpace(b.String())
	if answer != "" {
		// We have content. A non-clean stream end must NOT throw away a complete
		// or partial answer. If Dify never signalled a finish, the answer is
		// truncated — flag it rather than passing it off as complete.
		if !finished {
			answer += truncatedNote
		}
		return Reply{Answer: answer, ConversationID: convID}, nil
	}
	// No content at all — retryable. Include the stream-end cause for logs.
	if readErr != nil {
		return Reply{ConversationID: convID}, fmt.Errorf("%w (stream cut: %v)", ErrEmptyAnswer, readErr)
	}
	return Reply{ConversationID: convID}, ErrEmptyAnswer
}
