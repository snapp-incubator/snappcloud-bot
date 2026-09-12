package bot

import (
	"strings"
)

// Commands are matched on the WHOLE message, which made them brittle: "alerts
// on" worked and "alerts on please" fell through to the model, which then
// answered something plausible instead of turning the feature on. Normalising
// first — case, punctuation, and the politeness people naturally add — keeps
// the command surface small without making users type it exactly.
//
// Anything that is not a command still goes to the model untouched. The rule is
// that a command must be the ENTIRE message; a question that merely contains
// the word "alerts" is a question.

// politeSuffixes are dropped from the end of a command before matching.
var politeSuffixes = []string{"please", "pls", "plz", "thanks", "thank you", "ty"}

// normalizeCommand lowercases a message and strips the decoration people put
// around commands, so the matcher sees the command alone.
func normalizeCommand(msg string) string {
	s := strings.ToLower(strings.TrimSpace(msg))
	s = strings.Trim(s, ".!?,;:")
	s = strings.Join(strings.Fields(s), " ")
	for {
		trimmed := false
		for _, p := range politeSuffixes {
			if strings.HasSuffix(s, " "+p) {
				s = strings.TrimSpace(strings.TrimSuffix(s, " "+p))
				s = strings.Trim(s, ".!?,;:")
				trimmed = true
			}
		}
		if !trimmed {
			return strings.Join(strings.Fields(s), " ")
		}
	}
}

// helpVerbs are the messages that ask what the bot can do.
var helpVerbs = map[string]bool{
	"help": true, "commands": true, "?": true, "what can you do": true,
	"what can i ask": true, "how do i use you": true,
}

// helpText lists what the bot understands. Features that are switched off are
// left out rather than advertised and then refused.
func helpText(schedules, alerts bool) string {
	var b strings.Builder
	b.WriteString("**Ask me anything about your workloads or your networking** — name the namespace and the cluster:\n")
	b.WriteString("```text\nwhy are the pods in my-namespace on teh-1 crashing?\nare packets being dropped for my-namespace on teh-1?\nis my-namespace hitting its quota?\n```\n")

	b.WriteString("\n**Your access**\n")
	b.WriteString("| Command | What it does |\n| --- | --- |\n")
	b.WriteString("| `what access do I have?` | the clusters and namespaces I can look at for you |\n")
	b.WriteString("| `refresh` | re-check your access now, if it was just changed |\n")

	if schedules {
		b.WriteString("\n**Recurring checks**\n")
		b.WriteString("| Command | What it does |\n| --- | --- |\n")
		b.WriteString("| `schedule every day at 09:00 <question>` | ask a question on a schedule |\n")
		b.WriteString("| `schedule every 12h starting at 08:00 <question>` | …choosing when it first runs |\n")
		b.WriteString("| `schedules` | list yours, with ids and next run |\n")
		b.WriteString("| `unschedule <id>` | remove one |\n")
	}

	if alerts {
		b.WriteString("\n**Alert channels** — run these in the channel where your alerts arrive\n")
		b.WriteString("| Command | What it does |\n| --- | --- |\n")
		b.WriteString("| `alerts on` | investigate alerts posted here, using **your** access |\n")
		b.WriteString("| `alerts off` | stop investigating alerts here |\n")
		b.WriteString("| `alerts status` | whose access it uses, and the current limits |\n")
	}

	b.WriteString("\nIn a channel, @-mention me. In a direct message, just write.")
	return b.String()
}

// alertVerb maps the ways people say on/off/status to a canonical verb, so the
// feature is not gated behind one exact phrasing.
func alertVerb(cmd string) (verb string, ok bool) {
	// The phrasings that do not start with the noun, kept working.
	switch cmd {
	case "watch alerts", "mark alert channel", "watch this channel":
		return "on", true
	case "unwatch alerts", "unmark alert channel", "stop watching alerts":
		return "off", true
	}

	fields := strings.Fields(cmd)
	switch len(fields) {
	case 1:
		if fields[0] == "alerts" || fields[0] == "alert" {
			return "status", true
		}
		return "", false
	case 2:
		if fields[0] != "alerts" && fields[0] != "alert" {
			return "", false
		}
		switch fields[1] {
		case "on", "enable", "enabled", "start", "watch":
			return "on", true
		case "off", "disable", "disabled", "stop", "unwatch", "mute":
			return "off", true
		case "status", "state", "info":
			return "status", true
		}
	}
	return "", false
}

// capitalize upper-cases the first letter, so an error written as a sentence
// fragment ("that is too frequent") reads as a sentence when shown to a user.
func capitalize(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	r[0] = []rune(strings.ToUpper(string(r[0])))[0]
	return string(r)
}
