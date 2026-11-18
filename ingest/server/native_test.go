package server

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mucan54/izlek/ingest/wal"
	"github.com/mucan54/izlek/ingest/wire"
)

type wireVectors struct {
	Valid []struct {
		Name  string `json:"name"`
		Hex   string `json:"hex"`
		Batch struct {
			Records []json.RawMessage `json:"records"`
		} `json:"batch"`
	} `json:"valid"`
	Tolerant []struct {
		Name string `json:"name"`
		Hex  string `json:"hex"`
	} `json:"tolerant"`
	Invalid []struct {
		Name       string `json:"name"`
		Hex        string `json:"hex"`
		FrameOK    bool   `json:"frame_ok"`
		HTTPStatus int    `json:"http_status"`
	} `json:"invalid"`
}

func loadWireVectors(t testing.TB) wireVectors {
	t.Helper()
	raw, err := os.ReadFile("../../spec/vectors/wire.json")
	if err != nil {
		t.Fatal(err)
	}
	var v wireVectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Valid) == 0 || len(v.Invalid) == 0 || len(v.Tolerant) == 0 {
		t.Fatal("empty vectors")
	}
	return v
}

func mustHex(t testing.TB, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Every case of spec/vectors/wire.json through the real HTTP endpoint, plain
// and gzip-encoded: valid/tolerant -> 200, invalid -> the listed status.
func TestWireVectorsOverHTTP(t *testing.T) {
	h := newHarness(t, testTokens, nil)
	v := loadWireVectors(t)
	check := func(kind, name string, body []byte, want int) {
		t.Helper()
		if got := h.send(body); got != want {
			t.Errorf("%s/%s: status %d, want %d", kind, name, got, want)
		}
		if got := h.status("POST", "/v1/e", gz(body), "Authorization", "Bearer pk_test", "Content-Encoding", "gzip"); got != want {
			t.Errorf("%s/%s (gzip): status %d, want %d", kind, name, got, want)
		}
	}
	for _, c := range v.Valid {
		check("valid", c.Name, mustHex(t, c.Hex), 200)
	}
	for _, c := range v.Tolerant {
		check("tolerant", c.Name, mustHex(t, c.Hex), 200)
	}
	for _, c := range v.Invalid {
		want := c.HTTPStatus
		if c.FrameOK && want != 200 {
			t.Fatalf("vector %s: frame_ok with status %d", c.Name, want)
		}
		check("invalid", c.Name, mustHex(t, c.Hex), want)
	}

	// What reached the WAL: each distinct non-empty accepted batch exactly
	// once (the gzip resend and vectors sharing a batch_id are deduped),
	// stored byte-for-byte as the uncompressed LA1 batch.
	want := map[string]bool{} // by batch_id: first accepted body with that id
	wantBody := map[string][]byte{}
	accept := func(body []byte) {
		f, err := wire.Validate(body)
		if err != nil || f.Records == 0 {
			return
		}
		id := string(f.BatchID[:])
		if !want[id] {
			want[id] = true
			wantBody[id] = body
		}
	}
	for _, c := range v.Valid {
		accept(mustHex(t, c.Hex))
	}
	for _, c := range v.Tolerant {
		accept(mustHex(t, c.Hex))
	}
	for _, c := range v.Invalid {
		if c.FrameOK {
			accept(mustHex(t, c.Hex))
		}
	}
	entries := h.shipped()
	if len(entries) != len(want) {
		t.Fatalf("WAL has %d entries, want %d distinct batches", len(entries), len(want))
	}
	for _, e := range entries {
		if e.Format != wal.FormatNative || e.ProjectID != 1017 || string(e.Country[:]) != "ZZ" || e.Flags != 0 {
			t.Fatalf("envelope %+v", e.Envelope)
		}
		if d := time.Since(time.UnixMilli(int64(e.RecvTSMs))); d < 0 || d > time.Minute {
			t.Fatalf("recv_ts %d is not 'now'", e.RecvTSMs)
		}
		f, err := wire.Validate(e.Body)
		if err != nil {
			t.Fatalf("WAL body does not validate: %v", err)
		}
		if !bytes.Equal(e.Body, wantBody[string(f.BatchID[:])]) {
			t.Fatalf("WAL body for batch %x differs from what was sent", f.BatchID)
		}
		delete(want, string(f.BatchID[:]))
	}
	if len(want) != 0 {
		t.Fatalf("%d accepted batches missing from the WAL", len(want))
	}
}

func TestNativeStatusCodes(t *testing.T) {
	h := newHarness(t, testTokens, nil)
	good := mkBatch(1, 3)

	if got := h.status("POST", "/v1/e", good); got != 401 {
		t.Errorf("no token: %d", got)
	}
	if got := h.status("POST", "/v1/e", good, "Authorization", "Bearer pk_nope"); got != 401 {
		t.Errorf("unknown token: %d", got)
	}
	if got := h.status("POST", "/v1/e", good, "Authorization", "Basic abc"); got != 401 {
		t.Errorf("non-bearer auth: %d", got)
	}
	if got := h.status("POST", "/v1/e", good, "Authorization", "Bearer pk_off"); got != 403 {
		t.Errorf("disabled token: %d", got)
	}
	if got := h.status("POST", "/v1/e?t=pk_off", good); got != 403 {
		t.Errorf("disabled token via query: %d", got)
	}
	if got := h.status("GET", "/v1/e", nil, "Authorization", "Bearer pk_test"); got != 405 {
		t.Errorf("GET: %d", got)
	}
	if got := h.status("POST", "/v1/e", good, "Authorization", "Bearer pk_test", "Content-Encoding", "br"); got != 400 {
		t.Errorf("unsupported content-encoding: %d", got)
	}
	if got := h.status("POST", "/v1/e", []byte("\x1f\x8bnot really gzip"), "Authorization", "Bearer pk_test", "Content-Encoding", "gzip"); got != 400 {
		t.Errorf("corrupt gzip: %d", got)
	}
	if got := h.status("POST", "/v1/e", gz(good)[:20], "Authorization", "Bearer pk_test", "Content-Encoding", "gzip"); got != 400 {
		t.Errorf("truncated gzip: %d", got)
	}
	if got := h.status("POST", "/nope", good, "Authorization", "Bearer pk_test"); got != 404 {
		t.Errorf("unknown path: %d", got)
	}

	// Size limits (spec/wire.md §3): 1 MiB encoded and decoded -> 413.
	hdrOnly := wire.AppendHeader(nil, &wire.Batch{SDK: "t"})
	rec := wire.AppendRecord(nil, &wire.Record{Name: "pad", Props: []wire.Prop{{Key: "p", Tag: wire.TagString, Str: strings.Repeat("x", 8000)}}})
	big := append([]byte(nil), hdrOnly...)
	for len(big) <= wire.MaxBatchBytes {
		big = append(big, rec...)
	}
	if f, err := wire.Validate(big); err != nil || f.Records > wire.MaxRecords {
		t.Fatalf("test batch should be frame-valid and under the record limit: %v %d", err, f.Records)
	}
	if got := h.send(big); got != 413 {
		t.Errorf("oversized body: %d", got)
	}
	// Small on the wire, too large once decompressed (gzip bomb guard).
	if comp := gz(big); len(comp) > 100_000 {
		t.Fatalf("test body did not compress: %d", len(comp))
	} else if got := h.status("POST", "/v1/e", comp, "Authorization", "Bearer pk_test", "Content-Encoding", "gzip"); got != 413 {
		t.Errorf("gzip bomb: %d", got)
	}
	// Exactly at the limit is fine: fill up to 1 MiB with one sized record.
	atLimit := append([]byte(nil), hdrOnly...)
	for len(atLimit)+len(rec) <= wire.MaxBatchBytes-200 {
		atLimit = append(atLimit, rec...)
	}
	fill := wire.MaxBatchBytes - len(atLimit)
	for v := 1; v <= 3; v++ {
		if lenPrefix := wire.AppendUvarint(nil, uint64(fill-v)); len(lenPrefix) == v {
			atLimit = append(append(atLimit, lenPrefix...), make([]byte, fill-v)...)
			break
		}
	}
	if _, err := wire.Validate(atLimit); err != nil || len(atLimit) != wire.MaxBatchBytes {
		t.Fatalf("at-limit test batch: len %d, err %v", len(atLimit), err)
	}
	if got := h.send(atLimit); got != 200 {
		t.Errorf("body of exactly 1 MiB: %d", got)
	}
	if got := h.send(append(atLimit, rec...)); got != 413 {
		t.Errorf("body just over 1 MiB: %d", got)
	}
	// Chunked upload (no Content-Length) over the limit.
	req, _ := http.NewRequest("POST", h.ts.URL+"/v1/e", struct{ *bytes.Reader }{bytes.NewReader(big)})
	req.Header.Set("Authorization", "Bearer pk_test")
	req.ContentLength = -1
	if resp, err := h.client.Do(req); err != nil {
		t.Errorf("chunked oversized: %v", err)
	} else {
		resp.Body.Close()
		if resp.StatusCode != 413 {
			t.Errorf("chunked oversized: %d", resp.StatusCode)
		}
	}

	// Empty batch: 200, nothing written. ?t= works like the header.
	empty := wire.AppendHeader(nil, &wire.Batch{SDK: "t"})
	if got := h.status("POST", "/v1/e?t=pk_test", empty); got != 200 {
		t.Errorf("empty batch: %d", got)
	}
	if got := h.status("POST", "/v1/e?x=1&t=pk_test", good); got != 200 {
		t.Errorf("token in query: %d", got)
	}
	resp := h.do("POST", "/v1/e", mkBatch(2, 1), "Authorization", "bearer pk_test", "X-Request-Id", "client-req-42")
	if body := readAll(t, resp); resp.StatusCode != 200 || body != "" {
		t.Errorf("good batch: %d body %q", resp.StatusCode, body)
	}
	if got := resp.Header.Get("X-Request-Id"); got != "client-req-42" {
		t.Errorf("request id not echoed: %q", got)
	}
	resp = h.do("POST", "/v1/e", mkBatch(3, 1), "Authorization", "Bearer pk_test", "X-Request-Id", "bad id with spaces\"")
	resp.Body.Close()
	if got := resp.Header.Get("X-Request-Id"); !strings.HasPrefix(got, "ing-t1-") {
		t.Errorf("generated request id: %q", got)
	}

	entries := h.shipped()
	if len(entries) != 4 {
		t.Fatalf("WAL has %d entries, want 4 (empty and rejected batches must not be written)", len(entries))
	}
	if got := h.metric("la_ingest_records_in_total"); got < 5 {
		t.Errorf("records_in_total = %v", got)
	}
}

func TestDedupe(t *testing.T) {
	h := newHarness(t, testTokens, nil)
	b := mkBatch(7, 5)
	for i := 0; i < 3; i++ {
		if got := h.send(b); got != 200 {
			t.Fatalf("send %d: %d", i, got)
		}
	}
	// gzip-encoded resend of the same batch is the same batch.
	if got := h.status("POST", "/v1/e", gz(b), "Authorization", "Bearer pk_test", "Content-Encoding", "gzip"); got != 200 {
		t.Fatal(got)
	}
	// Same batch_id under another project is a different batch.
	if got := h.status("POST", "/v1/e", b, "Authorization", "Bearer pk_other"); got != 200 {
		t.Fatal(got)
	}
	// A different batch.
	if got := h.send(mkBatch(8, 1)); got != 200 {
		t.Fatal(got)
	}
	entries := h.shipped()
	if len(entries) != 3 {
		t.Fatalf("WAL has %d entries, want 3", len(entries))
	}
	projects := map[uint32]int{}
	for _, e := range entries {
		projects[e.ProjectID]++
	}
	if projects[1017] != 2 || projects[1018] != 1 {
		t.Fatalf("entries per project: %v", projects)
	}
	if got := h.metric("la_ingest_dup_batches_total"); got != 3 {
		t.Fatalf("dup_batches_total = %v, want 3", got)
	}
	if got := h.metric("la_ingest_records_in_total"); got != 11 {
		t.Fatalf("records_in_total = %v, want 11 (duplicates not counted)", got)
	}
}

func TestRateLimits(t *testing.T) {
	t.Run("per token", func(t *testing.T) {
		h := newHarness(t, testTokens, nil)
		codes := map[int]int{}
		var retryAfter string
		start := time.Now()
		for i := 0; i < 12; i++ { // pk_slow: 5 req/s, burst 5
			resp := h.do("POST", "/v1/e", mkBatch(uint32(100+i), 1), "Authorization", "Bearer pk_slow")
			resp.Body.Close()
			codes[resp.StatusCode]++
			if resp.StatusCode == 429 {
				retryAfter = resp.Header.Get("Retry-After")
			}
		}
		// Burst of 5, plus whatever refilled while the loop ran (slow CI).
		maxOK := 5 + int(time.Since(start).Seconds()*5) + 1
		if codes[200] < 5 || codes[200] > maxOK || codes[429] != 12-codes[200] || codes[429] == 0 {
			t.Fatalf("status codes %v, want 5..%d x 200 and the rest 429", codes, maxOK)
		}
		if retryAfter != "1" {
			t.Fatalf("Retry-After %q", retryAfter)
		}
		if got := h.metric("la_ingest_rate_limited_total"); got != float64(codes[429]) {
			t.Fatalf("rate_limited_total = %v, want %d", got, codes[429])
		}
		// Another token is unaffected.
		if got := h.send(mkBatch(1, 1)); got != 200 {
			t.Fatalf("other token: %d", got)
		}
		// Refill.
		time.Sleep(450 * time.Millisecond)
		if got := h.status("POST", "/v1/e", mkBatch(999, 1), "Authorization", "Bearer pk_slow"); got != 200 {
			t.Fatalf("after refill: %d", got)
		}
		// Rate-limited batches were not written.
		if n := len(h.shipped()); n != codes[200]+2 {
			t.Fatalf("WAL has %d entries, want %d", n, codes[200]+2)
		}
	})
	t.Run("per ip", func(t *testing.T) {
		h := newHarness(t, testTokens, func(c *Config) {
			c.RateIP = 1 // burst 1, one more per second: robust against a slow machine
			c.TrustedProxies = 1
		})
		send := func(ip string, id uint32) int {
			return h.status("POST", "/v1/e", mkBatch(id, 1), "Authorization", "Bearer pk_test", "X-Forwarded-For", ip)
		}
		if got := send("198.51.100.1", 1); got != 200 {
			t.Fatalf("first request: %d", got)
		}
		if got := send("198.51.100.1", 10); got != 429 {
			t.Fatalf("second request from one ip within a second: %d", got)
		}
		if got := send("198.51.100.2", 11); got != 200 {
			t.Fatalf("other ip: %d", got)
		}
		// A client cannot dodge the limit by prepending fake hops.
		if got := send("9.9.9.9, 198.51.100.1", 12); got != 429 {
			t.Fatalf("spoofed X-Forwarded-For prefix: %d", got)
		}
		// The PostHog adapter is limited by IP before the body is read.
		resp := h.do("POST", "/batch", []byte(`{"api_key":"pk_test","batch":[{"event":"e","distinct_id":"d"}]}`), "X-Forwarded-For", "198.51.100.1")
		resp.Body.Close()
		if resp.StatusCode != 429 || resp.Header.Get("Retry-After") != "1" {
			t.Fatalf("posthog ip limit: %d", resp.StatusCode)
		}
	})
}

func TestBackpressure(t *testing.T) {
	h := newHarness(t, testTokens, func(c *Config) { c.WALMaxLocalBytes = 6000 })
	if got := h.status("GET", "/readyz", nil); got != 200 {
		t.Fatalf("readyz before: %d", got)
	}
	h.s3.SetFault(2) // pause: uploads hang, the local WAL can only grow
	body := mkBatch(1, 40)
	if len(body) < 2000 {
		t.Fatalf("test batch too small: %d", len(body))
	}
	accepted := 0
	var resp *http.Response
	for i := 0; i < 50; i++ {
		resp = h.do("POST", "/v1/e", mkBatch(uint32(i), 40), "Authorization", "Bearer pk_test")
		resp.Body.Close()
		if resp.StatusCode != 200 {
			break
		}
		accepted++
	}
	if resp.StatusCode != 503 || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("expected 503 + Retry-After once over the limit, got %d (accepted %d)", resp.StatusCode, accepted)
	}
	if accepted < 2 || accepted > 6 {
		t.Fatalf("accepted %d batches of %d bytes with a %d byte limit", accepted, len(body), h.cfg.WALMaxLocalBytes)
	}
	resp = h.do("GET", "/readyz", nil)
	if txt := readAll(t, resp); resp.StatusCode != 503 || !strings.Contains(txt, "wal_backpressure") {
		t.Fatalf("readyz under backpressure: %d %s", resp.StatusCode, txt)
	}
	if h.metric("la_ingest_ready") != 0 || h.metric("la_ingest_wal_local_bytes") <= 6000 {
		t.Fatalf("metrics under backpressure: ready=%v local=%v", h.metric("la_ingest_ready"), h.metric("la_ingest_wal_local_bytes"))
	}
	// Other write endpoints push back too; liveness stays up.
	if got := h.status("POST", "/batch", []byte(`{"api_key":"pk_test","batch":[{"event":"e","distinct_id":"d"}]}`)); got != 503 {
		t.Fatalf("posthog under backpressure: %d", got)
	}
	if got := h.status("GET", "/healthz", nil); got != 200 {
		t.Fatalf("healthz: %d", got)
	}

	h.s3.SetFault(0)
	h.waitShipped()
	if got := h.status("GET", "/readyz", nil); got != 200 {
		t.Fatalf("readyz after recovery: %d", got)
	}
	if got := h.send(mkBatch(1000, 1)); got != 200 {
		t.Fatalf("send after recovery: %d", got)
	}
	// Nothing acknowledged was lost, nothing rejected was written.
	if n := len(h.shipped()); n != accepted+1 {
		t.Fatalf("WAL has %d entries, want %d", n, accepted+1)
	}
}

