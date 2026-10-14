package brain

import "context"

type reviewKey struct{}

// WithReview marks a request as unattended: its answer is posted where nobody
// can ask a follow-up, so it is checked against its own evidence first (when
// Options.ReviewUnattended allows). A context value rather than a parameter,
// because the four callers of Answer share one signature and only two of them
// — schedules and alerts — ever want this.
func WithReview(ctx context.Context) context.Context {
	return context.WithValue(ctx, reviewKey{}, true)
}

// ReviewRequested reports whether WithReview marked ctx.
func ReviewRequested(ctx context.Context) bool {
	v, _ := ctx.Value(reviewKey{}).(bool)
	return v
}
