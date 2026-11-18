package api_test

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mucan54/izlek/control/auth"
	"github.com/mucan54/izlek/control/config"
	"github.com/mucan54/izlek/control/db"
)

func TestSetupOnlyWhileEmpty(t *testing.T) {
	e := newEnv(t)
	c := e.anon()
	if got := c.get("/api/setup").want(200).object(); got["needs_setup"] != true {
		t.Fatalf("needs_setup on an empty database: %v", got)
	}
	// Everything else needs a session.
	c.get("/api/me").wantError(401, "unauthorized")
	c.get("/api/projects").wantError(401, "unauthorized")

	body := func(email, password, org string) map[string]any {
		return map[string]any{"email": email, "password": password, "org_name": org}
	}
	c.post("/api/setup", body("not-an-email", testPassword, "Acme")).wantError(400, "invalid_email")
	c.post("/api/setup", body("owner@example.com", "short", "Acme")).wantError(400, "weak_password")
	c.post("/api/setup", body("owner@example.com", testPassword, "  ")).wantError(400, "bad_request")
	if got := c.get("/api/setup").object(); got["needs_setup"] != true {
		t.Fatal("a rejected setup created something")
	}

	res := c.post("/api/setup", body(" Owner@Example.COM ", testPassword, "Acme")).want(200)
	if c.cookie == "" {
		t.Fatal("setup did not open a session")
	}
	_ = res
	m := c.me()
	if m.User.Email != "owner@example.com" || m.User.TOTPEnabled || len(m.Orgs) != 1 || m.Orgs[0].Name != "Acme" || m.Orgs[0].Role != "owner" || m.CSRF == "" {
		t.Fatalf("me after setup: %+v", m)
	}
	if got := c.get("/api/setup").object(); got["needs_setup"] != false {
		t.Fatalf("needs_setup after setup: %v", got)
	}
	// Second setup: refused, whoever asks.
	e.anon().post("/api/setup", body("evil@example.com", testPassword, "Evil")).wantError(409, "already_set_up")
	c.post("/api/setup", body("evil@example.com", testPassword, "Evil")).wantError(409, "already_set_up")
	if _, err := db.UserByEmail(context.Background(), e.db.R(), "evil@example.com"); err == nil {
		t.Fatal("second setup created a user")
	}
}

func TestSessionCookieAttributes(t *testing.T) {
	for _, secure := range []bool{true, false} {
		t.Run(fmt.Sprintf("secure=%v", secure), func(t *testing.T) {
			e := newEnv(t, func(c *config.Config) { c.CookieSecure = secure })
			e.setup()
			c := e.anon()
			res := c.post("/api/auth/login", map[string]any{"email": "owner@example.com", "password": testPassword}).want(200)
			raw := res.Header.Get("Set-Cookie")
			for _, attr := range []string{"la_session=", "HttpOnly", "SameSite=Lax", "Path=/", "Max-Age=2592000"} {
				if !strings.Contains(raw, attr) {
					t.Errorf("Set-Cookie %q lacks %q", raw, attr)
				}
			}
			if strings.Contains(raw, "Secure") != secure {
				t.Errorf("Secure attribute: %q with LA_COOKIE_SECURE=%v", raw, secure)
			}
			// 256 random bits, and only their SHA-256 in the database.
			if len(c.cookie) != 64 {
				t.Fatalf("cookie value %q: want 64 hex characters", c.cookie)
			}
			var n int
			if err := e.db.R().QueryRow(`SELECT COUNT(*) FROM sessions WHERE id_hash = ?`, c.cookie).Scan(&n); err != nil || n != 0 {
				t.Fatalf("the raw cookie value is stored in the database (%d, %v)", n, err)
			}
			if err := e.db.R().QueryRow(`SELECT COUNT(*) FROM sessions WHERE id_hash = ?`, auth.HashSecret(c.cookie)).Scan(&n); err != nil || n != 1 {
				t.Fatalf("session not stored by hash (%d, %v)", n, err)
			}
			if hsts := res.Header.Get("Strict-Transport-Security"); (hsts != "") != secure {
				t.Errorf("Strict-Transport-Security %q with LA_COOKIE_SECURE=%v", hsts, secure)
			}
		})
	}
}

