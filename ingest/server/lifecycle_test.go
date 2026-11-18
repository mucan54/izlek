package server

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/mucan54/izlek/golib/devs3"
	"github.com/mucan54/izlek/golib/objstore"
	"github.com/mucan54/izlek/ingest/geoip/geoiptest"
	"github.com/mucan54/izlek/ingest/wal"
	"github.com/mucan54/izlek/ingest/wire"
)

func batchIDs(t *testing.T, entries []wal.Entry) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, e := range entries {
		if e.Format != wal.FormatNative {
			continue
		}
		f, err := wire.Validate(e.Body)
		if err != nil {
			t.Fatal(err)
		}
		out[string(f.BatchID[:])]++
	}
	return out
}

func idOf(body []byte) string {
	f, _ := wire.Validate(body)
	return string(f.BatchID[:])
}

func TestTokenReloadAndLastGood(t *testing.T) {
	h := newHarness(t, testTokens, func(c *Config) { c.TokensPoll = 15 * time.Millisecond })
	ctx := context.Background()
	if got := h.send(mkBatch(1, 1)); got != 200 {
		t.Fatal(got)
	}
	waitStatus := func(desc string, token string, want int) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		id := uint32(time.Now().UnixNano())
		for {
			id++
			got := h.status("POST", "/v1/e", mkBatch(id, 1), "Authorization", "Bearer "+token)
			if got == want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: status stayed %d, want %d", desc, got, want)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	// Control plane disables the token, then removes it, then adds a new one.
	put := func(doc string) {
		if _, err := h.store.Put(ctx, "control/tokens.json", []byte(doc)); err != nil {
			t.Fatal(err)
		}
	}
	put(`{"version":2,"tokens":{"pk_test":{"project_id":1017,"status":"disabled"}}}`)
	waitStatus("disabled by reload", "pk_test", 403)
	put(`{"version":3,"tokens":{"pk_new":{"project_id":7,"status":"active"}}}`)
	waitStatus("removed by reload", "pk_test", 401)
	waitStatus("added by reload", "pk_new", 200)

	// A broken file and then a store outage: the last good set keeps working.
	put(`{"version":4,"tokens":`)
	time.Sleep(60 * time.Millisecond)
	if got := h.status("POST", "/v1/e", mkBatch(50, 1), "Authorization", "Bearer pk_new"); got != 200 {
		t.Fatalf("after broken tokens file: %d", got)
	}
	put(`{"version":5,"tokens":{"pk_new":{"project_id":7,"status":"active"},"pk_v5":{"project_id":8,"status":"active"}}}`)
	waitStatus("v5", "pk_v5", 200)
	h.waitShipped()
	h.s3.SetFault(devs3.FaultFail)
	time.Sleep(60 * time.Millisecond)
	if got := h.status("POST", "/v1/e", mkBatch(51, 1), "Authorization", "Bearer pk_v5"); got != 200 {
		t.Fatalf("during store outage: %d", got)
	}
	if h.metric("la_ingest_tokens_refresh_failures") < 2 {
		t.Fatalf("refresh failures not counted: %v", h.metric("la_ingest_tokens_refresh_failures"))
	}

	// Restart during the outage: the node comes up from the disk copy.
	h.kill()
	cache, err := os.ReadFile(filepath.Join(h.cfg.WALDir, "tokens.json"))
	if err != nil || !strings.Contains(string(cache), `"version":5`) {
		t.Fatalf("last-good copy on disk: %v %s", err, cache)
	}
	h.start()
	if got := h.status("POST", "/v1/e", mkBatch(52, 1), "Authorization", "Bearer pk_v5"); got != 200 {
		t.Fatalf("after restart during outage: %d", got)
	}
	if got := h.status("POST", "/v1/e", mkBatch(53, 1), "Authorization", "Bearer pk_test"); got != 401 {
		t.Fatalf("removed token after restart: %d", got)
	}
	if got := h.status("GET", "/readyz", nil); got != 200 {
		t.Fatalf("readyz with last-good tokens: %d", got)
	}
	if !strings.Contains(h.logs.String(), "using last-good copy") {
		t.Fatal("fallback not logged")
	}
	h.s3.SetFault(devs3.FaultNone)
	h.waitShipped()
}

// Without any token set a node cannot tell a valid token from an invalid
// one. It must answer 503 (retry), never 401 (SDK drops the batch).
func TestNoTokensMeans503(t *testing.T) {
	h := newHarness(t, "", func(c *Config) { c.TokensPoll = 15 * time.Millisecond })
	resp := h.do("POST", "/v1/e", mkBatch(1, 1), "Authorization", "Bearer pk_test")
	resp.Body.Close()
	if resp.StatusCode != 503 || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("no tokens loaded: %d", resp.StatusCode)
	}
	if got := h.status("POST", "/batch", []byte(`{"api_key":"pk_test","batch":[{"event":"e"}]}`)); got != 503 {
		t.Fatalf("posthog without tokens: %d", got)
	}
	resp = h.do("GET", "/readyz", nil)
	if txt := readAll(t, resp); resp.StatusCode != 503 || !strings.Contains(txt, "tokens_not_loaded") {
		t.Fatalf("readyz: %d %s", resp.StatusCode, txt)
	}
	if h.metric("la_ingest_tokens_age_seconds") != -1 {
		t.Fatalf("tokens_age_seconds = %v, want -1", h.metric("la_ingest_tokens_age_seconds"))
	}
	// The control plane publishes the file: the poller picks it up.
	h.store.Put(context.Background(), "control/tokens.json", []byte(testTokens))
	deadline := time.Now().Add(5 * time.Second)
	for h.status("GET", "/readyz", nil) != 200 {
		if time.Now().After(deadline) {
			t.Fatal("node never became ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := h.send(mkBatch(2, 1)); got != 200 {
		t.Fatalf("after tokens arrived: %d", got)
	}
}

func TestOriginAndCORS(t *testing.T) {
	h := newHarness(t, testTokens, nil)
	const good, evil = "https://app.example.com", "https://evil.example"

	// Preflight: no Authorization header is available.
	resp := h.do("OPTIONS", "/v1/e", nil, "Origin", evil, "Access-Control-Request-Method", "POST",
		"Access-Control-Request-Headers", "authorization,content-encoding")
	resp.Body.Close()
	// pk_test has no allow-list, so some token accepts any origin.
	if resp.StatusCode != 204 || resp.Header.Get("Access-Control-Allow-Origin") != evil ||
		resp.Header.Get("Access-Control-Max-Age") != "86400" ||
		resp.Header.Get("Access-Control-Allow-Headers") != "authorization,content-encoding" ||
		!strings.Contains(resp.Header.Get("Access-Control-Allow-Methods"), "POST") {
		t.Fatalf("preflight: %d %v", resp.StatusCode, resp.Header)
	}
	// With the token in the URL the preflight is token-specific.
	resp = h.do("OPTIONS", "/v1/e?t=pk_web", nil, "Origin", evil, "Access-Control-Request-Method", "POST")
	resp.Body.Close()
	if resp.StatusCode != 403 || resp.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("preflight for a foreign origin on a restricted token: %d %v", resp.StatusCode, resp.Header)
	}
	resp = h.do("OPTIONS", "/v1/e?t=pk_web", nil, "Origin", good, "Access-Control-Request-Method", "POST")
	resp.Body.Close()
	if resp.StatusCode != 204 || resp.Header.Get("Access-Control-Allow-Origin") != good {
		t.Fatalf("preflight for the allowed origin: %d %v", resp.StatusCode, resp.Header)
	}

	// Actual requests are checked against their own token.
	resp = h.do("POST", "/v1/e", mkBatch(1, 1), "Authorization", "Bearer pk_web", "Origin", good)
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Access-Control-Allow-Origin") != good || !strings.Contains(resp.Header.Get("Vary"), "Origin") {
		t.Fatalf("allowed origin: %d %v", resp.StatusCode, resp.Header)
	}
	resp = h.do("POST", "/v1/e", mkBatch(2, 1), "Authorization", "Bearer pk_web", "Origin", evil)
	resp.Body.Close()
	if resp.StatusCode != 403 || resp.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("foreign origin on restricted token: %d %v", resp.StatusCode, resp.Header)
	}
	// No Origin header (mobile / server SDK): the allow-list does not apply.
	if got := h.status("POST", "/v1/e", mkBatch(3, 1), "Authorization", "Bearer pk_web"); got != 200 {
		t.Fatalf("no origin on restricted token: %d", got)
	}
	// Unrestricted token accepts any origin.
	resp = h.do("POST", "/v1/e", mkBatch(4, 1), "Authorization", "Bearer pk_test", "Origin", evil)
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Access-Control-Allow-Origin") != evil {
		t.Fatalf("unrestricted token: %d", resp.StatusCode)
	}
	// Error statuses stay readable for the web SDK's retry logic.
	resp = h.do("POST", "/v1/e", []byte("junk"), "Authorization", "Bearer pk_web", "Origin", good)
	resp.Body.Close()
	if resp.StatusCode != 400 || resp.Header.Get("Access-Control-Allow-Origin") != good {
		t.Fatalf("400 without CORS header: %d %v", resp.StatusCode, resp.Header)
	}
	// PostHog adapter: same allow-list, token from the body.
	phBody := []byte(`{"api_key":"pk_web","batch":[{"event":"e","distinct_id":"d"}]}`)
	if got := h.status("POST", "/batch/", phBody, "Origin", evil); got != 403 {
		t.Fatalf("posthog foreign origin: %d", got)
	}
	resp = h.do("POST", "/batch/", phBody, "Origin", good)
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Access-Control-Allow-Origin") != good {
		t.Fatalf("posthog allowed origin: %d", resp.StatusCode)
	}

	// Envelope flags bit0 = the request carried an Origin header.
	web := map[string]uint8{}
	for _, e := range h.shipped() {
		if e.Format == wal.FormatNative {
			web[idOf(e.Body)] = e.Flags
		} else if e.Flags != wal.FlagWeb || e.ProjectID != 2000 {
			t.Fatalf("posthog web entry envelope %+v", e.Envelope)
		}
	}
	if len(web) != 3 || web[idOf(mkBatch(1, 1))] != wal.FlagWeb || web[idOf(mkBatch(3, 1))] != 0 || web[idOf(mkBatch(4, 1))] != wal.FlagWeb {
		t.Fatalf("web flags: %v", web)
	}
}

