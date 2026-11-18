package api_test

import (
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/mucan54/izlek/control/config"
)

func TestQueryProxyForwardsWithInternalIdentity(t *testing.T) {
	e := newEnv(t)
	owner, org := e.setup()
	p := e.project(owner, org, "shop")
	other := e.project(owner, org, "other")
	base := fmt.Sprintf("/api/projects/%d/q/", p.ID)
	viewer := e.member(owner, org, "viewer")

	e.upstream.set(func(w http.ResponseWriter, r *http.Request, body []byte) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-LA-Internal", "upstream must not be able to leak this back")
		w.Header().Set("Set-Cookie", "evil=1")
		_, _ = w.Write([]byte(`{"buckets":[],"series":[],"meta":{"elapsed_ms":1}}`))
	})

	// A hostile client tries to pick another project and to present its own
	// internal credential, in several spellings.
	c := *viewer
	c.headers = map[string]string{
		"X-LA-Project":  fmt.Sprint(other.ID),
		"x-la-internal": "guessed-secret",
		"X-La-Whatever": "1",
		"X-LA-CSRF":     viewer.csrf,
		"Cookie2":       "x",
		"Accept":        "application/json",
	}
	reqBody := `{"from":"2026-11-01","to":"2026-11-18","interval":"day","series":[{"event":"purchase","agg":"count"}],"project":999}`
	res := c.post(base+"trends", reqBody).want(200)
	if string(res.Body) != `{"buckets":[],"series":[],"meta":{"elapsed_ms":1}}` || res.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("response not passed through: %q (%s)", res.Body, res.Header.Get("Content-Type"))
	}
	if res.Header.Get("Set-Cookie") != "" || res.Header.Get("X-LA-Internal") != "" {
		t.Fatalf("upstream headers leaked to the client: %v", res.Header)
	}
	if res.Header.Get("Cache-Control") != "no-store" || res.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("proxied response headers: %v", res.Header)
	}

	got := e.upstream.last()
	if got.Method != "POST" || got.Path != "/q/trends" || string(got.Body) != reqBody {
		t.Fatalf("upstream saw %s %s %q", got.Method, got.Path, got.Body)
	}
	if v := got.Header.Values("X-La-Project"); len(v) != 1 || v[0] != fmt.Sprint(p.ID) {
		t.Fatalf("X-LA-Project upstream = %v, want exactly [%d] (the client-supplied value must not win)", v, p.ID)
	}
	if v := got.Header.Values("X-La-Internal"); len(v) != 1 || v[0] != testSecret {
		t.Fatalf("X-LA-Internal upstream = %v", v)
	}
	for name := range got.Header {
		lower := strings.ToLower(name)
		if strings.HasPrefix(lower, "x-la-") && lower != "x-la-internal" && lower != "x-la-project" {
			t.Errorf("client header %s reached the query service", name)
		}
		if lower == "cookie" || lower == "authorization" || lower == "cookie2" {
			t.Errorf("credential header %s reached the query service", name)
		}
	}
	if got.Header.Get("Content-Type") != "application/json" || got.Header.Get("Accept") != "application/json" {
		t.Errorf("content negotiation headers not forwarded: %v", got.Header)
	}
	if got.Header.Get("X-Request-Id") == "" || got.Header.Get("X-Request-Id") != res.Header.Get("X-Request-Id") {
		t.Errorf("request id not carried end to end: %q vs %q", got.Header.Get("X-Request-Id"), res.Header.Get("X-Request-Id"))
	}

	// Every documented endpoint is routed; nothing else is.
	for _, ep := range []string{"trends", "funnel", "retention", "events", "sql", "schema"} {
		before := e.upstream.count()
		viewer.post(base+ep, map[string]any{}).want(200)
		if e.upstream.count() != before+1 || e.upstream.last().Path != "/q/"+ep {
			t.Errorf("%s not forwarded to /q/%s", ep, ep)
		}
	}
	before := e.upstream.count()
	viewer.post(base+"admin", map[string]any{}).wantError(404, "not_found")
	viewer.post(base+"..%2Fmetrics", map[string]any{}).wantError(404, "not_found")
	viewer.post(base+"replay", map[string]any{}).wantError(405, "method_not_allowed")
	viewer.get(base+"trends").wantError(405, "method_not_allowed")
	viewer.post(base+"trends", strings.Repeat("x", 1<<20+1)).wantError(413, "too_large")
	if e.upstream.count() != before {
		t.Fatal("a refused request reached the query service")
	}

	// A secret key of the project may use the proxy too; its own
	// Authorization header stays behind.
	key, _ := e.secretKey(owner, p.ID, "admin")
	key.post(base+"sql", map[string]any{"sql": "SELECT 1"}).want(200)
	if got := e.upstream.last(); got.Header.Get("Authorization") != "" || got.Header.Get("X-La-Project") != fmt.Sprint(p.ID) {
		t.Fatalf("key request upstream headers: %v", got.Header)
	}
}