func TestMetricsAndHealth(t *testing.T) {
	h := newHarness(t, testTokens, nil)
	h.send(mkBatch(1, 2))
	h.send(mkBatch(1, 2))
	h.status("POST", "/v1/e", []byte("junk"), "Authorization", "Bearer pk_test")
	h.shipped()

	resp := h.do("GET", "/metrics", nil)
	text := readAll(t, resp)
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain; version=0.0.4") {
		t.Fatalf("metrics: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	// Every metric named in design §16.1 "Ingestion", with the la_ingest_ prefix.
	for _, name := range []string{"requests_total", "request_seconds", "bytes_in_total", "records_in_total", "fsync_seconds",
		"wal_local_bytes", "wal_upload_lag_seconds", "segments_uploaded_total", "rate_limited_total", "dup_batches_total", "tokens_age_seconds"} {
		if !strings.Contains(text, "\n# TYPE la_ingest_"+name+" ") {
			t.Errorf("metric la_ingest_%s missing", name)
		}
	}
	for _, line := range []string{
		`la_ingest_requests_total{code="200",format="native"} 2`,
		`la_ingest_requests_total{code="400",format="native"} 1`,
		`la_ingest_records_in_total 2`,
		`la_ingest_dup_batches_total 1`,
		`la_ingest_segments_uploaded_total 1`,
		`la_ingest_wal_local_bytes 0`,
		`la_ingest_wal_upload_lag_seconds 0`,
		`la_ingest_request_seconds_count 3`,
		`la_ingest_ready 1`,
	} {
		if !strings.Contains(text, "\n"+line+"\n") {
			t.Errorf("metrics output lacks %q", line)
		}
	}
	if h.metric("la_ingest_fsync_seconds_count") < 1 || h.metric("la_ingest_tokens_age_seconds") < 0 {
		t.Errorf("fsync count %v, tokens age %v", h.metric("la_ingest_fsync_seconds_count"), h.metric("la_ingest_tokens_age_seconds"))
	}
	if body := readAll(t, h.do("GET", "/healthz", nil)); body != "ok\n" {
		t.Errorf("healthz body %q", body)
	}
	if body := readAll(t, h.do("GET", "/readyz", nil)); !strings.Contains(body, `"ready":true`) {
		t.Errorf("readyz body %q", body)
	}

	// Structured JSON logs with request ids and without event content.
	logs := h.logs.String()
	if strings.Contains(logs, "s3cr3t-payload-value") || strings.Contains(logs, "secret_marker") || strings.Contains(logs, "pk_test") {
		t.Fatal("logs contain payload content or the token")
	}
	sawRequest := false
	for _, line := range strings.Split(strings.TrimSpace(logs), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line is not JSON: %q", line)
		}
		if rec["msg"] == "request" {
			sawRequest = true
			if id, _ := rec["request_id"].(string); !strings.HasPrefix(id, "ing-t1-") {
				t.Fatalf("request log without request_id: %q", line)
			}
		}
	}
	if !sawRequest {
		t.Fatal("no request log lines at debug level")
	}
}

