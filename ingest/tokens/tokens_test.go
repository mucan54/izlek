package tokens

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mucan54/izlek/golib/devs3"
	"github.com/mucan54/izlek/golib/objstore"
)

const docV12 = `{ "version": 12, "generated_at": 1795046400000,
  "tokens": { "pk_live_abc": { "project_id": 1017, "status": "active",
              "rate_limit": 2000, "allowed_origins": ["https://app.example.com"],
              "replay": true },
              "pk_off": { "project_id": 5, "status": "disabled" },
              "pk_plain": { "project_id": 6, "status": "active", "future_field": {"x": 1} } },
  "unknown_top_level": true }`

func setup(t *testing.T) (*devs3.Server, objstore.Store, string) {
	t.Helper()
	srv := devs3.New()
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	st, err := objstore.Open("s3://bkt", objstore.S3Config{Endpoint: ts.URL})
	if err != nil {
		t.Fatal(err)
	}
	return srv, st, filepath.Join(t.TempDir(), "state", "tokens.json")
}

func TestLoadLookup(t *testing.T) {
	_, st, cache := setup(t)
	ctx := context.Background()
	if _, err := st.Put(ctx, "control/tokens.json", []byte(docV12)); err != nil {
		t.Fatal(err)
	}
	r := New(Options{Store: st, CachePath: cache, DefaultRate: 50})
	if r.Loaded() || r.AgeSeconds() != -1 {
		t.Fatal("new registry claims to be loaded")
	}
	if _, ok := r.Lookup("pk_live_abc"); ok {
		t.Fatal("lookup before load succeeded")
	}
	if err := r.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if !r.Loaded() || r.Version() != 12 || r.Count() != 3 {
		t.Fatalf("loaded=%v version=%d count=%d", r.Loaded(), r.Version(), r.Count())
	}
	tok, ok := r.Lookup("pk_live_abc")
	if !ok || tok.ProjectID != 1017 || tok.Disabled || !tok.Replay || tok.Limiter.Rate() != 2000 {
		t.Fatalf("pk_live_abc: %+v", tok)
	}
	if !tok.OriginAllowed("https://app.example.com") || !tok.OriginAllowed("HTTPS://APP.example.com/") || tok.OriginAllowed("https://evil.example") {
		t.Fatal("origin allow-list")
	}
	off, ok := r.Lookup("pk_off")
	if !ok || !off.Disabled || off.ProjectID != 5 {
		t.Fatalf("pk_off: %+v", off)
	}
	plain, _ := r.Lookup("pk_plain")
	if plain.Replay || !plain.OriginAllowed("https://anything.example") || plain.Limiter.Rate() != 50 {
		t.Fatalf("pk_plain: %+v", plain)
	}
	if _, ok := r.Lookup("pk_unknown"); ok {
		t.Fatal("unknown token found")
	}
	// pk_plain has no allow-list, so any origin may pass a preflight.
	if !r.AnyTokenAllowsOrigin("https://whatever.example") {
		t.Fatal("AnyTokenAllowsOrigin with an open token")
	}
	if got, err := os.ReadFile(cache); err != nil || string(got) != docV12 {
		t.Fatalf("last-good copy: %v", err)
	}
	if age := r.AgeSeconds(); age < 0 || age > 5 {
		t.Fatalf("age %v", age)
	}
}

func TestReloadWithETag(t *testing.T) {
	srv, st, cache := setup(t)
	ctx := context.Background()
	st.Put(ctx, "control/tokens.json", []byte(docV12))
	now := time.Unix(1_800_000_000, 0)
	r := New(Options{Store: st, CachePath: cache, Now: func() time.Time { return now }})
	if err := r.Load(ctx); err != nil {
		t.Fatal(err)
	}
	old, _ := r.Lookup("pk_live_abc")

	// Unchanged object: conditional GET -> 304, age refreshed, same snapshot.
	now = now.Add(30 * time.Second)
	if r.AgeSeconds() != 30 {
		t.Fatalf("age before refresh %v", r.AgeSeconds())
	}
	before := srv.Requests()
	if err := r.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if srv.Requests() != before+1 || r.AgeSeconds() != 0 {
		t.Fatalf("304 path: requests +%d, age %v", srv.Requests()-before, r.AgeSeconds())
	}
	if same, _ := r.Lookup("pk_live_abc"); same != old {
		t.Fatal("snapshot replaced on 304")
	}

	// Changed object: token disabled, one added, one removed.
	v13 := `{"version":13,"tokens":{"pk_live_abc":{"project_id":1017,"status":"disabled","rate_limit":2000},
	         "pk_new":{"project_id":9,"status":"active","allowed_origins":["https://a.example","https://b.example/"]}}}`
	st.Put(ctx, "control/tokens.json", []byte(v13))
	if err := r.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	tok, _ := r.Lookup("pk_live_abc")
	if !tok.Disabled || r.Version() != 13 {
		t.Fatalf("reload not applied: %+v v=%d", tok, r.Version())
	}
	if tok.Limiter != old.Limiter {
		t.Fatal("rate bucket was reset by a reload")
	}
	if _, ok := r.Lookup("pk_plain"); ok {
		t.Fatal("removed token still present")
	}
	if _, ok := r.Lookup("pk_new"); !ok {
		t.Fatal("added token missing")
	}
	// No open token any more: preflight only for listed origins of active tokens.
	if r.AnyTokenAllowsOrigin("https://app.example.com") || !r.AnyTokenAllowsOrigin("https://b.example") {
		t.Fatal("AnyTokenAllowsOrigin after reload")
	}
	if got, _ := os.ReadFile(cache); string(got) != v13 {
		t.Fatal("last-good copy not updated")
	}
}

