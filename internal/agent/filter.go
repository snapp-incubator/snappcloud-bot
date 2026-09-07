package agent

import (
	"encoding/json"
	"net"
	"regexp"
	"sort"
	"strings"
)

// Result filtering is the enforcement boundary for B: MCP tool output carries
// namespace information in the DATA (a Hubble flow names its source/destination
// namespaces), not in the call arguments. Before the model ever sees a result we
// drop every record that references a namespace the user is not authorized for,
// so the model physically cannot leak another team's data.
//
// A record with no discernible namespace is kept (it can't leak a namespace it
// doesn't name); a record touching any unauthorized namespace is dropped. A
// document with no array of records is gated whole (blocked if it names an
// unauthorized namespace).
//
// PEER-SHAPED records (a Hubble flow, which names a source AND a destination)
// are the exception: they are kept when the caller owns EITHER side. Every
// egress flow a tenant has names a peer outside their namespaces — CoreDNS,
// another team's service, the world — so the all-sides rule hid a tenant's own
// traffic from them, including DNS failures and cross-namespace policy denials.
// The caller is a party to that traffic and can already observe it with tcpdump
// from inside their own pod, so showing the peer reveals nothing they cannot
// already obtain. A flow where they own NEITHER side is still dropped.

var ipRe = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`)

// ExtractRefs walks a (JSON) tool result and returns the explicit namespaces it
// names plus the IP addresses it references (which the caller resolves to
// namespaces via mcp-authz before filtering).
func ExtractRefs(result string) (namespaces, ips []string) {
	var v any
	if json.Unmarshal([]byte(result), &v) == nil {
		namespaces = uniqSorted(collectNamespaces(v))
	}
	ips = uniqSorted(extractIPs(result))
	return namespaces, ips
}

// FilterResult removes records that reference a namespace not in `allowed`.
// `resolved` maps an IP (or other bare ref) to its namespace(s), so a record
// that only names an IP is still gated. Returns the filtered result, how many
// records were removed, and whether a non-array document was blocked wholesale.
func FilterResult(result string, allowed map[string]bool, resolved map[string][]string) (out string, removed int, blocked bool) {
	var v any
	if json.Unmarshal([]byte(result), &v) != nil {
		return result, 0, false // not JSON — cannot structurally filter, pass through
	}
	switch t := v.(type) {
	case []any:
		kept, n := filterArray(t, allowed, resolved)
		b, _ := json.Marshal(kept)
		return string(b), n, false
	case map[string]any:
		// Aggregated results (Hubble's flow_summary) hide the namespace in the
		// map KEY -- {"top_source_pods": {"team-b/api-0": 42}} -- where the
		// namespace-key scan cannot see it, so the whole summary would pass.
		// Scope those entries before anything else.
		removed += filterKeyedCounts(t, allowed)

		if key := dominantArrayKey(t); key != "" {
			arr, _ := t[key].([]any)
			kept, n := filterArray(arr, allowed, resolved)
			t[key] = kept
			b, _ := json.Marshal(t)
			return string(b), n + removed, false
		}
		if unauthorized(recordNamespaces(t, resolved), allowed) {
			return "", 0, true
		}
		if removed > 0 {
			b, _ := json.Marshal(t)
			return string(b), removed, false
		}
		return result, 0, false
	default:
		return result, 0, false
	}
}

func filterArray(arr []any, allowed map[string]bool, resolved map[string][]string) ([]any, int) {
	kept := make([]any, 0, len(arr))
	removed := 0
	for _, el := range arr {
		if !recordAllowed(el, allowed, resolved) {
			removed++
			continue
		}
		kept = append(kept, el)
	}
	return kept, removed
}

// recordAllowed decides one record. Peer-shaped records (both a source and a
// destination namespace) pass when the caller owns either side; everything else
// keeps the strict rule, so a single-namespace resource never becomes visible
// because it happens to mention an authorized namespace somewhere.
func recordAllowed(el any, allowed map[string]bool, resolved map[string][]string) bool {
	src := sideNamespaces(el, "source", resolved)
	dst := sideNamespaces(el, "destination", resolved)
	if len(src) > 0 && len(dst) > 0 {
		return anyAllowed(src, allowed) || anyAllowed(dst, allowed)
	}
	return !unauthorized(recordNamespaces(el, resolved), allowed)
}

func anyAllowed(ns []string, allowed map[string]bool) bool {
	for _, n := range ns {
		if allowed[n] {
			return true
		}
	}
	return false
}

// sideNamespaces returns the namespaces named on one side of a peer-shaped
// record: the subtrees under keys containing "source" or "destination"
// ("source", "source_ip", "destination_pod"), including namespaces its IPs
// resolve to.
func sideNamespaces(el any, side string, resolved map[string][]string) []string {
	m, ok := el.(map[string]any)
	if !ok {
		return nil
	}
	var out []string
	for k, v := range m {
		if !strings.Contains(strings.ToLower(k), side) {
			continue
		}
		out = append(out, collectNamespaces(v)...)
		if s, ok := v.(string); ok && strings.Contains(strings.ToLower(k), "namespace") {
			out = append(out, s)
		}
		if len(resolved) > 0 {
			blob, _ := json.Marshal(v)
			for _, ip := range extractIPs(string(blob)) {
				out = append(out, resolved[ip]...)
			}
		}
	}
	return out
}

// recordNamespaces returns every namespace a single record references: the
// explicit namespace keys plus the namespaces its IPs resolve to.
func recordNamespaces(el any, resolved map[string][]string) []string {
	ns := collectNamespaces(el)
	// A Namespace object's identity IS a namespace: gate it by its own name so
	// listing namespaces can never return ones outside the caller's scope.
	if n := namespaceSelfName(el); n != "" {
		ns = append(ns, n)
	}
	if len(resolved) > 0 {
		blob, _ := json.Marshal(el)
		for _, ip := range extractIPs(string(blob)) {
			ns = append(ns, resolved[ip]...)
		}
	}
	return ns
}

// namespaceSelfName returns the record's own name when the record represents a
// Namespace (its name is the namespace to gate on). Recognizes both a raw
// Kubernetes object (kind: Namespace) and a summarized record that carries a
// "kind":"Namespace" field.
func namespaceSelfName(el any) string {
	m, ok := el.(map[string]any)
	if !ok {
		return ""
	}
	if kind, _ := m["kind"].(string); !strings.EqualFold(kind, "Namespace") {
		return ""
	}
	if name, ok := m["name"].(string); ok && name != "" {
		return name
	}
	if meta, ok := m["metadata"].(map[string]any); ok {
		if name, ok := meta["name"].(string); ok {
			return name
		}
	}
	return ""
}

// unauthorized reports whether any named namespace is outside `allowed`. A
// record naming no namespace is authorized (nothing to leak).
func unauthorized(ns []string, allowed map[string]bool) bool {
	for _, n := range ns {
		if !allowed[n] {
			return true
		}
	}
	return false
}

// collectNamespaces walks a decoded JSON value and returns every string value
// held under a key whose name contains "namespace" (e.g. "namespace",
// "source_namespace", "k8s_namespace_name", nested source.namespace).
func collectNamespaces(v any) []string {
	var out []string
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if s, ok := val.(string); ok && strings.Contains(strings.ToLower(k), "namespace") {
				if s != "" {
					out = append(out, s)
				}
				continue
			}
			out = append(out, collectNamespaces(val)...)
		}
	case []any:
		for _, e := range t {
			out = append(out, collectNamespaces(e)...)
		}
	}
	return out
}

// dominantArrayKey returns the object key holding the largest array of objects,
// i.e. the record list to filter (e.g. "flows", "items"). "" if none.
func dominantArrayKey(m map[string]any) string {
	best, bestLen := "", 0
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys) // deterministic on ties
	for _, k := range keys {
		if arr, ok := m[k].([]any); ok && len(arr) > bestLen {
			best, bestLen = k, len(arr)
		}
	}
	return best
}

func extractIPs(s string) []string {
	var out []string
	for _, m := range ipRe.FindAllString(s, -1) {
		if net.ParseIP(m) != nil {
			out = append(out, m)
		}
	}
	return out
}

func uniqSorted(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// nsQualifiedRe matches a "namespace/name" map key, the way aggregated tool
// output identifies a pod or service ("team-a/web-0"). Anchored and restricted
// to DNS-1123 characters so it cannot match a URL path, an FQDN, or a CIDR.
var nsQualifiedRe = regexp.MustCompile(`^([a-z0-9]([-a-z0-9]*[a-z0-9])?)/([a-z0-9][-a-z0-9._]*)$`)

// filterKeyedCounts walks nested maps and drops entries whose KEY is
// "namespace/name" for a namespace the caller cannot access. This is the
// aggregate counterpart to filterArray: a summary is not a list of records, so
// per-record filtering never sees it, and the namespace lives in the key rather
// than in a field. Returns how many entries were dropped.
func filterKeyedCounts(m map[string]any, allowed map[string]bool) int {
	removed := 0
	for _, v := range m {
		sub, ok := v.(map[string]any)
		if !ok {
			continue
		}
		for k := range sub {
			match := nsQualifiedRe.FindStringSubmatch(k)
			if match == nil {
				// Not namespace-qualified (a verdict, a protocol, an FQDN):
				// recurse in case it nests further.
				continue
			}
			if !allowed[match[1]] {
				delete(sub, k)
				removed++
			}
		}
		removed += filterKeyedCounts(sub, allowed)
	}
	return removed
}