func TestCountryFromClientIP(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "country.mmdb")
	db := geoiptest.CountryDB(map[string]string{"127.0.0.0/8": "XL", "81.214.0.0/16": "TR", "2001:db8::/32": "DE"})
	if err := os.WriteFile(dbPath, db, 0o644); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, testTokens, func(c *Config) {
		c.GeoDB = dbPath
		c.TrustedProxies = 1
	})
	send := func(id uint32, xff string) {
		hdr := []string{"Authorization", "Bearer pk_test"}
		if xff != "" {
			hdr = append(hdr, "X-Forwarded-For", xff)
		}
		if got := h.status("POST", "/v1/e", mkBatch(id, 1), hdr...); got != 200 {
			t.Fatalf("send %d: %d", id, got)
		}
	}
	send(1, "81.214.33.44")
	send(2, "9.9.9.9, 81.214.33.45") // only the hop added by the trusted proxy counts
	send(3, "2001:db8::beef")
	send(4, "203.0.113.50") // not in the database
	send(5, "")             // no header: TCP peer (loopback)
	if got := h.status("POST", "/batch", []byte(`{"api_key":"pk_test","batch":[{"event":"e","distinct_id":"d"}]}`), "X-Forwarded-For", "81.214.1.1"); got != 200 {
		t.Fatal(got)
	}
	if err := h.stop(); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{idOf(mkBatch(1, 1)): "TR", idOf(mkBatch(2, 1)): "TR", idOf(mkBatch(3, 1)): "DE",
		idOf(mkBatch(4, 1)): "ZZ", idOf(mkBatch(5, 1)): "XL"}
	entries := h.shipped()
	if len(entries) != 6 {
		t.Fatalf("%d entries", len(entries))
	}
	for _, e := range entries {
		if e.Format == wal.FormatPostHog {
			if string(e.Country[:]) != "TR" {
				t.Fatalf("posthog entry country %s", e.Country)
			}
			continue
		}
		if got := string(e.Country[:]); got != want[idOf(e.Body)] {
			t.Fatalf("country %s, want %s", got, want[idOf(e.Body)])
		}
	}
	// The address itself is stored nowhere: not in the WAL objects...
	objs, _ := h.store.List(context.Background(), "wal/")
	for _, o := range objs {
		raw, _, _ := h.store.Get(context.Background(), o.Key, nil)
		zr, _ := zstd.NewReader(bytes.NewReader(raw))
		plain, _ := io.ReadAll(zr)
		zr.Close()
		for _, ip := range []string{"81.214.33.44", "81.214.33.45", "2001:db8::beef", "203.0.113.50", "9.9.9.9", "81.214.1.1"} {
			if bytes.Contains(plain, []byte(ip)) {
				t.Fatalf("WAL contains client address %s", ip)
			}
		}
		for _, raw4 := range [][]byte{{81, 214, 33, 44}, {81, 214, 33, 45}, {203, 0, 113, 50}} {
			if bytes.Contains(plain, raw4) {
				t.Fatalf("WAL contains raw client address %v", raw4)
			}
		}
	}
	// ...and not in the logs.
	for _, ip := range []string{"81.214.33.44", "2001:db8::beef", "203.0.113.50"} {
		if strings.Contains(h.logs.String(), ip) {
			t.Fatalf("logs contain client address %s", ip)
		}
	}

	// A configured but unreadable database is a startup error, not a silent "ZZ".
	cfg := h.cfg
	cfg.GeoDB = filepath.Join(t.TempDir(), "missing.mmdb")
	if _, err := New(cfg, h.store, nil); err == nil {
		t.Fatal("started with a missing LA_GEO_DB")
	}
}

