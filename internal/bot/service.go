// Package bot is the orchestration core of the SnappCloud bot. For each incoming
// Mattermost message it resolves the user's SSO identity, asks the per-region
// mcp-authz APIs which namespaces the user may access on each cluster, and — only
// if the user is authorized somewhere — forwards the query to Dify with that
// per-cluster scope, then posts the answer. An unauthorized user never reaches
// Dify.
package bot

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/snapp-incubator/snappcloud-bot/internal/authzclient"
	"github.com/snapp-incubator/snappcloud-bot/internal/mattermost"
)

type difyClient interface {
	Chat(ctx context.Context, user, query string, inputs map[string]any) (string, error)
}

type mmClient interface {
	GetUser(ctx context.Context, userID string) (mattermost.User, error)
	CreatePost(ctx context.Context, channelID, message, rootID string) error
}

// Service ties identity, authorization, and the Dify workflow together.
type Service struct {
	mm             mmClient
	dify           difyClient
	resolver       authzclient.Resolver
	identityMap    map[string]string
	botUsername    string
	requireMention bool
	log            *slog.Logger
}

// New builds the orchestration service.
func New(mm mmClient, d difyClient, resolver authzclient.Resolver, identityMap map[string]string, botUsername string, requireMention bool, log *slog.Logger) *Service {
	return &Service{
		mm:             mm,
		dify:           d,
		resolver:       resolver,
		identityMap:    identityMap,
		botUsername:    botUsername,
		requireMention: requireMention,
		log:            log,
	}
}

const (
	msgUnauthorized = "You are not authorized: you have no namespaces you can query on any cluster. Contact your cluster admin if this is unexpected."
	msgBackendError = "Authorization is temporarily unavailable. Please try again shortly."
	msgDifyError    = "Sorry — I couldn't complete that request. Please try again."
)

// OnPost handles one incoming Mattermost post.
func (s *Service) OnPost(ctx context.Context, p mattermost.Post) error {
	// Answer every direct message; in channels, only when @-mentioned.
	if !p.IsDirect() && s.requireMention && !p.Mentioned {
		return nil
	}
	query := strings.TrimSpace(s.stripMention(p.Message))
	if query == "" {
		return nil
	}

	// 1. Resolve the authenticated SSO identity.
	user, err := s.mm.GetUser(ctx, p.UserID)
	if err != nil {
		return err
	}
	identity := s.resolveIdentity(user.Email)
	if identity == "" {
		s.replyTo(ctx, p, msgUnauthorized)
		return nil
	}

	// 2. Authorize across all regions (via the per-region mcp-authz APIs).
	scope, err := s.resolver.Resolve(ctx, identity)
	if err != nil {
		s.log.Error("authorize", "user", identity, "err", err)
		s.replyTo(ctx, p, msgBackendError)
		return nil
	}
	if scope.Empty() {
		s.log.Info("denied", "user", identity, "reason", "no allowed namespaces on any cluster")
		s.replyTo(ctx, p, msgUnauthorized)
		return nil
	}
	s.log.Info("authorized", "user", identity, "clusters", scope.Clusters())

	// 3. Forward to Dify, scoped to the allowed clusters/namespaces.
	answer, err := s.dify.Chat(ctx, identity, query, map[string]any{
		"allowed_namespaces": formatScope(scope),
	})
	if err != nil {
		s.log.Error("dify", "user", identity, "err", err)
		s.replyTo(ctx, p, msgDifyError)
		return nil
	}
	s.replyTo(ctx, p, answer)
	return nil
}

// formatScope renders the per-cluster scope as a deterministic, agent-readable
// block, e.g.:
//
//	okd4-teh-1: team-a, team-b
//	okd4-ts-2: team-c
func formatScope(scope authzclient.Scope) string {
	clusters := scope.Clusters()
	lines := make([]string, 0, len(clusters))
	for _, c := range clusters {
		ns := append([]string(nil), scope[c]...)
		sort.Strings(ns)
		lines = append(lines, fmt.Sprintf("%s: %s", c, strings.Join(ns, ", ")))
	}
	return strings.Join(lines, "\n")
}

func (s *Service) stripMention(msg string) string {
	if s.botUsername == "" {
		return msg
	}
	return strings.ReplaceAll(msg, "@"+s.botUsername, "")
}

func (s *Service) resolveIdentity(email string) string {
	email = strings.TrimSpace(email)
	if mapped, ok := s.identityMap[email]; ok {
		return mapped
	}
	return email
}

// replyTo answers a post. In channels it threads the reply under the original
// (mentioned) message; in direct messages it posts plainly.
func (s *Service) replyTo(ctx context.Context, p mattermost.Post, msg string) {
	root := ""
	if !p.IsDirect() {
		root = p.ThreadRoot()
	}
	if err := s.mm.CreatePost(ctx, p.ChannelID, msg, root); err != nil {
		s.log.Error("post reply", "channel", p.ChannelID, "err", err)
	}
}
