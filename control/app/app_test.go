package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mucan54/izlek/control/app"
	"github.com/mucan54/izlek/control/auth"
	"github.com/mucan54/izlek/control/config"
	"github.com/mucan54/izlek/control/db"
	"github.com/mucan54/izlek/control/publish"
	"github.com/mucan54/izlek/golib/devs3"
	"github.com/mucan54/izlek/golib/objstore"
	"github.com/mucan54/izlek/ingest/tokens"
)

const password = "correct horse battery"

var fastHash = auth.Params{MemoryKiB: 64, Time: 1, Threads: 1}

func openDB(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func TestBootstrapIsIdempotent(t *testing.T) {
	ctx := context.Background()
	d := openDB(t)
	h := auth.NewHasher(fastHash, 1)
	in := app.BootstrapInput{Email: " Admin@Example.com ", Password: password, Org: "Acme", Project: "Shop"}
	now := time.UnixMilli(1_795_046_400_000)

	first, err := app.Bootstrap(ctx, d, h, in, now)
	if err != nil {
		t.Fatal(err)
	}
	if first.UserID != 1 || first.OrgID != 1 || first.ProjectID != 1 || !auth.LooksLikeWriteToken(first.Token) || !auth.LooksLikeSecretKey(first.Secret) {
		t.Fatalf("first run: %+v", first)
	}
	if !reflect.DeepEqual(first.Created, []string{"user", "org", "membership", "project", "token", "key"}) {
		t.Fatalf("first run created %v", first.Created)
	}

	counts := func() map[string]int {
		out := map[string]int{}
		for _, table := range []string{"users", "orgs", "memberships", "projects", "tokens", "keys", "audit_log"} {
			var n int
			if err := d.R().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
				t.Fatal(err)
			}
			out[table] = n
		}
		return out
	}
	before := counts()

	// Run again — with a different password, which must not change anything.
	again := in
	again.Password = "some other password"
	second, err := app.Bootstrap(ctx, d, h, again, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if second.UserID != first.UserID || second.OrgID != first.OrgID || second.ProjectID != first.ProjectID || second.Token != first.Token {
		t.Fatalf("second run returned different ids: %+v vs %+v", second, first)
	}
	if second.Secret != "" || len(second.Created) != 0 {
		t.Fatalf("second run created something: %+v", second)
	}
	if after := counts(); !reflect.DeepEqual(after, before) {
		t.Fatalf("second run changed the database:\n%v\n%v", before, after)
	}
	u, err := db.UserByEmail(ctx, d.R(), "admin@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if ok, _, _ := h.Verify(ctx, u.PasswordHash, password); !ok {
		t.Fatal("a repeated bootstrap changed the password")
	}

	// The secret is stored only as a hash, with both scopes.
	k, err := db.KeyByHash(ctx, d.R(), auth.HashSecret(first.Secret))
	if err != nil || !reflect.DeepEqual(k.Scopes, []string{"query", "admin"}) || k.ProjectID != first.ProjectID {
		t.Fatalf("bootstrap key: %+v, %v", k, err)
	}

	// --rotate-key hands out a new secret and keeps the old one working.
	rot := in
	rot.RotateKey = true
	third, err := app.Bootstrap(ctx, d, h, rot, now)
	if err != nil || !auth.LooksLikeSecretKey(third.Secret) || third.Secret == first.Secret || !reflect.DeepEqual(third.Created, []string{"key"}) {
		t.Fatalf("rotate: %+v, %v", third, err)
	}

	// A second project in the same organisation, a second organisation for
	// the same user: only what is missing is created.
	p2 := in
	p2.Project = "Blog"
	fourth, err := app.Bootstrap(ctx, d, h, p2, now)
	if err != nil || fourth.OrgID != first.OrgID || fourth.ProjectID != 2 || fourth.Token == first.Token || !reflect.DeepEqual(fourth.Created, []string{"project", "token", "key"}) {
		t.Fatalf("second project: %+v, %v", fourth, err)
	}
	o2 := in
	o2.Org = "Other"
	fifth, err := app.Bootstrap(ctx, d, h, o2, now)
	if err != nil || fifth.OrgID == first.OrgID || fifth.UserID != first.UserID || fifth.ProjectID != 3 {
		t.Fatalf("second org: %+v, %v", fifth, err)
	}

	// Input validation.
	for _, bad := range []app.BootstrapInput{
		{Email: "nope", Password: password, Org: "A", Project: "P"},
		{Email: "new@example.com", Password: "short", Org: "A", Project: "P"},
		{Email: "new@example.com", Password: password, Org: " ", Project: "P"},
		{Email: "new@example.com", Password: password, Org: "A", Project: strings.Repeat("x", 101)},
	} {
		if _, err := app.Bootstrap(ctx, d, h, bad, now); err == nil {
			t.Errorf("Bootstrap(%+v) succeeded", bad)
		}
	}
	if _, err := db.UserByEmail(ctx, d.R(), "new@example.com"); err == nil {
		t.Fatal("a rejected bootstrap created a user")
	}
}

type httpClient struct {
	t    *testing.T
	base string
	c    *http.Client
	csrf string
}

func (h *httpClient) do(method, path string, body any) (int, []byte) {
	h.t.Helper()
	var rd io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, h.base+path, rd)
	if err != nil {
		h.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if h.csrf != "" && method != "GET" {
		req.Header.Set("X-LA-CSRF", h.csrf)
	}
	res, err := h.c.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer res.Body.Close()
	out, _ := io.ReadAll(res.Body)
	return res.StatusCode, out
}

func (h *httpClient) must(method, path string, body any, want int) []byte {
	h.t.Helper()
	status, out := h.do(method, path, body)
	if status != want {
		h.t.Fatalf("%s %s: status %d, want %d: %s", method, path, status, want, out)
	}
	return out
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The whole service over real HTTP with its background loops running:
// setup → project → changes appear in the store within the deadline; a
// store outage does not fail API calls and is caught up afterwards; a
// restart keeps versions increasing; deletion status flips on its own.
func TestServiceEndToEnd(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "control.db")
	s3 := devs3.New()
	s3ts := httptest.NewServer(s3)
	defer s3ts.Close()
	store, err := objstore.Open("s3://la-e2e", objstore.S3Config{Endpoint: s3ts.URL, AccessKey: "ak", SecretKey: "sk"})
	if err != nil {
		t.Fatal(err)
	}
	get := func(key string) (string, bool) {
		raw, _, err := store.Get(ctx, key, nil)
		return string(raw), err == nil
	}
	version := func(key string) int64 {
		body, ok := get(key)
		if !ok {
			return -1
		}
		var doc struct {
			Version int64 `json:"version"`
			V       int64 `json:"v"`
		}
		_ = json.Unmarshal([]byte(body), &doc)
		return max(doc.Version, doc.V)
	}

	cfg := config.Defaults()
	cfg.CookieSecure = false // plain-HTTP test server: the cookie jar would not send a Secure cookie back
	cfg.PublishDebounce = 20 * time.Millisecond
	cfg.DeletionCheck = 30 * time.Millisecond
	cfg.InternalSecret = "x"

	start := func() (*app.App, *httptest.Server, func()) {
		d, err := db.Open(ctx, dbPath)
		if err != nil {
			t.Fatal(err)
		}
		a, err := app.New(app.Options{Config: cfg, DB: d, Store: store, Hasher: auth.NewHasher(fastHash, 4),
			PublishRetryMin: 10 * time.Millisecond, PublishRetryMax: 50 * time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		runCtx, cancel := context.WithCancel(ctx)
		a.Start(runCtx)
		ts := httptest.NewServer(a.Server)
		var once sync.Once
		stop := func() {
			once.Do(func() {
				ts.Close()
				cancel()
				sctx, c2 := context.WithTimeout(ctx, 5*time.Second)
				defer c2()
				if err := a.Stop(sctx); err != nil {
					t.Errorf("stop: %v", err)
				}
				d.Close()
			})
		}
		t.Cleanup(stop)
		return a, ts, stop
	}

	a, ts, stop := start()
	jar, _ := cookiejar.New(nil)
	h := &httpClient{t: t, base: ts.URL, c: &http.Client{Jar: jar, Timeout: 10 * time.Second}}

	// An empty installation still publishes (empty) control files at start,
	// so the data plane never waits on a missing object.
	waitFor(t, "initial publish", func() bool { return version(publish.TokensKey) == 1 })
	if body, _ := get(publish.TokensKey); !strings.Contains(body, `"tokens":{}`) {
		t.Fatalf("initial tokens.json: %s", body)
	}

	h.must("POST", "/api/setup", map[string]any{"email": "owner@example.com", "password": password, "org_name": "Acme"}, 200)
	var me struct {
		CSRF string `json:"csrf"`
		Orgs []struct {
			ID int64 `json:"id"`
		} `json:"orgs"`
	}
	_ = json.Unmarshal(h.must("GET", "/api/me", nil, 200), &me)
	h.csrf = me.CSRF

	var p struct {
		ID    uint32 `json:"id"`
		Token string `json:"token"`
	}
	changed := time.Now()
	_ = json.Unmarshal(h.must("POST", "/api/projects", map[string]any{"org_id": me.Orgs[0].ID, "name": "Shop"}, 200), &p)
	waitFor(t, "token published", func() bool { body, _ := get(publish.TokensKey); return strings.Contains(body, p.Token) })
	if took := time.Since(changed); took > 2*time.Second {
		t.Fatalf("change → store took %v (contract: ≤ 2 s)", took)
	}
	waitFor(t, "ruleset published", func() bool { _, ok := get(publish.FlagsKey(p.Token)); return ok })
	// Ingestion's own loader accepts what the running service wrote.
	reg := tokens.New(tokens.Options{Store: store})
	if err := reg.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if tk, ok := reg.Lookup(p.Token); !ok || tk.ProjectID != p.ID || tk.Disabled {
		t.Fatalf("ingestion view of the new token: %+v, %v", tk, ok)
	}

	base := fmt.Sprintf("/api/projects/%d", p.ID)

	// --- store outage ---
	tokensBefore := version(publish.TokensKey)
	s3.SetFault(devs3.FaultFail)
	var tok struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(h.must("POST", base+"/tokens", map[string]any{"name": "during outage"}, 200), &tok)
	h.must("POST", base+"/flags", map[string]any{"key": "outage-flag", "enabled": true}, 200)
	var del struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	_ = json.Unmarshal(h.must("POST", base+"/deletions", map[string]any{"user_ids": []string{"42"}}, 200), &del)
	// The database has it all, whatever the store says.
	if !strings.Contains(string(h.must("GET", base+"/tokens", nil, 200)), tok.Token) {
		t.Fatal("token created during the outage is not in the database")
	}
	waitFor(t, "lag metric", func() bool { return a.Publisher.LagSeconds() > 0 && a.Publisher.Stats().Failed >= 2 })
	metrics := string(h.must("GET", "/metrics", nil, 200))
	if strings.Contains(metrics, "la_control_publish_lag_seconds 0\n") || !strings.Contains(metrics, "la_control_publish_lag_seconds ") {
		t.Fatalf("publish lag not exported during the outage:\n%s", metrics)
	}
	// readyz stays green: the control plane itself is fine.
	h.must("GET", "/readyz", nil, 200)

	s3.SetFault(devs3.FaultNone)
	waitFor(t, "catch-up after the outage", func() bool { return a.Publisher.LagSeconds() == 0 })
	if body, _ := get(publish.TokensKey); !strings.Contains(body, tok.Token) {
		t.Fatal("token not published after the outage")
	}
	if v := version(publish.TokensKey); v != tokensBefore+1 {
		t.Fatalf("tokens.json version %d after the outage, want %d", v, tokensBefore+1)
	}
	if body, _ := get(publish.FlagsKey(tok.Token)); !strings.Contains(body, "outage-flag") {
		t.Fatalf("ruleset after the outage: %s", body)
	}
	tombs, err := store.List(ctx, fmt.Sprintf("tombstones/p=%d/", p.ID))
	if err != nil || len(tombs) != 1 || !strings.HasSuffix(tombs[0].Key, "-"+del.ID+".json") {
		t.Fatalf("tombstones after the outage: %v, %v", tombs, err)
	}

	// --- deletion status flips without anyone asking (no data → nothing to rewrite) ---
	waitFor(t, "deletion done", func() bool {
		return strings.Contains(string(h.must("GET", base+"/deletions", nil, 200)), `"status":"done"`)
	})

	// --- restart ---
	flagsBefore := version(publish.FlagsKey(p.Token))
	projectsBefore := version(publish.ProjectsKey)
	tokensBefore = version(publish.TokensKey)
	stop()
	_, ts2, _ := start()
	h.base = ts2.URL
	// Same session, same database: still logged in after the restart.
	jar2, _ := cookiejar.New(nil)
	h2 := &httpClient{t: t, base: ts2.URL, c: &http.Client{Jar: jar2, Timeout: 10 * time.Second}}
	h2.must("POST", "/api/auth/login", map[string]any{"email": "owner@example.com", "password": password}, 200)
	_ = json.Unmarshal(h2.must("GET", "/api/me", nil, 200), &me)
	h2.csrf = me.CSRF
	h2.must("PUT", base+"/flags/outage-flag", map[string]any{"enabled": false}, 200)
	h2.must("PATCH", base, map[string]any{"retention_days": 30}, 200)
	waitFor(t, "publish after restart", func() bool {
		return version(publish.FlagsKey(p.Token)) == flagsBefore+1 && version(publish.ProjectsKey) == projectsBefore+1
	})
	if v := version(publish.TokensKey); v != tokensBefore {
		t.Fatalf("tokens.json version changed across a restart without a token change: %d → %d", tokensBefore, v)
	}
}

func TestStopFlushesPendingChanges(t *testing.T) {
	ctx := context.Background()
	d := openDB(t)
	store, err := objstore.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.PublishDebounce = 2 * time.Second // long enough that only the shutdown flush can publish
	a, err := app.New(app.Options{Config: cfg, DB: d, Store: store, Hasher: auth.NewHasher(fastHash, 1)})
	if err != nil {
		t.Fatal(err)
	}
	a.Start(ctx)
	waitFor(t, "initial publish", func() bool { return a.Publisher.Stats().Published >= 1 })
	res, err := app.Bootstrap(ctx, d, auth.NewHasher(fastHash, 1), app.BootstrapInput{Email: "a@example.com", Password: password, Org: "A", Project: "P"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	a.Publisher.Notify()
	if err := a.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	raw, _, err := store.Get(ctx, publish.TokensKey, nil)
	if err != nil || !strings.Contains(string(raw), res.Token) {
		t.Fatalf("pending change not flushed at shutdown: %s, %v", raw, err)
	}
}

// Many clients mutate at once while the publisher runs: no lost updates, no
// data races (run with -race), and the store converges on the database.
func TestConcurrentMutationsConverge(t *testing.T) {
	ctx := context.Background()
	d := openDB(t)
	s3 := devs3.New()
	s3ts := httptest.NewServer(s3)
	defer s3ts.Close()
	store, err := objstore.Open("s3://la-conc", objstore.S3Config{Endpoint: s3ts.URL, AccessKey: "ak", SecretKey: "sk"})
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.CookieSecure = false
	cfg.PublishDebounce = 5 * time.Millisecond
	a, err := app.New(app.Options{Config: cfg, DB: d, Store: store, Hasher: auth.NewHasher(fastHash, 4)})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	a.Start(runCtx)
	ts := httptest.NewServer(a.Server)
	defer func() {
		ts.Close()
		cancel()
		_ = a.Stop(ctx)
	}()

	boot, err := app.Bootstrap(ctx, d, auth.NewHasher(fastHash, 1), app.BootstrapInput{Email: "a@example.com", Password: password, Org: "A", Project: "P"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	base := fmt.Sprintf("%s/api/projects/%d", ts.URL, boot.ProjectID)
	call := func(method, url string, body any) (int, []byte) {
		raw, _ := json.Marshal(body)
		req, _ := http.NewRequest(method, url, bytes.NewReader(raw))
		req.Header.Set("Authorization", "Bearer "+boot.Secret)
		req.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Error(err)
			return 0, nil
		}
		defer res.Body.Close()
		out, _ := io.ReadAll(res.Body)
		return res.StatusCode, out
	}

	const workers, perWorker = 8, 6
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				if status, out := call("POST", base+"/tokens", map[string]any{"name": fmt.Sprintf("t-%d-%d", w, i)}); status != 200 {
					t.Errorf("create token: %d %s", status, out)
				}
				key := fmt.Sprintf("flag-%d-%d", w, i)
				if status, out := call("POST", base+"/flags", map[string]any{"key": key, "enabled": true}); status != 200 {
					t.Errorf("create flag: %d %s", status, out)
				}
				// Everyone also fights over one shared flag.
				call("POST", base+"/flags", map[string]any{"key": "shared"})
				if status, out := call("PUT", base+"/flags/shared", map[string]any{"enabled": i%2 == 0, "name": fmt.Sprint(w)}); status != 200 {
					t.Errorf("put shared flag: %d %s", status, out)
				}
				call("GET", base+"/flags", nil)
			}
		}(w)
	}
	wg.Wait()
	if t.Failed() {
		return
	}

	// The shared flag saw exactly workers × perWorker updates after its creation.
	var shared struct {
		Version int64 `json:"version"`
	}
	_, out := call("GET", base+"/flags/shared", nil)
	_ = json.Unmarshal(out, &shared)
	if shared.Version != workers*perWorker+1 {
		t.Fatalf("shared flag version %d, want %d (lost update)", shared.Version, workers*perWorker+1)
	}
	var versions []struct {
		Version int64 `json:"version"`
	}
	_, out = call("GET", base+"/flags/shared/versions", nil)
	_ = json.Unmarshal(out, &versions)
	if len(versions) != workers*perWorker+1 {
		t.Fatalf("history has %d entries", len(versions))
	}

	// The store converges on the database.
	toks, err := db.AllTokens(ctx, d.R())
	if err != nil || len(toks) != workers*perWorker+1 {
		t.Fatalf("%d tokens in the database, %v", len(toks), err)
	}
	waitFor(t, "store to converge", func() bool {
		if a.Publisher.LagSeconds() != 0 {
			return false
		}
		raw, _, err := store.Get(ctx, publish.TokensKey, nil)
		if err != nil {
			return false
		}
		var doc publish.TokensDoc
		if json.Unmarshal(raw, &doc) != nil || len(doc.Tokens) != len(toks) {
			return false
		}
		for _, tk := range toks {
			raw, _, err := store.Get(ctx, publish.FlagsKey(tk.Token), nil)
			var rs publish.RulesetDoc
			if err != nil || json.Unmarshal(raw, &rs) != nil || len(rs.Flags) != workers*perWorker+1 {
				return false
			}
		}
		return true
	})
	// Every ruleset of the project is the same document.
	first, _, _ := store.Get(ctx, publish.FlagsKey(toks[0].Token), nil)
	for _, tk := range toks[1:] {
		if other, _, _ := store.Get(ctx, publish.FlagsKey(tk.Token), nil); !bytes.Equal(first, other) {
			t.Fatalf("rulesets of one project differ:\n%s\n%s", first, other)
		}
	}
}
