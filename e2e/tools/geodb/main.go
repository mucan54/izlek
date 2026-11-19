// Command geodb writes a small synthetic country database (MMDB) for demos
// and screenshots: a handful of made-up IPv4 prefixes, one per country. It is
// not real GeoIP data.
//
//	go run ./e2e/tools/geodb /tmp/demo-geo.mmdb
package main

import (
	"fmt"
	"os"

	"github.com/mucan54/izlek/ingest/geoip/geoiptest"
)

// Prefixes maps demo client prefixes to ISO country codes.
var Prefixes = map[string]string{
	"11.1.0.0/16": "TR", "11.2.0.0/16": "DE", "11.3.0.0/16": "US",
	"11.4.0.0/16": "GB", "11.5.0.0/16": "NL", "11.6.0.0/16": "FR",
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: geodb <output.mmdb>")
		os.Exit(2)
	}
	if err := os.WriteFile(os.Args[1], geoiptest.CountryDB(Prefixes), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
