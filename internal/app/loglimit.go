package app

import (
	"sync"
	"time"
)

// logLimiter lets a repeating problem reach the log once per interval:
// a failure that recurs every scan would otherwise fill the log with the
// same line. It counts what it held back, so the next line says how often
// the problem happened meanwhile.
type logLimiter struct {
	every time.Duration
	now   func() time.Time

	mu         sync.Mutex
	last       time.Time // zero: nothing logged yet
	suppressed int
}

func newLogLimiter(every time.Duration, now func() time.Time) *logLimiter {
	if now == nil {
		now = time.Now
	}
	return &logLimiter{every: every, now: now}
}

// allow reports whether this occurrence should be logged and, if so, how
// many occurrences were held back since the last one logged.
func (l *logLimiter) allow() (ok bool, suppressed int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if !l.last.IsZero() && now.Sub(l.last) < l.every {
		l.suppressed++
		return false, 0
	}
	l.last, suppressed, l.suppressed = now, l.suppressed, 0
	return true, suppressed
}
