// Package scopetoken mints the HMAC-signed token that carries a user's
// per-cluster authorized namespaces. The bot signs it; the mcp-gateway in front
// of each MCP server verifies it and enforces, so the agent can never query a
// namespace outside the token. Format must match the gateway's verifier:
// base64url(payloadJSON) + "." + base64url(HMAC-SHA256(secret, payloadJSON)).
package scopetoken

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"time"
)

// Scope maps cluster name -> authorized namespaces.
type Scope map[string][]string

// Claims is the token payload.
type Claims struct {
	User  string `json:"u"`
	Scope Scope  `json:"s"`
	Exp   int64  `json:"exp"`
}

// Sign produces a token for user+scope valid for ttl, signed with secret.
func Sign(user string, scope Scope, ttl time.Duration, secret string) (string, error) {
	payload, err := json.Marshal(Claims{User: user, Scope: scope, Exp: time.Now().Add(ttl).Unix()})
	if err != nil {
		return "", err
	}
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil)), nil
}
