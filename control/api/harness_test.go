package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mucan54/izlek/control/app"
	"github.com/mucan54/izlek/control/auth"
	"github.com/mucan54/izlek/control/config"
	"github.com/mucan54/izlek/control/db"
	"github.com/mucan54/izlek/golib/devs3"
	"github.com/mucan54/izlek/golib/objstore"
)

const (
	testBucket   = "la-test"
	testPassword = "correct horse battery"
	testSecret   = "internal-secret-for-tests"
)

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

// upstream is a fake query service that records what it receives.
type upstream struct {
	mu       sync.Mutex
	requests []*recorded
	handler  func(w http.ResponseWriter, r *http.Request, body []byte)
	srv      *httptest.Server
}

type recorded struct {
	Method string
	Path   string
	Query  string
	Header http.Header
	Body   []byte
}

func (u *upstream) last() *recorded {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.requests) == 0 {
		return nil
	}
	return u.requests[len(u.requests)-1]
}

func (u *upstream) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.requests)
}

func (u *upstream) set(h func(w http.ResponseWriter, r *http.Request, body []byte)) {
	u.mu.Lock()
	u.handler = h
	u.mu.Unlock()
}

type env struct {
	t        *testing.T
	app      *app.App
	db       *db.DB
	s3       *devs3.Server
	store    objstore.Store
	clock    *clock
	upstream *upstream
	cfg      config.Config
}

type envOption func(*config.Config)

// newEnv builds a control plane over a temp database, an in-memory S3 and a
// fake query service. Background loops are not started: tests publish
// explicitly with e.publish() so that nothing depends on timing.
func newEnv(t *testing.T, opts ...envOption) *env {
	t.Helper()
	return newEnvWith(t, nil, opts...)
}

// newEnvWith is newEnv with a hook to adjust the application's options.
func newEnvWith(t *testing.T, appOpt func(*app.Options), opts ...envOption) *env {
	t.Helper()
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	s3 := devs3.New()
	s3ts := httptest.NewServer(s3)
	store, err := objstore.Open("s3://"+testBucket, objstore.S3Config{Endpoint: s3ts.URL, AccessKey: "ak", SecretKey: "sk"})
	if err != nil {
		t.Fatal(err)
	}
	up := &upstream{}
	up.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		up.mu.Lock()
		up.requests = append(up.requests, &recorded{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Header: r.Header.Clone(), Body: body})
		h := up.handler
		up.mu.Unlock()
		if h != nil {
			h(w, r, body)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	cfg := config.Defaults()
	cfg.CookieSecure = true
	cfg.QueryURL = up.srv.URL
	cfg.InternalSecret = testSecret
	cfg.QueryTimeout = 5 * time.Second
	for _, o := range opts {
		o(&cfg)
	}
	clk := &clock{t: time.UnixMilli(1_795_046_400_000)}
	appOptions := app.Options{
		Config: cfg, DB: d, Store: store, Now: clk.Now,
		// Cheap argon2 parameters: the tests create dozens of users.
		Hasher: auth.NewHasher(auth.Params{MemoryKiB: 64, Time: 1, Threads: 1}, 4),
	}
	if appOpt != nil {
		appOpt(&appOptions)
	}
	a, err := app.New(appOptions)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		a.Server.Close()
		up.srv.Close()
		s3ts.Close()
		d.Close()
	})
	return &env{t: t, app: a, db: d, s3: s3, store: store, clock: clk, upstream: up, cfg: cfg}
}

func (e *env) publish() {
	e.t.Helper()
	if err := e.app.Publisher.Flush(context.Background()); err != nil {
		e.t.Fatalf("publish: %v", err)
	}
}

func (e *env) object(key string) (string, bool) {
	e.t.Helper()
	raw, _, err := e.store.Get(context.Background(), key, nil)
	if errors.Is(err, objstore.ErrNotFound) {
		return "", false
	}
	if err != nil {
		e.t.Fatalf("get %s: %v", key, err)
	}
	return string(raw), true
}

