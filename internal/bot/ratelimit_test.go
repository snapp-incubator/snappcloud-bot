package bot

import "testing"

func TestRateLimiterBurstThenBlock(t *testing.T) {
	r := NewRateLimiter(60, 3) // 1/s, burst 3
	for i := 0; i < 3; i++ {
		if !r.Allow("u") {
			t.Fatalf("burst token %d should pass", i)
		}
	}
	if r.Allow("u") {
		t.Fatal("4th immediate request must be blocked")
	}
	if !r.Allow("other") {
		t.Fatal("a different user must not be limited by u")
	}
}

func TestRateLimiterDisabled(t *testing.T) {
	r := NewRateLimiter(0, 0)
	for i := 0; i < 100; i++ {
		if !r.Allow("u") {
			t.Fatal("disabled limiter must always allow")
		}
	}
}
