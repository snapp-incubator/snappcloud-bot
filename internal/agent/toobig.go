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

// missingRequiredMessage names each missing argument with the description the
// server gave it, so the retry has what it needs without another round trip.
func missingRequiredMessage(schema map[string]any, missing []string) string {
	props, _ := schema["properties"].(map[string]any)
	var b strings.Builder
	b.WriteString("this tool was not called: it requires ")
	b.WriteString(strings.Join(missing, ", "))
	b.WriteString(", which you did not provide. Call it again with them set.")
	for _, name := range missing {
		p, _ := props[name].(map[string]any)
		desc, _ := p["description"].(string)
		if desc == "" {
			continue
		}
		if len([]rune(desc)) > 240 {
			desc = string([]rune(desc)[:239]) + "…"
		}
		fmt.Fprintf(&b, "\n- %s: %s", name, desc)
	}
	return b.String()
}