func TestAdminListenerHidesMetrics(t *testing.T) {
	h := newHarness(t, testTokens, func(c *Config) { c.AdminListen = "127.0.0.1:0" })
	if got := h.status("GET", "/metrics", nil); got != 404 {
		t.Fatalf("/metrics on the public listener with an admin listener configured: %d", got)
	}
	rec := newRecorder()
	h.srv.AdminHandler().ServeHTTP(rec, mustRequest("GET", "/metrics"))
	if rec.code != 200 || !strings.Contains(rec.body.String(), "la_ingest_requests_total") {
		t.Fatalf("admin /metrics: %d", rec.code)
	}
	rec = newRecorder()
	h.srv.AdminHandler().ServeHTTP(rec, mustRequest("GET", "/v1/e"))
	if rec.code != 404 {
		t.Fatalf("admin listener serves ingestion paths: %d", rec.code)
	}
}

type recorder struct {
	hdr  http.Header
	code int
	body bytes.Buffer
}

func newRecorder() *recorder                    { return &recorder{hdr: http.Header{}, code: 200} }
func (r *recorder) Header() http.Header         { return r.hdr }
func (r *recorder) WriteHeader(c int)           { r.code = c }
func (r *recorder) Write(p []byte) (int, error) { return r.body.Write(p) }
func mustRequest(method, path string) *http.Request {
	r, _ := http.NewRequest(method, path, nil)
	return r
}

