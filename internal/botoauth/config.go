// Package botoauth implements the optional delegated MCP authentication modes.
package botoauth

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Incoming struct {
	JWKSURL          string            `yaml:"jwksURL"`
	Audience         string            `yaml:"audience"`
	SubjectUsers     map[string]string `yaml:"subjectUsers"`
	Resource         string            `yaml:"resource"`
	Issuer           string            `yaml:"issuer"`
	IntrospectionURL string            `yaml:"introspectionURL"`
	ClientID         string            `yaml:"clientID"`
	Scope            string            `yaml:"scope"`
}
type Outgoing struct {
	Resource     string `yaml:"resource"`
	AuthorizeURL string `yaml:"authorizeURL"`
	TokenURL     string `yaml:"tokenURL"`
	RevokeURL    string `yaml:"revokeURL"`
	CallbackURL  string `yaml:"callbackURL"`
	ClientID     string `yaml:"clientID"`
	StoreDir     string `yaml:"storeDir"`
}

func httpsURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil && u.RawQuery == "" && u.Fragment == ""
}
func (o Incoming) Validate() error {
	u, _ := url.Parse(o.Resource)
	if !httpsURL(o.Resource) || u.Path != "/oauth/mcp" || !httpsURL(o.Issuer) {
		return errors.New("invalid OAuth resource or issuer")
	}
	if o.JWKSURL != "" || o.Audience != "" || o.SubjectUsers != nil {
		if !httpsURL(o.JWKSURL) || strings.TrimSpace(o.Audience) == "" || o.IntrospectionURL != "" || o.ClientID != "" || o.Scope != "" {
			return errors.New("platform SSO requires jwksURL and audience and cannot be combined with introspection")
		}
		for sub, user := range o.SubjectUsers {
			if strings.TrimSpace(sub) == "" || !clusterUser(user) {
				return errors.New("invalid SSO subject mapping")
			}
		}
		return nil
	}
	if !httpsURL(o.IntrospectionURL) || o.ClientID == "" || len(strings.Fields(o.Scope)) != 1 || strings.ContainsAny(o.Scope, " \\\"\r\n\t") {
		return errors.New("invalid incoming OAuth resource, issuer, introspection URL, client ID or scope")
	}
	return nil
}
func (o Outgoing) Validate() error {
	u, _ := url.Parse(o.CallbackURL)
	if !httpsURL(o.Resource) || !httpsURL(o.AuthorizeURL) || !httpsURL(o.TokenURL) || !httpsURL(o.RevokeURL) || !httpsURL(o.CallbackURL) || u.Path != "/oauth/callback" || o.ClientID == "" || !strings.HasPrefix(o.StoreDir, "/") {
		return errors.New("invalid Pardis OAuth HTTPS endpoints, callback, client ID or absolute storage directory")
	}
	return nil
}

var client = &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