func zstdBytes(b []byte) []byte {
	enc, _ := zstd.NewWriter(nil)
	defer enc.Close()
	return enc.EncodeAll(b, nil)
}

type replayIndex struct {
	session  [16]byte
	seq      uint64
	startTS  uint64
	duration uint64
	bytes    uint64
	encoding byte
	anon     string
	user     string
	key      string
}

// parseReplayIndex decodes a format=3 body per spec/wal.md §2.3.
func parseReplayIndex(t *testing.T, b []byte) replayIndex {
	t.Helper()
	var r replayIndex
	if len(b) < 16 {
		t.Fatalf("replay index too short: %d", len(b))
	}
	copy(r.session[:], b)
	b = b[16:]
	uv := func() uint64 {
		v, n := binary.Uvarint(b)
		if n <= 0 {
			t.Fatal("bad varint in replay index")
		}
		b = b[n:]
		return v
	}
	str := func() string {
		n := uv()
		s := string(b[:n])
		b = b[n:]
		return s
	}
	r.seq, r.startTS, r.duration, r.bytes = uv(), uv(), uv(), uv()
	r.encoding = b[0]
	b = b[1:]
	r.anon, r.user, r.key = str(), str(), str()
	if len(b) != 0 {
		t.Fatalf("%d trailing bytes in replay index", len(b))
	}
	return r
}