func (e *env) mustObject(key string) string {
	e.t.Helper()
	body, ok := e.object(key)
	if !ok {
		e.t.Fatalf("object %s does not exist; store has %v", key, e.s3.Keys(testBucket))
	}
	return body
}

// ---- HTTP client ----

type client struct {
	e      *env
	cookie string
	csrf   string
	bearer string
	ip     string
	// extra headers sent with every request
	headers map[string]string
	// noCSRF suppresses the X-LA-CSRF header.
	noCSRF bool
	// rawContentType overrides Content-Type ("-" = send none).
	rawContentType string
	email          string
	userID         int64
}

type response struct {
	t      *testing.T
	Status int
	Header http.Header
	Body   []byte
	req    string
}

func (e *env) anon() *client { return &client{e: e, ip: "198.51.100.7"} }

func (c *client) do(method, path string, body any) *response {
	c.e.t.Helper()
	var rd io.Reader
	hasBody := false
	switch b := body.(type) {
	case nil:
	case string:
		rd, hasBody = strings.NewReader(b), true
	case []byte:
		rd, hasBody = bytes.NewReader(b), true
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			c.e.t.Fatal(err)
		}
		rd, hasBody = bytes.NewReader(raw), true
	}
	req := httptest.NewRequest(method, path, rd)
	req.RemoteAddr = c.ip + ":40000"
	if hasBody {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.rawContentType == "-" {
		req.Header.Del("Content-Type")
	} else if c.rawContentType != "" {
		req.Header.Set("Content-Type", c.rawContentType)
	}
	if c.cookie != "" {
		req.AddCookie(&http.Cookie{Name: "la_session", Value: c.cookie})
	}
	if c.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.bearer)
	}
	if c.csrf != "" && !c.noCSRF && method != http.MethodGet && method != http.MethodHead {
		req.Header.Set("X-LA-CSRF", c.csrf)
	}
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	c.e.app.Server.ServeHTTP(rec, req)
	res := rec.Result()
	for _, ck := range res.Cookies() {
		if ck.Name == "la_session" {
			if ck.MaxAge < 0 || ck.Value == "" {
				c.cookie = ""
			} else {
				c.cookie = ck.Value
			}
		}
	}
	return &response{t: c.e.t, Status: res.StatusCode, Header: res.Header, Body: rec.Body.Bytes(), req: method + " " + path}
}

func (c *client) get(path string) *response             { return c.do("GET", path, nil) }
func (c *client) post(path string, body any) *response  { return c.do("POST", path, body) }
func (c *client) put(path string, body any) *response   { return c.do("PUT", path, body) }
func (c *client) patch(path string, body any) *response { return c.do("PATCH", path, body) }
func (c *client) del(path string) *response             { return c.do("DELETE", path, nil) }

// want asserts the status code and returns the response for chaining.
func (r *response) want(status int) *response {
	r.t.Helper()
	if r.Status != status {
		r.t.Fatalf("%s: status %d, want %d; body: %s", r.req, r.Status, status, r.Body)
	}
	return r
}

// wantError asserts the status and the error envelope's code.
func (r *response) wantError(status int, code string) *response {
	r.t.Helper()
	r.want(status)
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(r.Body, &env); err != nil {
		r.t.Fatalf("%s: error body is not the JSON envelope: %s", r.req, r.Body)
	}
	if env.Error.Code != code || env.Error.Message == "" {
		r.t.Fatalf("%s: error %q (%q), want code %q with a message", r.req, env.Error.Code, env.Error.Message, code)
	}
	return r
}

func (r *response) json(v any) *response {
	r.t.Helper()
	if err := json.Unmarshal(r.Body, v); err != nil {
		r.t.Fatalf("%s: cannot decode %s: %v", r.req, r.Body, err)
	}
	return r
}

func (r *response) object() map[string]any {
	r.t.Helper()
	var m map[string]any
	r.json(&m)
	return m
}

func (r *response) list() []map[string]any {
	r.t.Helper()
	if len(r.Body) == 0 || r.Body[0] != '[' {
		r.t.Fatalf("%s: want a bare JSON array, got %s", r.req, r.Body)
	}
	var l []map[string]any
	r.json(&l)
	return l
}

