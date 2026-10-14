package llm

import (
	"errors"
	"regexp"
)

// A gateway that does not translate a model's native tool-call tokens returns
// the call as text with no tool_use block. That is a failed call, not an
// answer: report it as an error so failover can serve the request.

// ErrLeakedToolCall marks a response whose text contains tool-call markup the
// API did not turn into tool_use blocks.
var ErrLeakedToolCall = errors.New("model emitted tool calls as text (gateway did not translate them)")

// leakedToolCallRe matches known text fallbacks for tool calls across model
// families.
var leakedToolCallRe = regexp.MustCompile(`(?is)` +
	`<tools>\s*<call\b|<call\s+tool=|` +
	`<tool_call>|<function_call>|<minimax:tool_call>|<invoke\s+name=|<function=|` +
	`<\|tool_calls?_(?:section_)?begin\|>|<\|tool_call_start\|>|` +
	`\[TOOL_CALLS\]`)

// leakedToolCall reports whether a response with no parsed tool calls carries
// tool-call markup in its text.
func leakedToolCall(text string, calls int) bool {
	return calls == 0 && leakedToolCallRe.MatchString(text)
}
