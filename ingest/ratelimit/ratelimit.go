// Package ratelimit provides the node-local token buckets of design §6.8:
// one bucket per write token and one per client IP.
package ratelimit

import (
	"net/netip"
	"sync"
	"time"
)

// Bucket is a token bucket refilled at Rate tokens/second up to Burst.
type Bucket struct {
	mu     sync.Mutex
	rate   float64
	burst  float64
	tokens float64
	last   int64 // unix nanoseconds of the last refill
}

// NewBucket returns a full bucket. burst <= 0 means one second's worth of
// tokens (at least 1).
func NewBucket(rate, burst float64) *Bucket {
	if burst <= 0 {
		burst = max(rate, 1)
	}
	return &Bucket{rate: rate, burst: burst, tokens: burst}
}

// Rate returns the refill rate.
func (b *Bucket) Rate() float64 { return b.rate }

// Allow takes one token if available.
func (b *Bucket) Allow(now time.Time) bool {
	t := now.UnixNano()
	b.mu.Lock()
	if b.last != 0 && t > b.last {
		b.tokens = min(b.burst, b.tokens+float64(t-b.last)*1e-9*b.rate)
	}
	if t > b.last {
		b.last = t
	}
	ok := b.tokens >= 1
	if ok {
		b.tokens--
	}
	b.mu.Unlock()
	return ok
}

const ipShards = 64

type ipState struct {
	tokens float64
	last   int64
}

type ipShard struct {
	mu sync.Mutex
	m  map[netip.Addr]ipState
}

// IPLimiter keeps one token bucket per client address (IPv6 addresses are
// grouped by /64). Memory is bounded: buckets that have refilled completely
// carry no information and are dropped by Sweep, and a shard that is full of
// active buckets fails open for new addresses.
type IPLimiter struct {
	rate, burst float64
	maxPerShard int
	shards      [ipShards]ipShard
}

// NewIPLimiter returns a limiter allowing rate requests/second per address
// with a burst of one second's worth. maxEntries bounds the number of tracked
// addresses (default 1,000,000).
func NewIPLimiter(rate float64, maxEntries int) *IPLimiter {
	if maxEntries <= 0 {
		maxEntries = 1_000_000
	}
	l := &IPLimiter{rate: rate, burst: max(rate, 1), maxPerShard: max(maxEntries/ipShards, 1)}
	for i := range l.shards {
		l.shards[i].m = make(map[netip.Addr]ipState)
	}
	return l
}

func normalize(addr netip.Addr) netip.Addr {
	addr = addr.Unmap()
	if addr.Is6() {
		if p, err := addr.Prefix(64); err == nil {
			return p.Addr()
		}
	}
	return addr
}

// Allow takes one token from addr's bucket. An invalid address is allowed.
func (l *IPLimiter) Allow(addr netip.Addr, now time.Time) bool {
	if !addr.IsValid() {
		return true
	}
	addr = normalize(addr)
	b := addr.As16()
	sh := &l.shards[(b[15]^b[14]^b[7]^b[6])%ipShards]
	t := now.UnixNano()
	sh.mu.Lock()
	defer sh.mu.Unlock()
	st, ok := sh.m[addr]
	if !ok {
		if len(sh.m) >= l.maxPerShard {
			l.sweepLocked(sh, t)
			if len(sh.m) >= l.maxPerShard {
				return true // fail open rather than grow without bound
			}
		}
		st = ipState{tokens: l.burst, last: t}
	} else if t > st.last {
		st.tokens = min(l.burst, st.tokens+float64(t-st.last)*1e-9*l.rate)
		st.last = t
	}
	allowed := st.tokens >= 1
	if allowed {
		st.tokens--
	}
	sh.m[addr] = st
	return allowed
}

// Sweep drops buckets that have been idle long enough to be full again.
// Call it periodically.
func (l *IPLimiter) Sweep(now time.Time) {
	t := now.UnixNano()
	for i := range l.shards {
		sh := &l.shards[i]
		sh.mu.Lock()
		l.sweepLocked(sh, t)
		sh.mu.Unlock()
	}
}

func (l *IPLimiter) sweepLocked(sh *ipShard, t int64) {
	for a, st := range sh.m {
		if st.tokens+float64(t-st.last)*1e-9*l.rate >= l.burst {
			delete(sh.m, a)
		}
	}
}

// Len returns the number of tracked addresses.
func (l *IPLimiter) Len() int {
	n := 0
	for i := range l.shards {
		l.shards[i].mu.Lock()
		n += len(l.shards[i].m)
		l.shards[i].mu.Unlock()
	}
	return n
}
