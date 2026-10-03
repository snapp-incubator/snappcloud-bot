package agent

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// An opaque identifier is one the model cannot derive: a Grafana dashboard or
// datasource uid is a random string, not a name, so there is nothing to reason
// from. Asked for a dashboard it had not searched for, a model does not stop —
// it supplies a plausible-looking value. A daily report spent two of its calls
// on get_dashboard_panel_queries(uid="test") and get_dashboard_property(uid=
// "test"), got 404 from both, and reported the metrics as unavailable.
//
// Prose did not hold it: the tool guidance already says a uid "is never
// something you can guess, only something a search returns". So the loop checks
// it. An identifier argument must have appeared in an earlier result of this
// turn (or in the question itself, where a user may paste one); otherwise the
// call is refused before it is spent, and the refusal names the tool to call
// instead.

// isIDArg reports whether an argument name holds an opaque identifier: uid,
// datasourceUid, dashboard_uid, UIDs. The boundary matters — "liquid" also ends
// in those three letters — so the suffix counts only at a word start, after a
// separator, or at a camelCase hump.
func isIDArg(name string) bool {
	n := strings.TrimSuffix(strings.ToLower(name), "s")
	if !strings.HasSuffix(n, "uid") {
		return false
	}
	at := len(n) - 3
	if at == 0 {
		return true
	}
	switch prev := rune(name[at-1]); {
	case prev < 'A' || prev > 'z', prev > 'Z' && prev < 'a': // a separator or digit
		return true
	default: // a letter: only a camelCase hump ("...sourceUid") is a boundary
		return name[at] == 'U'
	}
}

// idToken matches an identifier as it appears in a tool result: the value of a
// JSON "uid" field, which is how both search_dashboards and list_datasources
// return one.
var idToken = regexp.MustCompile(`(?i)"[a-z_]*uid"\s*:\s*"([^"]{1,128})"`)

// wordToken matches a bare word in the question, so a uid the user pasted
// counts as seen.
var wordToken = regexp.MustCompile(`[A-Za-z0-9_.:-]{2,128}`)

// idArgs lists the arguments of a tool that hold an opaque identifier.
func idArgs(schema map[string]any) []string {
	props, _ := schema["properties"].(map[string]any)
	out := make([]string, 0, len(props))
	for name := range props {
		if isIDArg(name) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// noteIDs records the identifiers a tool result carried, so a later call may
// use them. Only uid-shaped fields are kept: the point is to recognise a value
// the model read, not to index the whole result.
func noteIDs(seen map[string]bool, result string) {
	for _, m := range idToken.FindAllStringSubmatch(result, -1) {
		seen[strings.ToLower(m[1])] = true
	}
}

// noteQuestionWords records the words of the question. A user who pastes a uid
// into a question has supplied it as surely as a search would have.
func noteQuestionWords(seen map[string]bool, query string) {
	for _, w := range wordToken.FindAllString(query, -1) {
		seen[strings.ToLower(w)] = true
	}
}

// noteConfiguredIDs records identifiers pinned in the system prompt, so guidance
// that names a datasource outright keeps working. Only uid-shaped words count
// here: the prompt is thousands of words of English, and taking all of them
// would let any of those words pass as an identifier.
func noteConfiguredIDs(seen map[string]bool, system string) {
	noteIDs(seen, system)
	for _, w := range wordToken.FindAllString(system, -1) {
		if uidShaped(w) {
			seen[strings.ToLower(w)] = true
		}
	}
}

// uidShaped reports whether a word looks like a generated identifier rather than
// a word someone wrote: long, and mixing letters with digits or case.
func uidShaped(w string) bool {
	if len(w) < 9 {
		return false
	}
	var digits, upper, lower, other int
	for _, r := range w {
		switch {
		case r >= '0' && r <= '9':
			digits++
		case r >= 'A' && r <= 'Z':
			upper++
		case r >= 'a' && r <= 'z':
			lower++
		default:
			other++
		}
	}
	if other > 0 {
		return false
	}
	return digits > 0 && (upper+lower) > 0 || upper >= 9
}

// unseenIDs lists the identifier arguments of a call whose values have not
// appeared anywhere this turn — the ones the model invented.
func unseenIDs(schema map[string]any, args map[string]any, seen map[string]bool) []string {
	var out []string
	for _, name := range idArgs(schema) {
		v, ok := args[name].(string)
		if !ok || strings.TrimSpace(v) == "" {
			continue
		}
		if seen[strings.ToLower(strings.TrimSpace(v))] {
			continue
		}
		out = append(out, fmt.Sprintf("%s=%q", name, v))
	}
	return out
}

// unseenIDMessage refuses a call that carries an invented identifier, and names
// the call that produces a real one. Like missingRequiredMessage it has to be
// unmistakably about the arguments: a model told only "not found" concludes the
// object does not exist and reports the question as unanswerable.
func unseenIDMessage(tool string, unseen []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s was NOT called — it carries an identifier that nothing in this "+
		"conversation gave you: %s. ", tool, strings.Join(unseen, ", "))
	b.WriteString("A uid is a random string, not a name: it cannot be guessed, derived from " +
		"what a thing is called, or carried over from another cluster. Nothing is wrong with the " +
		"tool or with the object you are after — you simply do not have its uid yet. ")
	b.WriteString("Call search_dashboards (for a dashboard) or list_datasources (for a datasource) " +
		"first, take a uid from what it returns, and call ")
	fmt.Fprintf(&b, "%s again with that value. Do not report this as missing, not found, or "+
		"unmeasurable: you have not looked it up yet.", tool)
	return b.String()
}