func TestReplayUpload(t *testing.T) {
	for _, backend := range []string{"s3", "file"} {
		t.Run(backend, func(t *testing.T) {
			h := newHarness(t, testTokens, nil)
			if backend == "file" {
				// Same node, file:// store (object key = relative path).
				h.stop()
				st, err := objstore.Open("file://"+t.TempDir(), objstore.S3Config{})
				if err != nil {
					t.Fatal(err)
				}
				st.Put(context.Background(), "control/tokens.json", []byte(testTokens))
				h.store = st
				h.start()
			}
			ctx := context.Background()
			const sess = "0193f1a2b3c47d5e8a9b0c1d2e3f4a5c"
			chunk := zstdBytes(append([]byte("LAR1"), bytes.Repeat([]byte("frame-data "), 2000)...))
			gzChunk := gz([]byte("LAR1 some other chunk"))
			day := time.Now().UTC().Format("2006-01-02")
			path := "/v1/r?s=" + strings.ToUpper(sess) + "&n=7&ts=1795046400123&d=5000&a=anon-1&u=user-42"

			if got := h.status("POST", path, chunk, "Authorization", "Bearer pk_test", "Content-Encoding", "zstd"); got != 200 {
				t.Fatalf("zstd upload: %d", got)
			}
			// gzip chunk, token in the query, no user id, chunked transfer (unknown length).
			req, _ := http.NewRequest("POST", h.ts.URL+"/v1/r?s="+sess+"&n=8&ts=1795046405123&d=4000&a=anon-1&t=pk_test",
				struct{ io.Reader }{bytes.NewReader(gzChunk)})
			req.Header.Set("Content-Encoding", "gzip")
			resp, err := h.client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Fatalf("gzip chunked upload: %d", resp.StatusCode)
			}
			// Re-sending the same (s, n) overwrites the object (content is the same).
			if got := h.status("POST", path, chunk, "Authorization", "Bearer pk_test", "Content-Encoding", "zstd"); got != 200 {
				t.Fatalf("re-upload: %d", got)
			}

			zKey := fmt.Sprintf("replay/p=1017/d=%s/%s/000007.bin.zst", day, sess)
			gKey := fmt.Sprintf("replay/p=1017/d=%s/%s/000008.bin.gz", day, sess)
			for key, want := range map[string][]byte{zKey: chunk, gKey: gzChunk} {
				got, _, err := h.store.Get(ctx, key, nil)
				if err != nil || !bytes.Equal(got, want) {
					t.Fatalf("stored object %s: err=%v equal=%v", key, err, bytes.Equal(got, want))
				}
			}
			objs, _ := h.store.List(ctx, "replay/")
			if len(objs) != 2 {
				t.Fatalf("replay objects: %v", objs)
			}

			rejects := []struct {
				name, path string
				body       []byte
				hdr        []string
				want       int
			}{
				{"no token", "/v1/r?s=" + sess + "&n=1&ts=1&d=1&a=a", chunk, []string{"Content-Encoding", "zstd"}, 401},
				{"token without replay", path, chunk, []string{"Authorization", "Bearer pk_other", "Content-Encoding", "zstd"}, 403},
				{"disabled token", path, chunk, []string{"Authorization", "Bearer pk_off", "Content-Encoding", "zstd"}, 403},
				{"missing content-encoding", path, chunk, []string{"Authorization", "Bearer pk_test"}, 400},
				{"unsupported content-encoding", path, chunk, []string{"Authorization", "Bearer pk_test", "Content-Encoding", "br"}, 400},
				{"body is not zstd", "/v1/r?s=" + sess + "&n=20&ts=1&d=1&a=a", []byte("plain text, not compressed"), []string{"Authorization", "Bearer pk_test", "Content-Encoding", "zstd"}, 400},
				{"gzip body declared zstd", "/v1/r?s=" + sess + "&n=21&ts=1&d=1&a=a", gzChunk, []string{"Authorization", "Bearer pk_test", "Content-Encoding", "zstd"}, 400},
				{"one byte body", "/v1/r?s=" + sess + "&n=23&ts=1&d=1&a=a", []byte{0x28}, []string{"Authorization", "Bearer pk_test", "Content-Encoding", "zstd"}, 400},
				{"empty body", "/v1/r?s=" + sess + "&n=22&ts=1&d=1&a=a", nil, []string{"Authorization", "Bearer pk_test", "Content-Encoding", "zstd"}, 400},
				{"short session", "/v1/r?s=abcd&n=1&ts=1&d=1&a=a", chunk, []string{"Authorization", "Bearer pk_test", "Content-Encoding", "zstd"}, 400},
				{"non-hex session", "/v1/r?s=" + strings.Repeat("zz", 16) + "&n=1&ts=1&d=1&a=a", chunk, []string{"Authorization", "Bearer pk_test", "Content-Encoding", "zstd"}, 400},
				{"path traversal in session", "/v1/r?s=..%2F..%2F..%2F..%2F..%2F..%2F..%2F..%2Fetc%2Fx&n=1&ts=1&d=1&a=a", chunk, []string{"Authorization", "Bearer pk_test", "Content-Encoding", "zstd"}, 400},
				{"missing seq", "/v1/r?s=" + sess + "&ts=1&d=1&a=a", chunk, []string{"Authorization", "Bearer pk_test", "Content-Encoding", "zstd"}, 400},
				{"seq too large for key", "/v1/r?s=" + sess + "&n=1000000&ts=1&d=1&a=a", chunk, []string{"Authorization", "Bearer pk_test", "Content-Encoding", "zstd"}, 400},
				{"negative ts", "/v1/r?s=" + sess + "&n=1&ts=-5&d=1&a=a", chunk, []string{"Authorization", "Bearer pk_test", "Content-Encoding", "zstd"}, 400},
				{"missing anon id", "/v1/r?s=" + sess + "&n=1&ts=1&d=1", chunk, []string{"Authorization", "Bearer pk_test", "Content-Encoding", "zstd"}, 400},
				{"too large", "/v1/r?s=" + sess + "&n=30&ts=1&d=1&a=a", append([]byte{0x28, 0xb5, 0x2f, 0xfd}, make([]byte, 1<<20)...), []string{"Authorization", "Bearer pk_test", "Content-Encoding", "zstd"}, 413},
				{"GET", path, nil, []string{"Authorization", "Bearer pk_test"}, 405},
			}
			for _, c := range rejects {
				method := "POST"
				if c.name == "GET" {
					method = "GET"
				}
				if got := h.status(method, c.path, c.body, c.hdr...); got != c.want {
					t.Errorf("%s: status %d, want %d", c.name, got, c.want)
				}
			}
			// Too large without a Content-Length (stream cut off mid-upload).
			req, _ = http.NewRequest("POST", h.ts.URL+"/v1/r?s="+sess+"&n=31&ts=1&d=1&a=a&t=pk_test",
				struct{ io.Reader }{bytes.NewReader(append([]byte{0x1f, 0x8b}, make([]byte, 1<<20)...))})
			req.Header.Set("Content-Encoding", "gzip")
			if resp, err = h.client.Do(req); err == nil {
				resp.Body.Close()
				if resp.StatusCode != 413 {
					t.Errorf("chunked too large: %d", resp.StatusCode)
				}
			}
			// None of the rejected uploads left an object behind.
			if objs, _ := h.store.List(ctx, "replay/"); len(objs) != 2 {
				t.Fatalf("rejected uploads left objects: %v", objs)
			}

			// WAL: one format=3 index entry per accepted upload.
			var idx []replayIndex
			for _, e := range h.shipped() {
				if e.Format != wal.FormatReplay || e.ProjectID != 1017 {
					t.Fatalf("envelope %+v", e.Envelope)
				}
				idx = append(idx, parseReplayIndex(t, e.Body))
			}
			if len(idx) != 3 {
				t.Fatalf("%d replay index entries, want 3", len(idx))
			}
			wantSID := [16]byte{0x01, 0x93, 0xf1, 0xa2, 0xb3, 0xc4, 0x7d, 0x5e, 0x8a, 0x9b, 0x0c, 0x1d, 0x2e, 0x3f, 0x4a, 0x5c}
			z := replayIndex{session: wantSID, seq: 7, startTS: 1795046400123, duration: 5000, bytes: uint64(len(chunk)),
				encoding: 1, anon: "anon-1", user: "user-42", key: zKey}
			g := replayIndex{session: wantSID, seq: 8, startTS: 1795046405123, duration: 4000, bytes: uint64(len(gzChunk)),
				encoding: 2, anon: "anon-1", user: "", key: gKey}
			if idx[0] != z || idx[1] != g || idx[2] != z {
				t.Fatalf("index entries:\n %+v\n %+v\n %+v", idx[0], idx[1], idx[2])
			}
		})
	}
}

