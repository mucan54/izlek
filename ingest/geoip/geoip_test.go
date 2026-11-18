package geoip

import (
	"encoding/binary"
	"math/rand"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
)

// ---- synthetic MMDB writer (test fixture only) ------------------------------

func mmCtrl(typ, size int) []byte {
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

func mmString(s string) []byte { return append(mmCtrl(typeString, len(s)), s...) }
func mmMap(n int) []byte       { return mmCtrl(typeMap, n) }
func mmUint(typ int, v uint64, width int) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	return append(mmCtrl(typ, width), b[8-width:]...)
}

// mmPointer encodes a pointer to data-section offset off using the smallest
// form (sizes 0..2 are exercised by the fixture; 3 is forced with wide).
func mmPointer(off int, wide bool) []byte {
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

type mmNet struct {
	prefix string
	record []byte
}

// buildMMDB assembles a complete database: search tree, 16-byte separator,
// data section, metadata.
func buildMMDB(t testing.TB, ipVersion, recordSize int, prelude []byte, nets []mmNet) []byte {
	t.Helper()
	data := append([]byte(nil), prelude...)
	root := &trieNode{data: -1}
	for _, n := range nets {
		p := netip.MustParsePrefix(n.prefix)
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
		data = append(data, n.record...)
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
			t.Fatalf("record size %d", recordSize)
		}
	}
	out := append(tree, make([]byte, 16)...)
	out = append(out, data...)
	out = append(out, metadataMarker...)
	meta := mmMap(5)
	meta = append(meta, mmString("binary_format_major_version")...)
	meta = append(meta, mmUint(typeUint16, 2, 2)...)
	meta = append(meta, mmString("database_type")...)
	meta = append(meta, mmString("Test-Country")...)
	meta = append(meta, mmString("ip_version")...)
	meta = append(meta, mmUint(typeUint16, uint64(ipVersion), 2)...)
	meta = append(meta, mmString("node_count")...)
	meta = append(meta, mmUint(typeUint32, uint64(nodeCount), 4)...)
	meta = append(meta, mmString("record_size")...)
	meta = append(meta, mmUint(typeUint16, uint64(recordSize), 2)...)
	return append(out, meta...)
}

func countryRec(code string) []byte {
	// {"country": {"iso_code": code}}
	r := mmMap(1)
	r = append(r, mmString("country")...)
	r = append(r, mmMap(1)...)
	r = append(r, mmString("iso_code")...)
	return append(r, mmString(code)...)
}

// fixture returns a database exercising the layouts and encodings the reader
// must cope with.
func fixture(t testing.TB, ipVersion, recordSize int, pad int) []byte {
	// Prelude: shared strings at fixed data offsets, referenced by pointers.
	prelude := mmString("country") // offset 0, 8 bytes
	isoOff := len(prelude)
	prelude = append(prelude, mmString("iso_code")...)
	prelude = append(prelude, make([]byte, pad)...) // pushes later offsets into wider pointer forms
	sharedOff := len(prelude)
	prelude = append(prelude, countryRec("NL")...)

	// GeoLite2-like record with other fields first and pointer-encoded keys.
	rich := mmMap(4)
	rich = append(rich, mmString("continent")...)
	rich = append(rich, mmMap(2)...)
	rich = append(rich, mmString("code")...)
	rich = append(rich, mmString("EU")...)
	rich = append(rich, mmString("names")...)
	rich = append(rich, mmCtrl(typeArray, 3)...)
	rich = append(rich, mmString("Europe")...)
	rich = append(rich, mmCtrl(typeBool, 1)...)
	rich = append(rich, append(mmCtrl(typeDouble, 8), 1, 2, 3, 4, 5, 6, 7, 8)...)
	rich = append(rich, mmString("big")...)
	rich = append(rich, mmUint(typeUint128, 7, 8)...)
	rich = append(rich, mmString("long")...)
	rich = append(rich, mmString(string(make([]byte, 400)))...) // 2-byte size form
	rich = append(rich, mmPointer(0, false)...)                 // key "country" via pointer
	rich = append(rich, mmMap(2)...)
	rich = append(rich, mmString("geoname_id")...)
	rich = append(rich, mmUint(typeUint32, 298795, 4)...)
	rich = append(rich, mmPointer(isoOff, true)...) // key "iso_code" via 4-byte pointer
	rich = append(rich, mmString("TR")...)

	flat := append(append(mmMap(1), mmString("country")...), mmString("FR")...)
	cc := append(append(mmMap(1), mmString("country_code")...), mmString("JP")...)
	reg := mmMap(1)
	reg = append(reg, mmString("registered_country")...)
	reg = append(reg, mmMap(1)...)
	reg = append(reg, mmString("iso_code")...)
	reg = append(reg, mmString("BR")...)

	nets := []mmNet{
		{"81.214.0.0/16", rich},
		{"8.8.8.0/24", countryRec("US")},
		{"1.1.1.1/32", countryRec("AU")},
		{"192.0.2.0/25", mmPointer(sharedOff, false)}, // whole record is a pointer
		{"198.51.100.0/24", countryRec("tr")},         // lower case is normalised
		{"203.0.113.0/24", countryRec("T1")},          // not two letters -> ZZ
		{"100.64.0.0/10", mmMap(0)},                   // no country at all -> ZZ
		{"10.1.0.0/16", flat},
		{"10.2.0.0/16", cc},
		{"10.3.0.0/16", reg},
	}
	if ipVersion == 6 {
		nets = append(nets, mmNet{"2001:db8::/32", countryRec("DE")}, mmNet{"2a02:ff80::/28", countryRec("SE")})
	}
	return buildMMDB(t, ipVersion, recordSize, prelude, nets)
}

