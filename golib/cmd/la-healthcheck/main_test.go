package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestProbe(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			w.Write([]byte("ok"))
		case "/nocontent":
			w.WriteHeader(http.StatusNoContent)
		case "/slow":
			time.Sleep(400 * time.Millisecond)
		default:
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer ts.Close()

	for path, wantOK := range map[string]bool{"/ok": true, "/ok?x=1": true, "/nocontent": true, "/down": false} {
		err := probe(ts.URL+path, time.Second)
		if (err == nil) != wantOK {
			t.Errorf("%s: err = %v, want ok = %v", path, err, wantOK)
		}
	}
	if err := probe(ts.URL+"/slow", 100*time.Millisecond); err == nil {
		t.Error("a reply slower than the timeout must fail")
	}
	if err := probe("https://example.com/", time.Second); err == nil || !strings.Contains(err.Error(), "only http://") {
		t.Errorf("https must be refused, got %v", err)
	}

	// Nothing listening.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()
	if err := probe("http://"+addr+"/ok", 300*time.Millisecond); err == nil {
		t.Error("a closed port must fail")
	}

	// A peer that is not HTTP.
	ln, _ = net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Write([]byte("hello\n"))
			c.Close()
		}
	}()
	if err := probe("http://"+ln.Addr().String()+"/", 300*time.Millisecond); err == nil {
		t.Error("a non-HTTP reply must fail")
	}
}
