//go:build compat

package conformance

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mucan54/izlek/control/app"
	"github.com/mucan54/izlek/control/auth"
	"github.com/mucan54/izlek/control/config"
	"github.com/mucan54/izlek/control/db"
	"github.com/mucan54/izlek/control/oauth"
	"github.com/mucan54/izlek/golib/objstore"
)

const (
	testEmail    = "owner@example.com"
	testPassword = "correct horse battery"
	// The metadata document is served by a local TLS listener, but the
	// client_id must look like what a real client would use: a public https
	// URL. The test fetcher dials the listener instead of resolving the name.
	cimdClientID = "https://example.com/oauth/conformance-client.json"
)

type stack struct {
	t       *testing.T
	db      *db.DB
	base    string // public URL of the control plane
	boot    app.BootstrapResult
	cookie  string // la_session of the owner
	offset  atomic.Int64
	control *httptest.Server
	queries atomic.Int64
}

func (s *stack) now() time.Time { return time.Now().Add(time.Duration(s.offset.Load())) }

// start runs a control plane on a real TCP listener, with a fake query
// service behind it and a TLS server for the client metadata document.
func start(t *testing.T) *stack {
	t.Helper()
	if _, err := os.Stat("node_modules/@modelcontextprotocol/sdk/package.json"); err != nil {
		t.Fatal("npm packages missing: run `npm ci` in control/mcp/conformance first")
	}
	ctx := context.Background()
	s := &stack{t: t}
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	s.db = d
	store, err := objstore.Open("file://"+t.TempDir(), objstore.S3Config{})
	if err != nil {
		t.Fatal(err)
	}

	// Fake query service: just enough for the query tools.
	query := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.queries.Add(1)
		if r.Header.Get("X-LA-Internal") != "conformance-secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/q/schema":
			io.WriteString(w, `{"events":["pageview","purchase"],"props":[{"key":"amount","type":"float","promoted":false}],"person_props":["plan"]}`)
		case "/q/sql":
			fmt.Fprintf(w, `{"columns":[{"name":"event","type":"Utf8"},{"name":"platform","type":"Utf8"},{"name":"events","type":"Int64"},{"name":"last_ts","type":"Timestamp"}],"rows":[["pageview","web",12,%d],["purchase",null,3,%d]],"meta":{"elapsed_ms":3,"bytes_scanned":1024,"files":1,"cached":false}}`,
				s.now().Add(-time.Minute).UnixMilli(), s.now().Add(-2*time.Minute).UnixMilli())
		case "/q/trends":
			io.WriteString(w, `{"buckets":["2026-11-01T00:00:00Z"],"series":[{"index":0,"label":"purchase","values":[3]}],"meta":{"elapsed_ms":2,"bytes_scanned":10,"files":1,"cached":false}}`)
		case "/q/funnel":
			io.WriteString(w, `{"steps":[{"event":"signup","count":120,"median_seconds":null},{"event":"purchase","count":31,"median_seconds":5400.0}],"meta":{"elapsed_ms":2,"bytes_scanned":10,"files":1,"cached":false}}`)
		case "/q/retention":
			io.WriteString(w, `{"cohorts":[{"period_start":"2026-11-02T00:00:00Z","size":42,"returned":[42,13]}],"meta":{"elapsed_ms":2,"bytes_scanned":10,"files":1,"cached":true}}`)
		case "/q/events":
			io.WriteString(w, `{"events":[{"uuid":"0190","ts":1795046400000,"recv_ts":1795046400123,"event":"purchase","person_key":"u:42","anon_id":"a","user_id":"42","session_id":null,"platform":"ios","country":"TR","url_path":null,"screen":"Home","flags":0,"props":{"amount":12.5}}],"next_cursor":null,"meta":{"elapsed_ms":2,"bytes_scanned":10,"files":1,"cached":false}}`)
		default:
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":{"code":"bad_request","message":"not implemented by the fake"}}`)
		}
	}))

	// The client metadata document, over TLS.
	cimd := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/conformance-client.json" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "public, max-age=300")
		json.NewEncoder(w).Encode(map[string]any{
			"client_id": cimdClientID, "client_name": "Conformance CIMD client", "client_uri": "https://example.com",
			"redirect_uris": []string{"http://127.0.0.1/callback", "http://localhost/callback"},
			"grant_types":   []string{"authorization_code", "refresh_token"}, "response_types": []string{"code"},
			"token_endpoint_auth_method": "none",
		})
	}))
	pool := x509.NewCertPool()
	pool.AddCert(cimd.Certificate())

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s.base = "http://" + ln.Addr().String()

	cfg := config.Defaults()
	cfg.CookieSecure = false // plain http on loopback
	cfg.PublicURL = s.base
	cfg.PublicIngestURL = "https://in.example.com"
	cfg.QueryURL = query.URL
	cfg.InternalSecret = "conformance-secret"
	cfg.QueryTimeout = 10 * time.Second
	hasher := auth.NewHasher(auth.Params{MemoryKiB: 64, Time: 1, Threads: 1}, 4)
	a, err := app.New(app.Options{
		Config: cfg, DB: d, Store: store, Now: s.now, Hasher: hasher,
		ClientMetadata: &oauth.Fetcher{
			DialAddr:  cimd.Listener.Addr().String(),
			AllowAddr: func(netip.Addr) bool { return true },
			TLSConfig: &tls.Config{RootCAs: pool},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: a.Server, ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(ln)

	s.boot, err = app.Bootstrap(ctx, d, hasher, app.BootstrapInput{Email: testEmail, Password: testPassword, Org: "Acme", Project: "Shop"}, s.now())
	if err != nil {
		t.Fatal(err)
	}

	// A browser session of the owner, for the consent page.
	body, _ := json.Marshal(map[string]string{"email": testEmail, "password": testPassword})
	resp, err := http.Post(s.base+"/api/auth/login", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	for _, ck := range resp.Cookies() {
		if ck.Name == "la_session" {
			s.cookie = ck.Value
		}
	}
	if resp.StatusCode != 200 || s.cookie == "" {
		t.Fatalf("login: status %d, cookie %q", resp.StatusCode, s.cookie)
	}

	// The scripts move the server's clock to let access tokens expire.
	s.control = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secs, err := strconv.Atoi(r.URL.Query().Get("seconds"))
		if r.URL.Path != "/advance" || err != nil || secs < 0 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		s.offset.Add(int64(time.Duration(secs) * time.Second))
		io.WriteString(w, "ok")
	}))

	t.Cleanup(func() {
		srv.Close()
		a.Server.Close()
		s.control.Close()
		cimd.Close()
		query.Close()
		d.Close()
	})
	return s
}