// Concurrent senders: every 200 is in the WAL exactly once and group commit
// actually groups.
func TestConcurrentIngest(t *testing.T) {
	h := newHarness(t, testTokens, func(c *Config) { c.SegmentBytes = 64 << 10 })
	const workers, perWorker = 16, 40
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				if got := h.send(mkBatch(uint32(w<<16|i), 3)); got != 200 {
					t.Errorf("worker %d batch %d: %d", w, i, got)
				}
			}
		}(w)
	}
	wg.Wait()
	entries := h.shipped()
	seen := map[string]int{}
	for _, e := range entries {
		f, err := wire.Validate(e.Body)
		if err != nil {
			t.Fatal(err)
		}
		seen[string(f.BatchID[:])]++
	}
	if len(entries) != workers*perWorker || len(seen) != workers*perWorker {
		t.Fatalf("%d entries / %d distinct, want %d", len(entries), len(seen), workers*perWorker)
	}
	fsyncs := h.metric("la_ingest_fsync_seconds_count")
	if fsyncs >= workers*perWorker {
		t.Fatalf("%v fsyncs for %d batches: no grouping", fsyncs, workers*perWorker)
	}
	t.Logf("%d batches, %v fsyncs, %v segments", workers*perWorker, fsyncs, h.metric("la_ingest_segments_uploaded_total"))
}

