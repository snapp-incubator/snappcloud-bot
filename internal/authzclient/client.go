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
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/snapp-incubator/snappcloud-bot/internal/metrics"
)

// ClusterScope is one cluster's grant: the namespaces the subject may query,
// and whether they hold cluster-wide access (cluster-admin) — which gates
// cluster-infrastructure tools (nodes, BGP state).
type ClusterScope struct {
	Namespaces  []string
	ClusterWide bool
}

// Scope is the per-cluster grant set. Clusters the subject cannot access are
// omitted. Cluster names match the agent per-cluster MCP tool groups.
type Scope map[string]ClusterScope

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
	log     *slog.Logger
}

// New builds a client over the region endpoints, presenting token as a bearer.
func New(regions []Region, token string, timeout time.Duration, log *slog.Logger) *Client {
	if log == nil {
		log = slog.Default()
	}
	return &Client{
		regions: regions,
		token:   token,
		http:    &http.Client{Timeout: timeout},
		log:     log,
	}
}

// namespaces calls one region's /v1/namespaces for the user.
func (c *Client) namespaces(ctx context.Context, region Region, user string, groups []string) (ClusterScope, error) {
	u := strings.TrimRight(region.URL, "/") + "/v1/namespaces?user=" + url.QueryEscape(user)
	if len(groups) > 0 {
		// ServiceAccounts carry their authorization in group membership
		// (system:serviceaccounts:<ns>), so the SAR must see the groups the
		// API server resolved for the token.
		u += "&groups=" + url.QueryEscape(strings.Join(groups, ","))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return ClusterScope{}, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return ClusterScope{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<10))
		return ClusterScope{}, fmt.Errorf("region %s: %s: %s", region.Name, resp.Status, strings.TrimSpace(string(msg)))
	}
	var out struct {
		Namespaces  []string `json:"namespaces"`
		ClusterWide bool     `json:"clusterWide"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return ClusterScope{}, fmt.Errorf("region %s: decode: %w", region.Name, err)
	}
	return ClusterScope{Namespaces: out.Namespaces, ClusterWide: out.ClusterWide}, nil
}

// Identity is an authenticated caller: the username the cluster resolved the
// token to, its groups, and which region validated it.
type Identity struct {
	User   string
	Groups []string
	Region string
}

// Authenticate verifies a bearer token against every configured region and
// returns the identity of the first cluster that recognizes it. A token is
// issued by one cluster, so at most one region authenticates it. The bot holds
// no cluster credentials — mcp-authz performs the TokenReview.
//
// Fail-closed: if no region authenticates the token, an error is returned and
// the caller is refused.
func (c *Client) Authenticate(ctx context.Context, token string) (Identity, error) {
	if strings.TrimSpace(token) == "" {
		return Identity{}, fmt.Errorf("empty token")
	}
	body, _ := json.Marshal(map[string]string{"token": token})

	type result struct {
		id  Identity
		ok  bool
		err error
	}
	ch := make(chan result, len(c.regions))
	// Cancel the remaining probes as soon as one region claims the token.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	for _, r := range c.regions {
		r := r
		go func() {
			id, ok, err := c.authenticateAt(ctx, r, body)
			ch <- result{id: id, ok: ok, err: err}
		}()
	}

	var firstErr error
	for range c.regions {
		res := <-ch
		if res.err != nil {
			if firstErr == nil {
				firstErr = res.err
			}
			continue
		}
		if res.ok {
			return res.id, nil
		}
	}
	if firstErr != nil {
		return Identity{}, fmt.Errorf("authentication unavailable: %w", firstErr)
	}
	return Identity{}, fmt.Errorf("token is not valid on any known cluster")
}

func (c *Client) authenticateAt(ctx context.Context, region Region, body []byte) (Identity, bool, error) {
	u := strings.TrimRight(region.URL, "/") + "/v1/authenticate"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(string(body)))
	if err != nil {
		return Identity{}, false, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return Identity{}, false, nil // cancelled because another region won
		}
		return Identity{}, false, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		// Region running an older mcp-authz without /v1/authenticate.
		return Identity{}, false, fmt.Errorf("region %s: /v1/authenticate not available", region.Name)
	}
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<10))
		return Identity{}, false, fmt.Errorf("region %s: %s: %s", region.Name, resp.Status, strings.TrimSpace(string(msg)))
	}
	var out struct {
		Authenticated bool     `json:"authenticated"`
		User          string   `json:"user"`
		Groups        []string `json:"groups"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return Identity{}, false, fmt.Errorf("region %s: decode: %w", region.Name, err)
	}
	if !out.Authenticated {
		return Identity{}, false, nil
	}
	return Identity{User: out.User, Groups: out.Groups, Region: region.Name}, true, nil
}

