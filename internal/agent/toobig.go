package agent

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/snapp-incubator/snappcloud-bot/internal/mcp"
)

// A tool whose whole output is refused for size is not a dead end — it is a
// call made too broadly. But "narrow the query (a namespace, a node, a
// selector, or a smaller limit)" describes a generic tool, and the model is
// holding a specific one: envoy_config_dump takes a resource_type, envoy_
// listeners takes an ingress class. Told only to narrow, it retried the same
// call and lost the round; twice, on teh-1, in one investigation.
//
// The bot has each tool's JSON Schema. When a call is refused for size, it can
// say which of THAT tool's arguments were left unset — turning a dead end into
// the next call.

// tooLarge reports whether a tool error is a size refusal.
func tooLarge(err error) bool { return errors.Is(err, mcp.ErrTooLarge) }

// narrowingHint lists the arguments of a tool the call did not set, so a
// refused result names its own remedy. Required arguments are excluded: they
// were already supplied, and they are not what widens a call.
func narrowingHint(schema map[string]any, args map[string]any) string {
	props, _ := schema["properties"].(map[string]any)
	if len(props) == 0 {
		return ""
	}
	required := map[string]bool{}
	if rs, ok := schema["required"].([]any); ok {
		for _, r := range rs {
			if s, ok := r.(string); ok {
				required[s] = true
			}
		}
	}
	var unused []string
	for name := range props {
		if required[name] {
			continue
		}
		if v, set := args[name]; set && !isEmpty(v) {
			continue
		}
		unused = append(unused, name)
	}
	if len(unused) == 0 {
		return ""
	}
	sort.Strings(unused)
	return fmt.Sprintf(" This tool takes arguments you did not set: %s. "+
		"Set whichever of them narrows what you are asking for and call it again — "+
		"do not repeat the call unchanged, and do not give up on the question.",
		strings.Join(unused, ", "))
}

func isEmpty(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(t) == ""
	case []any:
		return len(t) == 0
	case map[string]any:
		return len(t) == 0
	}
	return false
}

// missingRequired lists the arguments a tool declares required that the call
// left out. The schema is the server's own statement of what it needs, and
// checking it here costs nothing; not checking it costs a round trip and an
// error message written in the server's terms rather than the caller's.
func missingRequired(schema map[string]any, args map[string]any) []string {
	rs, ok := schema["required"].([]any)
	if !ok {
		return nil
	}
	var missing []string
	for _, r := range rs {
		name, ok := r.(string)
		if !ok {
			continue
		}
		if v, set := args[name]; !set || isEmpty(v) {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	return missing
}

// missingRequiredMessage tells the model exactly how to place the call again.
// The first version said "this tool was not called: it requires datasourceUid,
// expr, endTime, which you did not provide" — and a model read that as a
// description of the tool, concluding query_prometheus "exposes a datasource
// uid, not a PromQL endpoint", and reported the capability as absent. So the
// message names the tool, separates what was sent from what is wanted, and is
// unmistakably an instruction.
func missingRequiredMessage(tool string, schema map[string]any, missing []string, args map[string]any) string {
	props, _ := schema["properties"].(map[string]any)
	var b strings.Builder
	fmt.Fprintf(&b, "%s was NOT called — the arguments were wrong, and the tool itself is fine. ", tool)
	if sent := argNames(args); len(sent) > 0 {
		fmt.Fprintf(&b, "You sent: %s. ", strings.Join(sent, ", "))
	} else {
		b.WriteString("You sent no arguments. ")
	}
	if unknown := unknownArgs(schema, args); len(unknown) > 0 {
		fmt.Fprintf(&b, "These are not arguments of this tool at all: %s — you are using names from a "+
			"different tool or a different version. ", strings.Join(unknown, ", "))
	}
	fmt.Fprintf(&b, "It requires %s. Call %s again with exactly those names:",
		strings.Join(missing, ", "), tool)
	for _, name := range missing {
		p, _ := props[name].(map[string]any)
		desc, _ := p["description"].(string)
		if len([]rune(desc)) > 240 {
			desc = string([]rune(desc)[:239]) + "…"
		}
		if desc == "" {
			fmt.Fprintf(&b, "\n- %s", name)
			continue
		}
		fmt.Fprintf(&b, "\n- %s: %s", name, desc)
	}
	return b.String()
}

// argNames lists the argument names a call actually carried, sorted.
func argNames(args map[string]any) []string {
	out := make([]string, 0, len(args))
	for k := range args {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// unknownArgs lists arguments the tool does not declare. A model reaching for
// a name it remembers from another server is the usual cause, and saying so
// costs nothing.
func unknownArgs(schema map[string]any, args map[string]any) []string {
	props, ok := schema["properties"].(map[string]any)
	if !ok || len(props) == 0 {
		return nil
	}
	var out []string
	for k := range args {
		if _, declared := props[k]; !declared {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// retryHint turns a classified tool failure into the next move. Without it a
// model reads "404 not found" as an invitation to try another spelling: one
// report spent eight of its thirteen tool calls guessing dashboard names —
// "cluster-capacity", "snappcloud-nodes", "snappcloud-ingress" — none of which
// existed, when one search would have returned the real ones.
func retryHint(reason string) string {
	switch reason {
	case "not_found":
		return " The identifier you passed does not exist. Two things it is usually NOT: another spelling " +
			"of the right one, and an identifier of a different kind of object — a uid that came out of one " +
			"tool's output names the thing that tool lists, not whatever this tool looks up. Call the tool " +
			"that lists or searches for THIS kind of thing and take the identifier from what it returns. " +
			"If that search comes back empty, say the thing does not exist here rather than guessing further."
	case "auth":
		return " This is a permissions or credentials failure, not a missing thing: whatever you were " +
			"looking for may well exist. Report it as unavailable to you right now, name the tool, and " +
			"do not conclude from it that the resource, the metric or the cluster is absent."
	case "timeout", "unreachable":
		return " The server did not answer. Try once more if the answer matters, and if it fails again " +
			"say which tool was unreachable and what that leaves unanswered — never report its subject " +
			"as absent."
	default:
		return ""
	}
}
