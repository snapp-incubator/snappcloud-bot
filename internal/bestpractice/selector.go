package bestpractice

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// Selector is a Kubernetes label selector in the string form `kubectl -l`
// takes: comma-separated requirements, all of which must hold —
// `key`, `!key`, `key=value` (or `==`), `key!=value`, `key in (a,b)`,
// `key notin (a,b)`. As in Kubernetes, `!=` and `notin` also match a
// namespace that does not have the key at all.
type Selector struct {
	text string
	reqs []requirement
}

type requirement struct {
	key    string
	op     string // exists, !exists, =, !=, in, notin
	values []string
}

var (
	setReqRe = regexp.MustCompile(`^([^\s!=(),]+)\s+(in|notin)\s*\(([^()]*)\)$`)
	keyRe    = regexp.MustCompile(`^[^\s!=(),]+$`)
	valueRe  = regexp.MustCompile(`^[^\s!=(),]*$`)
)

// ParseSelector parses one selector. An empty one is refused: it would match
// every namespace and so exclude the whole report.
func ParseSelector(s string) (Selector, error) {
	sel := Selector{text: strings.TrimSpace(s)}
	if sel.text == "" {
		return Selector{}, fmt.Errorf("empty label selector")
	}
	for _, part := range splitTopLevel(sel.text) {
		r, err := parseRequirement(strings.TrimSpace(part))
		if err != nil {
			return Selector{}, fmt.Errorf("label selector %q: %w", sel.text, err)
		}
		sel.reqs = append(sel.reqs, r)
	}
	return sel, nil
}

// splitTopLevel splits on commas outside parentheses.
func splitTopLevel(s string) []string {
	var parts []string
	depth, start := 0, 0
	for i, c := range s {
		switch c {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, s[start:i])
				start = i + 1
			}
		}
	}
	return append(parts, s[start:])
}

func parseRequirement(p string) (requirement, error) {
	if m := setReqRe.FindStringSubmatch(p); m != nil {
		var values []string
		for _, v := range strings.Split(m[3], ",") {
			if v = strings.TrimSpace(v); v != "" {
				if !valueRe.MatchString(v) {
					return requirement{}, fmt.Errorf("bad value %q", v)
				}
				values = append(values, v)
			}
		}
		if len(values) == 0 {
			return requirement{}, fmt.Errorf("%q: empty value set", p)
		}
		return requirement{key: m[1], op: m[2], values: values}, nil
	}
	op, key, value := "", "", ""
	switch {
	case strings.HasPrefix(p, "!"):
		op, key = "!exists", strings.TrimSpace(p[1:])
	case strings.Contains(p, "!="):
		op = "!="
		key, value, _ = strings.Cut(p, "!=")
	case strings.Contains(p, "=="):
		op = "="
		key, value, _ = strings.Cut(p, "==")
	case strings.Contains(p, "="):
		op = "="
		key, value, _ = strings.Cut(p, "=")
	default:
		op, key = "exists", p
	}
	key, value = strings.TrimSpace(key), strings.TrimSpace(value)
	if !keyRe.MatchString(key) {
		return requirement{}, fmt.Errorf("%q: bad key %q", p, key)
	}
	if !valueRe.MatchString(value) {
		return requirement{}, fmt.Errorf("%q: bad value %q", p, value)
	}
	return requirement{key: key, op: op, values: []string{value}}, nil
}

// Matches reports whether labels satisfy every requirement.
func (s Selector) Matches(labels map[string]string) bool {
	for _, r := range s.reqs {
		v, has := labels[r.key]
		var ok bool
		switch r.op {
		case "exists":
			ok = has
		case "!exists":
			ok = !has
		case "=":
			ok = has && v == r.values[0]
		case "!=":
			ok = !has || v != r.values[0]
		case "in":
			ok = has && slices.Contains(r.values, v)
		case "notin":
			ok = !has || !slices.Contains(r.values, v)
		}
		if !ok {
			return false
		}
	}
	return true
}

func (s Selector) String() string { return s.text }
