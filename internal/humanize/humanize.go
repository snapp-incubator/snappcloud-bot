// Package humanize renders values for people rather than for logs.
package humanize

import (
	"fmt"
	"strings"
	"time"
)

// Duration formats like "30m", "1h30m", "90s", built from components
// (trimming time.Duration.String() corrupts values like 30m).
func Duration(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	d = d.Round(time.Second)

	h := int(d / time.Hour)
	m := int(d % time.Hour / time.Minute)
	s := int(d % time.Minute / time.Second)

	var b strings.Builder
	if h > 0 {
		fmt.Fprintf(&b, "%dh", h)
	}
	if m > 0 {
		fmt.Fprintf(&b, "%dm", m)
	}
	// Seconds only when they carry information: "90s" matters, the "0s" in
	// "30m0s" does not.
	if s > 0 || b.Len() == 0 {
		fmt.Fprintf(&b, "%ds", s)
	}
	return b.String()
}
