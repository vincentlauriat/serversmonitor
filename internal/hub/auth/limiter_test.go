package auth

import (
	"testing"
	"time"
)

func TestLimiterLocksAfterMaxFailures(t *testing.T) {
	l := NewLimiter(5, time.Minute)
	now := time.Unix(1000, 0)
	for i := 0; i < 5; i++ {
		if !l.Allowed("ip", now) {
			t.Fatalf("attempt %d should be allowed", i)
		}
		l.Fail("ip", now)
	}
	if l.Allowed("ip", now) {
		t.Fatal("sixth attempt must be blocked")
	}
	if !l.Allowed("other", now) {
		t.Fatal("other key must not be affected")
	}
	if !l.Allowed("ip", now.Add(61*time.Second)) {
		t.Fatal("lockout must expire")
	}
	l.Fail("ip", now)
	l.Reset("ip")
	if !l.Allowed("ip", now) {
		t.Fatal("reset must clear failures")
	}
}
