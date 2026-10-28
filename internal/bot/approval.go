package bot

import (
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/snapp-incubator/snappcloud-bot/internal/agent"
)

// A proposal waits in its thread for a reply. Anyone who replies "yes" there
// approves it for themselves: the tool then runs with the replier's access, so
// approving grants nothing the replier does not already hold.

// pendingTTL is how long a proposal waits for its approval.
const pendingTTL = 30 * time.Minute

type pending struct {
	proposals []agent.Proposal
	// history is the thread so far, so a replier who never spoke in it (an
	// alert thread) still gets the investigation it continues.
	history string
	expires time.Time
}

type pendingStore struct {
	mu sync.Mutex
	m  map[string]pending
}

func newPendingStore() *pendingStore { return &pendingStore{m: map[string]pending{}} }

func (s *pendingStore) put(thread string, ps []agent.Proposal, history string) {
	if thread == "" || len(ps) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for k, p := range s.m {
		if now.After(p.expires) {
			delete(s.m, k)
		}
	}
	s.m[thread] = pending{proposals: ps, history: history, expires: now.Add(pendingTTL)}
}

// take removes and returns the thread's live proposal.
func (s *pendingStore) take(thread string) (pending, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.m[thread]
	delete(s.m, thread)
	if !ok || time.Now().After(p.expires) {
		return pending{}, false
	}
	return p, true
}

func (s *pendingStore) has(thread string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.m[thread]
	return ok && time.Now().Before(p.expires)
}

// threadKey names the thread a post belongs to, for every participant alike.
func threadKey(channelID, root string) string { return channelID + "|" + root }

var approvals = map[string]bool{
	"yes": true, "y": true, "yes please": true, "ok": true, "okay": true, "approve": true, "approved": true,
	"go": true, "go ahead": true, "do it": true, "run it": true, "sure": true, "yep": true,
	"بله": true, "آره": true, "باشه": true, "اوکی": true, "تایید": true, "تأیید": true,
}

var declines = map[string]bool{
	"no": true, "n": true, "nope": true, "cancel": true, "don't": true, "dont": true, "stop": true,
	"نه": true, "خیر": true,
}

// replyKind reads a reply as an approval, a decline, or neither. Only a
// reply that is nothing else counts: "yes, but check X first" is a question.
func replyKind(text string) (approve, decline bool) {
	t := strings.ToLower(strings.TrimSpace(strings.TrimFunc(text, func(r rune) bool {
		return unicode.IsPunct(r) || unicode.IsSpace(r)
	})))
	t = strings.Join(strings.Fields(t), " ")
	return approvals[t], declines[t]
}

// approvedQuery is what the agent is asked once the user approved.
func approvedQuery(ps []agent.Proposal) string {
	var b strings.Builder
	b.WriteString("The user approved what you proposed:\n")
	for _, p := range ps {
		fmt.Fprintf(&b, "- %s on cluster %s, namespace %s", p.Tool, p.Cluster, p.Namespace)
		if p.Reason != "" {
			b.WriteString(" (" + p.Reason + ")")
		}
		b.WriteString("\n")
	}
	b.WriteString("Run it now and do the checks you proposed. If it creates something that must be cleaned up " +
		"(a debug pod), finish it when you are done. Then give your findings, starting with what the new checks showed.")
	return b.String()
}