func TestLoginLogout(t *testing.T) {
	e := newEnv(t)
	e.setup()
	c := e.anon()
	creds := func(email, password string) map[string]any { return map[string]any{"email": email, "password": password} }

	c.post("/api/auth/login", creds("owner@example.com", "wrong password!")).wantError(401, "invalid_credentials")
	c.post("/api/auth/login", creds("nobody@example.com", testPassword)).wantError(401, "invalid_credentials")
	c.post("/api/auth/login", creds("", "")).wantError(401, "invalid_credentials")
	if c.cookie != "" {
		t.Fatal("a failed login set a cookie")
	}
	// E-mail matching ignores case and surrounding space.
	c.post("/api/auth/login", creds("  OWNER@example.com ", testPassword)).want(200)
	m := c.me()

	// Logout needs the CSRF token like every other cookie mutation…
	old := c.cookie
	c.noCSRF = true
	c.post("/api/auth/logout", nil).wantError(403, "csrf")
	c.noCSRF = false
	c.get("/api/me").want(200)
	// …and ends the session on the server, not just in the browser.
	res := c.post("/api/auth/logout", nil).want(200)
	if !strings.Contains(res.Header.Get("Set-Cookie"), "Max-Age=0") {
		t.Errorf("logout does not clear the cookie: %q", res.Header.Get("Set-Cookie"))
	}
	replay := e.anon()
	replay.cookie, replay.csrf = old, m.CSRF
	replay.get("/api/me").wantError(401, "unauthorized")
	replay.post("/api/auth/logout", nil).wantError(401, "unauthorized")

	// Garbage cookies are just "not logged in".
	for _, v := range []string{"x", strings.Repeat("0", 64), strings.Repeat("z", 64), strings.Repeat("0", 200)} {
		bad := e.anon()
		bad.cookie = v
		bad.get("/api/me").wantError(401, "unauthorized")
	}
}

func TestLoginRateLimit(t *testing.T) {
	e := newEnv(t)
	e.setup()
	attempt := func(ip, email, password string) *response {
		c := e.anon()
		c.ip = ip
		return c.post("/api/auth/login", map[string]any{"email": email, "password": password})
	}
	const ip = "203.0.113.5"
	for i := 0; i < 5; i++ {
		attempt(ip, "owner@example.com", "wrong password!").wantError(401, "invalid_credentials")
		e.clock.Advance(time.Second)
	}
	// The sixth attempt within a minute is refused — even with the right password.
	res := attempt(ip, "owner@example.com", testPassword).wantError(429, "rate_limited")
	retry, err := strconv.Atoi(res.Header.Get("Retry-After"))
	if err != nil || retry < 1 || retry > 60 {
		t.Fatalf("Retry-After %q", res.Header.Get("Retry-After"))
	}
	// The limit is per IP + e-mail: another address or another account is unaffected.
	attempt("203.0.113.6", "owner@example.com", testPassword).want(200)
	attempt(ip, "someone-else@example.com", testPassword).wantError(401, "invalid_credentials")
	// Case games do not create a new bucket.
	attempt(ip, "OWNER@example.com", testPassword).wantError(429, "rate_limited")

	// The window slides.
	e.clock.Advance(time.Duration(retry) * time.Second)
	attempt(ip, "owner@example.com", testPassword).want(200)

	// Successful logins are not counted.
	for i := 0; i < 12; i++ {
		attempt("203.0.113.9", "owner@example.com", testPassword).want(200)
	}

	// One address spraying many accounts hits the per-IP ceiling.
	e.clock.Advance(2 * time.Minute)
	blocked := false
	for i := 0; i < 60; i++ {
		r := attempt("203.0.113.77", fmt.Sprintf("victim-%d@example.com", i), "wrong password!")
		if r.Status == 429 {
			blocked = true
			if i < 50 {
				t.Fatalf("per-IP limit hit after only %d attempts", i)
			}
			break
		}
		r.wantError(401, "invalid_credentials")
	}
	if !blocked {
		t.Fatal("60 failed logins from one address were all processed")
	}
}