// ---- fixtures ----

type meBody struct {
	User struct {
		ID          int64  `json:"id"`
		Email       string `json:"email"`
		TOTPEnabled bool   `json:"totp_enabled"`
	} `json:"user"`
	Orgs []struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
		Role string `json:"role"`
	} `json:"orgs"`
	CSRF string `json:"csrf"`
}

func (c *client) me() meBody {
	c.e.t.Helper()
	var m meBody
	c.get("/api/me").want(200).json(&m)
	c.csrf, c.email, c.userID = m.CSRF, m.User.Email, m.User.ID
	return m
}

// setup runs first-run setup and returns the owner's client and org id.
func (e *env) setup() (*client, int64) {
	e.t.Helper()
	c := e.anon()
	c.post("/api/setup", map[string]any{"email": "owner@example.com", "password": testPassword, "org_name": "Acme"}).want(200)
	m := c.me()
	if len(m.Orgs) != 1 || m.Orgs[0].Role != "owner" {
		e.t.Fatalf("setup: %+v", m)
	}
	return c, m.Orgs[0].ID
}

func (e *env) login(email, password string) *client {
	e.t.Helper()
	c := e.anon()
	c.post("/api/auth/login", map[string]any{"email": email, "password": password}).want(200)
	c.me()
	return c
}

var userSeq int

// member adds a new user with the given role to org and returns a logged-in
// client for it.
func (e *env) member(owner *client, org int64, role string) *client {
	e.t.Helper()
	userSeq++
	email := fmt.Sprintf("%s-%d@example.com", role, userSeq)
	owner.post(fmt.Sprintf("/api/orgs/%d/members", org), map[string]any{"email": email, "role": role, "password": testPassword}).want(200)
	return e.login(email, testPassword)
}

type projectBody struct {
	ID                   uint32   `json:"id"`
	OrgID                int64    `json:"org_id"`
	Name                 string   `json:"name"`
	RetentionDays        int      `json:"retention_days"`
	ReplayRetentionDays  int      `json:"replay_retention_days"`
	DropKeys             []string `json:"drop_keys"`
	ScanLimitBytes       int64    `json:"scan_limit_bytes"`
	MaxConcurrentQueries int      `json:"max_concurrent_queries"`
	ReplaySampleRate     float64  `json:"replay_sample_rate"`
	CreatedAt            int64    `json:"created_at"`
	Token                string   `json:"token"`
}

func (e *env) project(c *client, org int64, name string) projectBody {
	e.t.Helper()
	var p projectBody
	c.post("/api/projects", map[string]any{"org_id": org, "name": name}).want(200).json(&p)
	if p.ID == 0 || p.Token == "" {
		e.t.Fatalf("project: %+v", p)
	}
	return p
}

// secretKey creates an sk_ key with the given scopes and returns a client
// that authenticates with it (no cookie, no CSRF token).
func (e *env) secretKey(c *client, pid uint32, scopes ...string) (*client, int64) {
	e.t.Helper()
	var k struct {
		ID     int64  `json:"id"`
		Secret string `json:"secret"`
	}
	c.post(fmt.Sprintf("/api/projects/%d/keys", pid), map[string]any{"name": "test key", "scopes": scopes}).want(200).json(&k)
	if !strings.HasPrefix(k.Secret, "sk_") {
		e.t.Fatalf("key secret %q", k.Secret)
	}
	kc := e.anon()
	kc.bearer = k.Secret
	return kc, k.ID
}

// bootstrapOrg creates a user owning a new organisation with one project,
// the way `la-control bootstrap` does.
func bootstrapOrg(t *testing.T, e *env, email, org, project string) app.BootstrapResult {
	t.Helper()
	res, err := app.Bootstrap(context.Background(), e.db, auth.NewHasher(auth.Params{MemoryKiB: 64, Time: 1, Threads: 1}, 1),
		app.BootstrapInput{Email: email, Password: testPassword, Org: org, Project: project}, e.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	return res
}