func TestCountryLookup(t *testing.T) {
	cases := []struct{ ip, want string }{
		{"81.214.10.20", "TR"},
		{"81.214.255.255", "TR"},
		{"81.215.0.0", "ZZ"},
		{"8.8.8.8", "US"},
		{"8.8.9.8", "ZZ"},
		{"1.1.1.1", "AU"},
		{"1.1.1.2", "ZZ"},
		{"192.0.2.100", "NL"},
		{"192.0.2.200", "ZZ"},
		{"198.51.100.7", "TR"},
		{"203.0.113.9", "ZZ"},
		{"100.64.1.1", "ZZ"},
		{"10.1.2.3", "FR"},
		{"10.2.2.3", "JP"},
		{"10.3.2.3", "BR"},
		{"::ffff:8.8.8.8", "US"}, // IPv4-mapped is looked up as IPv4
	}
	v6 := []struct{ ip, want string }{
		{"2001:db8::1", "DE"},
		{"2001:db8:ffff:ffff:ffff:ffff:ffff:ffff", "DE"},
		{"2001:db9::1", "ZZ"},
		{"2a02:ff8f::1", "SE"},
		{"2a02:ff90::1", "ZZ"},
		{"::1", "ZZ"},
	}
	for _, v := range []struct{ ipVersion, recordSize, pad int }{
		{6, 24, 0}, {6, 28, 0}, {6, 32, 0}, {4, 24, 0}, {4, 28, 0},
		{6, 24, 3000},   // record offsets need 3-byte pointers
		{6, 32, 600000}, // ... and 4-byte ones
	} {
		raw := fixture(t, v.ipVersion, v.recordSize, v.pad)
		db, err := Parse(raw)
		if err != nil {
			t.Fatalf("v%d/%d: %v", v.ipVersion, v.recordSize, err)
		}
		all := cases
		if v.ipVersion == 6 {
			all = append(append([]struct{ ip, want string }{}, cases...), v6...)
		}
		for _, c := range all {
			got := db.Country(netip.MustParseAddr(c.ip))
			if string(got[:]) != c.want {
				t.Errorf("v%d rs%d pad%d: Country(%s) = %s, want %s", v.ipVersion, v.recordSize, v.pad, c.ip, got, c.want)
			}
		}
		if v.ipVersion == 4 {
			if got := db.Country(netip.MustParseAddr("2001:db8::1")); got != Unknown {
				t.Errorf("IPv6 lookup in IPv4-only db = %s", got)
			}
		}
	}
}

func TestOpenFileAndNilDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "country.mmdb")
	if err := os.WriteFile(path, fixture(t, 6, 24, 0), 0o644); err != nil {
		t.Fatal(err)
	}
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := db.Country(netip.MustParseAddr("8.8.8.8")); string(got[:]) != "US" {
		t.Fatalf("got %s", got)
	}
	if got := db.Country(netip.Addr{}); got != Unknown {
		t.Fatalf("invalid addr: %s", got)
	}
	var none *DB
	if got := none.Country(netip.MustParseAddr("8.8.8.8")); got != Unknown {
		t.Fatalf("nil db: %s", got)
	}
	if _, err := Open(filepath.Join(t.TempDir(), "missing.mmdb")); err == nil {
		t.Fatal("opened a missing file")
	}
	for _, bad := range [][]byte{nil, []byte("not an mmdb"), metadataMarker, append([]byte("xx"), metadataMarker...)} {
		if _, err := Parse(bad); err == nil {
			t.Fatalf("parsed garbage %q", bad)
		}
	}
	allocs := testing.AllocsPerRun(200, func() { db.Country(netip.MustParseAddr("81.214.1.1")) })
	if allocs != 0 {
		t.Fatalf("Country allocates %v per lookup", allocs)
	}
}

