package publish

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mucan54/izlek/control/auth"
	"github.com/mucan54/izlek/control/db"
	"github.com/mucan54/izlek/golib/devs3"
	"github.com/mucan54/izlek/golib/flags"
	"github.com/mucan54/izlek/golib/objstore"
	"github.com/mucan54/izlek/ingest/tokens"
)

const bucket = "la-test"

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type fixture struct {
	t     *testing.T
	ctx   context.Context
	db    *db.DB
	s3    *devs3.Server
	store objstore.Store
	clock *clock
	org   int64
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	s3 := devs3.New()
	ts := httptest.NewServer(s3)
	t.Cleanup(ts.Close)
	store, err := objstore.Open("s3://"+bucket, objstore.S3Config{Endpoint: ts.URL, AccessKey: "ak", SecretKey: "sk"})
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, ctx: ctx, db: d, s3: s3, store: store, clock: &clock{t: time.UnixMilli(1_795_046_400_000)}}
	f.write(func(tx *sql.Tx) error {
		var err error
		f.org, err = db.CreateOrg(ctx, tx, "Acme", f.ms())
		return err
	})
	return f
}

func (f *fixture) ms() int64 { return f.clock.Now().UnixMilli() }

func (f *fixture) write(fn func(tx *sql.Tx) error) {
	f.t.Helper()
	if err := f.db.Write(f.ctx, fn); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) publisher() *Publisher {
	return New(Options{DB: f.db, Store: f.store, Now: f.clock.Now, OpTimeout: 5 * time.Second})
}

func (f *fixture) project(name string) db.Project {
	f.t.Helper()
	var p db.Project
	f.write(func(tx *sql.Tx) error {
		var err error
		p, err = db.CreateProject(f.ctx, tx, db.Project{OrgID: f.org, Name: name, RetentionDays: 365, ReplayRetentionDays: 30,
			DropKeys: []string{}, MaxConcurrentQueries: 4, ReplaySampleRate: 0.1}, f.ms())
		return err
	})
	return p
}

func (f *fixture) token(pid uint32, mod func(*db.Token)) db.Token {
	f.t.Helper()
	tok := db.Token{Token: auth.NewWriteToken(), ProjectID: pid, Name: "t", Status: "active", AllowedOrigins: []string{}, CreatedAt: f.ms()}
	if mod != nil {
		mod(&tok)
	}
	f.write(func(tx *sql.Tx) error { return db.CreateToken(f.ctx, tx, tok) })
	return tok
}

func (f *fixture) flag(pid uint32, key string, enabled bool, rules string) {
	f.t.Helper()
	f.write(func(tx *sql.Tx) error {
		_, err := db.CreateFlag(f.ctx, tx, pid, db.Flag{Key: key, Enabled: enabled, Salt: "s-" + key, Rules: json.RawMessage(rules),
			Default: json.RawMessage(`null`), UpdatedAt: f.ms(), UpdatedBy: db.SystemActor()})
		return err
	})
}

func (f *fixture) get(key string) string {
	f.t.Helper()
	raw, _, err := f.store.Get(f.ctx, key, nil)
	if err != nil {
		f.t.Fatalf("get %s: %v", key, err)
	}
	return string(raw)
}

func (f *fixture) exists(key string) bool {
	_, _, err := f.store.Get(f.ctx, key, nil)
	if err != nil && !errors.Is(err, objstore.ErrNotFound) {
		f.t.Fatalf("get %s: %v", key, err)
	}
	return err == nil
}

