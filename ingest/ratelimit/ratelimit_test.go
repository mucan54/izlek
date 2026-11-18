package ratelimit

import (
	"fmt"
	"net/netip"
	"testing"
	"time"
)

func TestBucket(t *testing.T) {
	b := NewBucket(10, 0) // 10/s, burst 10
	t0 := time.Unix(1000, 0)
	for i := 0; i < 10; i++ {
		if !b.Allow(t0) {
			t.Fatalf("burst request %d denied", i)
		}
	}
	if b.Allow(t0) {
		t.Fatal("11th request in the same instant allowed")
	}
	if b.Allow(t0.Add(50 * time.Millisecond)) { // 0.5 token
		t.Fatal("allowed before a full token was refilled")
	}
	if !b.Allow(t0.Add(100 * time.Millisecond)) {
		t.Fatal("denied after one token refilled")
	}
	// A long pause refills only up to the burst.
	later := t0.Add(time.Hour)
	n := 0
	for b.Allow(later) {
		n++
	}
	if n != 10 {
		t.Fatalf("after long idle %d allowed, want burst 10", n)
	}
	// A clock step backwards neither refills nor panics.
	if b.Allow(t0) {
		t.Fatal("allowed on clock going backwards with empty bucket")
	}
	// Sustained rate over 10 simulated seconds ≈ rate.
	b = NewBucket(100, 0)
	allowed := 0
	for i := 0; i < 100_000; i++ { // 10k req/s offered
		if b.Allow(t0.Add(time.Duration(i) * 100 * time.Microsecond)) {
			allowed++
		}
	}
	if allowed < 1090 || allowed > 1110 { // 100 burst + 100/s * 10s
		t.Fatalf("sustained: %d allowed, want ~1100", allowed)
	}
	if NewBucket(0.5, 0).burst != 1 {
		t.Fatal("burst floor")
	}
}

func TestIPLimiter(t *testing.T) {
	l := NewIPLimiter(5, 0)
	t0 := time.Unix(1000, 0)
	a := netip.MustParseAddr("203.0.113.7")
	b := netip.MustParseAddr("203.0.113.8")
	for i := 0; i < 5; i++ {
		if !l.Allow(a, t0) {
			t.Fatalf("request %d denied", i)
		}
	}
	if l.Allow(a, t0) {
		t.Fatal("6th request allowed")
	}
	if !l.Allow(b, t0) {
		t.Fatal("another address was limited by the first one's bucket")
	}
	if !l.Allow(a, t0.Add(200*time.Millisecond)) {
		t.Fatal("no refill")
	}
	// IPv4-mapped form shares the IPv4 bucket.
	mapped := netip.MustParseAddr("::ffff:203.0.113.7")
	if l.Allow(mapped, t0.Add(200*time.Millisecond)) {
		t.Fatal("v4-mapped address got its own bucket")
	}
	// IPv6: one bucket per /64.
	v6a := netip.MustParseAddr("2001:db8:1:2::1")
	v6b := netip.MustParseAddr("2001:db8:1:2:ffff::9")
	v6c := netip.MustParseAddr("2001:db8:1:3::1")
	for i := 0; i < 5; i++ {
		l.Allow(v6a, t0)
	}
	if l.Allow(v6b, t0) {
		t.Fatal("same /64 not limited together")
	}
	if !l.Allow(v6c, t0) {
		t.Fatal("different /64 limited")
	}
	if !l.Allow(netip.Addr{}, t0) {
		t.Fatal("invalid address must be allowed")
	}

	// Sweep drops only buckets that are full again.
	if l.Len() != 4 {
		t.Fatalf("tracked %d addresses, want 4", l.Len())
	}
	l.Sweep(t0.Add(100 * time.Millisecond)) // nobody is back to a full bucket yet
	if l.Len() != 4 {
		t.Fatalf("sweep at +100ms left %d, want 4", l.Len())
	}
	l.Sweep(t0.Add(10 * time.Second))
	if l.Len() != 0 {
		t.Fatalf("sweep after refill left %d", l.Len())
	}
}

func TestIPLimiterBounded(t *testing.T) {
	l := NewIPLimiter(1, 64*4) // 4 addresses per shard
	t0 := time.Unix(1000, 0)
	for i := 0; i < 50_000; i++ {
		addr := netip.MustParseAddr(fmt.Sprintf("10.%d.%d.%d", i>>16&255, i>>8&255, i&255))
		if !l.Allow(addr, t0) { // first request of every address: must pass, tracked or not
			t.Fatalf("first request of %v denied", addr)
		}
	}
	if l.Len() > 64*4 {
		t.Fatalf("limiter tracks %d addresses, cap is %d", l.Len(), 64*4)
	}
}
