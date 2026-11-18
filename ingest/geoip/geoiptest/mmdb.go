// Package geoiptest builds synthetic MaxMind DB (MMDB) files for tests. It
// is an independent writer of the format (it shares no code with the reader
// in package geoip), so reader bugs are not mirrored here.
package geoiptest

import (
	"encoding/binary"
	"fmt"
	"net/netip"
)

// MMDB data type numbers.
const (
	TypePointer = 1
	TypeString  = 2
	TypeDouble  = 3
	TypeUint16  = 5
	TypeUint32  = 6
	TypeMap     = 7
	TypeUint128 = 10
	TypeArray   = 11
	TypeBool    = 14
)

var metadataMarker = []byte("\xab\xcd\xefMaxMind.com")

func Ctrl(typ, size int) []byte {
	var out []byte
	first := byte(0)
	if typ <= 7 {
		first = byte(typ) << 5
	}
	switch {
	case size < 29:
		out = []byte{first | byte(size)}
	case size < 285:
		out = []byte{first | 29, byte(size - 29)}
	default:
		out = []byte{first | 30, byte((size - 285) >> 8), byte(size - 285)}
	}
	if typ > 7 {
		// the extended type byte follows the first control byte
		out = append(out[:1], append([]byte{byte(typ - 7)}, out[1:]...)...)
	}
	return out
}

func String(s string) []byte { return append(Ctrl(TypeString, len(s)), s...) }
func Map(n int) []byte       { return Ctrl(TypeMap, n) }
func Uint(typ int, v uint64, width int) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	return append(Ctrl(typ, width), b[8-width:]...)
}

// Pointer encodes a pointer to data-section offset off using the smallest
// form (sizes 0..2 are exercised by the fixture; 3 is forced with wide).
func Pointer(off int, wide bool) []byte {
	switch {
	case wide:
		return []byte{1<<5 | 3<<3, byte(off >> 24), byte(off >> 16), byte(off >> 8), byte(off)}
	case off < 2048:
		return []byte{1<<5 | byte(off>>8), byte(off)}
	case off < 526336:
		off -= 2048
		return []byte{1<<5 | 1<<3 | byte(off>>16), byte(off >> 8), byte(off)}
	default:
		off -= 526336
		return []byte{1<<5 | 2<<3 | byte(off>>24), byte(off >> 16), byte(off >> 8), byte(off)}
	}
}

type trieNode struct {
	child [2]*trieNode
	data  int // offset into the data section, -1 if none
	num   int
}

// Net maps a CIDR prefix to an encoded data record.
type Net struct {
	Prefix string
	Record []byte
}

// Build assembles a complete database (prelude is placed at data offset 0, before the
// records): search tree, 16-byte separator,
// data section, metadata.
func Build(ipVersion, recordSize int, prelude []byte, nets []Net) []byte {
	data := append([]byte(nil), prelude...)
	root := &trieNode{data: -1}
	for _, n := range nets {
		p := netip.MustParsePrefix(n.Prefix)
		var bits []byte
		addr := p.Addr()
		raw := addr.AsSlice()
		nbits := p.Bits()
		if addr.Is4() && ipVersion == 6 {
			raw = append(make([]byte, 12), raw...) // ::a.b.c.d
			nbits += 96
		}
		for i := 0; i < nbits; i++ {
			bits = append(bits, raw[i>>3]>>(7-uint(i&7))&1)
		}
		node := root
		for _, b := range bits {
			if node.child[b] == nil {
				node.child[b] = &trieNode{data: -1}
			}
			node = node.child[b]
		}
		node.data = len(data)
		data = append(data, n.Record...)
	}
	// Number the internal nodes breadth-first.
	var internal []*trieNode
	queue := []*trieNode{root}
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		n.num = len(internal)
		internal = append(internal, n)
		for _, c := range n.child {
			if c != nil && c.data < 0 {
				queue = append(queue, c)
			}
		}
	}
	nodeCount := len(internal)
	recVal := func(c *trieNode) uint32 {
		switch {
		case c == nil:
			return uint32(nodeCount)
		case c.data >= 0:
			return uint32(nodeCount + 16 + c.data)
		default:
			return uint32(c.num)
		}
	}
	var tree []byte
	for _, n := range internal {
		l, r := recVal(n.child[0]), recVal(n.child[1])
		switch recordSize {
		case 24:
			tree = append(tree, byte(l>>16), byte(l>>8), byte(l), byte(r>>16), byte(r>>8), byte(r))
		case 28:
			tree = append(tree, byte(l>>16), byte(l>>8), byte(l), byte(l>>24)<<4|byte(r>>24)&0x0f, byte(r>>16), byte(r>>8), byte(r))
		case 32:
			tree = binary.BigEndian.AppendUint32(tree, l)
			tree = binary.BigEndian.AppendUint32(tree, r)
		default:
			panic(fmt.Sprintf("geoiptest: record size %d", recordSize))
		}
	}
	out := append(tree, make([]byte, 16)...)
	out = append(out, data...)
	out = append(out, metadataMarker...)
	meta := Map(5)
	meta = append(meta, String("binary_format_major_version")...)
	meta = append(meta, Uint(TypeUint16, 2, 2)...)
	meta = append(meta, String("database_type")...)
	meta = append(meta, String("Test-Country")...)
	meta = append(meta, String("ip_version")...)
	meta = append(meta, Uint(TypeUint16, uint64(ipVersion), 2)...)
	meta = append(meta, String("node_count")...)
	meta = append(meta, Uint(TypeUint32, uint64(nodeCount), 4)...)
	meta = append(meta, String("record_size")...)
	meta = append(meta, Uint(TypeUint16, uint64(recordSize), 2)...)
	return append(out, meta...)
}

// CountryRecord encodes {"country": {"iso_code": code}}.
func CountryRecord(code string) []byte {
	r := Map(1)
	r = append(r, String("country")...)
	r = append(r, Map(1)...)
	r = append(r, String("iso_code")...)
	return append(r, String(code)...)
}

// CountryDB builds an IPv6-tree database (record size 24) mapping each CIDR
// prefix to a country code.
func CountryDB(prefixToCountry map[string]string) []byte {
	var nets []Net
	for p, c := range prefixToCountry {
		nets = append(nets, Net{Prefix: p, Record: CountryRecord(c)})
	}
	return Build(6, 24, nil, nets)
}
