// Package humanize renders values for people rather than for logs.
package humanize

import (
	"fmt"
	"strings"
	"time"
)

// Duration formats a duration the way it is spoken: "30m", "1h30m", "4h", "90s".
//
// time.Duration.String() renders "30m0s" and "4h0m0s", and trimming the zero
// suffixes off that text is how "30m" became "3" — TrimSuffix(s, "0m") removes
// the "0m" from "30m". So build the string from the components instead of
// editing the stringified form.
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