// If the object store is down the chunk cannot be stored: 503, and no index
// entry may point at a missing object.
func TestReplayStoreOutage(t *testing.T) {
	h := newHarness(t, testTokens, nil)
	h.s3.SetFault(devs3.FaultFail)
	resp := h.do("POST", "/v1/r?s=0193f1a2b3c47d5e8a9b0c1d2e3f4a5c&n=1&ts=1&d=1&a=a", zstdBytes([]byte("LAR1")),
		"Authorization", "Bearer pk_test", "Content-Encoding", "zstd")
	resp.Body.Close()
	if resp.StatusCode != 503 || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("replay during outage: %d", resp.StatusCode)
	}
	h.s3.SetFault(devs3.FaultNone)
	if n := len(h.shipped()); n != 0 {
		t.Fatalf("index entries written for a failed upload: %d", n)
	}
}

// Design §6.10: SIGTERM -> /readyz fails -> wait -> finish open requests ->
// seal -> upload -> exit.
func TestGracefulShutdown(t *testing.T) {
	h := newHarness(t, testTokens, func(c *Config) {
		c.DrainDelay = 300 * time.Millisecond
		c.SegmentAge = time.Hour // nothing is shipped before shutdown seals it
	})
	h.ts.Close() // use a real listener driven by Server.Serve instead
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- h.srv.Serve(ln, nil) }()
	base := "http://" + ln.Addr().String()
	post := func(body io.Reader) (*http.Response, error) {
		req, _ := http.NewRequest("POST", base+"/v1/e", body)
		req.Header.Set("Authorization", "Bearer pk_test")
		return h.client.Do(req)
	}
	for i := 0; i < 5; i++ {
		resp, err := post(bytes.NewReader(mkBatch(uint32(i), 2)))
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("send %d: %v", i, err)
		}
		resp.Body.Close()
	}
	if len(h.localFiles()) != 1 {
		t.Fatalf("expected one active local segment, got %v", h.localFiles())
	}

	// A slow request that is still uploading its body when shutdown starts.
	slow := mkBatch(100, 2)
	pr, pw := io.Pipe()
	slowDone := make(chan int, 1)
	go func() {
		resp, err := post(pr)
		if err != nil {
			slowDone <- -1
			return
		}
		resp.Body.Close()
		slowDone <- resp.StatusCode
	}()
	pw.Write(slow[:10])
	time.Sleep(50 * time.Millisecond)

	shutdownDone := make(chan error, 1)
	start := time.Now()
	go func() { shutdownDone <- h.srv.Shutdown(context.Background()) }()

	// During the drain delay: not ready, but still accepting.
	time.Sleep(50 * time.Millisecond)
	resp, err := h.client.Get(base + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	if txt := readAll(t, resp); resp.StatusCode != 503 || !strings.Contains(txt, "shutting_down") {
		t.Fatalf("readyz during drain: %d %s", resp.StatusCode, txt)
	}
	resp, err = post(bytes.NewReader(mkBatch(200, 1)))
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("request during drain delay: %v", err)
	}
	resp.Body.Close()

	// Shutdown waits for the open request.
	select {
	case err := <-shutdownDone:
		t.Fatalf("shutdown returned while a request was still open: %v", err)
	case <-time.After(500 * time.Millisecond):
	}
	pw.Write(slow[10:])
	pw.Close()
	if code := <-slowDone; code != 200 {
		t.Fatalf("in-flight request finished with %d", code)
	}
	if err := <-shutdownDone; err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if time.Since(start) < 300*time.Millisecond {
		t.Fatal("shutdown did not wait for the drain delay")
	}
	if err := <-served; err != nil {
		t.Fatalf("Serve returned %v", err)
	}
	h.ts = nil

	// New connections are refused, the local WAL is empty, and every
	// acknowledged batch is in the store.
	if _, err := h.client.Get(base + "/healthz"); err == nil {
		t.Fatal("listener still accepting after shutdown")
	}
	if files := h.localFiles(); len(files) != 0 {
		t.Fatalf("local WAL after shutdown: %v", files)
	}
	ids := batchIDs(t, storeEntries(t, h.store))
	if len(ids) != 7 {
		t.Fatalf("%d batches in the store, want 7", len(ids))
	}
	for _, b := range [][]byte{mkBatch(0, 2), mkBatch(4, 2), slow, mkBatch(200, 1)} {
		if ids[idOf(b)] != 1 {
			t.Fatalf("batch %x appears %d times", idOf(b), ids[idOf(b)])
		}
	}
	if !strings.Contains(h.logs.String(), "shutdown: complete") {
		t.Fatal("shutdown completion not logged")
	}
	// Shutdown is idempotent.
	if err := h.srv.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// Shutdown while the store is down: data stays on disk, the error says so,
