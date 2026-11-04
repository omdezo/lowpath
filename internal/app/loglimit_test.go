package app

import (
	"testing"
	"time"
)

// The first occurrence is logged, repeats within the interval are counted
// instead, and the next one logged says how many were held back.
func TestLogLimiter(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	l := newLogLimiter(time.Minute, func() time.Time { return now })
	step := func(d time.Duration, wantOK bool, wantHeld int) {
		t.Helper()
		now = now.Add(d)
		if ok, held := l.allow(); ok != wantOK || held != wantHeld {
			t.Errorf("at +%v: allow = %v, %d; want %v, %d", d, ok, held, wantOK, wantHeld)
		}
	}
	step(0, true, 0)
	step(3*time.Second, false, 0)
	step(50*time.Second, false, 0)
	step(7*time.Second, true, 2) // a minute after the first
	step(time.Minute, true, 0)
}
