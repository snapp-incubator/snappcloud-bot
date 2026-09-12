package humanize

import (
	"testing"
	"time"
)

func TestDuration(t *testing.T) {
	for _, tc := range []struct {
		in   time.Duration
		want string
	}{
		// The regressions: suffix-trimming turned these into "3" and "1h3".
		{30 * time.Minute, "30m"},
		{90 * time.Minute, "1h30m"},
		{4 * time.Hour, "4h"},
		{time.Minute, "1m"},
		{5 * time.Minute, "5m"},
		{90 * time.Second, "1m30s"},
		{45 * time.Second, "45s"},
		{24 * time.Hour, "24h"},
		{0, "0s"},
		{-time.Minute, "0s"},
	} {
		if got := Duration(tc.in); got != tc.want {
			t.Errorf("Duration(%s) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