func TestTrustedProxyAddressForRateLimit(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.TrustedProxies = 1 })
	e.setup()
	attempt := func(xff, password string) *response {
		c := e.anon()
		c.ip = "10.0.0.1" // the load balancer
		c.headers = map[string]string{"X-Forwarded-For": xff}
		return c.post("/api/auth/login", map[string]any{"email": "owner@example.com", "password": password})
	}
	for i := 0; i < 5; i++ {
		// The left entry is client-controlled and changes every time; the
		// right one (added by the trusted proxy) is what counts.
		attempt(fmt.Sprintf("1.2.3.%d, 203.0.113.50", i), "wrong password!").wantError(401, "invalid_credentials")
	}
	attempt("9.9.9.9, 203.0.113.50", testPassword).wantError(429, "rate_limited")
	attempt("203.0.113.50, 203.0.113.51", testPassword).want(200)
}

func TestCSRF(t *testing.T) {
	e := newEnv(t)
	owner, org := e.setup()
	p := e.project(owner, org, "shop")
	base := fmt.Sprintf("/api/projects/%d", p.ID)
	tokenBody := map[string]any{"name": "web"}

	// Cookie + correct token: fine.
	owner.post(base+"/tokens", tokenBody).want(200)

	// Cookie without / with a wrong token: every mutating method is refused.
	for _, mode := range []string{"missing", "wrong", "other session"} {
		c := *owner
		switch mode {
		case "missing":
			c.noCSRF = true
		case "wrong":
			c.csrf = strings.Repeat("0", 32)
		case "other session":
			c.csrf = e.login("owner@example.com", testPassword).csrf
		}
		c.post(base+"/tokens", tokenBody).wantError(403, "csrf")
		c.patch(base, map[string]any{"name": "x"}).wantError(403, "csrf")
		c.put(base+"/flags/f", map[string]any{}).wantError(403, "csrf")
		c.del(base).wantError(403, "csrf")
		c.post("/api/me/password", map[string]any{"current": testPassword, "new": "another password"}).wantError(403, "csrf")
		c.post(base+"/q/trends", map[string]any{}).wantError(403, "csrf")
		// Reads never need it.
		c.get(base).want(200)
		c.get(base + "/tokens").want(200)
	}
	if n := len(owner.get(base + "/tokens").list()); n != 2 {
		t.Fatalf("%d tokens; refused requests must not have created any", n-1)
	}

	// A browser-declared cross-site request is refused even with the token.
	cross := *owner
	cross.headers = map[string]string{"Sec-Fetch-Site": "cross-site"}
	cross.post(base+"/tokens", tokenBody).wantError(403, "csrf")
	// …including the unauthenticated login (login CSRF).
	evil := e.anon()
	evil.headers = map[string]string{"Sec-Fetch-Site": "cross-site"}
	evil.post("/api/auth/login", map[string]any{"email": "owner@example.com", "password": testPassword}).wantError(403, "csrf")

	// Cookie-authenticated bodies must be application/json: a cross-origin
	// HTML form cannot send that without a preflight.
	form := *owner
	form.rawContentType = "text/plain"
	form.post(base+"/tokens", `{"name":"form"}`).wantError(415, "unsupported_media_type")
	form.rawContentType = "-"
	form.post(base+"/tokens", `{"name":"form"}`).wantError(415, "unsupported_media_type")

	// sk_ requests carry no cookie and need no CSRF token or content type.
	key, _ := e.secretKey(owner, p.ID, "admin")
	key.post(base+"/tokens", tokenBody).want(200)
	key.rawContentType = "-"
	key.post(base+"/tokens", `{"name":"curl"}`).want(200)
	// A stray CSRF header or cookie next to a valid key changes nothing.
	key.csrf, key.cookie = "bogus", strings.Repeat("0", 64)
	key.post(base+"/tokens", tokenBody).want(200)
}

