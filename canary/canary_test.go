package canary

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// world is a fake ingest + query pair. It understands just enough of the
// canary's batches (fixed layout) and of its three SQL shapes.
type world struct {
	mu        sync.Mutex
	batches   map[string][]int64 // batch_id → seqs (server-side idempotency)
	rows      []int64            // visible seq values (may contain dups / lose rows)
	failNext  int                // answer 503 this many times
	status    int                // forced status for /v1/e (0 = normal)
	posts     int
	dedupe    bool
	dropSeq   map[int64]bool
	lastBodys [][]byte
}

var seqRe = regexp.MustCompile(`<= (\d+)`)

func (w *world) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/e", func(rw http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.mu.Lock()
		defer w.mu.Unlock()
		w.posts++
		w.lastBodys = append(w.lastBodys, body)
		if r.Header.Get("Authorization") != "Bearer pk_test" {
			rw.WriteHeader(401)
			return
		}
		if w.status != 0 {
			rw.WriteHeader(w.status)
			return
		}
		if w.failNext > 0 {
			w.failNext--
			rw.WriteHeader(503)
			return
		}
		id := string(body[5:21]) // magic(4) + header len(1) + batch_id(16)
		if _, ok := w.batches[id]; ok && w.dedupe {
			rw.WriteHeader(200)
			return
		}
		seqs := parseSeqs(body)
		w.batches[id] = seqs
		for _, s := range seqs {
			if !w.dropSeq[s] {
				w.rows = append(w.rows, s)
			}
		}
		rw.WriteHeader(200)
	})
	mux.HandleFunc("/q/sql", func(rw http.ResponseWriter, r *http.Request) {
		var req struct{ From, To, SQL string }
		_ = json.NewDecoder(r.Body).Decode(&req)
		if r.Header.Get("Authorization") != "Bearer sk_test" {
			rw.WriteHeader(401)
			_, _ = rw.Write([]byte(`{"error":{"code":"unauthorized","message":"no"}}`))
			return
		}
		w.mu.Lock()
		defer w.mu.Unlock()
		limit := int64(1 << 62)
		if m := seqRe.FindStringSubmatch(req.SQL); m != nil {
			limit, _ = strconv.ParseInt(m[1], 10, 64)
		}
		counts := map[int64]int64{}
		var total, max int64
		for _, s := range w.rows {
			if s <= limit {
				counts[s]++
				total++
				if s > max {
					max = s
				}
			}
		}
		var rows [][]any
		switch {
		case strings.Contains(req.SQL, "GROUP BY"):
			for s, n := range counts {
				rows = append(rows, []any{s, n})
			}
		case strings.Contains(req.SQL, "count(DISTINCT"):
			rows = [][]any{{total, int64(len(counts)), max}}
		default:
			rows = [][]any{{max}}
		}
		_ = json.NewEncoder(rw).Encode(map[string]any{"columns": []any{}, "rows": rows})
	})
	return mux
}

