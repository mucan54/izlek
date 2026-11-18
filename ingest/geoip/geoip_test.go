package geoip

import (
	"math/rand"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"testing"

	"github.com/mucan54/izlek/ingest/geoip/geoiptest"
)

func countryRec(code string) []byte { return geoiptest.CountryRecord(code) }

// fixture returns a database exercising the layouts and encodings the reader
// must cope with.
func fixture(t testing.TB, ipVersion, recordSize int, pad int) []byte {
	// Prelude: shared strings at fixed data offsets, referenced by pointers.
	prelude := geoiptest.String("country") // offset 0, 8 bytes
	isoOff := len(prelude)
	prelude = append(prelude, geoiptest.String("iso_code")...)
	prelude = append(prelude, make([]byte, pad)...) // pushes later offsets into wider pointer forms
	sharedOff := len(prelude)
	prelude = append(prelude, countryRec("NL")...)

	// GeoLite2-like record with other fields first and pointer-encoded keys.
	rich := geoiptest.Map(4)
	rich = append(rich, geoiptest.String("continent")...)
	rich = append(rich, geoiptest.Map(2)...)
	rich = append(rich, geoiptest.String("code")...)
	rich = append(rich, geoiptest.String("EU")...)
	rich = append(rich, geoiptest.String("names")...)
	rich = append(rich, geoiptest.Ctrl(typeArray, 3)...)
	rich = append(rich, geoiptest.String("Europe")...)
	rich = append(rich, geoiptest.Ctrl(typeBool, 1)...)
	rich = append(rich, append(geoiptest.Ctrl(typeDouble, 8), 1, 2, 3, 4, 5, 6, 7, 8)...)
	rich = append(rich, geoiptest.String("big")...)
	rich = append(rich, geoiptest.Uint(typeUint128, 7, 8)...)
	rich = append(rich, geoiptest.String("long")...)
	rich = append(rich, geoiptest.String(string(make([]byte, 400)))...) // 2-byte size form
	rich = append(rich, geoiptest.Pointer(0, false)...)                 // key "country" via pointer
	rich = append(rich, geoiptest.Map(2)...)
	rich = append(rich, geoiptest.String("geoname_id")...)
	rich = append(rich, geoiptest.Uint(typeUint32, 298795, 4)...)
	rich = append(rich, geoiptest.Pointer(isoOff, true)...) // key "iso_code" via 4-byte pointer
	rich = append(rich, geoiptest.String("TR")...)

	flat := append(append(geoiptest.Map(1), geoiptest.String("country")...), geoiptest.String("FR")...)
	cc := append(append(geoiptest.Map(1), geoiptest.String("country_code")...), geoiptest.String("JP")...)
	reg := geoiptest.Map(1)
	reg = append(reg, geoiptest.String("registered_country")...)
	reg = append(reg, geoiptest.Map(1)...)
	reg = append(reg, geoiptest.String("iso_code")...)
	reg = append(reg, geoiptest.String("BR")...)

	nets := []geoiptest.Net{
		{Prefix: "81.214.0.0/16", Record: rich},
		{Prefix: "8.8.8.0/24", Record: countryRec("US")},
		{Prefix: "1.1.1.1/32", Record: countryRec("AU")},
		{Prefix: "192.0.2.0/25", Record: geoiptest.Pointer(sharedOff, false)}, // whole record is a pointer
		{Prefix: "198.51.100.0/24", Record: countryRec("tr")},                 // lower case is normalised
		{Prefix: "203.0.113.0/24", Record: countryRec("T1")},                  // not two letters -> ZZ
		{Prefix: "100.64.0.0/10", Record: geoiptest.Map(0)},                   // no country at all -> ZZ
		{Prefix: "10.1.0.0/16", Record: flat},
		{Prefix: "10.2.0.0/16", Record: cc},
		{Prefix: "10.3.0.0/16", Record: reg},
	}
	if ipVersion == 6 {
		nets = append(nets, geoiptest.Net{Prefix: "2001:db8::/32", Record: countryRec("DE")}, geoiptest.Net{Prefix: "2a02:ff80::/28", Record: countryRec("SE")})
	}
	return geoiptest.Build(ipVersion, recordSize, prelude, nets)
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
