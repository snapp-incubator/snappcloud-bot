package agent

import (
	"context"
	"regexp"
	"strings"

	"github.com/snapp-incubator/snappcloud-bot/internal/metrics"
)

// DefaultReviewPrompt is the instruction for re-checking an unattended answer
// against the tool results in its own transcript.
const DefaultReviewPrompt = "Before this is posted, check your draft above against the tool results in this " +
	"conversation — they are the only evidence. Correct what is wrong; do not restyle what is right.\n" +
	"- A figure that appears in two places is the same figure in both. Where a summary line and a table " +
	"disagree, the table's source decides: fix the summary.\n" +
	"- A number that no tool result contains, and that cannot be computed from them, is removed.\n" +
	"- A count of things — restarts, pods, objects — is a whole number.\n" +
	"- Two columns holding the same value in every row means one was never measured: keep the one you " +
	"measured and say the other was not.\n" +
	"- A cause stated as fact needs a tool result that shows it. If none does, either mark it as a " +
	"hypothesis with the one check that would confirm it, or remove it. Remove any speculation the draft " +
	"itself says is not evidenced.\n" +
	"- Add nothing: no new findings, no new sections. Drop nothing a tool result supports.\n" +
	"Reply with the corrected answer only, complete and in the same layout. No preface, no list of what " +
	"you changed."

// minReviewRatio: a review shorter than this share of the draft dropped content.
const minReviewRatio = 0.6

var structured = regexp.MustCompile(`(?m)^\s*(?:#|\|)`)

// SetReviewPrompt overrides DefaultReviewPrompt; "" keeps the default.
func (a *Agent) SetReviewPrompt(p string) {
	if strings.TrimSpace(p) != "" {
		a.reviewPrompt = p
	}
}

// review returns the draft corrected against its transcript, or the draft
// unchanged when the correction cannot be trusted. It never fails the turn.
func (a *Agent) review(ctx context.Context, reqID string, t transcript, draft string) string {
	lg := a.log.With("req", reqID)
	keep := func(outcome string, extra ...any) string {
		metrics.AnswerReviews.WithLabelValues(outcome).Inc()
		lg.Info("answer review kept the draft", append([]any{"outcome", outcome}, extra...)...)
		return draft
	}

	prompt := a.reviewPrompt
	if prompt == "" {
		prompt = DefaultReviewPrompt
	}
	msgs := reviewMessages(t.msgs, draft, prompt)
	resp, err := a.llm.Complete(ctx, Request{System: t.system, Messages: msgs})
	if err != nil {
		return keep("error", "err", err)
	}
	text := strings.TrimSpace(resp.Text)
	if resp.Truncated {
		full, ferr := a.finish(ctx, t.system, msgs, nil, text)
		if ferr != nil {
			return keep("truncated")
		}
		text = strings.TrimSpace(full)
	}

	d, r := len([]rune(draft)), len([]rune(text))
	switch {
	case r == 0:
		return keep("empty")
	case float64(r) < minReviewRatio*float64(d):
		return keep("shrank", "draftRunes", d, "reviewRunes", r)
	case structured.MatchString(draft) && !structured.MatchString(text):
		return keep("lost-structure")
	}
	metrics.AnswerReviews.WithLabelValues("revised").Inc()
	lg.Info("answer revised against its evidence", "draftRunes", d, "reviewRunes", r)
	return text
}

// reviewMessages ends the transcript with the draft and the instruction,
// dropping a trailing final answer so turns still alternate.
func reviewMessages(msgs []Turn, draft, prompt string) []Turn {
	out := append([]Turn(nil), msgs...)
	for len(out) > 0 && out[len(out)-1].Role == "assistant" && len(out[len(out)-1].Calls) == 0 {
		out = out[:len(out)-1]
	}
	return append(out,
		Turn{Role: "assistant", Text: draft},
		Turn{Role: "user", Text: prompt},
	)
}
