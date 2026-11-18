// Package murmur3 implements MurmurHash3 x86_32, the hash used for flag
// bucketing and shard selection (spec/flags.md §3, spec/manifest.md §8).
package murmur3

import "math/bits"

const (
	c1 = 0xcc9e2d51
	c2 = 0x1b873593
)

// Sum32 returns the MurmurHash3 x86_32 hash of data with seed 0.
func Sum32(data []byte) uint32 { return Sum32Seed(data, 0) }

// Sum32String is Sum32 over the UTF-8 bytes of s.
func Sum32String(s string) uint32 { return Sum32Seed([]byte(s), 0) }

// Sum32Seed returns the MurmurHash3 x86_32 hash of data with the given seed.
func Sum32Seed(data []byte, seed uint32) uint32 {
	h := seed
	n := len(data)
	i := 0
	for ; i+4 <= n; i += 4 {
		k := uint32(data[i]) | uint32(data[i+1])<<8 | uint32(data[i+2])<<16 | uint32(data[i+3])<<24
		k *= c1
		k = bits.RotateLeft32(k, 15)
		k *= c2
		h ^= k
		h = bits.RotateLeft32(h, 13)
		h = h*5 + 0xe6546b64
	}
	var k uint32
	switch n - i {
	case 3:
		k ^= uint32(data[i+2]) << 16
		fallthrough
	case 2:
		k ^= uint32(data[i+1]) << 8
		fallthrough
	case 1:
		k ^= uint32(data[i])
		k *= c1
		k = bits.RotateLeft32(k, 15)
		k *= c2
		h ^= k
	}
	h ^= uint32(n)
	h ^= h >> 16
	h *= 0x85ebca6b
	h ^= h >> 13
	h *= 0xc2b2ae35
	h ^= h >> 16
	return h
}
