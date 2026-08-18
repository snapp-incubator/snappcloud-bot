// Package identity carries the authenticated caller's identity through a request
// context, so the MCP client can forward it to identity-aware servers. Kept in
// its own package so both the agent (which sets it) and the mcp client (which
// reads it) can share the key without an import cycle.
package identity

import "context"

type userKey struct{}

// WithUser returns a context carrying the caller's identity (email / SSO
// username).
func WithUser(ctx context.Context, user string) context.Context {
	if user == "" {
		return ctx
	}
	return context.WithValue(ctx, userKey{}, user)
}

// User returns the caller identity carried in ctx, or "" if none.
func User(ctx context.Context) string {
	u, _ := ctx.Value(userKey{}).(string)
	return u
}
