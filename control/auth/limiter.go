package auth

import (
	"sync"
	"time"
)

// Limiter is a sliding-window event counter: at most Max events per Window
// for each key. It is used to throttle failed logins.
type Limiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	now    func() time.Time
	hits   map[string][]time.Time
	// maxKeys bounds memory. When it is reached expired keys are swept; if
	// the map is still full, events for new keys are not recorded. Callers
	// pair a fine-grained key with a coarse one (per IP), so the coarse
	// limit still holds in that state.
	maxKeys int
}

// NewLimiter returns a limiter allowing max events per window per key.
func NewLimiter(max int, window time.Duration, now func() time.Time) *Limiter {
	if now == nil {
		now = time.Now
	}
	return &Limiter{max: max, window: window, now: now, hits: make(map[string][]time.Time), maxKeys: 200_000}
}

func (l *Limiter) prune(key string, now time.Time) []time.Time {
	hits := l.hits[key]
	cut := now.Add(-l.window)
	i := 0
	for i < len(hits) && !hits[i].After(cut) {
		i++
	}
	if i == len(hits) {
		delete(l.hits, key)
		return nil
	}
	if i > 0 {
		hits = hits[i:]
		l.hits[key] = hits
	}
	return hits
}

// Blocked reports whether key has used up its budget, and for how long.
func (l *Limiter) Blocked(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	hits := l.prune(key, now)
	if len(hits) < l.max {
		return false, 0
	}
	// The oldest of the last max events leaving the window frees a slot.
	return true, hits[len(hits)-l.max].Add(l.window).Sub(now)
}

// Add records one event for key.
func (l *Limiter) Add(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	hits := l.prune(key, now)
	if hits == nil && len(l.hits) >= l.maxKeys {
		for k := range l.hits {
			l.prune(k, now)
		}
		if len(l.hits) >= l.maxKeys {
			return
		}
	}
	if len(hits) >= l.max {
		// Already blocked: keep the window sliding without growing the slice.
		copy(hits, hits[1:])
		hits[len(hits)-1] = now
		return
	}
	l.hits[key] = append(hits, now)
}

// Len returns the number of tracked keys (for tests and metrics).
func (l *Limiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.hits)
}
