// Command devs3 runs the development S3-compatible server.
//
//	devs3 -listen 127.0.0.1:9000 [-dir /path/to/data]
//
// Point clients at it with LA_S3_ENDPOINT=http://127.0.0.1:9000 and any
// bucket name / credentials (signatures are not verified).
package main

import (
	"flag"
	"log"
	"net"
	"net/http"

	"github.com/mucan54/izlek/golib/devs3"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:9000", "listen address")
	dir := flag.String("dir", "", "persist objects under this directory (default: memory only)")
	flag.Parse()

	srv := devs3.New()
	if *dir != "" {
		var err error
		if srv, err = devs3.NewDir(*dir); err != nil {
			log.Fatalf("devs3: %v", err)
		}
	}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatalf("devs3: %v", err)
	}
	log.Printf("devs3 listening on http://%s (dir=%q)", ln.Addr(), *dir)
	log.Fatal(http.Serve(ln, srv))
}
