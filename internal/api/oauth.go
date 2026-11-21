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
	mux.HandleFunc("GET "+metadataPath, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"resource": cfg.Resource, "authorization_servers": []string{cfg.Issuer}, "scopes_supported": []string{cfg.Scope}, "bearer_methods_supported": []string{"header"}})
	})
	mux.HandleFunc("POST /oauth/mcp", func(w http.ResponseWriter, r *http.Request) {
		email, err := v.Email(r.Context(), bearer(r))
		if err != nil {
			if errors.Is(err, botoauth.ErrInvalid) {
				w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+metadata+`", scope="`+cfg.Scope+`"`)
				http.Error(w, "OAuth access token required", http.StatusUnauthorized)
			} else {
				http.Error(w, "Authorization server unavailable", http.StatusServiceUnavailable)
			}
			return
		}
		if mapped, ok := identityMap[email]; ok {
			email = mapped
		}
		if email == "" {
			http.Error(w, "No mapped identity", http.StatusForbidden)
			return
		}
		ctx := context.WithValue(r.Context(), oauthIdentity{}, authzclient.Identity{User: email, Region: "oauth"})
		h.mcp(w, r.WithContext(ctx))
	})
}