// and the next start ships it.
func TestShutdownDuringStoreOutage(t *testing.T) {
	h := newHarness(t, testTokens, func(c *Config) { c.ShutdownUploadTimeout = 200 * time.Millisecond })
	h.s3.SetFault(devs3.FaultFail)
	for i := 0; i < 3; i++ {
		if got := h.send(mkBatch(uint32(i), 1)); got != 200 {
			t.Fatal(got)
		}
	}
	err := h.stop()
	if err == nil || !strings.Contains(err.Error(), "not uploaded") {
		t.Fatalf("shutdown during outage: %v", err)
	}
	if len(h.localFiles()) == 0 {
		t.Fatal("segments vanished from local disk during outage shutdown")
	}
	h.s3.SetFault(devs3.FaultNone)
	h.start()
	if ids := batchIDs(t, h.shipped()); len(ids) != 3 {
		t.Fatalf("after restart %d batches in the store, want 3", len(ids))
	}
	if files := h.localFiles(); len(files) != 0 {
		t.Fatalf("local WAL after restart: %v", files)
	}
}

// Design §17.5: kill ingestion between fsync and response. The client never
// sees the 200 and retries.
func TestCrashBetweenFsyncAndResponse(t *testing.T) {
	// Case 1: restart on the same WAL directory while the segment is still
	// local. Recovery ships the batch and re-seeds the dedupe cache from the
	// recovered segment, so the retry is acknowledged without a second write:
	// no loss and no duplicate.
	t.Run("retry after restart is deduplicated", func(t *testing.T) {
		h := newHarness(t, testTokens, func(c *Config) { c.SegmentAge = time.Hour })
		if got := h.send(mkBatch(1, 2)); got != 200 {
			t.Fatal(got)
		}
		var fsynced atomic.Int32
		h.srv.afterFsync = func() {
			fsynced.Add(1)
			panic(http.ErrAbortHandler) // die after fsync, before the response
		}
		lost := mkBatch(2, 3)
		req, _ := http.NewRequest("POST", h.ts.URL+"/v1/e", bytes.NewReader(lost))
		req.Header.Set("Authorization", "Bearer pk_test")
		if resp, err := h.client.Do(req); err == nil {
			resp.Body.Close()
			t.Fatalf("client received a response (%d) from a crashed handler", resp.StatusCode)
		}
		if fsynced.Load() != 1 {
			t.Fatalf("handler aborted before fsync (%d)", fsynced.Load())
		}
		h.kill()
		if files := h.localFiles(); len(files) != 1 || !strings.HasSuffix(files[0], ".wal") {
			t.Fatalf("crashed node should leave its active segment: %v", files)
		}

		h.start() // same WAL dir, fresh process state
		if got := h.send(lost); got != 200 {
			t.Fatalf("client retry after restart: %d", got)
		}
		if got := h.metric("la_ingest_dup_batches_total"); got != 1 {
			t.Fatalf("dup_batches_total = %v, want 1 (retry must hit the re-seeded dedupe cache)", got)
		}
		if got := h.send(mkBatch(3, 1)); got != 200 {
			t.Fatal(got)
		}
		if err := h.stop(); err != nil {
			t.Fatal(err)
		}
		ids := batchIDs(t, h.shipped())
		if len(ids) != 3 || ids[idOf(mkBatch(1, 2))] != 1 || ids[idOf(lost)] != 1 || ids[idOf(mkBatch(3, 1))] != 1 {
			t.Fatalf("store content after crash + retry: %d distinct, lost batch x%d", len(ids), ids[idOf(lost)])
		}
	})

	// Case 2: the documented limit of batch_id dedupe. The cache is
	// node-local and in-memory; it is rebuilt only from segments still on
	// local disk. If the segment holding the batch was already uploaded (and
	// deleted locally) before the crash, the restarted node cannot see it and
	// writes the retry again. Nothing is lost; the duplicate is removed
	// downstream by record uuid (shipper/compaction/query), as the spec says
	// for cross-node retries.
	t.Run("retry after the segment was shipped is written twice", func(t *testing.T) {
		h := newHarness(t, testTokens, nil) // 40 ms rotation: shipped almost immediately
		var mu sync.Mutex
		release := make(chan struct{})
		h.srv.afterFsync = func() {
			mu.Lock()
			defer mu.Unlock()
			<-release // hold the response until the segment has been uploaded
			panic(http.ErrAbortHandler)
		}
		lost := mkBatch(9, 2)
		errc := make(chan error, 1)
		go func() {
			req, _ := http.NewRequest("POST", h.ts.URL+"/v1/e", bytes.NewReader(lost))
			req.Header.Set("Authorization", "Bearer pk_test")
			resp, err := h.client.Do(req)
			if err == nil {
				resp.Body.Close()
				err = errors.New("unexpected response")
			} else {
				err = nil
			}
			errc <- err
		}()
		h.waitShippedAtLeast(1)
		close(release)
		if err := <-errc; err != nil {
			t.Fatal(err)
		}
		h.kill()
		if files := h.localFiles(); len(files) != 0 {
			t.Fatalf("expected no local segments, got %v", files)
		}
		h.start()
		if got := h.send(lost); got != 200 {
			t.Fatalf("retry: %d", got)
		}
		if got := h.metric("la_ingest_dup_batches_total"); got != 0 {
			t.Fatalf("dup_batches_total = %v; this scenario is expected to bypass the cache", got)
		}
		h.stop()
		entries := h.shipped()
		ids := batchIDs(t, entries)
		if ids[idOf(lost)] != 2 || len(entries) != 2 {
			t.Fatalf("expected the batch twice in the WAL (uuid dedupe is downstream), got %d", ids[idOf(lost)])
		}
		if !bytes.Equal(entries[0].Body, entries[1].Body) {
			t.Fatal("the two copies differ")
		}
	})
}