func (f *fixture) keys(prefix string) []string {
	var out []string
	for _, k := range f.s3.Keys(bucket) {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	return out
}

func mustPublish(t *testing.T, p *Publisher) {
	t.Helper()
	if err := p.PublishNow(context.Background()); err != nil {
		t.Fatalf("publish: %v", err)
	}
}

// The three control files have exactly the JSON of spec/manifest.md §8.
func TestControlFilesMatchTheSpec(t *testing.T) {
	f := newFixture(t)
	p1 := f.project("Acme")
	f.write(func(tx *sql.Tx) error {
		p1.RetentionDays, p1.DropKeys, p1.ScanLimitBytes = 90, []string{"email"}, 1 << 30
		return db.UpdateProject(f.ctx, tx, p1)
	})
	tok := f.token(p1.ID, func(t *db.Token) {
		t.Token, t.RateLimit, t.AllowedOrigins, t.Replay = "pk_live_abc", 2000, []string{"https://app.example.com"}, true
	})
	off := f.token(p1.ID, func(t *db.Token) { t.Token, t.Status = "pk_off", "disabled" })
	secret := auth.NewSecretKey()
	f.write(func(tx *sql.Tx) error {
		_, err := db.CreateKey(f.ctx, tx, db.Key{ProjectID: p1.ID, Name: "ci", Scopes: []string{"query"}, Hash: auth.HashSecret(secret), Prefix: auth.SecretPrefixOf(secret), CreatedAt: f.ms()})
		return err
	})
	mustPublish(t, f.publisher())

	now := f.ms()
	wantTokens := fmt.Sprintf(`{"version":1,"generated_at":%d,"tokens":{"pk_live_abc":{"project_id":1,"status":"active","rate_limit":2000,"allowed_origins":["https://app.example.com"],"replay":true},"pk_off":{"project_id":1,"status":"disabled","rate_limit":0,"allowed_origins":[],"replay":false}}}`, now)
	if got := f.get(TokensKey); got != wantTokens {
		t.Errorf("tokens.json\n got %s\nwant %s", got, wantTokens)
	}
	wantKeys := fmt.Sprintf(`{"version":1,"keys":{"%s":{"project_id":1,"scopes":["query"]}}}`, auth.HashSecret(secret))
	if got := f.get(KeysKey); got != wantKeys {
		t.Errorf("keys.json\n got %s\nwant %s", got, wantKeys)
	}
	wantProjects := `{"version":1,"projects":{"1":{"name":"Acme","retention_days":90,"replay_retention_days":30,"drop_keys":["email"],"scan_limit_bytes":1073741824,"max_concurrent_queries":4}}}`
	if got := f.get(ProjectsKey); got != wantProjects {
		t.Errorf("projects.json\n got %s\nwant %s", got, wantProjects)
	}

	// Nothing secret reaches the store: neither the key nor its display prefix.
	for _, k := range f.s3.Keys(bucket) {
		body := f.get(k)
		if strings.Contains(body, secret) || strings.Contains(body, secret[3:]) || strings.Contains(body, "sk_") {
			t.Errorf("%s leaks secret key material: %s", k, body)
		}
	}

	// The real ingestion registry loads the file.
	reg := tokens.New(tokens.Options{Store: f.store})
	if err := reg.Refresh(f.ctx); err != nil {
		t.Fatalf("ingest/tokens rejects tokens.json: %v", err)
	}
	if reg.Version() != 1 || reg.Count() != 2 {
		t.Fatalf("registry: version %d, %d tokens", reg.Version(), reg.Count())
	}
	got, ok := reg.Lookup(tok.Token)
	if !ok || got.ProjectID != 1 || got.Disabled || !got.Replay || !reflect.DeepEqual(got.Origins, []string{"https://app.example.com"}) ||
		got.Limiter == nil || got.Limiter.Rate() != 2000 {
		t.Fatalf("registry entry: %+v, %v", got, ok)
	}
	if !got.OriginAllowed("https://APP.example.com/") || got.OriginAllowed("https://evil.example.com") {
		t.Error("origin allow-list not honoured by the registry")
	}
	if d, ok := reg.Lookup(off.Token); !ok || !d.Disabled {
		t.Fatalf("disabled token in the registry: %+v, %v", d, ok)
	}
}

func docVersion(t *testing.T, body string) int64 {
	t.Helper()
	var doc struct {
		Version int64 `json:"version"`
		V       int64 `json:"v"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatal(err)
	}
	return max(doc.Version, doc.V)
}

func TestVersionsIncreaseOnlyOnChangeAndSurviveRestart(t *testing.T) {
	f := newFixture(t)
	p1 := f.project("one")
	tok := f.token(p1.ID, nil)
	pub := f.publisher()
	mustPublish(t, pub)
	first := f.get(TokensKey)
	if docVersion(t, first) != 1 {
		t.Fatalf("first version: %s", first)
	}

	// Republishing unchanged state is byte-identical (same version, same
	// generated_at), so conditional GETs by the data plane stay 304.
	f.clock.Advance(time.Hour)
	mustPublish(t, pub)
	if again := f.get(TokensKey); again != first {
		t.Fatalf("unchanged republish differs:\n%s\n%s", first, again)
	}

	// A token change bumps tokens.json only.
	f.token(p1.ID, nil)
	mustPublish(t, pub)
	if v := docVersion(t, f.get(TokensKey)); v != 2 {
		t.Fatalf("tokens.json version after a token change: %d", v)
	}
	if v := docVersion(t, f.get(KeysKey)); v != 1 {
		t.Fatalf("keys.json version after a token change: %d", v)
	}
	if v := docVersion(t, f.get(ProjectsKey)); v != 1 {
		t.Fatalf("projects.json version after a token change: %d", v)
	}

	// "Restart": a new publisher over the same database and store.
	pub2 := f.publisher()
	mustPublish(t, pub2)
	if v := docVersion(t, f.get(TokensKey)); v != 2 {
		t.Fatalf("restart without changes moved the version to %d", v)
	}
	f.write(func(tx *sql.Tx) error {
		p1.Name = "renamed"
		return db.UpdateProject(f.ctx, tx, p1)
	})
	f.flag(p1.ID, "a", true, `[]`)
	mustPublish(t, pub2)
	if v := docVersion(t, f.get(ProjectsKey)); v != 2 {
		t.Fatalf("projects.json after restart + change: %d", v)
	}
	if v := docVersion(t, f.get(FlagsKey(tok.Token))); v != 2 {
		t.Fatalf("flags v after restart + change: %d", v)
	}

	// The database is restored from an old backup (it remembers lower
	// versions than the store holds): numbers must still only go up.
	f.write(func(tx *sql.Tx) error {
		_, err := tx.ExecContext(f.ctx, `DELETE FROM publish_state`)
		return err
	})
	pub3 := f.publisher()
	mustPublish(t, pub3)
	if v := docVersion(t, f.get(TokensKey)); v != 3 {
		t.Fatalf("tokens.json after losing publish state: %d, want 3", v)
	}
	if v := docVersion(t, f.get(ProjectsKey)); v != 3 {
		t.Fatalf("projects.json after losing publish state: %d, want 3", v)
	}
	if v := docVersion(t, f.get(FlagsKey(tok.Token))); v != 3 {
		t.Fatalf("flags v after losing publish state: %d, want 3", v)
	}
	mustPublish(t, pub3)
	if v := docVersion(t, f.get(TokensKey)); v != 3 {
		t.Fatalf("version kept climbing without changes: %d", v)
	}
}

func TestFlagsFilesFollowTokensAndFlags(t *testing.T) {
	f := newFixture(t)
	p1, p2 := f.project("one"), f.project("two")
	a := f.token(p1.ID, nil)
	b := f.token(p1.ID, nil)
	c := f.token(p2.ID, nil)
	rules := `[{"when":{"and":[{"prop":"platform","op":"eq","value":"ios"},{"prop":"app_version","op":"semver_gte","value":"3.2.0"}]},"rollout":2500,"variants":[{"key":"control","w":5000},{"key":"b","w":5000}],"stable_on":"anon_id"}]`
	f.flag(p1.ID, "new_checkout", true, rules)
	pub := f.publisher()
	mustPublish(t, pub)

	want := `{"v":1,"flags":[{"key":"new_checkout","enabled":true,"salt":"s-new_checkout","rules":` + rules + `,"default":null}]}`
	for _, tok := range []db.Token{a, b} {
		if got := f.get(FlagsKey(tok.Token)); got != want {
			t.Errorf("flags/%s.json\n got %s\nwant %s", tok.Token, got, want)
		}
	}
	if got := f.get(FlagsKey(c.Token)); got != `{"v":1,"flags":[]}` {
		t.Errorf("another project's ruleset: %s", got)
	}
	// The shared evaluator reads exactly what was published.
	rs, err := flags.ParseRuleset([]byte(f.get(FlagsKey(a.Token))))
	if err != nil || rs.V != 1 || rs.Find("new_checkout") == nil {
		t.Fatalf("published ruleset does not parse: %+v, %v", rs, err)
	}

	// A second flag: v + 1 for every token of that project, nothing else moves.
	f.flag(p1.ID, "beta", false, `[]`)
	mustPublish(t, pub)
	for _, tok := range []db.Token{a, b} {
		if v := docVersion(t, f.get(FlagsKey(tok.Token))); v != 2 {
			t.Errorf("token %s: v = %d, want 2", tok.Token, v)
		}
	}
	if v := docVersion(t, f.get(FlagsKey(c.Token))); v != 1 {
		t.Errorf("unrelated project's v moved to %d", v)
	}
	var doc RulesetDoc
	if err := json.Unmarshal([]byte(f.get(FlagsKey(a.Token))), &doc); err != nil || len(doc.Flags) != 2 || doc.Flags[0].Key != "beta" {
		t.Fatalf("flags are ordered by key: %+v, %v", doc, err)
	}

	// A new token gets the current ruleset at the current v.
	d := f.token(p1.ID, nil)
	mustPublish(t, pub)
	if got, cur := f.get(FlagsKey(d.Token)), f.get(FlagsKey(a.Token)); got != cur {
		t.Errorf("new token's ruleset differs:\n%s\n%s", got, cur)
	}

	// Disabling a token removes its ruleset; enabling brings it back.
	f.write(func(tx *sql.Tx) error {
		b.Status = "disabled"
		return db.UpdateToken(f.ctx, tx, b)
	})
	mustPublish(t, pub)
	if f.exists(FlagsKey(b.Token)) {
		t.Error("disabled token still has a ruleset")
	}
	f.write(func(tx *sql.Tx) error {
		b.Status = "active"
		return db.UpdateToken(f.ctx, tx, b)
	})
	mustPublish(t, pub)
	if !f.exists(FlagsKey(b.Token)) {
		t.Error("re-enabled token has no ruleset")
	}

	// Deleting a token removes it from tokens.json and deletes its ruleset —
	// also when the publish that notices is an incremental one.
	f.write(func(tx *sql.Tx) error { return db.DeleteToken(f.ctx, tx, p1.ID, a.Token) })
	pub.Notify()
	if err := pub.Flush(f.ctx); err != nil {
		t.Fatal(err)
	}
	if f.exists(FlagsKey(a.Token)) {
		t.Error("deleted token still has a ruleset")
	}
	if strings.Contains(f.get(TokensKey), a.Token) {
		t.Error("deleted token still in tokens.json")
	}

	// Closing a project: tokens stay listed as disabled, retention drops to
	// 0, rulesets and keys go away.
	secret := auth.NewSecretKey()
	f.write(func(tx *sql.Tx) error {
		if _, err := db.CreateKey(f.ctx, tx, db.Key{ProjectID: p2.ID, Name: "k", Scopes: []string{"query"}, Hash: auth.HashSecret(secret), Prefix: "sk_…", CreatedAt: f.ms()}); err != nil {
			return err
		}
		return nil
	})
	mustPublish(t, pub)
	if !strings.Contains(f.get(KeysKey), auth.HashSecret(secret)) {
		t.Fatal("key not published")
	}
	f.write(func(tx *sql.Tx) error { return db.CloseProject(f.ctx, tx, p2.ID, f.ms()) })
	mustPublish(t, pub)
	var td TokensDoc
	if err := json.Unmarshal([]byte(f.get(TokensKey)), &td); err != nil {
		t.Fatal(err)
	}
	if e, ok := td.Tokens[c.Token]; !ok || e.Status != "disabled" {
		t.Errorf("closed project's token: %+v, %v", e, ok)
	}
	if f.exists(FlagsKey(c.Token)) {
		t.Error("closed project's ruleset still published")
	}
	if strings.Contains(f.get(KeysKey), auth.HashSecret(secret)) {
		t.Error("closed project's key still published")
	}
	var pd ProjectsDoc
	if err := json.Unmarshal([]byte(f.get(ProjectsKey)), &pd); err != nil {
		t.Fatal(err)
	}
	if e := pd.Projects["2"]; e.RetentionDays != 0 || e.ReplayRetentionDays != 0 || !e.Closed {
		t.Errorf("closed project entry: %+v", e)
	}
	if e := pd.Projects["1"]; e.RetentionDays != 365 || e.Closed {
		t.Errorf("open project entry: %+v", e)
	}
}

// A full publish repairs objects that were deleted by hand, removes stray
// rulesets, and leaves foreign objects alone. An incremental publish only
// writes what changed.
func TestFullRepublishRepairsTheStore(t *testing.T) {
	f := newFixture(t)
	p1 := f.project("one")
	tok := f.token(p1.ID, nil)
	pub := f.publisher()
	mustPublish(t, pub)

	stray := FlagsKey(auth.NewWriteToken())
	foreign := "flags/README.txt"
	for _, k := range []string{stray, foreign} {
		if _, err := f.store.Put(f.ctx, k, []byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	for _, k := range []string{TokensKey, KeysKey, ProjectsKey, FlagsKey(tok.Token)} {
		if err := f.store.Delete(f.ctx, k); err != nil {
			t.Fatal(err)
		}
	}

	// Incremental publish with nothing changed: no writes at all.
	before := f.s3.Requests()
	pub.Notify()
	if err := pub.Flush(f.ctx); err != nil {
		t.Fatal(err)
	}
	if n := f.s3.Requests() - before; n != 0 {
		t.Errorf("incremental publish without changes made %d store requests", n)
	}
	if f.exists(TokensKey) {
		t.Fatal("incremental publish rewrote an unchanged object")
	}

	mustPublish(t, pub) // what the 5-minute timer does
	for _, k := range []string{TokensKey, KeysKey, ProjectsKey, FlagsKey(tok.Token)} {
		if !f.exists(k) {
			t.Errorf("%s not restored by the full republish", k)
		}
	}
	if docVersion(t, f.get(TokensKey)) != 1 {
		t.Error("restoring an object changed its version")
	}
	if f.exists(stray) {
		t.Error("stray ruleset survived the full republish")
	}
	if !f.exists(foreign) {
		t.Error("an object that is not a ruleset was deleted")
	}
}

func TestStoreOutageIsRetriedInTheBackground(t *testing.T) {
	f := newFixture(t)
	p1 := f.project("one")
	pub := New(Options{DB: f.db, Store: f.store, Now: time.Now, Debounce: 5 * time.Millisecond, Interval: time.Hour,
		RetryMin: 10 * time.Millisecond, RetryMax: 40 * time.Millisecond, OpTimeout: 2 * time.Second})
	ctx, cancel := context.WithCancel(f.ctx)
	done := make(chan struct{})
	go func() { pub.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	waitFor(t, "initial publish", func() bool { return pub.Stats().Published >= 1 })
	if pub.LagSeconds() != 0 {
		t.Fatalf("lag after a successful publish: %v", pub.LagSeconds())
	}

	f.s3.SetFault(devs3.FaultFail)
	tok := f.token(p1.ID, nil)
	pub.Notify()
	waitFor(t, "failed attempts", func() bool { return pub.Stats().Failed >= 3 })
	time.Sleep(20 * time.Millisecond)
	if lag := pub.LagSeconds(); lag <= 0 {
		t.Fatalf("lag during the outage: %v", lag)
	}
	l1 := pub.LagSeconds()
	time.Sleep(30 * time.Millisecond)
	if l2 := pub.LagSeconds(); l2 <= l1 {
		t.Fatalf("lag does not grow during the outage: %v then %v", l1, l2)
	}

	f.s3.SetFault(devs3.FaultNone)
	waitFor(t, "catch-up", func() bool { return pub.LagSeconds() == 0 })
	if !strings.Contains(f.get(TokensKey), tok.Token) {
		t.Fatal("token created during the outage was not published afterwards")
	}
	if !f.exists(FlagsKey(tok.Token)) {
		t.Fatal("ruleset of the token created during the outage is missing")
	}
	if v := docVersion(t, f.get(TokensKey)); v != 2 {
		t.Fatalf("version after the outage: %d (retries must not burn versions)", v)
	}
}

// A burst of changes is coalesced, and the result is in the store well
// inside the 2 s the contract allows.
func TestChangesArePublishedWithinTheDeadline(t *testing.T) {
	f := newFixture(t)
	p1 := f.project("one")
	pub := New(Options{DB: f.db, Store: f.store, Now: time.Now, Debounce: 250 * time.Millisecond, Interval: time.Hour})
	ctx, cancel := context.WithCancel(f.ctx)
	done := make(chan struct{})
	go func() { pub.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	waitFor(t, "initial publish", func() bool { return pub.Stats().Published >= 1 })

	start := time.Now()
	var last db.Token
	for i := 0; i < 10; i++ {
		last = f.token(p1.ID, nil)
		pub.Notify()
	}
	waitFor(t, "burst published", func() bool {
		raw, _, err := f.store.Get(f.ctx, TokensKey, nil)
		return err == nil && strings.Contains(string(raw), last.Token)
	})
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("publication took %v", took)
	}
	if n := pub.Stats().Published; n > 4 {
		t.Errorf("10 changes caused %d publishes; they should be coalesced", n-1)
	}
	if v := docVersion(t, f.get(TokensKey)); v > 4 {
		t.Errorf("tokens.json version %d after one burst", v)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (f *fixture) deletion(pid uint32, users, anons []string) db.Deletion {
	f.t.Helper()
	var d db.Deletion
	f.write(func(tx *sql.Tx) error {
		var err error
		d, err = db.CreateDeletion(f.ctx, tx, db.Deletion{ID: "del_" + auth.NewULID(f.clock.Now()), ProjectID: pid, UserIDs: users, AnonIDs: anons, CreatedBy: db.SystemActor()}, f.ms())
		return err
	})
	return d
}

func TestTombstones(t *testing.T) {
	f := newFixture(t)
	p1 := f.project("one")
	pub := f.publisher()
	mustPublish(t, pub)

	d1 := f.deletion(p1.ID, []string{"42", "user with spaces"}, []string{"0190aaaa"})
	d2 := f.deletion(p1.ID, nil, []string{"0190bbbb"})
	if d2.TSMillis <= d1.TSMillis {
		t.Fatalf("ts_ms not increasing: %d, %d", d1.TSMillis, d2.TSMillis)
	}
	pub.Notify()
	if err := pub.Flush(f.ctx); err != nil {
		t.Fatal(err)
	}
	key1 := fmt.Sprintf("tombstones/p=%d/%013d-%s.json", p1.ID, d1.TSMillis, d1.ID)
	if key1 != TombstoneKey(p1.ID, d1.TSMillis, d1.ID) || len(strings.Split(key1, "/")[2]) != 13+1+len(d1.ID)+5 {
		t.Fatalf("tombstone key %q", key1)
	}
	want1 := fmt.Sprintf(`{"id":"%s","ts_ms":%d,"person_keys":["u:42","u:user with spaces"],"anon_ids":["0190aaaa"]}`, d1.ID, d1.TSMillis)
	if got := f.get(key1); got != want1 {
		t.Errorf("tombstone\n got %s\nwant %s", got, want1)
	}
	want2 := fmt.Sprintf(`{"id":"%s","ts_ms":%d,"person_keys":[],"anon_ids":["0190bbbb"]}`, d2.ID, d2.TSMillis)
	if got := f.get(TombstoneKey(p1.ID, d2.TSMillis, d2.ID)); got != want2 {
		t.Errorf("tombstone\n got %s\nwant %s", got, want2)
	}
	list, err := db.ListDeletions(f.ctx, f.db.R(), p1.ID)
	if err != nil || len(list) != 2 || list[0].PublishedAt == 0 || list[1].PublishedAt == 0 {
		t.Fatalf("published_at not recorded: %+v, %v", list, err)
	}

	// Already-written tombstones are not written again by later publishes…
	before := len(f.keys("tombstones/"))
	reqs := f.s3.Requests()
	pub.Notify()
	if err := pub.Flush(f.ctx); err != nil {
		t.Fatal(err)
	}
	if f.s3.Requests() != reqs || len(f.keys("tombstones/")) != before {
		t.Error("an incremental publish touched tombstones again")
	}
	// …but a full publish restores one that was removed by hand, and leaves
	// the compactor's own objects in that prefix alone.
	state := fmt.Sprintf("tombstones/p=%d/_applied.state", p1.ID)
	if _, err := f.store.Put(f.ctx, state, []byte(`{"side_ts":1}`)); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Delete(f.ctx, key1); err != nil {
		t.Fatal(err)
	}
	mustPublish(t, pub)
	if got := f.get(key1); got != want1 {
		t.Errorf("tombstone not restored: %s", got)
	}
	if f.get(state) != `{"side_ts":1}` {
		t.Error("compactor state object was modified")
	}

	// A tombstone past its retention is not resurrected.
	if err := f.store.Delete(f.ctx, key1); err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(396 * 24 * time.Hour)
	mustPublish(t, pub)
	if f.exists(key1) {
		t.Error("a 396-day-old tombstone was written again")
	}
}

// During an outage tombstones wait; afterwards they appear in ts_ms order
// and a failure in the middle never lets a later one overtake an earlier one.
func TestTombstonesKeepTheirOrderAcrossFailures(t *testing.T) {
	f := newFixture(t)
	p1 := f.project("one")
	pub := f.publisher()
	mustPublish(t, pub)
	f.s3.SetFault(devs3.FaultFail)
	d1 := f.deletion(p1.ID, []string{"1"}, nil)
	d2 := f.deletion(p1.ID, []string{"2"}, nil)
	pub.Notify()
	if err := pub.Flush(f.ctx); err == nil {
		t.Fatal("publish succeeded during the outage")
	}
	if pub.LagSeconds() < 0 || len(f.keys("tombstones/")) != 0 {
		t.Fatal("tombstones appeared during the outage")
	}
	list, _ := db.ListDeletions(f.ctx, f.db.R(), p1.ID)
	for _, d := range list {
		if d.PublishedAt != 0 {
			t.Fatalf("%s marked published during the outage", d.ID)
		}
	}
	f.s3.SetFault(devs3.FaultNone)
	if err := pub.Flush(f.ctx); err != nil {
		t.Fatal(err)
	}
	keys := f.keys("tombstones/")
	if len(keys) != 2 || keys[0] != TombstoneKey(p1.ID, d1.TSMillis, d1.ID) || keys[1] != TombstoneKey(p1.ID, d2.TSMillis, d2.ID) {
		t.Fatalf("tombstones after recovery: %v", keys)
	}
}

func putManifest(t *testing.T, f *fixture, pid uint32, day string, version int, tombstoneTS int64) {
	t.Helper()
	key := fmt.Sprintf("data/p=%d/d=%s/_m/%08d.json", pid, day, version)
	body := fmt.Sprintf(`{"version":%d,"project":%d,"day":%q,"created_at":1,"plans":{"1-0":1},"tombstone_ts":%d,"files":[]}`, version, pid, day, tombstoneTS)
	if _, err := f.store.Put(f.ctx, key, []byte(body)); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) status(pid uint32, id string) string {
	f.t.Helper()
	list, err := db.ListDeletions(f.ctx, f.db.R(), pid)
	if err != nil {
		f.t.Fatal(err)
	}
	for _, d := range list {
		if d.ID == id {
			return d.Status
		}
	}
	f.t.Fatalf("deletion %s not found", id)
	return ""
}

func TestDeletionStatusFollowsTheManifests(t *testing.T) {
	f := newFixture(t)
	p1, p2 := f.project("one"), f.project("two")
	pub := f.publisher()
	chk := &Checker{DB: f.db, Store: f.store, Now: f.clock.Now}
	check := func() (int, int) {
		t.Helper()
		done, pending, err := chk.CheckOnce(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		return done, pending
	}

	// Two days of data, plus objects that must not be mistaken for manifests.
	putManifest(t, f, p1.ID, "2026-11-17", 1, 0)
	putManifest(t, f, p1.ID, "2026-11-18", 1, 0)
	putManifest(t, f, p1.ID, "2026-11-18", 2, 0)
	for _, k := range []string{"data/p=1/d=2026-11-18/L2-00000002-000.parquet", "data/p=1/d=2026-11-18/_m/notes.txt", "data/p=10/d=2026-11-18/_m/00000001.json"} {
		if _, err := f.store.Put(f.ctx, k, []byte(`{"tombstone_ts":0}`)); err != nil {
			t.Fatal(err)
		}
	}

	d1 := f.deletion(p1.ID, []string{"42"}, nil)

	// Not yet in the store: stays pending no matter what the manifests say.
	if done, pending := check(); done != 0 || pending != 1 {
		t.Fatalf("unpublished request: done=%d pending=%d", done, pending)
	}
	mustPublish(t, pub)
	if done, pending := check(); done != 0 || pending != 1 {
		t.Fatalf("manifests behind: done=%d pending=%d", done, pending)
	}

	// One day catches up, the other does not.
	putManifest(t, f, p1.ID, "2026-11-17", 2, d1.TSMillis)
	if done, _ := check(); done != 0 || f.status(p1.ID, d1.ID) != "pending" {
		t.Fatal("done although one day manifest is behind")
	}
	// An older manifest version with a high watermark does not count: only
	// the newest version of a day does.
	putManifest(t, f, p1.ID, "2026-11-18", 3, d1.TSMillis-1)
	if done, _ := check(); done != 0 {
		t.Fatal("done with tombstone_ts one millisecond short")
	}
	reqs := f.s3.Requests()
	check()
	// Unchanged manifests are not downloaded again (they are immutable).
	if n := f.s3.Requests() - reqs; n > 3 {
		t.Errorf("a repeated check made %d store requests", n)
	}

	d2 := f.deletion(p1.ID, nil, []string{"anon"})
	mustPublish(t, pub)
	putManifest(t, f, p1.ID, "2026-11-18", 4, d1.TSMillis)
	if done, pending := check(); done != 1 || pending != 1 {
		t.Fatalf("first request should complete, the newer one not: done=%d pending=%d", done, pending)
	}
	if f.status(p1.ID, d1.ID) != "done" || f.status(p1.ID, d2.ID) != "pending" {
		t.Fatalf("statuses: %s, %s", f.status(p1.ID, d1.ID), f.status(p1.ID, d2.ID))
	}

	// A project without any data has nothing to rewrite: done on first check.
	d3 := f.deletion(p2.ID, []string{"7"}, nil)
	mustPublish(t, pub)
	if done, _ := check(); done != 1 || f.status(p2.ID, d3.ID) != "done" {
		t.Fatalf("empty project: %s", f.status(p2.ID, d3.ID))
	}

	// Eight days on, a request completes even if a manifest never catches up.
	f.clock.Advance(DeletionGrace - time.Minute)
	if done, _ := check(); done != 0 {
		t.Fatal("completed before the 8-day limit")
	}
	f.clock.Advance(2 * time.Minute)
	if done, pending := check(); done != 1 || pending != 0 || f.status(p1.ID, d2.ID) != "done" {
		t.Fatalf("after 8 days: done=%d pending=%d status=%s", done, pending, f.status(p1.ID, d2.ID))
	}
}

func TestCheckerSurvivesStoreErrors(t *testing.T) {
	f := newFixture(t)
	p1 := f.project("one")
	pub := f.publisher()
	d1 := f.deletion(p1.ID, []string{"1"}, nil)
	mustPublish(t, pub)
	putManifest(t, f, p1.ID, "2026-11-18", 1, d1.TSMillis)
	chk := &Checker{DB: f.db, Store: f.store, Now: f.clock.Now}
	f.s3.SetFault(devs3.FaultFail)
	done, pending, err := chk.CheckOnce(f.ctx)
	if err == nil || done != 0 || pending != 1 {
		t.Fatalf("during an outage: done=%d pending=%d err=%v", done, pending, err)
	}
	f.s3.SetFault(devs3.FaultNone)
	if done, _, err := chk.CheckOnce(f.ctx); err != nil || done != 1 {
		t.Fatalf("after the outage: done=%d err=%v", done, err)
	}
}

func TestDayManifestKeyParsing(t *testing.T) {
	prefix := "data/p=7/"
	ok := map[string][2]any{
		"data/p=7/d=2026-11-18/_m/00000042.json": {"2026-11-18", int64(42)},
		"data/p=7/d=2026-01-01/_m/1.json":        {"2026-01-01", int64(1)},
	}
	for key, want := range ok {
		day, v, found := dayManifest(key, prefix)
		if !found || day != want[0] || v != want[1] {
			t.Errorf("dayManifest(%q) = %q, %d, %v", key, day, v, found)
		}
	}
	for _, key := range []string{"data/p=70/d=2026-11-18/_m/00000001.json", "data/p=7/d=2026-11-18/L0-1-0-0000000001.parquet",
		"data/p=7/d=2026-11-18/_m/x.json", "data/p=7/d=2026-11-18/_m/00000001.json.tmp", "data/p=7/_m/00000001.json",
		"data/p=7/d=2026-11-18/_m/sub/00000001.json", "data/p=7/d=2026-11-18/_m/00000000.json"} {
		if _, _, found := dayManifest(key, prefix); found {
			t.Errorf("dayManifest(%q) accepted", key)
		}
	}
}

// The file backend (single-machine deployments) behaves the same way.
func TestPublishToFileStore(t *testing.T) {
	f := newFixture(t)
	fs, err := objstore.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f.store = fs
	p1 := f.project("one")
	tok := f.token(p1.ID, nil)
	d1 := f.deletion(p1.ID, []string{"1"}, nil)
	pub := f.publisher()
	mustPublish(t, pub)
	objs, err := fs.List(f.ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, o := range objs {
		keys = append(keys, o.Key)
	}
	want := []string{KeysKey, ProjectsKey, TokensKey, FlagsKey(tok.Token), TombstoneKey(p1.ID, d1.TSMillis, d1.ID)}
	if len(keys) != len(want) {
		t.Fatalf("objects: %v", keys)
	}
	for _, w := range want {
		found := false
		for _, k := range keys {
			found = found || k == w
		}
		if !found {
			t.Errorf("%s missing from %v", w, keys)
		}
	}
	f.write(func(tx *sql.Tx) error { return db.DeleteToken(f.ctx, tx, p1.ID, tok.Token) })
	mustPublish(t, pub)
	if _, _, err := fs.Get(f.ctx, FlagsKey(tok.Token), nil); !errors.Is(err, objstore.ErrNotFound) {
		t.Fatalf("ruleset of a deleted token: %v", err)
	}
}