// node runs one of the scripts and returns the JSON object it prints last.
func (s *stack) node(script string, extraEnv ...string) map[string]any {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "node", script)
	cmd.Env = append(os.Environ(),
		"IZLEK_MCP_URL="+s.base+"/mcp",
		"IZLEK_BASE_URL="+s.base,
		"IZLEK_SECRET_KEY="+s.boot.Secret,
		"IZLEK_SESSION_COOKIE="+s.cookie,
		"IZLEK_PROJECT_ID="+strconv.FormatUint(uint64(s.boot.ProjectID), 10),
		"IZLEK_WRITE_TOKEN="+s.boot.Token,
		"IZLEK_CONTROL_URL="+s.control.URL,
		"IZLEK_CIMD_CLIENT_ID="+cimdClientID,
	)
	cmd.Env = append(cmd.Env, extraEnv...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		s.t.Fatalf("node %s: %v\nstdout:\n%s\nstderr:\n%s", script, err, stdout.String(), stderr.String())
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	var out map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &out); err != nil {
		s.t.Fatalf("node %s: last line of output is not JSON: %v\n%s", script, err, stdout.String())
	}
	s.t.Logf("node %s: %s", script, lines[len(lines)-1])
	return out
}

type auditRow struct {
	Actor, Action, Target, Detail string
}

func (s *stack) audit() []auditRow {
	s.t.Helper()
	var rows []auditRow
	err := s.db.Read(context.Background(), func(q db.Q) error {
		res, err := q.QueryContext(context.Background(), `SELECT actor, action, target, detail FROM audit_log ORDER BY id`)
		if err != nil {
			return err
		}
		defer res.Close()
		for res.Next() {
			var r auditRow
			if err := res.Scan(&r.Actor, &r.Action, &r.Target, &r.Detail); err != nil {
				return err
			}
			rows = append(rows, r)
		}
		return res.Err()
	})
	if err != nil {
		s.t.Fatal(err)
	}
	return rows
}

func (s *stack) count(query string, args ...any) int {
	s.t.Helper()
	var n int
	err := s.db.Write(context.Background(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(context.Background(), query, args...).Scan(&n)
	})
	if err != nil {
		s.t.Fatal(err)
	}
	return n
}