func TestQueryProxyErrorMapping(t *testing.T) {
	e := newEnv(t)
	owner, org := e.setup()
	p := e.project(owner, org, "shop")
	path := fmt.Sprintf("/api/projects/%d/q/trends", p.ID)

	// Query-level errors pass through unchanged: they are answers.
	for _, status := range []int{400, 403, 408, 422, 429, 500, 503} {
		body := fmt.Sprintf(`{"error":{"code":"upstream_%d","message":"from the query service"}}`, status)
		e.upstream.set(func(w http.ResponseWriter, r *http.Request, _ []byte) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		})
		res := owner.post(path, map[string]any{}).want(status)
		if string(res.Body) != body || res.Header.Get("Retry-After") != "7" {
			t.Errorf("status %d: body %q, Retry-After %q", status, res.Body, res.Header.Get("Retry-After"))
		}
	}

	// 401 from the query service is about the control plane's credential.
	// The UI treats 401 as "session lost", so it must not get through.
	e.upstream.set(func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"error":{"code":"unauthorized","message":"invalid internal credential"}}`))
	})
	owner.post(path, map[string]any{}).wantError(502, "upstream_auth")
	owner.get("/api/me").want(200)

	// The query service does not answer in time.
	slow := newEnv(t, func(c *config.Config) { c.QueryTimeout = 50 * time.Millisecond })
	so, sorg := slow.setup()
	sp := slow.project(so, sorg, "shop")
	release := make(chan struct{})
	slow.upstream.set(func(w http.ResponseWriter, r *http.Request, _ []byte) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	start := time.Now()
	so.post(fmt.Sprintf("/api/projects/%d/q/funnel", sp.ID), map[string]any{}).wantError(504, "upstream_timeout")
	close(release)
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("timeout took %v", took)
	}

	// The query service is down.
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	down := newEnv(t, func(c *config.Config) { c.QueryURL = deadURL })
	do, dorg := down.setup()
	dp := down.project(do, dorg, "shop")
	do.post(fmt.Sprintf("/api/projects/%d/q/trends", dp.ID), map[string]any{}).wantError(502, "upstream_unavailable")
	do.get(fmt.Sprintf("/api/projects/%d/q/replay?key=replay/p=%d/d=2026-11-18/s/000001.bin.gz", dp.ID, dp.ID)).wantError(502, "upstream_unavailable")
	// The rest of the API is unaffected.
	do.get(fmt.Sprintf("/api/projects/%d", dp.ID)).want(200)

	// No internal secret configured: the proxy is off, loudly.
	off := newEnv(t, func(c *config.Config) { c.InternalSecret = "" })
	oo, oorg := off.setup()
	op := off.project(oo, oorg, "shop")
	oo.post(fmt.Sprintf("/api/projects/%d/q/trends", op.ID), map[string]any{}).wantError(503, "query_not_configured")
	if off.upstream.count() != 0 {
		t.Fatal("a request was forwarded without an internal secret")
	}

	// Redirects are not followed (the secret must not travel elsewhere).
	e.upstream.set(func(w http.ResponseWriter, r *http.Request, _ []byte) {
		http.Redirect(w, r, "http://example.invalid/steal", http.StatusFound)
	})
	if res := owner.post(path, map[string]any{}); res.Status != 302 {
		t.Fatalf("redirect: status %d", res.Status)
	}
}

func TestReplayProxyStreamsCompressedBodiesUnchanged(t *testing.T) {
	e := newEnv(t)
	owner, org := e.setup()
	p := e.project(owner, org, "shop")
	other := e.project(owner, org, "other")
	viewer := e.member(owner, org, "viewer")

	raw := make([]byte, 300_000)
	_, _ = rand.Read(raw[:1000])
	copy(raw[1000:], bytes.Repeat([]byte("LAR1 replay chunk payload "), 11500))
	var gz bytes.Buffer
	gw := gzip.NewWriter(&gz)
	_, _ = gw.Write(raw)
	_ = gw.Close()
	zw, _ := zstd.NewWriter(nil)
	zs := zw.EncodeAll(raw, nil)

	session := strings.Repeat("ab", 16)
	for _, tc := range []struct {
		name, suffix, encoding string
		body                   []byte
	}{{"gzip", ".bin.gz", "gzip", gz.Bytes()}, {"zstd", ".bin.zst", "zstd", zs}} {
		t.Run(tc.name, func(t *testing.T) {
			key := fmt.Sprintf("replay/p=%d/d=2026-11-18/%s/000001%s", p.ID, session, tc.suffix)
			e.upstream.set(func(w http.ResponseWriter, r *http.Request, _ []byte) {
				if r.URL.Query().Get("key") != key {
					w.WriteHeader(400)
					return
				}
				w.Header().Set("Content-Type", "application/octet-stream")
				w.Header().Set("Content-Encoding", tc.encoding)
				w.WriteHeader(200)
				// Several writes with flushes in between: a stream, not one buffer.
				for off := 0; off < len(tc.body); off += 4096 {
					end := min(off+4096, len(tc.body))
					_, _ = w.Write(tc.body[off:end])
					w.(http.Flusher).Flush()
				}
			})
			c := *viewer
			// A browser advertises encodings; the proxy must neither let the
			// Go transport decode the body nor re-encode it.
			c.headers = map[string]string{"Accept-Encoding": "gzip, deflate, br, zstd", "X-LA-Project": fmt.Sprint(other.ID)}
			res := c.get(fmt.Sprintf("/api/projects/%d/q/replay?key=%s", p.ID, key)).want(200)
			if !bytes.Equal(res.Body, tc.body) {
				t.Fatalf("body changed in transit: got %d bytes, want %d", len(res.Body), len(tc.body))
			}
			if res.Header.Get("Content-Encoding") != tc.encoding || res.Header.Get("Content-Type") != "application/octet-stream" {
				t.Fatalf("headers: %v", res.Header)
			}
			if cc := res.Header.Get("Cache-Control"); !strings.Contains(cc, "private") {
				t.Errorf("replay Cache-Control %q", cc)
			}
			got := e.upstream.last()
			if got.Method != "GET" || got.Path != "/q/replay" || got.Header.Get("X-La-Project") != fmt.Sprint(p.ID) || got.Header.Get("X-La-Internal") != testSecret {
				t.Fatalf("upstream request: %s %s %v", got.Method, got.Path, got.Header)
			}
			if got.Header.Get("Accept-Encoding") != "gzip, deflate, br, zstd" {
				t.Errorf("Accept-Encoding upstream: %q", got.Header.Get("Accept-Encoding"))
			}
		})
	}

	// Keys outside the project's own prefix never leave the control plane.
	before := e.upstream.count()
	for _, key := range []string{
		fmt.Sprintf("replay/p=%d/d=2026-11-18/%s/000001.bin.gz", other.ID, session),
		fmt.Sprintf("replay/p=%d0/d=2026-11-18/%s/000001.bin.gz", p.ID, session),
		"control/keys.json", fmt.Sprintf("data/p=%d/x", p.ID), "/etc/passwd",
	} {
		viewer.get(fmt.Sprintf("/api/projects/%d/q/replay?key=%s", p.ID, key)).wantError(403, "forbidden")
	}
	viewer.get(fmt.Sprintf("/api/projects/%d/q/replay", p.ID)).wantError(400, "bad_request")
	if e.upstream.count() != before {
		t.Fatal("a foreign replay key was forwarded")
	}
	// Special characters in the key survive the hop.
	odd := fmt.Sprintf("replay/p=%d/d=2026-11-18/a b&c=d/%%41.bin.gz", p.ID)
	e.upstream.set(func(w http.ResponseWriter, r *http.Request, _ []byte) {
		_, _ = w.Write([]byte(r.URL.Query().Get("key")))
	})
	q := "key=" + strings.NewReplacer(" ", "%20", "&", "%26", "=", "%3D", "%", "%25").Replace(odd)
	if res := viewer.get(fmt.Sprintf("/api/projects/%d/q/replay?%s", p.ID, q)).want(200); string(res.Body) != odd {
		t.Fatalf("key arrived as %q, want %q", res.Body, odd)
	}
}

// Over a real connection: an upstream that dies mid-body must not look like
// a complete response to the client.
func TestReplayProxyAbortsOnTruncatedUpstream(t *testing.T) {
	e := newEnv(t)
	owner, org := e.setup()
	p := e.project(owner, org, "shop")
	e.upstream.set(func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Length", "100000")
		w.WriteHeader(200)
		_, _ = w.Write(bytes.Repeat([]byte("x"), 1000))
		w.(http.Flusher).Flush()
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			conn.Close()
		}
	})
	front := httptest.NewServer(e.app.Server)
	defer front.Close()
	req, _ := http.NewRequest("GET", fmt.Sprintf("%s/api/projects/%d/q/replay?key=replay/p=%d/d=2026-11-18/s/000001.bin.gz", front.URL, p.ID, p.ID), nil)
	req.AddCookie(&http.Cookie{Name: "la_session", Value: owner.cookie})
	res, err := front.Client().Do(req)
	if err != nil {
		return // the connection was torn down before the headers: acceptable
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err == nil && len(body) != 100000 {
		t.Fatalf("truncated upstream body (%d bytes) was delivered as a clean response", len(body))
	}
}

func TestStaticUI(t *testing.T) {
	dir := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(dir, "assets"), 0o755))
	must(os.WriteFile(filepath.Join(dir, "index.html"), []byte("<!doctype html><title>izlek</title>"), 0o644))
	must(os.WriteFile(filepath.Join(dir, "assets", "app-3HWNSSG6.js"), []byte("console.log(1)"), 0o644))
	must(os.WriteFile(filepath.Join(dir, "assets", "app-5OWILGBP.css"), []byte("body{}"), 0o644))
	must(os.WriteFile(filepath.Join(dir, "favicon.svg"), []byte("<svg/>"), 0o644))
	must(os.WriteFile(filepath.Join(dir, ".env"), []byte("SECRET=1"), 0o644))
	outside := filepath.Join(t.TempDir(), "secret.txt")
	must(os.WriteFile(outside, []byte("outside the UI directory"), 0o644))
	must(os.Symlink(outside, filepath.Join(dir, "assets", "link.txt")))

	e := newEnv(t, func(c *config.Config) { c.UIDir = dir })
	c := e.anon()
	const immutable = "public, max-age=31536000, immutable"

	index := c.get("/").want(200)
	if !strings.Contains(string(index.Body), "<title>izlek</title>") || index.Header.Get("Content-Type") != "text/html; charset=utf-8" || index.Header.Get("Cache-Control") != "no-cache" {
		t.Fatalf("index: %q %v", index.Body, index.Header)
	}
	if csp := index.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") || !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Errorf("UI Content-Security-Policy %q", csp)
	}
	if index.Header.Get("X-Content-Type-Options") != "nosniff" || index.Header.Get("Referrer-Policy") != "same-origin" || index.Header.Get("Strict-Transport-Security") == "" {
		t.Errorf("UI security headers: %v", index.Header)
	}
	// Client-side routes fall back to the shell, never cached.
	for _, route := range []string{"/login", "/projects/1/flags/new_checkout", "/p/1/insights/3?tab=chart", "/index.html", "/a/b/c/d/e", "/flags/v1.2"} {
		res := c.get(route).want(200)
		if !bytes.Equal(res.Body, index.Body) || res.Header.Get("Cache-Control") != "no-cache" {
			t.Errorf("%s: not the SPA shell (%q, %q)", route, res.Body, res.Header.Get("Cache-Control"))
		}
	}
	// Hashed assets are cacheable forever and typed correctly.
	js := c.get("/assets/app-3HWNSSG6.js").want(200)
	if string(js.Body) != "console.log(1)" || js.Header.Get("Cache-Control") != immutable || !strings.HasPrefix(js.Header.Get("Content-Type"), "text/javascript") {
		t.Fatalf("js asset: %v", js.Header)
	}
	css := c.get("/assets/app-5OWILGBP.css").want(200)
	if css.Header.Get("Cache-Control") != immutable || !strings.HasPrefix(css.Header.Get("Content-Type"), "text/css") {
		t.Fatalf("css asset: %v", css.Header)
	}
	// Other real files are served but revalidated.
	if svg := c.get("/favicon.svg").want(200); svg.Header.Get("Cache-Control") != "no-cache" || svg.Header.Get("Content-Type") != "image/svg+xml" {
		t.Fatalf("favicon: %v", svg.Header)
	}
	// A missing asset is a 404, not HTML pretending to be JavaScript.
	c.get("/assets/app-OLDHASH.js").wantError(404, "not_found")
	// Conditional requests work (deploys are picked up by revalidation).
	cond := *c
	cond.headers = map[string]string{"If-Modified-Since": index.Header.Get("Last-Modified")}
	if index.Header.Get("Last-Modified") == "" {
		t.Fatal("no Last-Modified on index.html")
	}
	cond.get("/").want(304)
	if head := c.do("HEAD", "/assets/app-3HWNSSG6.js", nil).want(200); len(head.Body) != 0 || head.Header.Get("Content-Length") != "14" {
		t.Errorf("HEAD: %d bytes, Content-Length %q", len(head.Body), head.Header.Get("Content-Length"))
	}

	// Nothing outside the directory, no dotfiles, no symlink escapes.
	for _, p := range []string{"/.env", "/assets/../.env", "/assets/.hidden"} {
		if res := c.get(p); res.Status == 200 && strings.Contains(string(res.Body), "SECRET") {
			t.Errorf("%s served a dotfile", p)
		}
	}
	c.get("/.env").wantError(404, "not_found")
	for _, p := range []string{"/../secret.txt", "/assets/../../secret.txt", "/%2e%2e/secret.txt", "/assets/link.txt", "/..%2f..%2fetc/passwd"} {
		req := httptest.NewRequest("GET", "/", nil)
		req.URL.Path = strings.ReplaceAll(strings.ReplaceAll(p, "%2e", "."), "%2f", "/")
		rec := httptest.NewRecorder()
		e.app.Server.ServeHTTP(rec, req)
		if strings.Contains(rec.Body.String(), "outside the UI directory") || strings.Contains(rec.Body.String(), "root:") {
			t.Errorf("%s escaped the UI directory", p)
		}
	}

	// /api/ never falls back to the shell, and only GET/HEAD reach the UI.
	c.get("/api/unknown").wantError(404, "not_found")
	c.get("/api").wantError(404, "not_found")
	c.get("/api/").wantError(404, "not_found")
	c.post("/login", map[string]any{}).wantError(405, "method_not_allowed")
	if res := c.get("/healthz").want(200); string(res.Body) != "ok\n" {
		t.Error("/healthz shadowed by the UI")
	}

	// A UI directory without index.html says so instead of serving nothing.
	empty := newEnv(t, func(c *config.Config) { c.UIDir = t.TempDir() })
	if res := empty.anon().get("/"); res.Status != 503 {
		t.Fatalf("missing index.html: %d", res.Status)
	}
}
