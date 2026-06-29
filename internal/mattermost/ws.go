package mattermost

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// Post is the subset of a Mattermost post the bot acts on, enriched with two
// fields from the WebSocket event envelope (not the post itself).
type Post struct {
	ID        string `json:"id"`
	UserID    string `json:"user_id"`
	ChannelID string `json:"channel_id"`
	Message   string `json:"message"`
	// RootID is the thread root when the post is itself a reply; empty for a
	// top-level post.
	RootID string `json:"root_id"`

	// ChannelType is the event's channel_type: "D" direct, "G" group, "O" open,
	// "P" private.
	ChannelType string `json:"-"`
	// Mentioned is true when the bot's user id is in the event's mentions.
	Mentioned bool `json:"-"`
}

// IsDirect reports whether the post is in a direct (1:1) message channel.
func (p Post) IsDirect() bool { return p.ChannelType == "D" }

// ThreadRoot returns the post id to reply under so the reply lands in the same
// thread: the existing thread root if the post is already a reply, else the post
// itself.
func (p Post) ThreadRoot() string {
	if p.RootID != "" {
		return p.RootID
	}
	return p.ID
}

// PostHandler is invoked for every incoming post that is not from the bot.
type PostHandler func(ctx context.Context, p Post) error

// Listen connects to the Mattermost WebSocket and dispatches "posted" events to
// h until ctx is cancelled. It reconnects with backoff on any connection error.
func (c *Client) Listen(ctx context.Context, botUserID string, h PostHandler, log *slog.Logger) {
	backoff := time.Second
	for ctx.Err() == nil {
		if err := c.listenOnce(ctx, botUserID, h, log); err != nil && ctx.Err() == nil {
			log.Warn("websocket disconnected, reconnecting", "err", err, "backoff", backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
	}
}

func (c *Client) listenOnce(ctx context.Context, botUserID string, h PostHandler, log *slog.Logger) error {
	wsURL := strings.Replace(c.baseURL, "http", "ws", 1) + "/api/v4/websocket"
	hdr := http.Header{"Authorization": []string{"Bearer " + c.token}}

	conn, _, err := websocket.DefaultDialer.DialContext(ctx, wsURL, hdr)
	if err != nil {
		return fmt.Errorf("dial %s: %w", wsURL, err)
	}
	defer func() { _ = conn.Close() }()
	c.setConn(conn)
	defer c.setConn(nil)
	log.Info("websocket connected", "url", wsURL)

	// done closes when this connection's read loop returns, so the keepalive
	// goroutines exit with it (no leak across reconnects).
	done := make(chan struct{})
	defer close(done)

	// Keepalive: without it an idle connection is dropped by the LB/proxy with a
	// 1006 abnormal closure roughly every minute.
	const pongWait = 90 * time.Second
	const pingPeriod = 60 * time.Second
	_ = conn.SetReadDeadline(time.Now().Add(pongWait))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(pongWait))
	})

	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()

	go func() {
		ticker := time.NewTicker(pingPeriod)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-done:
				return
			case <-ticker.C:
				if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second)); err != nil {
					_ = conn.Close()
					return
				}
			}
		}
	}()

	// Bounded worker pool: handle posts in goroutines so a slow turn (auth + a
	// long Dify agent run, tens of seconds) never blocks the read loop. Blocking
	// the loop starves pong handling and drops the connection.
	sem := make(chan struct{}, maxConcurrentHandlers)

	for {
		var ev struct {
			Event string `json:"event"`
			Data  struct {
				Post        string `json:"post"`
				ChannelType string `json:"channel_type"`
				Mentions    string `json:"mentions"`
			} `json:"data"`
		}
		if err := conn.ReadJSON(&ev); err != nil {
			return err
		}
		_ = conn.SetReadDeadline(time.Now().Add(pongWait))
		if ev.Event != "posted" || ev.Data.Post == "" {
			continue
		}
		var p Post
		if err := json.Unmarshal([]byte(ev.Data.Post), &p); err != nil {
			log.Warn("decode post", "err", err)
			continue
		}
		if p.UserID == botUserID {
			continue
		}
		p.ChannelType = ev.Data.ChannelType
		p.Mentioned = mentions(ev.Data.Mentions, botUserID)

		sem <- struct{}{}
		go func(p Post) {
			defer func() { <-sem }()
			if err := h(ctx, p); err != nil {
				log.Error("handle post", "post", p.ID, "user", p.UserID, "err", err)
			}
		}(p)
	}
}

// maxConcurrentHandlers bounds in-flight message handlers so a flood can't spawn
// unbounded goroutines.
const maxConcurrentHandlers = 16

func (c *Client) setConn(conn *websocket.Conn) {
	c.wsMu.Lock()
	c.wsConn = conn
	c.wsMu.Unlock()
}

// sendTyping emits one user_typing action on the live WebSocket. parentID is the
// thread root (empty for a top-level/DM message). No-op if not connected.
func (c *Client) sendTyping(channelID, parentID string) {
	c.wsMu.Lock()
	defer c.wsMu.Unlock()
	if c.wsConn == nil {
		return
	}
	c.seq++
	_ = c.wsConn.WriteJSON(map[string]any{
		"action": "user_typing",
		"seq":    c.seq,
		"data":   map[string]string{"channel_id": channelID, "parent_id": parentID},
	})
}

// Typing keeps the "bot is typing…" indicator alive in a channel until ctx is
// cancelled. The Mattermost indicator expires after a few seconds, so it is
// re-sent on an interval. Run it in a goroutine for the duration of processing.
func (c *Client) Typing(ctx context.Context, channelID, parentID string) {
	c.sendTyping(channelID, parentID)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.sendTyping(channelID, parentID)
		}
	}
}

// mentions reports whether botUserID is in the event's JSON-array mentions field.
func mentions(raw, botUserID string) bool {
	if raw == "" {
		return false
	}
	var ids []string
	if err := json.Unmarshal([]byte(raw), &ids); err != nil {
		return false
	}
	for _, id := range ids {
		if id == botUserID {
			return true
		}
	}
	return false
}