func TestSessionExpiryIsSliding(t *testing.T) {
	e := newEnv(t)
	owner, _ := e.setup()
	// Used every 20 days, the session lives on past 30 days from creation…
	for i := 0; i < 4; i++ {
		e.clock.Advance(20 * 24 * time.Hour)
		res := owner.get("/api/me").want(200)
		if !strings.Contains(res.Header.Get("Set-Cookie"), "Max-Age=2592000") {
			t.Fatalf("round %d: the cookie's lifetime was not renewed (%q)", i, res.Header.Get("Set-Cookie"))
		}
	}
	// …a request soon after the last one does not rewrite anything…
	e.clock.Advance(time.Minute)
	if res := owner.get("/api/me").want(200); res.Header.Get("Set-Cookie") != "" {
		t.Errorf("cookie re-sent after one minute: %q", res.Header.Get("Set-Cookie"))
	}
	// …and 30 idle days end it.
	e.clock.Advance(30*24*time.Hour + time.Minute)
	owner.get("/api/me").wantError(401, "unauthorized")
	owner.post("/api/auth/logout", nil).wantError(401, "unauthorized")

	// Expired rows are removed by the clean-up.
	var n int64
	err := e.db.Write(context.Background(), func(tx *sql.Tx) error {
		var err error
		n, err = db.DeleteExpiredSessions(context.Background(), tx, e.clock.Now().UnixMilli())
		return err
	})
	if err != nil || n != 1 {
		t.Fatalf("clean-up removed %d sessions, %v", n, err)
	}
}

func TestPasswordChangeInvalidatesOtherSessions(t *testing.T) {
	e := newEnv(t)
	here, _ := e.setup()
	elsewhere := e.login("owner@example.com", testPassword)
	elsewhere.get("/api/me").want(200)

	const next = "a brand new password"
	here.post("/api/me/password", map[string]any{"current": "not my password", "new": next}).wantError(403, "invalid_credentials")
	here.post("/api/me/password", map[string]any{"current": testPassword, "new": "short"}).wantError(400, "weak_password")
	elsewhere.get("/api/me").want(200) // nothing changed yet

	here.post("/api/me/password", map[string]any{"current": testPassword, "new": next}).want(200)
	here.get("/api/me").want(200)                          // this session survives
	elsewhere.get("/api/me").wantError(401, "unauthorized") // the other one is gone

	e.anon().post("/api/auth/login", map[string]any{"email": "owner@example.com", "password": testPassword}).wantError(401, "invalid_credentials")
	e.login("owner@example.com", next)

	// The stored hash is argon2id, never the password.
	u, err := db.UserByEmail(context.Background(), e.db.R(), "owner@example.com")
	if err != nil || !strings.HasPrefix(u.PasswordHash, "$argon2id$") || strings.Contains(u.PasswordHash, next) {
		t.Fatalf("stored hash %q, %v", u.PasswordHash, err)
	}

	// Guessing the current password through a stolen session is throttled.
	for i := 0; i < 5; i++ {
		here.post("/api/me/password", map[string]any{"current": "guess number " + strconv.Itoa(i), "new": "does not matter!"}).wantError(403, "invalid_credentials")
	}
	here.post("/api/me/password", map[string]any{"current": next, "new": "does not matter!"}).wantError(429, "rate_limited")
}

