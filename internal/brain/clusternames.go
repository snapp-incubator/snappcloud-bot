package brain

import (
	"sort"
	"strings"
)

// A cluster goes by several names. The bot configures okd4-snappgroup; the
// alert's cluster label says snappgroup-teh-1; the Grafana datasource says
// snappgroup; the user says "sg". Handed a name it did not recognise, the model
// concluded the user had no access to that cluster — an access statement with
// no basis — so the mapping is done here, deterministically, and the model is
// told the answer rather than left to infer it.

// ResolveCluster maps a cluster name as it appears outside the bot — an alert's
// cluster/region label, a datasource, a user's shorthand — to the configured
// cluster it denotes. Exact match on any known name wins; failing that, a label
// that starts with a known name (snappgroup-teh-1 → snappgroup) and, failing
// that, one that merely contains it, the longest known name winning either
// way so "teh1" inside "snappgroup-teh-1" does not beat "snappgroup". The
// comparison ignores case, the okd4- prefix and punctuation.
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
// alias, the operator-supplied extra names, and the name without its okd4-
// prefix, which is how people and alert rules mostly write it.
func clusterNames(name, alias string, extra []string) []string {
	out := []string{name}
	if alias != "" {
		out = append(out, alias)
	}
	if short := strings.TrimPrefix(name, "okd4-"); short != name {
		out = append(out, short)
	}
	out = append(out, extra...)
	return out
}

func normalizeClusterName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimPrefix(s, "okd4-")
	var sb strings.Builder
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			sb.WriteRune(r)
		}
	}
	return sb.String()
}