// waitShippedAtLeast waits until n segments have been uploaded and nothing
// is pending.
func (h *harness) waitShippedAtLeast(n int) {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if p, _ := h.srv.uploader.Pending(); p == 0 && h.srv.m.segmentsUploaded.Value() >= uint64(n) && h.srv.localBytes.Load() == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.t.Fatalf("segments uploaded: %d, want >= %d", h.srv.m.segmentsUploaded.Value(), n)
}

// Design §17.5: object storage outage. Ingestion keeps acknowledging (data
// is safe on local disk), then catches up when the store returns.
func TestObjectStoreOutage(t *testing.T) {
	for _, mode := range []devs3.Fault{devs3.FaultFail, devs3.FaultPause} {
		t.Run(fmt.Sprint("fault-", int(mode)), func(t *testing.T) {
			h := newHarness(t, testTokens, func(c *Config) { c.SegmentAge = 20 * time.Millisecond })
			var sent [][]byte
			send := func(n int) {
				for i := 0; i < n; i++ {
					b := mkBatch(uint32(len(sent)), 4)
					if got := h.send(b); got != 200 {
						t.Fatalf("batch %d: %d", len(sent), got)
					}
					sent = append(sent, b)
				}
			}
			send(5)
			h.waitShipped()
			before := h.metric("la_ingest_segments_uploaded_total")

			h.s3.SetFault(mode)
			for round := 0; round < 6; round++ { // spread over several rotations
				send(5)
				time.Sleep(30 * time.Millisecond)
			}
			if got := h.metric("la_ingest_segments_uploaded_total"); got != before {
				t.Fatalf("segments uploaded during the outage: %v -> %v", before, got)
			}
			pending := h.metric("la_ingest_wal_segments_pending")
			if pending < 2 || h.metric("la_ingest_wal_local_bytes") <= 0 || len(h.localFiles()) < 2 {
				t.Fatalf("outage state: pending=%v local_bytes=%v files=%v", pending, h.metric("la_ingest_wal_local_bytes"), h.localFiles())
			}
			if mode == devs3.FaultFail && h.metric("la_ingest_segment_upload_errors_total") < 1 {
				t.Fatal("upload errors not counted")
			}
			time.Sleep(120 * time.Millisecond)
			if lag := h.metric("la_ingest_wal_upload_lag_seconds"); lag < 0.1 {
				t.Fatalf("wal_upload_lag_seconds = %v during outage", lag)
			}
			if got := h.status("GET", "/readyz", nil); got != 200 {
				t.Fatalf("readyz during outage (below the disk limit): %d", got)
			}

			h.s3.SetFault(devs3.FaultNone)
			h.waitShipped()
			send(2)
			ids := batchIDs(t, h.shipped())
			if len(ids) != len(sent) {
				t.Fatalf("%d distinct batches in the store, %d acknowledged", len(ids), len(sent))
			}
			for i, b := range sent {
				if ids[idOf(b)] != 1 {
					t.Fatalf("batch %d appears %d times", i, ids[idOf(b)])
				}
			}
			if h.metric("la_ingest_wal_upload_lag_seconds") != 0 || len(h.localFiles()) != 0 {
				t.Fatalf("not caught up: lag=%v files=%v", h.metric("la_ingest_wal_upload_lag_seconds"), h.localFiles())
			}
		})
	}
}

