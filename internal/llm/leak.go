package llm

import (
	"errors"
	"regexp"
)

// A model that wants to call a tool but is served through a gateway that does
// not translate its native tool-call tokens into tool_use blocks emits the
// call as TEXT — "<tools><call tool="query_prometheus">…" — and the API
// reports a finished answer with no calls. The agent then treats the model's
// half-sentence of narration plus that markup as the final answer and posts
// it. Seen on the first daily report after a model swap: the channel got
// "Datasources mapped. Now checking …" followed by tag soup.
//
// The response is not an answer; it is a failed tool call, and the model is
// systematically unable to call tools through this gateway. It is reported
// as an error so the failover serves the request from the backup and the
// breaker counts it against the primary.

// ErrLeakedToolCall marks a response whose text contains tool-call markup the
// API did not turn into tool_use blocks.
var ErrLeakedToolCall = errors.New("model emitted tool calls as text (gateway did not translate them)")

// leakedToolCallRe matches the tool-call syntaxes the model families behind
// the gateway are known to fall back to when their native format is not
// parsed: the <tools>/<call> and <tool_call> XML shapes, MiniMax's and
// Anthropic's <invoke>, Llama's <function=…>, Kimi's and Qwen's special
// tokens, and Mistral's [TOOL_CALLS].
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
