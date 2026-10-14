package brain

import (
	"regexp"
	"sort"
	"strings"
)

// Clusters have several names (config, alert labels, datasources, shorthand).
// Resolve them here so the model is told, not left to guess.

// ResolveCluster maps an outside name to a configured cluster: exact match,
// then prefix, then substring, longest known name winning; case, the okd4-
// prefix and punctuation ignored.
func (b *Brain) ResolveCluster(label string) (string, bool) {
	want := normalizeClusterName(label)
	if want == "" {
		return "", false
	}
	var best, bestBy string
	rank := 0 // 3 exact, 2 prefix, 1 contains
	for name, cm := range b.clusters {
		for _, known := range cm.names {
			k := normalizeClusterName(known)
			if k == "" {
				continue
			}
			var r int
			switch {
			case want == k:
				r = 3
			case strings.HasPrefix(want, k):
				r = 2
			case strings.Contains(want, k):
				r = 1
			default:
				continue
			}
			// Deterministic: rank, then longer match, then name order.
			if r > rank || (r == rank && (len(k) > len(bestBy) || (len(k) == len(bestBy) && name < best))) {
				best, bestBy, rank = name, k, r
			}
		}
	}
	return best, rank > 0
}

// KnownNames returns every name a configured cluster goes by, other than its
// configured name, sorted — for telling the model in the prompt.
func (b *Brain) KnownNames(cluster string) []string {
	cm, ok := b.clusters[cluster]
	if !ok {
		return nil
	}
	seen := map[string]bool{cluster: true}
	var out []string
	for _, n := range cm.names {
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// clusterNames is everything a cluster may be called: its configured name and
// alias, the name without any configured prefix, and the extra names.
func clusterNames(name, alias string, extra, prefixes []string) []string {
	out := []string{name}
	if alias != "" {
		out = append(out, alias)
	}
	for _, p := range prefixes {
		if short := strings.TrimPrefix(name, p); p != "" && short != name && short != "" {
			out = append(out, short)
		}
	}
	return append(out, extra...)
}

func normalizeClusterName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var sb strings.Builder
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

// clusterWordRe finds the words in a question that could name a cluster: the
// shapes cluster names and alert labels take (teh-1, okd4-teh-1, teh1,
// snappgroup-teh-1), not every word in the text.
var clusterWordRe = regexp.MustCompile(`(?i)\b[a-z][a-z0-9]*(?:[.-][a-z0-9]+)*\b`)

// clustersNamedIn reports which configured clusters a question names, under
// any of their names. Used to keep a named cluster's tools whole when the tool
// list has to be trimmed.
func (b *Brain) clustersNamedIn(query string) map[string]bool {
	if len(b.clusters) == 0 {
		return nil
	}
	out := map[string]bool{}
	for _, w := range clusterWordRe.FindAllString(query, -1) {
		if len(w) < 3 {
			continue
		}
		for name, cm := range b.clusters {
			for _, known := range cm.names {
				if matchesClusterWord(w, known) {
					out[name] = true
				}
			}
		}
	}
	return out
}

// matchesClusterWord reports whether a word names the cluster: whole word or
// name followed by a separator, never a bare substring.
func matchesClusterWord(word, known string) bool {
	k := normalizeClusterName(known)
	if k == "" {
		return false
	}
	if normalizeClusterName(word) == k {
		return true
	}
	w, kr := strings.ToLower(word), strings.ToLower(known)
	return kr != "" && (strings.HasPrefix(w, kr+"-") || strings.HasPrefix(w, kr+"."))
}
