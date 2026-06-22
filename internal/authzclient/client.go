// Package authzclient calls the per-region mcp-authz API and aggregates the
// answers into a single per-cluster scope, with caching. The bot holds no
// cluster credentials — every authorization decision is delegated to the
// mcp-authz instance running on that cluster.
package authzclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Scope is the set of namespaces a subject may query, per cluster. Clusters the
// subject cannot access are omitted. Cluster names match the Dify per-cluster
// MCP tool groups.
type Scope map[string][]string

// Clusters returns the scope's cluster names, sorted.
func (s Scope) Clusters() []string {
	out := make([]string, 0, len(s))
	for c := range s {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// Empty reports whether the subject can access no cluster at all.
func (s Scope) Empty() bool { return len(s) == 0 }

// Region is one cluster's mcp-authz endpoint.
type Region struct {
	Name string
	URL  string
}

// Client queries every region's mcp-authz API.
type Client struct {
	regions []Region
	token   string
	http    *http.Client
}

// New builds a client over the region endpoints, presenting token as a bearer.
func New(regions []Region, token string, timeout time.Duration) *Client {
	return &Client{
		regions: regions,
		token:   token,
		http:    &http.Client{Timeout: timeout},
	}
}

// namespaces calls one region's /v1/namespaces for the user.
func (c *Client) namespaces(ctx context.Context, region Region, user string) ([]string, error) {
	u := strings.TrimRight(region.URL, "/") + "/v1/namespaces?user=" + url.QueryEscape(user)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<10))
		return nil, fmt.Errorf("region %s: %s: %s", region.Name, resp.Status, strings.TrimSpace(string(msg)))
	}
	var out struct {
		Namespaces []string `json:"namespaces"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("region %s: decode: %w", region.Name, err)
	}
	return out.Namespaces, nil
}

// Resolve queries every region concurrently and returns the subject's per-cluster
// scope. Fail-closed per region: a region that errors is omitted (the user just
// can't query it this time) rather than risking a wrong grant. Only if EVERY
// region errors is an error returned, so the bot can say "temporarily
// unavailable" instead of "unauthorized".
func (c *Client) Resolve(ctx context.Context, user string) (Scope, error) {
	type result struct {
		name string
		ns   []string
		err  error
	}
	ch := make(chan result, len(c.regions))
	for _, r := range c.regions {
		r := r
		go func() {
			ns, err := c.namespaces(ctx, r, user)
			ch <- result{name: r.Name, ns: ns, err: err}
		}()
	}

	scope := Scope{}
	errCount := 0
	var firstErr error
	for range c.regions {
		res := <-ch
		if res.err != nil {
			errCount++
			if firstErr == nil {
				firstErr = res.err
			}
			continue
		}
		if len(res.ns) > 0 {
			sort.Strings(res.ns)
			scope[res.name] = res.ns
		}
	}
	if errCount == len(c.regions) && len(c.regions) > 0 {
		return nil, firstErr
	}
	return scope, nil
}