func TestTOTPLifecycle(t *testing.T) {
	e := newEnv(t)
	owner, _ := e.setup()
	code := func(secret string, offset int64) string {
		c, err := auth.TOTPCode(secret, auth.TOTPStep(e.clock.Now())+offset)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	login := func(extra map[string]any) (*client, *response) {
		c := e.anon()
		body := map[string]any{"email": "owner@example.com", "password": testPassword}
		for k, v := range extra {
			body[k] = v
		}
		return c, c.post("/api/auth/login", body)
	}

	owner.post("/api/me/totp/enable", map[string]any{"code": "123456"}).wantError(400, "totp_not_started")

	var begin struct {
		Secret string `json:"secret"`
		URL    string `json:"otpauth_url"`
	}
	owner.post("/api/me/totp/begin", nil).want(200).json(&begin)
	if len(begin.Secret) != 32 || !strings.HasPrefix(begin.URL, "otpauth://totp/") || !strings.Contains(begin.URL, "secret="+begin.Secret) ||
		!strings.Contains(begin.URL, "algorithm=SHA1") || !strings.Contains(begin.URL, "digits=6") || !strings.Contains(begin.URL, "period=30") {
		t.Fatalf("begin: %+v", begin)
	}
	// Not active until confirmed: login still works without a code.
	if _, res := login(nil); res.Status != 200 {
		t.Fatalf("login before TOTP is confirmed: %d %s", res.Status, res.Body)
	}
	other := e.login("owner@example.com", testPassword)

	owner.post("/api/me/totp/enable", map[string]any{"code": "000000"}).wantError(400, "invalid_totp")
	owner.post("/api/me/totp/enable", map[string]any{"code": code(begin.Secret, 0)}).want(200)
	if !owner.me().User.TOTPEnabled {
		t.Fatal("totp_enabled is false after enabling")
	}
	// Sessions opened without the second factor are closed.
	other.get("/api/me").wantError(401, "unauthorized")
	owner.post("/api/me/totp/begin", nil).wantError(409, "totp_already_enabled")

	// Login now needs the code.
	_, res := login(nil)
	res.wantError(401, "totp_required")
	_, res = login(map[string]any{"totp": ""})
	res.wantError(401, "totp_required")
	_, res = login(map[string]any{"totp": "000000"})
	res.wantError(401, "invalid_totp")
	// The code that confirmed the enrolment cannot be replayed.
	_, res = login(map[string]any{"totp": code(begin.Secret, 0)})
	res.wantError(401, "invalid_totp")
	// A wrong password never reveals that TOTP is on.
	c := e.anon()
	c.post("/api/auth/login", map[string]any{"email": "owner@example.com", "password": "wrong password!", "totp": code(begin.Secret, 0)}).wantError(401, "invalid_credentials")

	e.clock.Advance(2 * time.Minute) // also clears the failed attempts above
	good := code(begin.Secret, 0)
	logged, res := login(map[string]any{"totp": good})
	res.want(200)
	logged.me()
	// Used once, used up.
	_, res = login(map[string]any{"totp": good})
	res.wantError(401, "invalid_totp")
	// One period of clock skew is tolerated (and a number is accepted for a code).
	e.clock.Advance(90 * time.Second)
	n, _ := strconv.Atoi(code(begin.Secret, 1))
	_, res = login(map[string]any{"totp": n})
	res.want(200)
	e.clock.Advance(5 * time.Minute)
	_, res = login(map[string]any{"totp": code(begin.Secret, 3)})
	res.wantError(401, "invalid_totp")

	// Disabling needs the password.
	logged.post("/api/me/totp/disable", map[string]any{"password": "wrong password!"}).wantError(403, "invalid_credentials")
	if !logged.me().User.TOTPEnabled {
		t.Fatal("TOTP disabled with a wrong password")
	}
	logged.post("/api/me/totp/disable", map[string]any{"password": testPassword}).want(200)
	if logged.me().User.TOTPEnabled {
		t.Fatal("totp_enabled still true")
	}
	e.clock.Advance(2 * time.Minute)
	if _, res := login(nil); res.Status != 200 {
		t.Fatalf("login after disabling TOTP: %d %s", res.Status, res.Body)
	}
	// The secret is gone from the database.
	u, err := db.UserByEmail(context.Background(), e.db.R(), "owner@example.com")
	if err != nil || u.TOTPSecret != "" || u.TOTPPending != "" {
		t.Fatalf("TOTP secrets left behind: %+v, %v", u, err)
	}
}

func TestSecurityHeaders(t *testing.T) {
	e := newEnv(t)
	owner, _ := e.setup()
	check := func(name string, h http.Header) {
		t.Helper()
		if csp := h.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") && !strings.Contains(csp, "default-src 'self'") {
			t.Errorf("%s: Content-Security-Policy %q", name, csp)
		}
		if strings.Contains(h.Get("Content-Security-Policy"), "unsafe") || strings.Contains(h.Get("Content-Security-Policy"), "*") {
			t.Errorf("%s: Content-Security-Policy is not own-origin only: %q", name, h.Get("Content-Security-Policy"))
		}
		if h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Referrer-Policy") != "same-origin" {
			t.Errorf("%s: nosniff / referrer policy missing: %v", name, h)
		}
		if !strings.HasPrefix(h.Get("Strict-Transport-Security"), "max-age=") {
			t.Errorf("%s: no HSTS behind TLS", name)
		}
		if h.Get("X-Request-Id") == "" {
			t.Errorf("%s: no request id", name)
		}
	}
	check("200", owner.get("/api/me").want(200).Header)
	check("401", e.anon().get("/api/me").want(401).Header)
	check("404", owner.get("/api/nope").wantError(404, "not_found").Header)
	check("405", owner.do("PUT", "/api/me", nil).wantError(405, "method_not_allowed").Header)
	if cc := owner.get("/api/me").Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("API responses must not be cached: %q", cc)
	}
	// Without a UI directory, non-API paths are plain 404s.
	check("no ui", owner.get("/").wantError(404, "not_found").Header)

	// A caller-supplied request id is echoed if it is harmless, replaced if not.
	c := e.anon()
	c.headers = map[string]string{"X-Request-Id": "abc-123_X.9"}
	if got := c.get("/api/setup").Header.Get("X-Request-Id"); got != "abc-123_X.9" {
		t.Errorf("request id not echoed: %q", got)
	}
	c.headers = map[string]string{"X-Request-Id": "bad id\twith junk"}
	if got := c.get("/api/setup").Header.Get("X-Request-Id"); got == "" || strings.ContainsAny(got, " \t") {
		t.Errorf("unsafe request id echoed: %q", got)
	}
}