func TestLastGoodFallback(t *testing.T) {
	srv, st, cache := setup(t)
	ctx := context.Background()
	st.Put(ctx, "control/tokens.json", []byte(docV12))
	r := New(Options{Store: st, CachePath: cache})
	if err := r.Load(ctx); err != nil {
		t.Fatal(err)
	}

	// 1. Store outage while running: keep serving the current set.
	srv.SetFault(devs3.FaultFail)
	if err := r.Refresh(ctx); err == nil {
		t.Fatal("refresh succeeded during outage")
	}
	if _, ok := r.Lookup("pk_live_abc"); !ok || r.Failures() != 1 {
		t.Fatal("token set dropped during outage")
	}

	// 2. Restart during the outage: start from the disk copy.
	old := time.Now().Add(-20 * time.Minute)
	os.Chtimes(cache, old, old)
	r2 := New(Options{Store: st, CachePath: cache})
	if err := r2.Load(ctx); err != nil {
		t.Fatalf("load from last-good copy: %v", err)
	}
	if tok, ok := r2.Lookup("pk_live_abc"); !ok || tok.ProjectID != 1017 {
		t.Fatal("last-good copy not used")
	}
	if age := r2.AgeSeconds(); age < 19*60 || age > 21*60 {
		t.Fatalf("age after cache start = %v, want ~1200 (cache file age)", age)
	}

	// 3. Store back, but with a broken document: keep the good set.
	srv.SetFault(devs3.FaultNone)
	for _, bad := range []string{`{not json`, `{"version":14}`, `{"tokens":{"pk_x":{"project_id":0,"status":"active"}}}`,
		`{"tokens":{"pk_x":{"project_id":4294967296,"status":"active"}}}`, `{"tokens":{"pk_x":null}}`} {
		st.Put(ctx, "control/tokens.json", []byte(bad))
		if err := r2.Refresh(ctx); err == nil {
			t.Fatalf("accepted invalid document %s", bad)
		}
		if _, ok := r2.Lookup("pk_live_abc"); !ok || r2.Version() != 12 {
			t.Fatalf("good set lost after invalid document %s", bad)
		}
	}
	if got, _ := os.ReadFile(cache); string(got) != docV12 {
		t.Fatal("invalid document overwrote the last-good copy")
	}

	// 4. Object deleted: still keep the set.
	st.Delete(ctx, "control/tokens.json")
	if err := r2.Refresh(ctx); err == nil {
		t.Fatal("refresh succeeded with missing object")
	}
	if !r2.Loaded() {
		t.Fatal("set dropped when object disappeared")
	}

	// 5. Unknown status fails closed for that token only.
	st.Put(ctx, "control/tokens.json", []byte(`{"version":15,"tokens":{"pk_a":{"project_id":1,"status":"paused"},"pk_b":{"project_id":2,"status":"active"}}}`))
	if err := r2.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	a, _ := r2.Lookup("pk_a")
	b, _ := r2.Lookup("pk_b")
	if !a.Disabled || b.Disabled {
		t.Fatal("unknown status handling")
	}

	// 6. Nothing anywhere: Load fails and the registry stays empty.
	_, st3, cache3 := setup(t)
	r3 := New(Options{Store: st3, CachePath: cache3})
	if err := r3.Load(ctx); err == nil || r3.Loaded() {
		t.Fatalf("load with no source: err=%v loaded=%v", err, r3.Loaded())
	} else if !strings.Contains(err.Error(), "no last-good copy") {
		t.Fatalf("error should mention the missing fallback: %v", err)
	}
}

func TestRunPolls(t *testing.T) {
	_, st, cache := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st.Put(ctx, "control/tokens.json", []byte(docV12))
	r := New(Options{Store: st, CachePath: cache})
	if err := r.Load(ctx); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { r.Run(ctx, 10*time.Millisecond); close(done) }()
	st.Put(ctx, "control/tokens.json", []byte(`{"version":99,"tokens":{}}`))
	deadline := time.Now().Add(5 * time.Second)
	for r.Version() != 99 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if r.Version() != 99 || r.Count() != 0 {
		t.Fatalf("poller did not pick up the change (version %d)", r.Version())
	}
	cancel()
	<-done
}
