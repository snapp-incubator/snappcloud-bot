package agent

import (
	"fmt"
	"sort"
	"strings"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"
)

// PromQL cannot be filtered after the fact. A result is a set of samples whose
// labels the query itself chooses: `sum(rate(x[5m]))` returns one number with no
// namespace on it at all, so the result filter — which drops records naming a
// namespace the caller cannot access — has nothing to work with and would pass
// it through under its fail-open rule. Every tenant's data, in one number.
//
// So the restriction has to happen BEFORE the query runs: every selector is
// pinned to the caller's namespaces. Aggregation is then harmless, because it
// can only aggregate series the caller was allowed to read in the first place.
//
// This is the same approach as prom-label-proxy, done with Prometheus's own
// parser rather than string surgery: a query is parsed, every vector selector in
// the tree is examined — including the ones inside subqueries, matrix selectors
// and both sides of a binary operation — and the expression is printed back out.

const namespaceLabel = "namespace"

// PinNamespaces rewrites expr so every selector is restricted to allowed.
// A selector that already names a namespace the caller cannot access is an
// error rather than a silent rewrite: they asked for something specific, and
// quietly answering about a different namespace would be worse than refusing.
func PinNamespaces(expr string, allowed []string) (string, error) {
	if len(allowed) == 0 {
		return "", fmt.Errorf("no namespaces to query")
	}
	tree, err := parser.ParseExpr(expr)
	if err != nil {
		return "", fmt.Errorf("not a valid PromQL query: %w", err)
	}

	allowSet := make(map[string]bool, len(allowed))
	for _, ns := range allowed {
		allowSet[ns] = true
	}
	pinned := pinnedMatcher(allowed)

	var denied string
	parser.Inspect(tree, func(node parser.Node, _ []parser.Node) error {
		vs, ok := node.(*parser.VectorSelector)
		if !ok {
			return nil
		}
		kept := vs.LabelMatchers[:0]
		explicit := false
		for _, m := range vs.LabelMatchers {
			if m.Name != namespaceLabel {
				kept = append(kept, m)
				continue
			}
			// An exact namespace the caller owns is honoured as written, so a
			// query about one of their namespaces stays about that one.
			if m.Type == labels.MatchEqual && allowSet[m.Value] {
				kept = append(kept, m)
				explicit = true
				continue
			}
			if m.Type == labels.MatchEqual {
				denied = m.Value
				return nil
			}
			// Anything else — a regex, a negation — is dropped and replaced by
			// the caller's own set. Deciding whether one regex is a subset of
			// another is not something to get wrong here.
		}
		vs.LabelMatchers = kept
		if !explicit {
			vs.LabelMatchers = append(vs.LabelMatchers, pinned)
		}
		return nil
	})
	if denied != "" {
		return "", fmt.Errorf("not authorized for namespace %q", denied)
	}
	return tree.String(), nil
}

// pinnedMatcher builds namespace=~"a|b|c" for the caller's namespaces.
func pinnedMatcher(allowed []string) *labels.Matcher {
	ns := append([]string(nil), allowed...)
	sort.Strings(ns)
	for i, n := range ns {
		ns[i] = regexpEscape(n)
	}
	m, err := labels.NewMatcher(labels.MatchRegexp, namespaceLabel, strings.Join(ns, "|"))
	if err != nil {
		// NewMatcher only fails on an invalid regex, and the input is escaped.
		panic("promql: building namespace matcher: " + err.Error())
	}
	return m
}

// regexpEscape quotes the characters a namespace name could in principle carry.
// Kubernetes names are DNS-1123, so only "-" and "." are realistic, but an
// unescaped "." in a matcher would silently widen it to any character.
func regexpEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune(`\.+*?()|[]{}^$`, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}
