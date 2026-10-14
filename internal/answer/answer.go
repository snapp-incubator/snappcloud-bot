// Package answer cleans model output before anyone reads it.
package answer

import (
	"regexp"
	"strings"
)

// thinkRe matches reasoning blocks some "thinking" models wrap their
// chain-of-thought in. Stripped so they don't reach the user. (Plain-text
// reasoning with no tags can't be removed here — that needs a non-thinking
// model.)
var (
	thinkRe = regexp.MustCompile(`(?is)<think(?:ing)?>.*?</think(?:ing)?>`)
	// Some models leak tool-call / function-call markup or special control
	// tokens into the visible text; strip them so the user never sees them.
	toolCallRe = regexp.MustCompile(`(?is)<(tools|tool_call|function_call|tool_use|minimax:tool_call|antml:[^>]+)>.*?</(tools|tool_call|function_call|tool_use|minimax:tool_call|antml:[^>]+)>`)
	ctrlTokRe  = regexp.MustCompile(`<\|[^|>]*\|>`)
	// A whole answer wrapped in a single fenced block (```...```), which renders
	// as a code box instead of readable text.
	wholeFenceRe = regexp.MustCompile("(?s)^```[a-zA-Z0-9_-]*\n(.*)\n```$")
)

// Clean removes what must never reach a reader — reasoning blocks, leaked
// tool-call markup, control tokens, leading narration — and unwraps an answer
// that is wholly one non-code fence.
func Clean(s string) string {
	s = thinkRe.ReplaceAllString(s, "")
	s = toolCallRe.ReplaceAllString(s, "")
	s = ctrlTokRe.ReplaceAllString(s, "")
	s = strings.TrimSpace(s)
	s = dropLeadingNarration(s)
	// Unwrap an answer that is entirely one code fence (not real code) so it's
	// human-readable markdown, not a monospace block.
	if m := wholeFenceRe.FindStringSubmatch(s); m != nil {
		inner := strings.TrimSpace(m[1])
		// Only unwrap when the content clearly isn't code (has prose sentences).
		if !looksLikeCode(inner) {
			s = inner
		}
	}
	return strings.TrimSpace(s)
}

// codeSignal matches tokens that indicate the fenced content is real code /
// commands / logs (so its fence must be kept), not prose the model wrongly
// wrapped. Conservative: unwrap ONLY when none of these appear.
var codeSignal = regexp.MustCompile(`(?m)(^\s*[-$#>]|[;{}$|=]|--[a-z]|\b(oc|kubectl|cilium|curl|helm|sudo|apiVersion|kind)\b)`)

// looksLikeCode reports whether fenced content should keep its code fence.
func looksLikeCode(s string) bool {
	if strings.Contains(s, "```") {
		return true // nested fences → leave as-is
	}
	return codeSignal.MatchString(s)
}

// narrationLine matches a line of the model talking about its own progress
// rather than reporting anything: "Excellent — I have very rich data.",
// "I have enough to deliver the report. Now let me produce the report."
var narrationLine = regexp.MustCompile(`(?i)^(?:\*\*|#+\s*)?(?:` +
	`excellent|great|perfect|good[.,!—-]|ok[.,!—-]|alright|right[.,!—-]|` +
	`i (?:have|now have|'ve) (?:enough|what i need|rich|plenty|all)|` +
	`(?:now )?(?:let me|i'?ll|i will|here is|here'?s) (?:produce|write|deliver|compile|put together|assemble|give)|` +
	`writing the (?:final )?report|producing the report|key findings` +
	`)\b`)

// productionSentence matches a sentence about producing the answer
// ("Let me build the report.") rather than one that is part of it.
var productionSentence = regexp.MustCompile(`(?i)` +
	`\b(?:let me|let's|i'll|i will|i'm going to|i am going to|i can)\s+(?:\w+\s+){0,2}` +
	`(?:build|compose|produce|write|compile|assemble|put together|deliver|present|draft|format|create|summari[sz]e|give you)\b` +
	`|\b(?:now )?i (?:now )?(?:have|'ve got|have got|'ve gathered|have gathered|'ve collected|have collected) ` +
	`(?:all|enough|everything|what i need|the (?:key |necessary |required )?(?:data|information|results))` +
	`|\bthe (?:data|results?|information) (?:is|are) (?:rich|complete|sufficient|enough|compact|in)\b` +
	`|^here(?:'s| is) (?:the|your) (?:full |final |complete |daily |weekly )?(?:report|summary|answer)\b`)

// structuralLine is where an answer's body starts: a heading, a table, a list
// item, a quote, or a bold title line.
var structuralLine = regexp.MustCompile(`^(?:#|\||[-*>]|\d+[.)]\s)`)

// dropLeadingNarration strips sentences about producing the answer from above
// its body. It acts only when a structured body follows, and never on a line
// with a figure in it.
func dropLeadingNarration(s string) string {
	lines := strings.Split(s, "\n")
	body := -1
	for i, ln := range lines {
		if structuralLine.MatchString(strings.TrimSpace(ln)) {
			body = i
			break
		}
	}
	if body < 0 {
		return s
	}
	cut := 0
	for i := 0; i < body; i++ {
		t := strings.TrimSpace(lines[i])
		if t == "" {
			if cut == i {
				cut = i + 1
			}
			continue
		}
		if !isNarration(t) {
			break
		}
		cut = i + 1
	}
	if cut == 0 {
		return s
	}
	return strings.TrimSpace(strings.Join(lines[cut:], "\n"))
}

// isNarration reports whether a whole line is talk about producing the answer.
// Every sentence on it has to be, so a line that narrates and then states a
// finding is kept.
func isNarration(line string) bool {
	if strings.ContainsAny(line, "0123456789") {
		return false
	}
	if narrationLine.MatchString(line) {
		return true
	}
	sentences := splitSentences(line)
	if len(sentences) == 0 {
		return false
	}
	for _, sn := range sentences {
		if !productionSentence.MatchString(sn) {
			return false
		}
	}
	return true
}

// splitSentences breaks a line after sentence-ending punctuation.
func splitSentences(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if (s[i] == '.' || s[i] == '!' || s[i] == '?') && (i+1 == len(s) || s[i+1] == ' ') {
			if t := strings.TrimSpace(s[start : i+1]); t != "" {
				out = append(out, t)
			}
			start = i + 1
		}
	}
	if t := strings.TrimSpace(s[start:]); t != "" {
		out = append(out, t)
	}
	return out
}