// A torn tail left by a crash is cut on restart and everything before it is
// shipped.
func TestRestartTruncatesTornTail(t *testing.T) {
	h := newHarness(t, testTokens, func(c *Config) { c.SegmentAge = time.Hour })
	for i := 0; i < 3; i++ {
		if got := h.send(mkBatch(uint32(i), 2)); got != 200 {
			t.Fatal(got)
		}
	}
	h.kill()
	files := h.localFiles()
	if len(files) != 1 {
		t.Fatalf("local files: %v", files)
	}
	// Half-written entry at the end, as after a power cut mid-write.
	torn, _ := wal.AppendEntry(nil, wal.Envelope{Format: wal.FormatNative, ProjectID: 1017}, mkBatch(99, 5))
	f, err := os.OpenFile(filepath.Join(h.cfg.WALDir, files[0]), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.Write(torn[:len(torn)-7])
	f.Close()

	h.start()
	if got := h.metric("la_ingest_wal_truncated_tails_total"); got != 1 {
		t.Fatalf("wal_truncated_tails_total = %v", got)
	}
	ids := batchIDs(t, h.shipped())
	if len(ids) != 3 || ids[idOf(mkBatch(99, 5))] != 0 {
		t.Fatalf("after torn-tail recovery: %d batches (torn one present: %d)", len(ids), ids[idOf(mkBatch(99, 5))])
	}
	// The never-acknowledged batch is still accepted when the client retries it.
	if got := h.send(mkBatch(99, 5)); got != 200 {
		t.Fatal(got)
	}
	if err := h.stop(); err != nil {
		t.Fatal(err)
	}
	if ids := batchIDs(t, h.shipped()); ids[idOf(mkBatch(99, 5))] != 1 || len(ids) != 4 {
		t.Fatal("retried batch missing")
	}
}
