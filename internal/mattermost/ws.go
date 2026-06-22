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

	// ChannelType is the event's channel_type: "D" direct, "G" group, "O" open,
	// "P" private.
	ChannelType string `json:"-"`
	// Mentioned is true when the bot's user id is in the event's mentions.
	Mentioned bool `json:"-"`
}

// IsDirect reports whether the post is in a direct (1:1) message channel.
func (p Post) IsDirect() bool { return p.ChannelType == "D" }

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
	log.Info("websocket connected", "url", wsURL)

	// Keepalive: without it an idle connection is dropped by the LB/proxy with a
	// 1006 abnormal closure roughly every minute.
	const pongWait = 90 * time.Second
	const pingPeriod = 60 * time.Second
	_ = conn.SetReadDeadline(time.Now().Add(pongWait))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(pongWait))
	})

	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()

	go func() {
		ticker := time.NewTicker(pingPeriod)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second)); err != nil {
					_ = conn.Close()
					return
				}
			}
		}
	}()

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
		if err := h(ctx, p); err != nil {
			log.Error("handle post", "post", p.ID, "user", p.UserID, "err", err)
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
