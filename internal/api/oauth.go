package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/snapp-incubator/snappcloud-bot/internal/authzclient"
	"github.com/snapp-incubator/snappcloud-bot/internal/botoauth"
)

type oauthIdentity struct{}

// OAuthRoutes exposes a separate MCP resource. Authentication happens before
// dispatch, including discovery; authorization still uses the existing resolver.
func (h *Handler) OAuthRoutes(mux *http.ServeMux, v botoauth.Verifier, identityMap map[string]string) {
	cfg := v.Config
	metadataPath := "/.well-known/oauth-protected-resource/oauth/mcp"
	metadata := strings.TrimSuffix(cfg.Resource, "/oauth/mcp") + metadataPath
	metadataDoc := map[string]any{"resource": cfg.Resource, "authorization_servers": []string{cfg.Issuer}, "bearer_methods_supported": []string{"header"}}
	challenge := `Bearer resource_metadata="` + metadata + `"`
	verify := func(ctx context.Context, token string) (string, error) {
		email, err := v.Email(ctx, token)
		if mapped, ok := identityMap[email]; ok {
			return mapped, err
		}
		return email, err
	}
	if cfg.JWKSURL != "" {
		verify = botoauth.NewPlatformSSO(cfg).Username
	} else {
		metadataDoc["scopes_supported"] = []string{cfg.Scope}
		challenge += `, scope="` + cfg.Scope + `"`
	}
	mux.HandleFunc("GET "+metadataPath, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, metadataDoc)
	})
	mux.HandleFunc("/oauth/mcp", func(w http.ResponseWriter, r *http.Request) {
		user, err := verify(r.Context(), bearer(r))
		if err != nil {
			if errors.Is(err, botoauth.ErrInvalid) {
				w.Header().Set("WWW-Authenticate", challenge)
				http.Error(w, "OAuth access token required", http.StatusUnauthorized)
			} else {
				http.Error(w, "Authorization server unavailable", http.StatusServiceUnavailable)
			}
			return
		}
		if user == "" {
			http.Error(w, "No mapped identity", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		ctx := context.WithValue(r.Context(), oauthIdentity{}, authzclient.Identity{User: user, Region: "oauth"})
		h.mcp(w, r.WithContext(ctx))
	})
}
