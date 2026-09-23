// Delivery is how an answer reaches a user, and it is shared: interactive
// replies, scheduled runs and alert investigations all end here. It used to
// live in scheduling.go, which is why its error line read "post scheduled
// answer" for every alert the bot ever posted.
package bot

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/snapp-incubator/snappcloud-bot/internal/mattermost"
)

// postTimeout bounds delivery of an answer that has already been produced.
const postTimeout = 20 * time.Second

// post writes a message to a channel/thread, splitting long answers.
//
// Delivery runs on a context DETACHED from the caller's: a scheduled run and an
// alert batch are each bounded by a timeout that covers the investigation, and
// an investigation that used most of its budget would otherwise produce an
// answer and then fail to post it — the work paid for, the result thrown away,
// with only a "post answer" error to show for it. The deadline exists to stop
// runaway investigations, not to discard finished ones.
func (s *Service) post(ctx context.Context, channelID, rootID, msg string) error {
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), postTimeout)
	defer cancel()

	parts := splitMessage(msg, maxPostRunes)
	if len(parts) > maxPostParts {
		// Silently posting the first part of an answer is worse than a short
		// one: nothing tells the reader the rest existed.
		s.log.Warn("answer exceeded post-part guard, dropping tail",
			"channel", channelID, "parts", len(parts), "limit", maxPostParts)
		parts = parts[:maxPostParts]
		parts[len(parts)-1] += "\n\n_[answer truncated: too long to post]_"
	}
	for i, part := range parts {
		if strings.TrimSpace(part) == "" {
			continue
		}
		err := s.mm.CreatePost(dctx, channelID, part, rootID)
		if err != nil && rootID != "" {
			// The thread may be gone — an alert post deleted, or a root from a
			// channel that has since been archived. An answer in the channel
			// beats an answer nobody receives, so try again untethered.
			s.log.Warn("threaded post failed; retrying at channel level",
				"channel", channelID, "root", rootID, "err", err)
			rootID = ""
			err = s.mm.CreatePost(dctx, channelID, part, "")
		}
		if err != nil {
			s.log.Error("post answer", "channel", channelID, "root", rootID,
				"part", i+1, "of", len(parts), "err", err)
			return fmt.Errorf("post to %s: %w", channelID, err)
		}
	}
	return nil
}

// Mattermost rejects posts longer than its MaxPostSize (default 16383 chars).
// Split safely below that. maxPostParts is a flood guard only — the whole answer
// is delivered across posts; nothing is shown to the user about splitting.
const (
	maxPostRunes = 16000
	maxPostParts = 40
)

// replyTo answers a post. In channels it threads the reply under the original
// (mentioned) message; in direct messages it posts plainly. Long answers are
// split transparently into multiple posts (Mattermost caps post length); the
// user sees no truncation notice — the full answer is delivered.
func (s *Service) replyTo(ctx context.Context, p mattermost.Post, msg string) {
	root := ""
	if !p.IsDirect() {
		root = p.ThreadRoot()
	}
	parts := splitMessage(msg, maxPostRunes)
	if len(parts) > maxPostParts {
		s.log.Warn("answer exceeded post-part guard, dropping tail", "channel", p.ChannelID, "parts", len(parts))
		parts = parts[:maxPostParts]
		parts[len(parts)-1] += "\n\n_[answer truncated: too long to post]_"
	}
	for _, part := range parts {
		if strings.TrimSpace(part) == "" {
			continue
		}
		if err := s.mm.CreatePost(ctx, p.ChannelID, part, root); err != nil {
			s.log.Error("post reply", "channel", p.ChannelID, "err", err)
			return
		}
	}
}

// splitMessage breaks msg into chunks of at most max runes, preferring line
// boundaries. The whole message is returned across chunks (no truncation).
func splitMessage(msg string, max int) []string {
	if len([]rune(msg)) <= max {
		return []string{msg}
	}
	var parts []string
	var b strings.Builder
	bn := 0 // rune count in b
	flush := func() {
		if bn > 0 {
			parts = append(parts, b.String())
			b.Reset()
			bn = 0
		}
	}
	for _, line := range strings.Split(msg, "\n") {
		lr := []rune(line)
		for len(lr) > max { // a single over-long line: hard-split
			flush()
			parts = append(parts, string(lr[:max]))
			lr = lr[max:]
		}
		if bn+len(lr)+1 > max {
			flush()
		}
		if bn > 0 {
			b.WriteByte('\n')
			bn++
		}
		b.WriteString(string(lr))
		bn += len(lr)
	}
	flush()
	return parts
}

// refreshVerbs are the message texts that trigger a scope-cache refresh.
var refreshVerbs = map[string]bool{
	"refresh": true, "reload": true, "refresh access": true,
	"reload access": true, "refresh my access": true, "sync access": true,
}

// isRefreshCommand reports whether the message is a request to re-check access.
