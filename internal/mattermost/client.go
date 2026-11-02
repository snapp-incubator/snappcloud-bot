// Package mattermost is a minimal client for the bot: a REST client to resolve
// user identity and post replies, and a WebSocket listener for incoming posts.
package mattermost

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Client talks to the Mattermost REST API as the bot account, and (once Listen
// is running) sends typing indicators over the live WebSocket.
type Client struct {
	baseURL string
	token   string
	http    *http.Client

	// wsMu guards the active WebSocket connection and the action sequence. Data
	// writes (typing) must be serialized; the read loop and control pings are
	// separate per gorilla's concurrency rules.
	wsMu   sync.Mutex
	wsConn *websocket.Conn
	seq    int
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
	return c.send(req, out)
}

// send does a request and decodes a JSON response into out (when non-nil).
func (c *Client) send(req *http.Request, out any) error {
	method, path := req.Method, req.URL.Path
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
	_, err := c.CreatePostID(ctx, channelID, message, rootID)
	return err
}

// CreatePostID is CreatePost returning the new post's id, for a post that will
// be the root of a thread.
func (c *Client) CreatePostID(ctx context.Context, channelID, message, rootID string) (string, error) {
	return c.CreatePostFiles(ctx, channelID, message, rootID, nil)
}

// CreatePostFiles is CreatePostID with files attached: ids from UploadFile,
// uploaded to the same channel.
func (c *Client) CreatePostFiles(ctx context.Context, channelID, message, rootID string, fileIDs []string) (string, error) {
	body := map[string]any{"channel_id": channelID, "message": message}
	if rootID != "" {
		body["root_id"] = rootID
	}
	if len(fileIDs) > 0 {
		body["file_ids"] = fileIDs
	}
	var out struct {
		ID string `json:"id"`
	}
	err := c.do(ctx, http.MethodPost, "/api/v4/posts", body, &out)
	return out.ID, err
}

// UploadFile stores a file in a channel and returns its id, for a post to
// attach with CreatePostFiles. Until a post attaches it, nobody sees it.
func (c *Client) UploadFile(ctx context.Context, channelID, name, contentType string, data []byte) (string, error) {
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	if err := w.WriteField("channel_id", channelID); err != nil {
		return "", err
	}
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{"name": "files", "filename": name}))
	h.Set("Content-Type", contentType)
	part, err := w.CreatePart(h)
	if err != nil {
		return "", err
	}
	if _, err := part.Write(data); err != nil {
		return "", err
	}
	if err := w.Close(); err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/v4/files", &b)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", w.FormDataContentType())
	var out struct {
		FileInfos []struct {
			ID string `json:"id"`
		} `json:"file_infos"`
	}
	if err := c.send(req, &out); err != nil {
		return "", err
	}
	if len(out.FileInfos) == 0 || out.FileInfos[0].ID == "" {
		return "", fmt.Errorf("mattermost POST /api/v4/files: no file in the response")
	}
	return out.FileInfos[0].ID, nil
}