// parseSeqs pulls the zigzag-varint after each `seq` key (tag 3).
func parseSeqs(body []byte) []int64 {
	var out []int64
	needle := []byte{3, 's', 'e', 'q', 3}
	for i := 0; i+len(needle) < len(body); i++ {
		if string(body[i:i+len(needle)]) == string(needle) {
			var v uint64
			var shift uint
			for j := i + len(needle); j < len(body); j++ {
				v |= uint64(body[j]&0x7f) << shift
				if body[j]&0x80 == 0 {
					break
				}
				shift += 7
			}
			out = append(out, int64(v>>1)^-int64(v&1))
		}
	}
	return out
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func setup(t *testing.T, state string) (*world, *Canary, *clock) {
	t.Helper()
	w := &world{batches: map[string][]int64{}, dedupe: true, dropSeq: map[int64]bool{}}
	srv := httptest.NewServer(w.handler())
	t.Cleanup(srv.Close)
	ck := &clock{t: time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)}
	c, err := New(Config{IngestURL: srv.URL, Token: "pk_test", QueryURL: srv.URL, QueryKey: "sk_test",
		PerTick: 4, Grace: time.Minute, StatePath: state, Now: ck.now, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	return w, c, ck
}

func TestHealthyRun(t *testing.T) {
	w, c, ck := setup(t, "")
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := c.SendOnce(ctx); err != nil {
			t.Fatal(err)
		}
		ck.add(10 * time.Second)
	}
	if err := c.Verify(ctx); err != nil {
		t.Fatal(err)
	}
	s := c.Stats()
	if s.Sent != 12 || s.Missing != 0 || s.Duplicates != 0 || !s.Healthy {
		t.Fatalf("stats: %+v", s)
	}
	if got := len(w.rows); got != 12 {
		t.Fatalf("server saw %d rows", got)
	}
	if s.LastFreshnessSec <= 0 {
		t.Fatalf("freshness not measured: %+v", s)
	}
	if !strings.Contains(c.Metrics(), "la_canary_healthy 1\n") {
		t.Fatal(c.Metrics())
	}
}

func TestMissingIsOnlyRaisedAfterGrace(t *testing.T) {
	w, c, ck := setup(t, "")
	ctx := context.Background()
	w.dropSeq[2] = true // acked but never becomes visible
	if err := c.SendOnce(ctx); err != nil {
		t.Fatal(err)
	}
	ck.add(30 * time.Second)
	_ = c.Verify(ctx)
	if s := c.Stats(); s.Missing != 0 || !s.Healthy || s.Expected != 0 {
		t.Fatalf("inside grace: %+v", s)
	}
	ck.add(31 * time.Second)
	_ = c.Verify(ctx)
	if s := c.Stats(); s.Missing != 1 || s.Healthy || s.Expected != 4 {
		t.Fatalf("after grace: %+v", s)
	}
	if !strings.Contains(c.Metrics(), "la_canary_missing 1\n") {
		t.Fatal(c.Metrics())
	}
}

func TestDuplicatesAreDetected(t *testing.T) {
	w, c, ck := setup(t, "")
	ctx := context.Background()
	_ = c.SendOnce(ctx)
	w.mu.Lock()
	w.rows = append(w.rows, 3) // the pipeline doubled a row
	w.mu.Unlock()
	ck.add(2 * time.Minute)
	_ = c.Verify(ctx)
	if s := c.Stats(); s.Duplicates != 1 || s.Missing != 0 || s.Healthy {
		t.Fatalf("%+v", s)
	}
}

func TestRetriesSendIdenticalBytes(t *testing.T) {
	w, c, _ := setup(t, "")
	w.failNext = 2
	// Shrink the backoff by cancelling slow waits: use a context with enough time.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := c.SendOnce(ctx); err != nil {
		t.Fatal(err)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.posts != 3 || len(w.rows) != 4 {
		t.Fatalf("posts=%d rows=%d", w.posts, len(w.rows))
	}
	for _, b := range w.lastBodys[1:] {
		if string(b) != string(w.lastBodys[0]) {
			t.Fatal("retry body differs: idempotency key would change")
		}
	}
	if c.Stats().SendErrors != 2 {
		t.Fatalf("%+v", c.Stats())
	}
}

func TestPermanentErrorsAreNotRetried(t *testing.T) {
	w, c, _ := setup(t, "")
	w.status = 403
	if err := c.SendOnce(context.Background()); err == nil {
		t.Fatal("expected an error")
	}
	if w.posts != 1 || c.State().Acked != 0 {
		t.Fatalf("posts=%d acked=%d", w.posts, c.State().Acked)
	}
}

func TestIdempotencyResend(t *testing.T) {
	w, c, ck := setup(t, "")
	c.cfg.ResendEvery = 1
	ctx := context.Background()
	_ = c.SendOnce(ctx)
	_ = c.SendOnce(ctx)
	ck.add(2 * time.Minute)
	_ = c.Verify(ctx)
	if w.posts != 4 {
		t.Fatalf("posts=%d", w.posts)
	}
	if s := c.Stats(); !s.Healthy || s.Visible != 8 {
		t.Fatalf("with server-side dedupe the resend must be invisible: %+v", s)
	}
	// A server that does NOT dedupe gets caught.
	w.mu.Lock()
	w.dedupe = false
	w.mu.Unlock()
	_ = c.SendOnce(ctx)
	ck.add(2 * time.Minute)
	_ = c.Verify(ctx)
	if s := c.Stats(); s.Duplicates != 4 || s.Healthy {
		t.Fatalf("%+v", s)
	}
}

func TestRestartContinuesTheRunWithStableIDs(t *testing.T) {
	state := filepath.Join(t.TempDir(), "sub", "canary.json")
	w, c, ck := setup(t, state)
	ctx := context.Background()
	_ = c.SendOnce(ctx)
	run := c.State().Run

	// "Crash" after the server acked batch 2 but before the state was saved:
	// simulate by sending batch 2 from a copy whose state is then discarded.
	c2, err := New(Config{IngestURL: c.cfg.IngestURL, Token: "pk_test", QueryURL: c.cfg.QueryURL, QueryKey: "sk_test",
		PerTick: 4, Grace: time.Minute, StatePath: state, Now: ck.now})
	if err != nil {
		t.Fatal(err)
	}
	if c2.State().Run != run || c2.State().Acked != 4 {
		t.Fatalf("state not restored: %+v", c2.State())
	}
	c2.cfg.StatePath = "" // lose the ack
	_ = c2.SendOnce(ctx)

	c3, _ := New(Config{IngestURL: c.cfg.IngestURL, Token: "pk_test", QueryURL: c.cfg.QueryURL, QueryKey: "sk_test",
		PerTick: 4, Grace: time.Minute, StatePath: state, Now: ck.now})
	_ = c3.SendOnce(ctx) // re-sends seq 5..8 with the same batch_id
	ck.add(2 * time.Minute)
	_ = c3.Verify(ctx)
	if s := c3.Stats(); !s.Healthy || s.Visible != 8 || s.Expected != 8 {
		t.Fatalf("%+v (rows=%v)", s, w.rows)
	}
}

func TestProbe(t *testing.T) {
	_, c, _ := setup(t, "")
	c.cfg.Now = time.Now
	lag, err := c.Probe(context.Background(), 3, 5*time.Second, 10*time.Millisecond)
	if err != nil || lag < 0 {
		t.Fatalf("lag=%v err=%v", lag, err)
	}

	w2, c2, _ := setup(t, "")
	c2.cfg.Now = time.Now
	w2.dropSeq[7] = true
	if _, err := c2.Probe(context.Background(), 2, 300*time.Millisecond, 20*time.Millisecond); err == nil ||
		!strings.Contains(err.Error(), "1 of 8 events missing") {
		t.Fatalf("err=%v", err)
	}
}

func TestQueryErrorsAreCountedNotFatal(t *testing.T) {
	_, c, _ := setup(t, "")
	c.cfg.QueryKey = "sk_wrong"
	_ = c.SendOnce(context.Background())
	if err := c.Verify(context.Background()); err == nil {
		t.Fatal("expected error")
	}
	if s := c.Stats(); s.VerifyErrors != 1 || !s.Healthy {
		t.Fatalf("an unreachable query API is not evidence of loss: %+v", s)
	}
}

func TestConfigValidation(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("empty config accepted")
	}
	if _, err := New(Config{IngestURL: "x", Token: "x", QueryURL: "x", QueryKey: "x", PerTick: 501}); err == nil {
		t.Fatal("PerTick > 500 accepted")
	}
	if got := sqlQuote("a'b"); got != "'a''b'" {
		t.Fatal(got)
	}
}
