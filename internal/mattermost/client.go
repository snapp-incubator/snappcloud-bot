// Package mattermost is a minimal client for the bot: a REST client to resolve
// user identity and post replies, and a WebSocket listener for incoming posts.
package mattermost

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

// Client talks to the Mattermost REST API as the bot account.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// NewClient builds a REST client.
func NewClient(baseURL, token string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		http:    &http.Client{Timeout: 15 * time.Second},
	}
}

// User is the subset of a Mattermost user we need.
type User struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Email    string `json:"email"`
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<10))
		return fmt.Errorf("mattermost %s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(msg)))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// Me returns the bot's own user (to ignore its own posts and strip its mention).
func (c *Client) Me(ctx context.Context) (User, error) {
	var u User
	err := c.do(ctx, http.MethodGet, "/api/v4/users/me", nil, &u)
	return u, err
}

// GetUser resolves a user by id — the source of the authenticated SSO email.
func (c *Client) GetUser(ctx context.Context, userID string) (User, error) {
	var u User
	err := c.do(ctx, http.MethodGet, "/api/v4/users/"+userID, nil, &u)
	return u, err
}

// CreatePost posts a message in a channel. When rootID is non-empty the post is
// a threaded reply under that root.
func (c *Client) CreatePost(ctx context.Context, channelID, message, rootID string) error {
	body := map[string]string{"channel_id": channelID, "message": message}
	if rootID != "" {
		body["root_id"] = rootID
	}
	return c.do(ctx, http.MethodPost, "/api/v4/posts", body, nil)
}
