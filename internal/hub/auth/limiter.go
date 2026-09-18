package auth

import (
	"sync"
	"time"
)

type entry struct {
	failures int
	lockedAt time.Time
}

// Limiter blocks a key after maxFailures failed attempts, for lockout.
type Limiter struct {
	mu          sync.Mutex
	maxFailures int
	lockout     time.Duration
	entries     map[string]*entry
}

func NewLimiter(maxFailures int, lockout time.Duration) *Limiter {
	return &Limiter{maxFailures: maxFailures, lockout: lockout, entries: map[string]*entry{}}
}

func (l *Limiter) Allowed(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.entries[key]
	if !ok || e.failures < l.maxFailures {
		return true
	}
	if now.Sub(e.lockedAt) >= l.lockout {
		delete(l.entries, key)
		return true
	}
	return false
}

func (l *Limiter) Fail(key string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.entries[key]
	if !ok {
		e = &entry{}
		l.entries[key] = e
	}
	e.failures++
	if e.failures >= l.maxFailures {
		e.lockedAt = now
	}
}

func (l *Limiter) Reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, key)
}
