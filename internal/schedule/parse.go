package schedule

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Parse turns a scheduling phrase into a cadence and the query that follows it.
// Deliberately a small, deterministic grammar rather than free-form natural
// language: the user must be able to predict exactly when their schedule fires.
//
//	every day at 09:00 <query>
//	every monday at 08:30 <query>
//	every 6h <query>
//	hourly <query>       daily <query>
//
// An interval cadence may name its first run, which people reach for
// immediately when "every 4h" would otherwise start four hours from now:
//
//	every 4h starting at 16:10 <query>
//	every 6h <query> first run at 09:00
//
// now must carry the location schedules are expressed in — times the user types
// are read in that zone, and Next is returned in it.
//
// The returned Entry has Every, At, Spec and Next filled in; the caller supplies
// the owner and destination.
func Parse(input string, now time.Time) (*Entry, string, error) {
	s := strings.TrimSpace(input)
	low := strings.ToLower(s)

	// every <n><unit> ...
	if m := everyIntervalRe.FindStringSubmatch(low); m != nil {
		n, _ := strconv.Atoi(m[1])
		unit, err := unitDuration(m[2])
		if err != nil {
			return nil, "", err
		}
		d := time.Duration(n) * unit
		if d <= 0 {
			return nil, "", fmt.Errorf("that interval is not valid")
		}
		q := strings.TrimSpace(s[len(m[0]):])

		// "starting at HH:MM" / "first run at HH:MM" may sit anywhere in the
		// rest. Pull it out so it sets the first run instead of silently
		// becoming part of the question.
		q, first, hasFirst, err := cutFirstRun(q)
		if err != nil {
			return nil, "", err
		}
		e := &Entry{
			Every: Duration(d),
			Spec:  fmt.Sprintf("every %d%s", n, m[2]),
			Next:  now.Add(d),
		}
		if hasFirst {
			e.Next = nextDaily(now, first.hour, first.minute)
			e.Spec = fmt.Sprintf("%s, from %s", e.Spec, first.text)
		}
		return e, q, nil
	}

	// every [<weekday>] [at] HH:MM ...   /  daily at HH:MM
	if m := everyDayRe.FindStringSubmatch(low); m != nil {
		day, hhmm := m[1], m[2]
		hour, minute, err := parseClock(hhmm)
		if err != nil {
			return nil, "", err
		}
		q := strings.TrimSpace(s[len(m[0]):])
		if day == "" || day == "day" {
			next := nextDaily(now, hour, minute)
			return &Entry{
				Every: Duration(24 * time.Hour),
				At:    hhmm,
				Spec:  fmt.Sprintf("every day at %s", hhmm),
				Next:  next,
			}, q, nil
		}
		wd, err := weekday(day)
		if err != nil {
			return nil, "", err
		}
		next := nextWeekly(now, wd, hour, minute)
		return &Entry{
			Every: Duration(7 * 24 * time.Hour),
			At:    hhmm,
			Spec:  fmt.Sprintf("every %s at %s", day, hhmm),
			Next:  next,
		}, q, nil
	}

	// hourly / daily / weekly shorthands
	for word, d := range map[string]time.Duration{
		"hourly": time.Hour,
		"daily":  24 * time.Hour,
		"weekly": 7 * 24 * time.Hour,
	} {
		if strings.HasPrefix(low, word+" ") {
			q := strings.TrimSpace(s[len(word):])
			return &Entry{Every: Duration(d), Spec: word, Next: now.Add(d)}, q, nil
		}
	}

	return nil, "", errors.New("unrecognised schedule")
}

var (
	everyIntervalRe = regexp.MustCompile(`^every\s+(\d+)\s*(m|min|mins|minute|minutes|h|hr|hrs|hour|hours|d|day|days)\b`)
	everyDayRe      = regexp.MustCompile(`^(?:every\s+)?(day|monday|tuesday|wednesday|thursday|friday|saturday|sunday|)\s*(?:at\s+)?(\d{1,2}:\d{2})`)

	// "starting at 16:10", "start 16:10", "first run at 16:10", "schedule first
	// run to 16:10", "from 16:10" — the phrasings people actually reach for.
	firstRunRe = regexp.MustCompile(`(?i)\b(?:schedule\s+)?(?:starting|start|first\s+run|from)\s*(?:at|to|on)?\s*(\d{1,2}:\d{2})\b`)
)

// clock is a parsed HH:MM plus the text it came from.
type clock struct {
	hour, minute int
	text         string
}

// cutFirstRun removes a "starting at HH:MM" clause from q and reports the time
// it named. Without this the clause stays in the question and gets asked of the
// LLM verbatim, which is how "schedule first run to 16:10" ended up inside a
// query about restarting pods.
func cutFirstRun(q string) (rest string, c clock, found bool, err error) {
	m := firstRunRe.FindStringSubmatchIndex(q)
	if m == nil {
		return q, clock{}, false, nil
	}
	hhmm := q[m[2]:m[3]]
	hour, minute, perr := parseClock(hhmm)
	if perr != nil {
		return "", clock{}, false, perr
	}
	rest = strings.TrimSpace(q[:m[0]] + " " + q[m[1]:])
	return rest, clock{hour: hour, minute: minute, text: hhmm}, true, nil
}

func unitDuration(u string) (time.Duration, error) {
	switch u {
	case "m", "min", "mins", "minute", "minutes":
		return time.Minute, nil
	case "h", "hr", "hrs", "hour", "hours":
		return time.Hour, nil
	case "d", "day", "days":
		return 24 * time.Hour, nil
	}
	return 0, fmt.Errorf("unknown time unit %q", u)
}

func parseClock(hhmm string) (int, int, error) {
	parts := strings.SplitN(hhmm, ":", 2)
	h, err1 := strconv.Atoi(parts[0])
	m, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, 0, fmt.Errorf("%q is not a valid time of day", hhmm)
	}
	return h, m, nil
}

func weekday(name string) (time.Weekday, error) {
	for d := time.Sunday; d <= time.Saturday; d++ {
		if strings.EqualFold(d.String(), name) {
			return d, nil
		}
	}
	return 0, fmt.Errorf("unknown weekday %q", name)
}

// nextDaily returns the next occurrence of hh:mm strictly after now.
func nextDaily(now time.Time, hour, minute int) time.Time {
	t := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, now.Location())
	if !t.After(now) {
		t = t.AddDate(0, 0, 1)
	}
	return t
}

// nextWeekly returns the next hh:mm on the given weekday strictly after now.
func nextWeekly(now time.Time, wd time.Weekday, hour, minute int) time.Time {
	t := nextDaily(now.AddDate(0, 0, -1), hour, minute)
	for i := 0; i < 8; i++ {
		if t.Weekday() == wd && t.After(now) {
			return t
		}
		t = t.AddDate(0, 0, 1)
	}
	return t
}
