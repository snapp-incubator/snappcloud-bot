package botoauth

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// PlatformSSO validates access tokens from the configured issuer. Each resource
// has its own cache, including its refresh limit for unknown key IDs.
type PlatformSSO struct {
	cfg      Incoming
	mu       sync.Mutex
	keys     map[string]*rsa.PublicKey
	expires  time.Time
	nextMiss time.Time
}

func NewPlatformSSO(cfg Incoming) *PlatformSSO { return &PlatformSSO{cfg: cfg} }

func (v *PlatformSSO) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	now := time.Now()
	if now.Before(v.expires) {
		if key := v.keys[kid]; key != nil {
			return key, nil
		}
	}
	if now.Before(v.nextMiss) {
		return nil, ErrInvalid
	}
	// Permit one immediate rotation lookup after the initial successful load.
	first := v.keys == nil
	v.nextMiss = now.Add(10 * time.Second)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.cfg.JWKSURL, nil)
	if err != nil {
		return nil, ErrInvalid
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, ErrInvalid
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, ErrInvalid
	}
	var set jose.JSONWebKeySet
	if err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&set); err != nil {
		return nil, ErrInvalid
	}
	keys := make(map[string]*rsa.PublicKey)
	for _, jwk := range set.Keys {
		pub, ok := jwk.Key.(*rsa.PublicKey)
		if !ok || pub.N.BitLen() < 2048 || jwk.KeyID == "" || (jwk.Use != "" && jwk.Use != "sig") || (jwk.Algorithm != "" && jwk.Algorithm != "RS256") {
			continue
		}
		if _, duplicate := keys[jwk.KeyID]; duplicate {
			return nil, ErrInvalid
		}
		keys[jwk.KeyID] = pub
	}
	v.keys = keys
	v.expires = now.Add(5 * time.Minute)
	key := keys[kid]
	if key == nil {
		return nil, ErrInvalid
	}
	if first {
		v.nextMiss = time.Time{}
	}
	return key, nil
}

func clusterUser(user string) bool {
	return user != "" && strings.TrimSpace(user) == user && !strings.ContainsAny(user, "\r\n\x00") && !strings.HasPrefix(strings.ToLower(user), "system:")
}

func (v *PlatformSSO) Username(ctx context.Context, raw string) (string, error) {
	if raw == "" || len(raw) > 64<<10 {
		return "", ErrInvalid
	}
	token, err := jwt.ParseSigned(raw, []jose.SignatureAlgorithm{jose.RS256})
	if err != nil || len(token.Headers) != 1 || token.Headers[0].KeyID == "" {
		return "", ErrInvalid
	}
	key, err := v.key(ctx, token.Headers[0].KeyID)
	if err != nil {
		return "", err
	}
	var claims struct {
		jwt.Claims
		AuthorizedParty   string `json:"azp"`
		PreferredUsername string `json:"preferred_username"`
		TokenType         string `json:"typ"`
	}
	if err = token.Claims(key, &claims); err != nil {
		return "", ErrInvalid
	}
	now := time.Now()
	expected := jwt.Expected{Issuer: v.cfg.Issuer, Time: now}
	if claims.ValidateWithLeeway(expected, 0) != nil || claims.Expiry == nil || !now.Before(claims.Expiry.Time()) || strings.TrimSpace(claims.Subject) == "" || (claims.TokenType != "" && !strings.EqualFold(claims.TokenType, "Bearer")) {
		return "", ErrInvalid
	}
	// Keycloak may issue gateway tokens with only default realm audiences.
	audienceOK := v.cfg.Audience != "" && claims.Audience.Contains(v.cfg.Audience)
	partyOK := claims.AuthorizedParty != "" && slices.Contains(v.cfg.TrustedAuthorizedParties, claims.AuthorizedParty)
	if !audienceOK && !partyOK {
		return "", ErrInvalid
	}
	// OpenShift central-auth uses the issuer's preferred_username. The immutable
	// subject can also be mapped explicitly when the gateway omits that claim.
	user := claims.Subject
	if claims.PreferredUsername != "" {
		user = claims.PreferredUsername
	}
	if mapped, ok := v.cfg.SubjectUsers[claims.Subject]; ok {
		user = mapped
	}
	if !clusterUser(user) {
		return "", ErrInvalid
	}
	return user, nil
}