func wantVersions(t *testing.T, out map[string]any, key string, want ...string) {
	t.Helper()
	got, _ := out[key].([]any)
	var have []string
	for _, v := range got {
		have = append(have, fmt.Sprint(v))
	}
	if strings.Join(have, ",") != strings.Join(want, ",") {
		t.Fatalf("%s: protocol revisions exercised = %v, want %v", key, have, want)
	}
}

// TestLegacyClient: @modelcontextprotocol/sdk 1.x — the initialize
// handshake, an sk_ bearer, and OAuth with dynamic client registration.
func TestLegacyClient(t *testing.T) {
	s := start(t)
	out := s.node("legacy.mjs")
	wantVersions(t, out, "key_versions", "2025-11-25", "2025-06-18", "2025-03-26")
	if out["oauth_version"] != "2025-11-25" || out["registration"] != "dcr" || out["refreshed"] != true || out["reuse_revoked"] != true {
		t.Fatalf("unexpected result: %v", out)
	}
	if s.queries.Load() == 0 {
		t.Fatal("no query reached the query service")
	}

	// What the flow left behind.
	if n := s.count(`SELECT COUNT(*) FROM oauth_clients WHERE kind = 'dcr'`); n != 1 {
		t.Fatalf("dynamically registered clients: %d", n)
	}
	if n := s.count(`SELECT COUNT(*) FROM oauth_grants WHERE revoked_at IS NOT NULL AND revoke_reason = 'refresh_token_reuse'`); n != 1 {
		t.Fatalf("grants revoked for refresh token reuse: %d", n)
	}
	if n := s.count(`SELECT COUNT(*) FROM oauth_tokens`); n != 0 {
		t.Fatalf("%d tokens survive a revoked grant", n)
	}
	user := "user:" + strconv.FormatInt(s.boot.UserID, 10)
	seen := map[string]int{}
	var flagViaKey, flagViaOAuth bool
	for _, r := range s.audit() {
		seen[r.Action]++
		// Nothing secret may reach the audit log.
		for _, secret := range []string{s.boot.Secret, "oat_", "ort_", "oac_", s.cookie} {
			if strings.Contains(r.Detail, secret) || strings.Contains(r.Target, secret) {
				t.Fatalf("audit entry %s leaks a credential: %s", r.Action, r.Detail)
			}
		}
		if strings.HasPrefix(r.Action, "oauth.") && r.Actor != user {
			t.Fatalf("%s attributed to %s, want %s", r.Action, r.Actor, user)
		}
		if r.Action == "flag.create" {
			var d struct {
				Via struct {
					Tool   string `json:"mcp_tool"`
					Client string `json:"oauth_client"`
				} `json:"via"`
			}
			json.Unmarshal([]byte(r.Detail), &d)
			if d.Via.Tool != "create_flag" {
				t.Fatalf("flag.create without its MCP origin: %s", r.Detail)
			}
			if strings.HasPrefix(r.Actor, "key:") && d.Via.Client == "" {
				flagViaKey = true
			}
			if r.Actor == user && d.Via.Client == "Conformance legacy client" {
				flagViaOAuth = true
			}
		}
	}
	if !flagViaKey || !flagViaOAuth {
		t.Fatalf("flag.create audit entries: via key %v, via oauth %v", flagViaKey, flagViaOAuth)
	}
	if seen["oauth.authorize"] != 1 || seen["oauth.token"] != 2 || seen["oauth.revoke"] != 1 {
		t.Fatalf("oauth audit entries: %v", seen)
	}
}

// TestModernClient: @modelcontextprotocol/client 2.x — the stateless
// 2026-07-28 revision, and OAuth with a Client ID Metadata Document.
func TestModernClient(t *testing.T) {
	s := start(t)
	out := s.node("modern.mjs")
	wantVersions(t, out, "key_versions", "2026-07-28", "2026-07-28", "2025-11-25")
	if out["oauth_version"] != "2026-07-28" || out["registration"] != "cimd" || out["refreshed"] != true || out["read_only_step_up"] != true {
		t.Fatalf("unexpected result: %v", out)
	}
	if n := s.count(`SELECT COUNT(*) FROM oauth_clients WHERE kind = 'cimd' AND client_id = ?`, cimdClientID); n != 1 {
		t.Fatalf("cached metadata documents: %d", n)
	}
	if n := s.count(`SELECT COUNT(*) FROM oauth_clients WHERE kind = 'dcr'`); n != 0 {
		t.Fatalf("a client with a metadata document registered dynamically (%d)", n)
	}
	var viaOAuth bool
	for _, r := range s.audit() {
		if r.Action == "token.create" && strings.Contains(r.Detail, `"oauth_client":"Conformance CIMD client"`) && strings.Contains(r.Detail, `"mcp_tool":"create_write_token"`) {
			viaOAuth = true
		}
	}
	if !viaOAuth {
		t.Fatal("token.create is not attributed to the OAuth client in the audit log")
	}
}