func TestConfigFromEnv(t *testing.T) {
	env := map[string]string{
		"LA_LISTEN": ":9999", "LA_NODE_ID": "ing-eu-7", "LA_WAL_DIR": "/data/wal", "LA_WAL_SEGMENT_BYTES": "1048576",
		"LA_WAL_SEGMENT_SECONDS": "2.5", "LA_WAL_FSYNC_MS": "0", "LA_COMPAT_POSTHOG": "false", "LA_TRUSTED_PROXIES": "2",
		"LA_RATE_IP": "0", "LA_RATE_TOKEN": "123.5", "LA_LOG_LEVEL": "debug", "LA_TOKENS_KEY": "control/t2.json",
		"LA_WAL_MAX_LOCAL_BYTES": "5000000", "LA_GEO_DB": "/etc/la/country.mmdb", "LA_DRAIN_SECONDS": "0.5",
	}
	c, err := FromEnvFunc(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":9999" || c.NodeID != "ing-eu-7" || c.WALDir != "/data/wal" || c.SegmentBytes != 1<<20 ||
		c.SegmentAge != 2500*time.Millisecond || c.FsyncInterval != 0 || c.CompatPostHog || c.TrustedProxies != 2 ||
		c.RateIP != 0 || c.RateToken != 123.5 || c.LogLevel.String() != "DEBUG" || c.TokensKey != "control/t2.json" ||
		c.WALMaxLocalBytes != 5_000_000 || c.GeoDB != "/etc/la/country.mmdb" || c.DrainDelay != 500*time.Millisecond {
		t.Fatalf("parsed config: %+v", c)
	}
	if c.tokensCachePath() != "/data/wal/tokens.json" {
		t.Fatalf("tokens cache default: %s", c.tokensCachePath())
	}
	// Defaults are the design §6.9 values.
	d, err := FromEnvFunc(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if d.Listen != ":8080" || d.SegmentBytes != 64<<20 || d.SegmentAge != 10*time.Second || d.FsyncInterval != 2*time.Millisecond ||
		d.TokensKey != "control/tokens.json" || !d.CompatPostHog || d.RateToken != 2000 || d.RateIP != 50 ||
		d.DedupeEntries != 100_000 || d.DedupeTTL != 10*time.Minute || d.TokensPoll != 30*time.Second || d.DrainDelay != 5*time.Second {
		t.Fatalf("defaults: %+v", d)
	}
	// Bad values are all reported.
	bad := map[string]string{"LA_NODE_ID": "Ing_01", "LA_WAL_SEGMENT_BYTES": "lots", "LA_COMPAT_POSTHOG": "maybe", "LA_RATE_IP": "-1", "LA_LOG_LEVEL": "loud"}
	_, err = FromEnvFunc(func(k string) string { return bad[k] })
	if err == nil {
		t.Fatal("invalid configuration accepted")
	}
	for k := range bad {
		if !strings.Contains(err.Error(), k) {
			t.Errorf("error does not mention %s: %v", k, err)
		}
	}
}
