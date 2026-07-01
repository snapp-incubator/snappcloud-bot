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
		if key := dominantArrayKey(t); key != "" {
			arr, _ := t[key].([]any)
			kept, n := filterArray(arr, allowed, resolved)
			t[key] = kept
			b, _ := json.Marshal(t)
			return string(b), n, false
		}
		if unauthorized(recordNamespaces(t, resolved), allowed) {
			return "", 0, true
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
		if unauthorized(recordNamespaces(el, resolved), allowed) {
			removed++
			continue
		}
		kept = append(kept, el)
	}
	return kept, removed
}

// recordNamespaces returns every namespace a single record references: the
// explicit namespace keys plus the namespaces its IPs resolve to.
func recordNamespaces(el any, resolved map[string][]string) []string {
	ns := collectNamespaces(el)
	if len(resolved) > 0 {
		blob, _ := json.Marshal(el)
		for _, ip := range extractIPs(string(blob)) {
			ns = append(ns, resolved[ip]...)
		}
	}
	return ns
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
