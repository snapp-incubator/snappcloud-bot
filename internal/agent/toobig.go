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
