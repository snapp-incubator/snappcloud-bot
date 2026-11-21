package bot

import (
	"context"
	"errors"
	"strings"

	"github.com/snapp-incubator/snappcloud-bot/internal/botoauth"
	"github.com/snapp-incubator/snappcloud-bot/internal/mattermost"
)

type Pardis struct {
	Links  *botoauth.Links
	Verify func(ctx context.Context, token, email string) error
	Answer func(ctx context.Context, token, user, query, history, reqID string) (string, error)
}

func (s *Service) SetPardis(p *Pardis) { s.pardis = p }
func (s *Service) pardisDM(ctx context.Context, p mattermost.Post, user mattermost.User, query, id string) error {
	if !p.IsDirect() {
		s.replyTo(ctx, p, "Send me a direct message to query Pardis with your own account.")
		return nil
	}
	if !user.EmailVerified || user.ID != p.UserID || user.Email == "" {
		s.replyTo(ctx, p, "Verify your Mattermost work email before linking Pardis.")
		return nil
	}
	service := s.pardis
	switch strings.ToLower(strings.TrimSpace(query)) {
	case "connect pardis":
		s.replyTo(ctx, p, "Sign in to Pardis with your work account: "+service.Links.Start(user.ID, user.Email))
		return nil
	case "disconnect pardis":
		if err := service.Links.Disconnect(ctx, user.ID); err != nil {
			s.replyTo(ctx, p, "Could not confirm Pardis revocation. Retry disconnect pardis.")
			return nil
		}
		s.replyTo(ctx, p, "Pardis disconnected.")
		return nil
	}
	token, err := service.Links.Token(ctx, user.ID)
	if errors.Is(err, botoauth.ErrUnlinked) {
		s.replyTo(ctx, p, "Send `connect pardis` to link your account.")
		return nil
	}
	if err != nil {
		s.replyTo(ctx, p, "Pardis login unavailable. Try again shortly.")
		return nil
	}
	if err = service.Verify(ctx, token, user.Email); err != nil {
		s.replyTo(ctx, p, "Pardis could not verify your linked identity. Retry or send `connect pardis`.")
		return nil
	}
	key := convKeyFor(user.ID, p)
	history := s.conv.get(key)
	result, err := service.Answer(ctx, token, user.Email, query, history, id)
	if err != nil {
		s.replyTo(ctx, p, msgAgentError)
		return nil
	}
	clean := sanitize(result)
	if clean == "" {
		s.replyTo(ctx, p, msgEmptyAnswer)
		return nil
	}
	s.conv.put(key, appendTranscript(history, query, clean))
	s.replyTo(ctx, p, clean)
	return nil
}
