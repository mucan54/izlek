package dedupe

import (
	"encoding/binary"
	"sync"
	"testing"
	"time"
)

func key(project uint32, n uint64) Key {
	var id [16]byte
	binary.BigEndian.PutUint64(id[8:], n)
	return MakeKey(project, id)
}

func TestSeenAddTTL(t *testing.T) {
	c := New(4, 10*time.Minute)
	t0 := time.Unix(1_000_000, 0)
	if c.Seen(key(1, 1), t0) {
		t.Fatal("empty cache reports seen")
	}
	c.Add(key(1, 1), t0)
	if !c.Seen(key(1, 1), t0) || !c.Seen(key(1, 1), t0.Add(10*time.Minute-time.Nanosecond)) {
		t.Fatal("added key not seen within ttl")
	}
	if c.Seen(key(1, 1), t0.Add(10*time.Minute)) {
		t.Fatal("key still seen after ttl")
	}
	// Same batch_id under another project is a different batch.
	if c.Seen(key(2, 1), t0) {
		t.Fatal("dedupe leaked across projects")
	}
	// Re-adding refreshes the ttl.
	c.Add(key(1, 1), t0.Add(9*time.Minute))
	if !c.Seen(key(1, 1), t0.Add(15*time.Minute)) {
		t.Fatal("re-add did not refresh ttl")
	}
}

func TestLRUEviction(t *testing.T) {
	c := New(3, time.Hour)
	now := time.Unix(1, 0)
	for i := uint64(1); i <= 3; i++ {
		c.Add(key(1, i), now)
	}
	c.Add(key(1, 1), now) // refresh 1: order is now 1,3,2 (2 oldest)
	c.Add(key(1, 4), now) // evicts 2
	for i, want := range map[uint64]bool{1: true, 2: false, 3: true, 4: true} {
		if got := c.Seen(key(1, i), now); got != want {
			t.Errorf("key %d seen=%v, want %v", i, got, want)
		}
	}
	c.Add(key(1, 5), now) // evicts 3
	c.Add(key(1, 6), now) // evicts 1
	if c.Seen(key(1, 3), now) || c.Seen(key(1, 1), now) || !c.Seen(key(1, 4), now) || c.Len() != 3 {
		t.Fatalf("eviction order wrong (len %d)", c.Len())
	}
	// Churn far beyond capacity keeps exactly the newest entries.
	for i := uint64(100); i < 10_000; i++ {
		c.Add(key(7, i), now)
	}
	if c.Len() != 3 || !c.Seen(key(7, 9_999), now) || !c.Seen(key(7, 9_997), now) || c.Seen(key(7, 9_996), now) {
		t.Fatalf("after churn: len %d", c.Len())
	}
}

func TestConcurrent(t *testing.T) {
	c := New(1000, time.Hour)
	now := time.Now()
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := uint64(0); i < 5000; i++ {
				k := key(uint32(g), i)
				c.Add(k, now)
				c.Seen(k, now)
			}
		}(g)
	}
	wg.Wait()
	if c.Len() != 1000 {
		t.Fatalf("len %d, want 1000", c.Len())
	}
}

func TestNoAllocs(t *testing.T) {
	c := New(1024, time.Hour)
	now := time.Now()
	var n uint64
	allocs := testing.AllocsPerRun(5000, func() {
		n++
		k := key(1, n)
		if !c.Seen(k, now) {
			c.Add(k, now)
		}
	})
	if allocs != 0 {
		t.Fatalf("dedupe allocates %v per op", allocs)
	}
}