func TestHealthAndMetrics(t *testing.T) {
	e := newEnv(t)
	owner, org := e.setup()
	e.project(owner, org, "shop")
	e.publish()
	if res := e.anon().get("/healthz").want(200); string(res.Body) != "ok\n" {
		t.Errorf("healthz body %q", res.Body)
	}
	e.anon().get("/readyz").want(200)
	res := e.anon().get("/metrics").want(200)
	body := string(res.Body)
	for _, name := range []string{"la_control_publish_lag_seconds 0", "la_control_http_requests_total{code=\"200\"}", "la_control_http_request_seconds_bucket",
		"la_control_publish_success_total 1", "la_control_deletions_pending", "la_control_logins_total", "la_control_sessions 1"} {
		if !strings.Contains(body, name) {
			t.Errorf("/metrics lacks %q", name)
		}
	}
	for _, line := range strings.Split(body, "\n") {
		if line != "" && !strings.HasPrefix(line, "#") && !strings.HasPrefix(line, "la_control_") {
			t.Errorf("metric without the la_control_ prefix: %q", line)
		}
	}
	// No secrets, tokens or addresses in the exposition.
	if strings.Contains(body, "pk_") || strings.Contains(body, "sk_") || strings.Contains(body, "@") {
		t.Error("/metrics exposes identifiers")
	}

	// With an admin listener, the public one does not serve /metrics.
	e2 := newEnv(t, func(c *config.Config) { c.AdminListen = "127.0.0.1:0" })
	e2.anon().get("/metrics").wantError(404, "not_found")
	rec := httptest.NewRecorder()
	e2.app.Server.AdminHandler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "la_control_publish_lag_seconds") {
		t.Errorf("admin /metrics: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	e2.app.Server.AdminHandler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/me", nil))
	if rec.Code != 404 {
		t.Errorf("the admin listener serves the API: %d", rec.Code)
	}

	// readyz fails when the database is gone.
	e3 := newEnv(t)
	e3.db.Close()
	e3.anon().get("/readyz").want(503)
	e3.anon().get("/healthz").want(200)
}
