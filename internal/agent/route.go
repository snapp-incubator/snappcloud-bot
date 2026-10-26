package agent

import (
	"fmt"
	"sort"
	"strings"
)

// clusterArg is the argument that picks the cluster a merged tool runs on.
const clusterArg = "cluster"

// route is one offered tool and the clusters it can be called on. Each cluster's
// copy of a tool is one definition, not one per cluster: per-cluster copies
// divide the tool budget by the number of clusters a question names.
type route struct {
	name      string
	byKey     map[string]binding // every cluster that has the tool, offered or not
	clusters  []string           // clusters the caller may call it on, in listing order
	tool      Tool
	server    string // first server seen with the tool, for fair trimming
	preferred bool
}

// buildRoutes merges the clusters' tool lists by tool name. A cluster-admin
// tool is registered for every cluster but offered only where the caller is a
// cluster-admin, so a call elsewhere still gets the denial. Global (NoEnforce)
// sources keep an alias-qualified name and no cluster argument.
func (a *Agent) buildRoutes(lists []clusterList) (map[string]*route, []*route) {
	routes := map[string]*route{}
	var order []*route
	multi := 0
	for _, l := range lists {
		if !l.ct.NoEnforce {
			multi++
		}
	}
	for _, l := range lists {
		allowed := make(map[string]bool, len(l.ct.Allowed))
		for _, n := range l.ct.Allowed {
			allowed[n] = true
		}
		for _, t := range l.tools {
			name := nameUnsafe.ReplaceAllString(t.Name, "_")
			if l.ct.NoEnforce {
				name = qualifyRoute(l.ct.Alias, t.Name, routes)
			}
			r := routes[name]
			if r == nil {
				r = &route{name: name, byKey: map[string]binding{}, server: l.ct.Cluster + "/" + t.Server,
					tool: Tool{Name: name, Description: t.Description, InputSchema: copySchema(t.InputSchema)}}
				if l.ct.NoEnforce {
					r.tool.Description = fmt.Sprintf("[%s — general documentation, not cluster-scoped] %s", l.ct.Alias, t.Description)
				}
				routes[name] = r
				order = append(order, r)
			} else {
				mergeProperties(r.tool.InputSchema, t.InputSchema)
			}
			r.byKey[l.ct.Cluster] = binding{ct: l.ct, real: t.Name, allowed: allowed,
				selfAuthorized: t.SelfAuthorized, unscoped: t.Unscoped, schema: t.InputSchema}
			if !l.ct.ClusterAdmin && a.enforcer.ClusterAdminOnly(t.Name) {
				continue
			}
			r.clusters = append(r.clusters, l.ct.Cluster)
			r.preferred = r.preferred || l.ct.Preferred
		}
	}
	for _, r := range order {
		if multi > 1 && len(r.clusters) > 0 && !r.isGlobal() {
			addClusterArg(r.tool.InputSchema, r.clusters)
		}
	}
	return routes, order
}

func (r *route) isGlobal() bool {
	for _, b := range r.byKey {
		return b.ct.NoEnforce
	}
	return false
}

// resolve picks the binding a call runs on and returns its arguments without
// the cluster argument. A message means the call is refused with it.
func (r *route) resolve(args map[string]any) (binding, map[string]any, string) {
	want, _ := args[clusterArg].(string)
	rest := make(map[string]any, len(args))
	for k, v := range args {
		if k != clusterArg {
			rest[k] = v
		}
	}
	if r.isGlobal() {
		for _, b := range r.byKey {
			return b, args, ""
		}
	}
	if strings.TrimSpace(want) == "" {
		switch {
		case len(r.clusters) == 1:
			return r.byKey[r.clusters[0]], rest, ""
		case len(r.byKey) == 1:
			for _, b := range r.byKey {
				return b, rest, ""
			}
		}
		return binding{}, nil, fmt.Sprintf("%s was NOT called: pass %q, one of: %s. "+
			"For a question about several clusters, call it once per cluster.", r.name, clusterArg, strings.Join(r.clusters, ", "))
	}
	if b, ok := matchCluster(want, r.byKey); ok {
		return b, rest, ""
	}
	avail := r.clusters
	if len(avail) == 0 {
		avail = sortedClusterKeys(r.byKey)
	}
	return binding{}, nil, fmt.Sprintf("%s was NOT called: it is not available on cluster %q. It runs on: %s.",
		r.name, want, strings.Join(avail, ", "))
}

// matchCluster accepts the cluster's name, its alias, or a unique suffix after
// a dash ("teh-2" for "okd4-teh-2").
func matchCluster(want string, byKey map[string]binding) (binding, bool) {
	want = strings.ToLower(strings.TrimSpace(want))
	var hit []binding
	for c, b := range byKey {
		if strings.ToLower(c) == want || strings.ToLower(b.ct.Alias) == want {
			return b, true
		}
		if strings.HasSuffix(strings.ToLower(c), "-"+want) {
			hit = append(hit, b)
		}
	}
	if len(hit) == 1 {
		return hit[0], true
	}
	return binding{}, false
}

// legacyRoute maps an "<alias>__<tool>" name, the form tools had before they
// were merged, to the merged tool and the cluster argument it implied.
func legacyRoute(routes map[string]*route, name string) (*route, string) {
	i := strings.Index(name, "__")
	if i <= 0 {
		return nil, ""
	}
	r := routes[name[i+2:]]
	if r == nil {
		return nil, ""
	}
	return r, name[:i]
}

func sortedClusterKeys(m map[string]binding) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func qualifyRoute(alias, name string, routes map[string]*route) string {
	reg := make(map[string]binding, len(routes))
	for k := range routes {
		reg[k] = binding{}
	}
	return qualify(alias, name, reg)
}

func copySchema(s map[string]any) map[string]any {
	out := make(map[string]any, len(s)+2)
	for k, v := range s {
		out[k] = v
	}
	if props, ok := s["properties"].(map[string]any); ok {
		cp := make(map[string]any, len(props)+1)
		for k, v := range props {
			cp[k] = v
		}
		out["properties"] = cp
	}
	if req, ok := s["required"].([]any); ok {
		out["required"] = append([]any(nil), req...)
	}
	if out["type"] == nil {
		out["type"] = "object"
	}
	return out
}

// mergeProperties adds the properties a newer server version has, so one
// cluster running an older release does not hide them from the others.
func mergeProperties(dst, src map[string]any) {
	sp, _ := src["properties"].(map[string]any)
	if len(sp) == 0 {
		return
	}
	dp, _ := dst["properties"].(map[string]any)
	if dp == nil {
		dp = map[string]any{}
		dst["properties"] = dp
	}
	for k, v := range sp {
		if _, ok := dp[k]; !ok {
			dp[k] = v
		}
	}
}

func addClusterArg(schema map[string]any, clusters []string) {
	props, _ := schema["properties"].(map[string]any)
	if props == nil {
		props = map[string]any{}
		schema["properties"] = props
	}
	enum := make([]any, len(clusters))
	for i, c := range clusters {
		enum[i] = c
	}
	props[clusterArg] = map[string]any{"type": "string", "enum": enum,
		"description": "Cluster to run on. Call once per cluster for a cross-cluster question."}
	if len(clusters) > 1 {
		req, _ := schema["required"].([]any)
		schema["required"] = append(req, clusterArg)
	}
}
