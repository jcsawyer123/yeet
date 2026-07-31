// Package ratelimit provides simple in-memory, per-key token-bucket
// rate limiting. yeet is a single instance with no shared state store
// suitable for this (Redis would be pure overhead for one process), so
// an in-memory map is the right tool - it just needs bounding so a public
// endpoint can't be used to grow it without limit.
package ratelimit

import (
	"sync"
	"time"

	"golang.org/x/time/rate"
)

type Limiter struct {
	mu       sync.Mutex
	limiters map[string]*entry
	r        rate.Limit
	burst    int
	maxKeys  int
}

type entry struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

// New creates a limiter allowing r events/sec per key (burst additionally
// allowed), evicting keys idle for more than 10 minutes. maxKeys bounds
// memory use - once at capacity, new keys are rejected (fail closed)
// rather than letting the map grow without limit.
func New(r rate.Limit, burst, maxKeys int) *Limiter {
	l := &Limiter{
		limiters: make(map[string]*entry),
		r:        r,
		burst:    burst,
		maxKeys:  maxKeys,
	}
	go l.janitor()
	return l
}

func (l *Limiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	e, ok := l.limiters[key]
	if !ok {
		if len(l.limiters) >= l.maxKeys {
			return false
		}
		e = &entry{limiter: rate.NewLimiter(l.r, l.burst)}
		l.limiters[key] = e
	}
	e.lastSeen = time.Now()
	return e.limiter.Allow()
}

func (l *Limiter) janitor() {
	for range time.Tick(5 * time.Minute) {
		cutoff := time.Now().Add(-10 * time.Minute)
		l.mu.Lock()
		for k, e := range l.limiters {
			if e.lastSeen.Before(cutoff) {
				delete(l.limiters, k)
			}
		}
		l.mu.Unlock()
	}
}
