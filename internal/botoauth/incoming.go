package botoauth

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

var ErrInvalid = errors.New("invalid OAuth token")

type Verifier struct {
	Config Incoming
	Secret string
}

func (v Verifier) Email(ctx context.Context, bearer string) (string, error) {
	if bearer == "" {
		return "", ErrInvalid
	}
	form := url.Values{"token": {bearer}, "token_type_hint": {"access_token"}}
	req, err := http.NewRequestWithContext(ctx, "POST", v.Config.IntrospectionURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(v.Config.ClientID, v.Secret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		return "", errors.New("OAuth introspection unavailable")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		return "", errors.New("OAuth introspection unavailable")
	}
	var claims struct {
		Active                 bool
		Iss, Scope, Sub, Email string
		Verified               bool `json:"email_verified"`
		Aud                    json.RawMessage
		Type                   string `json:"token_type"`
		Exp                    int64
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&claims); err != nil {
		return "", errors.New("invalid introspection response")
	}
	var aud []string
	if json.Unmarshal(claims.Aud, &aud) != nil {
		var s string
		if json.Unmarshal(claims.Aud, &s) == nil {
			aud = []string{s}
		}
	}
	if !claims.Active || claims.Iss != v.Config.Issuer || claims.Exp <= time.Now().Unix() || !strings.EqualFold(claims.Type, "Bearer") || !slices.Contains(aud, v.Config.Resource) || !slices.Contains(strings.Fields(claims.Scope), v.Config.Scope) || claims.Sub == "" || !claims.Verified || !strings.Contains(claims.Email, "@") || strings.HasPrefix(strings.ToLower(claims.Email), "system:") {
		return "", ErrInvalid
	}
	return claims.Email, nil
}