// A damaged database must never panic or hang a request.
func TestCorruptDatabaseDoesNotPanic(t *testing.T) {
	good := fixture(t, 6, 28, 0)
	rng := rand.New(rand.NewSource(1))
	addrs := []netip.Addr{netip.MustParseAddr("81.214.1.1"), netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("192.0.2.1"),
		netip.MustParseAddr("2001:db8::1"), netip.MustParseAddr("10.3.0.1"), netip.MustParseAddr("255.255.255.255")}
	for i := 0; i < 20000; i++ {
		raw := append([]byte(nil), good...)
		for n := rng.Intn(4) + 1; n > 0; n-- {
			raw[rng.Intn(len(raw))] = byte(rng.Intn(256))
		}
		if rng.Intn(4) == 0 {
			raw = raw[:rng.Intn(len(raw))]
		}
		db, err := Parse(raw)
		if err != nil {
			continue
		}
		for _, a := range addrs {
			db.Country(a)
		}
	}
}

func FuzzParse(f *testing.F) {
	f.Add(fixture(f, 6, 24, 0))
	f.Add(fixture(f, 4, 28, 0))
	f.Add([]byte("\xab\xcd\xefMaxMind.com\xe0"))
	f.Fuzz(func(t *testing.T, raw []byte) {
		db, err := Parse(raw)
		if err != nil {
			return
		}
		db.Country(netip.MustParseAddr("81.214.1.1"))
		db.Country(netip.MustParseAddr("2001:db8::1"))
	})
}

func TestClientIP(t *testing.T) {
	req := func(remote string, xff ...string) *http.Request {
		r, _ := http.NewRequest("POST", "http://x/v1/e", nil)
		r.RemoteAddr = remote
		for _, v := range xff {
			r.Header.Add("X-Forwarded-For", v)
		}
		return r
	}
	cases := []struct {
		name string
		r    *http.Request
		hops int
		want string
	}{
		{"direct, header ignored", req("203.0.113.5:4711", "1.2.3.4"), 0, "203.0.113.5"},
		{"direct ipv6", req("[2001:db8::7]:4711"), 0, "2001:db8::7"},
		{"one proxy", req("10.0.0.1:1", "198.51.100.9"), 1, "198.51.100.9"},
		{"one proxy, client spoofs a prefix", req("10.0.0.1:1", "6.6.6.6, 198.51.100.9"), 1, "198.51.100.9"},
		{"two proxies", req("10.0.0.1:1", "6.6.6.6, 198.51.100.9, 10.0.0.2"), 2, "198.51.100.9"},
		{"two proxies, split headers", req("10.0.0.1:1", "6.6.6.6, 198.51.100.9", "10.0.0.2"), 2, "198.51.100.9"},
		{"more hops than entries: leftmost", req("10.0.0.1:1", "198.51.100.9"), 3, "198.51.100.9"},
		{"proxy configured but no header", req("203.0.113.5:4711"), 1, "203.0.113.5"},
		{"ipv6 and ports in header", req("10.0.0.1:1", "[2001:db8::9]:55000"), 1, "2001:db8::9"},
		{"v4-mapped peer", req("[::ffff:203.0.113.5]:80"), 0, "203.0.113.5"},
		{"garbage entry stops at last good hop", req("10.0.0.1:1", "198.51.100.9, unknown"), 1, "10.0.0.1"},
		{"garbage beyond trusted hops is never read", req("10.0.0.1:1", "<script>, 198.51.100.9"), 1, "198.51.100.9"},
		{"spaces", req("10.0.0.1:1", "  198.51.100.9  "), 1, "198.51.100.9"},
	}
	for _, c := range cases {
		if got := ClientIP(c.r, c.hops); got.String() != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
	if got := ClientIP(req("not-an-address"), 0); got.IsValid() {
		t.Errorf("unparseable peer: %s", got)
	}
}
