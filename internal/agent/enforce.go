// Package agent runs the MCP tool-calling loop inside the bot so that
// authorization is enforced deterministically: the LLM only proposes tool
// calls, and the bot executes a call ONLY if every namespace it touches is in
// the user's authorized scope for that cluster. The model is never trusted as
// the access-control boundary.
package agent

import (
	"fmt"
	"sort"
	"strings"
)

// NSFormat describes how a namespace is encoded in a tool argument's value.
type NSFormat int

const (
	// NSPlain: the argument value IS the namespace, e.g. "team-a".
	NSPlain NSFormat = iota
	// NSSlash: the argument value is "namespace/name", e.g. "team-a/pod-1".
	NSSlash
)

// ToolRule declares where a tool's namespace(s) live so the enforcer can find
// and check them. A tool with no rule that references a namespace is treated as
// non-namespaced (allowed) unless DefaultDeny is set on the enforcer.
type ToolRule struct {
	// NamespaceArgs are the argument names whose values carry a namespace.
	NamespaceArgs []string
	// Format is how each such value encodes the namespace.
	Format NSFormat
	// RequireNamespace denies the call when none of NamespaceArgs is present
	// (prevents an unscoped cluster-wide query slipping through).
	RequireNamespace bool
}

// Enforcer decides whether a proposed tool call is allowed for a user whose
// authorized namespaces on the tool's cluster are `allowed`.
type Enforcer struct {
	// Rules maps tool name -> rule. A tool absent here uses defaultRule.
	Rules map[string]ToolRule
	// DefaultRule applies to tools not in Rules (default: arg "namespace", plain).
	DefaultRule ToolRule
	// DefaultDeny denies any tool that has no namespace rule at all. Fail-closed
	// for tools we don't understand. Off by default (non-namespaced tools pass).
	DefaultDeny bool
}

// NewEnforcer returns an enforcer with the conventional default rule
// (a single "namespace" argument, plain format).
func NewEnforcer(rules map[string]ToolRule) *Enforcer {
	return &Enforcer{
		Rules:       rules,
		DefaultRule: ToolRule{NamespaceArgs: []string{"namespace"}, Format: NSPlain},
	}
}

// ruleFor returns the rule for a tool and whether it was explicitly configured.
func (e *Enforcer) ruleFor(tool string) (ToolRule, bool) {
	if r, ok := e.Rules[tool]; ok {
		return r, true
	}
	return e.DefaultRule, false
}

// Namespaces extracts every namespace referenced by a tool call's arguments.
func (e *Enforcer) Namespaces(tool string, args map[string]any) []string {
	rule, _ := e.ruleFor(tool)
	seen := map[string]bool{}
	var out []string
	for _, arg := range rule.NamespaceArgs {
		v, ok := args[arg]
		if !ok {
			continue
		}
		for _, ns := range valuesOf(v) {
			ns = namespaceFrom(ns, rule.Format)
			if ns != "" && !seen[ns] {
				seen[ns] = true
				out = append(out, ns)
			}
		}
	}
	sort.Strings(out)
	return out
}

// Check returns nil if the tool call is allowed for `allowed` namespaces on the
// cluster, or a deny error naming the offending namespace. Fail-closed.
func (e *Enforcer) Check(tool string, args map[string]any, allowed []string) error {
	rule, explicit := e.ruleFor(tool)
	if !explicit && e.DefaultDeny {
		return fmt.Errorf("tool %q has no authorization rule; denied", tool)
	}
	ns := e.Namespaces(tool, args)
	if len(ns) == 0 {
		if rule.RequireNamespace {
			return fmt.Errorf("tool %q requires a namespace you are authorized for; specify one", tool)
		}
		return nil // non-namespaced call
	}
	allow := make(map[string]bool, len(allowed))
	for _, a := range allowed {
		allow[a] = true
	}
	for _, n := range ns {
		if !allow[n] {
			return fmt.Errorf("not authorized for namespace %q", n)
		}
	}
	return nil
}

// valuesOf flattens a string or []any/[]string argument value into strings.
func valuesOf(v any) []string {
	switch t := v.(type) {
	case string:
		return []string{t}
	case []string:
		return t
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

// namespaceFrom pulls the namespace out of a raw value per format.
func namespaceFrom(v string, f NSFormat) string {
	v = strings.TrimSpace(v)
	if f == NSSlash {
		if i := strings.IndexByte(v, '/'); i >= 0 {
			return strings.TrimSpace(v[:i])
		}
	}
	return v
}
