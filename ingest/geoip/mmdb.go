// Package geoip derives a country code from the client address. The address
// itself is never stored (design §6.2, §15).
//
// It contains a minimal reader for the MaxMind DB (MMDB) binary format,
// enough to walk the search tree and read the country ISO code of a record;
// it works with GeoLite2-Country / DB-IP country style databases.
package geoip

import (
	"bytes"
	"errors"
	"fmt"
	"net/netip"
	"os"
)

// Unknown is the country code used when the address cannot be resolved.
var Unknown = [2]byte{'Z', 'Z'}

var metadataMarker = []byte("\xab\xcd\xefMaxMind.com")

// MMDB data types.
const (
	typePointer = 1
	typeString  = 2
	typeDouble  = 3
	typeBytes   = 4
	typeUint16  = 5
	typeUint32  = 6
	typeMap     = 7
	typeInt32   = 8
	typeUint64  = 9
	typeUint128 = 10
	typeArray   = 11
	typeBool    = 14
	typeFloat   = 15
)

var errCorrupt = errors.New("geoip: corrupt mmdb")

// DB is an open, fully in-memory MMDB.
type DB struct {
	tree       []byte // search tree
	data       []byte // data section
	nodeCount  uint32
	recordSize uint
	ipVersion  uint
	v4Start    uint32 // node where the IPv4 subtree starts in an IPv6 tree
}

// Open reads the database file at path.
func Open(path string) (*DB, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("geoip: %w", err)
	}
	return Parse(raw)
}

// Parse opens a database held in memory. raw must not be modified afterwards.
func Parse(raw []byte) (*DB, error) {
	i := bytes.LastIndex(raw, metadataMarker)
	if i < 0 {
		return nil, errors.New("geoip: not an mmdb file (metadata marker missing)")
	}
	meta := &DB{data: raw[i+len(metadataMarker):]}
	nodeCount, ok1 := meta.mapUint(0, "node_count")
	recordSize, ok2 := meta.mapUint(0, "record_size")
	ipVersion, ok3 := meta.mapUint(0, "ip_version")
	if !ok1 || !ok2 || !ok3 {
		return nil, errors.New("geoip: mmdb metadata incomplete")
	}
	if recordSize != 24 && recordSize != 28 && recordSize != 32 {
		return nil, fmt.Errorf("geoip: unsupported mmdb record size %d", recordSize)
	}
	if ipVersion != 4 && ipVersion != 6 {
		return nil, fmt.Errorf("geoip: unsupported mmdb ip version %d", ipVersion)
	}
	treeSize := nodeCount * recordSize / 4
	if nodeCount > 1<<31 || treeSize+16 > uint64(i) {
		return nil, errCorrupt
	}
	db := &DB{
		tree:       raw[:treeSize],
		data:       raw[treeSize+16 : i],
		nodeCount:  uint32(nodeCount),
		recordSize: uint(recordSize),
		ipVersion:  uint(ipVersion),
	}
	if db.ipVersion == 6 {
		// IPv4 addresses live under ::/96.
		node := uint32(0)
		for b := 0; b < 96 && node < db.nodeCount; b++ {
			node = db.record(node, 0)
		}
		db.v4Start = node
	}
	return db, nil
}

// record returns the left (bit 0) or right (bit 1) record of a node.
func (db *DB) record(node uint32, bit byte) uint32 {
	switch db.recordSize {
	case 24:
		p := db.tree[uint(node)*6+uint(bit)*3:]
		return uint32(p[0])<<16 | uint32(p[1])<<8 | uint32(p[2])
	case 28:
		p := db.tree[uint(node)*7:]
		if bit == 0 {
			return uint32(p[3]>>4)<<24 | uint32(p[0])<<16 | uint32(p[1])<<8 | uint32(p[2])
		}
		return uint32(p[3]&0x0f)<<24 | uint32(p[4])<<16 | uint32(p[5])<<8 | uint32(p[6])
	default:
		p := db.tree[uint(node)*8+uint(bit)*4:]
		return uint32(p[0])<<24 | uint32(p[1])<<16 | uint32(p[2])<<8 | uint32(p[3])
	}
}

// Country returns the ISO-3166-1 alpha-2 code for addr, or Unknown. It does
// not allocate.
func (db *DB) Country(addr netip.Addr) [2]byte {
	if db == nil || !addr.IsValid() {
		return Unknown
	}
	addr = addr.Unmap()
	var ip [16]byte
	var bits int
	node := uint32(0)
	if addr.Is4() {
		a4 := addr.As4()
		copy(ip[:], a4[:])
		bits = 32
		node = db.v4Start // 0 for an IPv4-only tree
	} else {
		if db.ipVersion == 4 {
			return Unknown
		}
		ip = addr.As16()
		bits = 128
	}
	for i := 0; i < bits && node < db.nodeCount; i++ {
		node = db.record(node, ip[i>>3]>>(7-uint(i&7))&1)
	}
	if node <= db.nodeCount { // == nodeCount: no data; < nodeCount: ran out of bits
		return Unknown
	}
	off := uint64(node) - uint64(db.nodeCount) - 16
	if off >= uint64(len(db.data)) {
		return Unknown
	}
	return db.countryAt(uint(off))
}