// TestConsentInABrowser signs in and approves in headless Chromium: the one
// place where the pages' Content-Security-Policy, the cookies and the
// same-origin rules of the forms are enforced for real. It needs Playwright
// (the PostHog compatibility test of ingest/compat uses the same install).
func TestConsentInABrowser(t *testing.T) {
	s := start(t)
	// The client's redirect URI: a local page that records what arrives.
	arrived := make(chan url.Values, 4)
	callback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/callback" {
			arrived <- r.URL.Query()
		}
		io.WriteString(w, "<!doctype html><title>callback</title>done")
	}))
	defer callback.Close()
	redirect := callback.URL + "/callback"

	reg, _ := json.Marshal(map[string]any{"client_name": "Browser conformance client", "redirect_uris": []string{redirect}})
	resp, err := http.Post(s.base+"/oauth/register", "application/json", bytes.NewReader(reg))
	if err != nil {
		t.Fatal(err)
	}
	var client struct {
		ClientID string `json:"client_id"`
	}
	json.NewDecoder(resp.Body).Decode(&client)
	resp.Body.Close()
	if resp.StatusCode != 201 || client.ClientID == "" {
		t.Fatalf("registration: %d", resp.StatusCode)
	}
	const verifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	const challenge = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM" // RFC 7636 appendix B

	for locale, want := range map[string][2]string{"en-US": {"Sign in to izlek", "Connect an application"}, "tr-TR": {"izlek'e giriş yapın", "Uygulama bağla"}} {
		state := "state-" + locale
		q := url.Values{"response_type": {"code"}, "client_id": {client.ClientID}, "redirect_uri": {redirect}, "state": {state},
			"scope": {"izlek:read izlek:admin"}, "code_challenge": {challenge}, "code_challenge_method": {"S256"}, "resource": {s.base + "/mcp"}}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		cmd := exec.CommandContext(ctx, "node", "browser.cjs", s.base+"/oauth/authorize?"+q.Encode(), callback.URL, testEmail, testPassword, locale)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		cancel()
		if err != nil {
			t.Fatalf("%s: browser run failed: %v\nstdout:\n%s\nstderr:\n%s", locale, err, stdout.String(), stderr.String())
		}
		var out struct {
			SignIn, Consent, Styled, Landed string
			Framed                          bool
			Problems                        []string
		}
		if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &out); err != nil {
			t.Fatalf("%s: %v: %s", locale, err, stdout.String())
		}
		t.Logf("%s: %+v", locale, out)
		// The pages follow the browser's language, are styled (the nonce
		// works) and nothing was blocked by the Content-Security-Policy —
		// in particular not the redirect back to the client after "Approve".
		if out.SignIn != want[0] || out.Consent != want[1] || out.Styled != "12px" || len(out.Problems) != 0 || !strings.HasPrefix(out.Landed, redirect+"?") {
			t.Fatalf("%s: unexpected browser result: %+v", locale, out)
		}
		var back url.Values
		select {
		case back = <-arrived:
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: the callback was never requested", locale)
		}
		if back.Get("state") != state || back.Get("iss") != s.base || !strings.HasPrefix(back.Get("code"), "oac_") {
			t.Fatalf("%s: callback parameters: %v", locale, back)
		}
		// The code the browser carried is redeemable.
		tok, err := http.PostForm(s.base+"/oauth/token", url.Values{"grant_type": {"authorization_code"}, "code": {back.Get("code")},
			"client_id": {client.ClientID}, "redirect_uri": {redirect}, "code_verifier": {verifier}, "resource": {s.base + "/mcp"}})
		if err != nil {
			t.Fatal(err)
		}
		tok.Body.Close()
		if tok.StatusCode != 200 {
			t.Fatalf("%s: token exchange: %d", locale, tok.StatusCode)
		}
	}
}
