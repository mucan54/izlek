// Package dedupe is the node-local idempotency cache for native batches:
// an LRU of (project_id, batch_id) with a TTL (spec/wire.md §4: 100,000
// entries / 10 minutes by default).
package dedupe

import (
	"encoding/binary"
	"sync"
	"time"
)

// Key identifies a batch within a project.
type Key [20]byte

// MakeKey builds the cache key for (projectID, batchID).
func MakeKey(projectID uint32, batchID [16]byte) (k Key) {
	binary.LittleEndian.PutUint32(k[:4], projectID)
	copy(k[4:], batchID[:])
	return k
}

type node struct {
	key        Key
	expires    int64 // unix nanoseconds
	prev, next int32
}

// Cache is a fixed-capacity LRU with per-entry expiry. Nodes live in one
// preallocated slice (index-linked list), so the cache adds no per-entry
// garbage. The zero value is not usable; call New.
type Cache struct {
	mu    sync.Mutex
	ttl   time.Duration
	nodes []node
	index map[Key]int32
	head  int32 // most recently added, -1 if empty
	tail  int32 // least recently added
	free  int32 // next never-used slot
}

// New returns a cache holding at most capacity keys for ttl each.
func New(capacity int, ttl time.Duration) *Cache {
	if capacity < 1 {
		capacity = 1
	}
	return &Cache{ttl: ttl, nodes: make([]node, capacity), index: make(map[Key]int32, capacity), head: -1, tail: -1}
}

// Seen reports whether k was added within the TTL.
func (c *Cache) Seen(k Key, now time.Time) bool {
	c.mu.Lock()
	i, ok := c.index[k]
	seen := ok && c.nodes[i].expires > now.UnixNano()
	c.mu.Unlock()
	return seen
}

// Add records k as seen at time now (refreshing it if already present),
// evicting the oldest entry when full.
func (c *Cache) Add(k Key, now time.Time) {
	exp := now.Add(c.ttl).UnixNano()
	c.mu.Lock()
	defer c.mu.Unlock()
	if i, ok := c.index[k]; ok {
		c.nodes[i].expires = exp
		c.unlink(i)
		c.pushFront(i)
		return
	}
	var i int32
	if int(c.free) < len(c.nodes) {
		i = c.free
		c.free++
	} else {
		i = c.tail
		c.unlink(i)
		delete(c.index, c.nodes[i].key)
	}
	c.nodes[i].key, c.nodes[i].expires = k, exp
	c.index[k] = i
	c.pushFront(i)
}

// Len returns the number of keys held (including expired, not yet evicted).
func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.index)
}

func (c *Cache) unlink(i int32) {
	n := &c.nodes[i]
	if n.prev >= 0 {
		c.nodes[n.prev].next = n.next
	} else {
		c.head = n.next
	}
	if n.next >= 0 {
		c.nodes[n.next].prev = n.prev
	} else {
		c.tail = n.prev
	}
}

func (c *Cache) pushFront(i int32) {
	n := &c.nodes[i]
	n.prev, n.next = -1, c.head
	if c.head >= 0 {
		c.nodes[c.head].prev = i
	}
	c.head = i
	if c.tail < 0 {
		c.tail = i
	}
}