func (db *DB) countryAt(rec uint) [2]byte {
	// GeoLite2 / DB-IP layout: {"country": {"iso_code": "TR"}}.
	if v, ok := db.mapGet(rec, "country"); ok {
		if c, ok := db.mapGet(v, "iso_code"); ok {
			return db.code(c)
		}
		if c := db.code(v); c != Unknown { // flat layout: {"country": "TR"}
			return c
		}
	}
	if v, ok := db.mapGet(rec, "country_code"); ok {
		return db.code(v)
	}
	if v, ok := db.mapGet(rec, "registered_country"); ok {
		if c, ok := db.mapGet(v, "iso_code"); ok {
			return db.code(c)
		}
	}
	return Unknown
}

func (db *DB) code(off uint) [2]byte {
	typ, size, p, err := db.control(off, 0)
	if err != nil || typ != typeString || size != 2 || p+2 > uint(len(db.data)) {
		return Unknown
	}
	c := [2]byte{db.data[p] &^ 0x20, db.data[p+1] &^ 0x20} // upper-case ASCII
	if c[0] < 'A' || c[0] > 'Z' || c[1] < 'A' || c[1] > 'Z' {
		return Unknown
	}
	return c
}

// control decodes the control byte(s) at off, following pointers. It returns
// the value type, its size field and the offset of its payload.
func (db *DB) control(off uint, depth int) (typ, size, payload uint, err error) {
	d := db.data
	if off >= uint(len(d)) || depth > 8 {
		return 0, 0, 0, errCorrupt
	}
	ctrl := d[off]
	off++
	typ = uint(ctrl >> 5)
	if typ == typePointer {
		psize := uint(ctrl>>3) & 3
		if off+psize+1 > uint(len(d)) {
			return 0, 0, 0, errCorrupt
		}
		var ptr uint
		switch psize {
		case 0:
			ptr = uint(ctrl&7)<<8 | uint(d[off])
		case 1:
			ptr = 2048 + (uint(ctrl&7)<<16 | uint(d[off])<<8 | uint(d[off+1]))
		case 2:
			ptr = 526336 + (uint(ctrl&7)<<24 | uint(d[off])<<16 | uint(d[off+1])<<8 | uint(d[off+2]))
		default:
			ptr = uint(d[off])<<24 | uint(d[off+1])<<16 | uint(d[off+2])<<8 | uint(d[off+3])
		}
		return db.control(ptr, depth+1)
	}
	if typ == 0 { // extended type
		if off >= uint(len(d)) {
			return 0, 0, 0, errCorrupt
		}
		typ = 7 + uint(d[off])
		off++
	}
	size = uint(ctrl & 0x1f)
	switch {
	case size == 29:
		if off+1 > uint(len(d)) {
			return 0, 0, 0, errCorrupt
		}
		size = 29 + uint(d[off])
		off++
	case size == 30:
		if off+2 > uint(len(d)) {
			return 0, 0, 0, errCorrupt
		}
		size = 285 + (uint(d[off])<<8 | uint(d[off+1]))
		off += 2
	case size == 31:
		if off+3 > uint(len(d)) {
			return 0, 0, 0, errCorrupt
		}
		size = 65821 + (uint(d[off])<<16 | uint(d[off+1])<<8 | uint(d[off+2]))
		off += 3
	}
	return typ, size, off, nil
}

// skip returns the offset just past the value starting at off (a pointer
// counts as its own bytes only; it is not followed).
func (db *DB) skip(off uint, depth int) (uint, error) {
	d := db.data
	if off >= uint(len(d)) || depth > 64 {
		return 0, errCorrupt
	}
	if d[off]>>5 == typePointer {
		return off + 2 + uint(d[off]>>3)&3, nil
	}
	typ, size, p, err := db.control(off, 0)
	if err != nil {
		return 0, err
	}
	switch typ {
	case typeMap:
		for i := uint(0); i < size; i++ {
			if p, err = db.skip(p, depth+1); err != nil { // key
				return 0, err
			}
			if p, err = db.skip(p, depth+1); err != nil { // value
				return 0, err
			}
		}
		return p, nil
	case typeArray:
		for i := uint(0); i < size; i++ {
			if p, err = db.skip(p, depth+1); err != nil {
				return 0, err
			}
		}
		return p, nil
	case typeBool:
		return p, nil // the size field is the value
	default:
		if p+size > uint(len(d)) {
			return 0, errCorrupt
		}
		return p + size, nil
	}
}

// mapGet looks key up in the map at off and returns the offset of its value.
func (db *DB) mapGet(off uint, key string) (uint, bool) {
	typ, n, p, err := db.control(off, 0)
	if err != nil || typ != typeMap {
		return 0, false
	}
	for i := uint(0); i < n; i++ {
		ktyp, ksize, kp, err := db.control(p, 0)
		if err != nil || ktyp != typeString || kp+ksize > uint(len(db.data)) {
			return 0, false
		}
		match := string(db.data[kp:kp+ksize]) == key
		if p, err = db.skip(p, 0); err != nil { // past the key (or the pointer to it)
			return 0, false
		}
		if match {
			return p, true
		}
		if p, err = db.skip(p, 0); err != nil {
			return 0, false
		}
	}
	return 0, false
}

// mapUint reads an unsigned integer field of the map at off.
func (db *DB) mapUint(off uint, key string) (uint64, bool) {
	v, ok := db.mapGet(off, key)
	if !ok {
		return 0, false
	}
	typ, size, p, err := db.control(v, 0)
	if err != nil || (typ != typeUint16 && typ != typeUint32 && typ != typeUint64) || size > 8 || p+size > uint(len(db.data)) {
		return 0, false
	}
	var n uint64
	for _, b := range db.data[p : p+size] {
		n = n<<8 | uint64(b)
	}
	return n, true
}