// ResolveIPs maps each IP to its namespace(s) on the given cluster, via that
// region's mcp-authz /v1/resolve. Used to gate MCP results that name only an IP.
func (c *Client) ResolveIPs(ctx context.Context, cluster string, ips []string) (map[string][]string, error) {
	var region Region
	found := false
	for _, r := range c.regions {
		if r.Name == cluster {
			region, found = r, true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("unknown cluster %q", cluster)
	}

	type ref struct {
		Kind  string `json:"kind"`
		Value string `json:"value"`
	}
	refs := make([]ref, 0, len(ips))
	for _, ip := range ips {
		refs = append(refs, ref{Kind: "ip", Value: ip})
	}
	body, _ := json.Marshal(map[string]any{"refs": refs})

	u := strings.TrimRight(region.URL, "/") + "/v1/resolve"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
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
		return nil, fmt.Errorf("region %s resolve: %s: %s", region.Name, resp.Status, strings.TrimSpace(string(msg)))
	}
	var out struct {
		Namespaces map[string][]string `json:"namespaces"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("region %s resolve decode: %w", region.Name, err)
	}
	return out.Namespaces, nil
}

// Resolve queries every region concurrently and returns the subject's per-cluster
// scope. Fail-closed per region: a region that errors is omitted (the user just
// can't query it this time) rather than risking a wrong grant. Only if EVERY
// region errors is an error returned, so the bot can say "temporarily
// unavailable" instead of "unauthorized".
func (c *Client) Resolve(ctx context.Context, user string) (Scope, error) {
	return c.ResolveWithGroups(ctx, user, nil)
}

// ResolveWithGroups is Resolve with explicit group membership, used for
// identities authenticated by token (ServiceAccounts especially, whose access
// comes from their system:serviceaccounts groups).
func (c *Client) ResolveWithGroups(ctx context.Context, user string, groups []string) (Scope, error) {
	start := time.Now()
	defer func() { metrics.AuthzDuration.Observe(time.Since(start).Seconds()) }()
	type result struct {
		name string
		cs   ClusterScope
		err  error
	}
	ch := make(chan result, len(c.regions))
	for _, r := range c.regions {
		r := r
		go func() {
			cs, err := c.namespaces(ctx, r, user, groups)
			ch <- result{name: r.Name, cs: cs, err: err}
		}()
	}

	scope := Scope{}
	errCount := 0
	var firstErr error
	for range c.regions {
		res := <-ch
		if res.err != nil {
			metrics.AuthzRequests.WithLabelValues(res.name, "error").Inc()
			// A region that errors is dropped (the user just can't query it this
			// time). Log it so a broken/unreachable region is not invisible —
			// otherwise it is indistinguishable from "user has no namespaces here".
			c.log.Warn("region authorization failed; region dropped for this user",
				"region", res.name, "user", user, "err", res.err)
			errCount++
			if firstErr == nil {
				firstErr = res.err
			}
			continue
		}
		metrics.AuthzRequests.WithLabelValues(res.name, "ok").Inc()
		if len(res.cs.Namespaces) > 0 {
			sort.Strings(res.cs.Namespaces)
			scope[res.name] = res.cs
		} else {
			c.log.Debug("region returned no namespaces for user", "region", res.name, "user", user)
		}
	}
	if errCount == len(c.regions) && len(c.regions) > 0 {
		return nil, firstErr
	}
	return scope, nil
}
