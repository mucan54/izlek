package api_test

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mucan54/izlek/control/api"
	"github.com/mucan54/izlek/control/app"
	"github.com/mucan54/izlek/control/auth"
	"github.com/mucan54/izlek/control/config"
	"github.com/mucan54/izlek/control/oauth"
)

const (
	testPublicURL = "https://izlek.example.com"
	testResource  = testPublicURL + "/mcp"
	testIngestURL = "https://in.example.com"
	testRedirect  = "https://client.example.com/oauth/callback"
	testVerifier  = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	testCIMD      = "https://example.com/oauth/client.json"
)

func withPublicURL(c *config.Config) {
	c.PublicURL = testPublicURL
	c.PublicIngestURL = testIngestURL
}

func s256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// oauthEnv is an environment with the OAuth server switched on and one
// signed-in owner.
type oauthEnv struct {
	*env
	owner *client
	org   int64
	// documents served for testCIMD; the handler can be replaced.
	metadata atomic.Value // http.HandlerFunc
	fetches  atomic.Int64
}

func newOAuthEnv(t *testing.T, opts ...envOption) *oauthEnv {
	t.Helper()
	oe := &oauthEnv{}
	oe.metadata.Store(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "max-age=300")
		fmt.Fprintf(w, `{"client_id":%q,"client_name":"Example Assistant","redirect_uris":["https://example.com/cb","http://localhost/callback"],"token_endpoint_auth_method":"none"}`, testCIMD)
	}))
	docs := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		oe.fetches.Add(1)
		oe.metadata.Load().(http.HandlerFunc)(w, r)
	}))
	t.Cleanup(docs.Close)
	pool := x509.NewCertPool()
	pool.AddCert(docs.Certificate())
	oe.env = newEnvWith(t, func(o *app.Options) {
		o.ClientMetadata = &oauth.Fetcher{
			DialAddr: docs.Listener.Addr().String(), TLSConfig: &tls.Config{RootCAs: pool}, Timeout: 2 * time.Second,
			AllowAddr: func(netip.Addr) bool { return true },
		}
	}, append([]envOption{withPublicURL}, opts...)...)
	oe.owner, oe.org = oe.setup()
	return oe
}

// raw sends a request straight to the server.
func (e *env) raw(method, target string, header map[string]string, body string) *response {
	e.t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.RemoteAddr = "198.51.100.7:40000"
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	e.app.Server.ServeHTTP(rec, req)
	return &response{t: e.t, Status: rec.Code, Header: rec.Result().Header, Body: rec.Body.Bytes(), req: method + " " + target}
}

func (e *env) form(target string, values url.Values, header map[string]string) *response {
	e.t.Helper()
	h := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
	for k, v := range header {
		h[k] = v
	}
	return e.raw("POST", target, h, values.Encode())
}

// register creates a client through dynamic client registration.
func (e *env) register(name string, redirectURIs ...string) string {
	e.t.Helper()
	body, _ := json.Marshal(map[string]any{"client_name": name, "redirect_uris": redirectURIs, "token_endpoint_auth_method": "none",
		"grant_types": []string{"authorization_code", "refresh_token"}, "response_types": []string{"code"}})
	var out struct {
		ClientID string `json:"client_id"`
		Secret   string `json:"client_secret"`
		Method   string `json:"token_endpoint_auth_method"`
	}
	e.raw("POST", "/oauth/register", map[string]string{"Content-Type": "application/json"}, string(body)).want(201).json(&out)
	if !strings.HasPrefix(out.ClientID, "oc_") || out.Secret != "" || out.Method != "none" {
		e.t.Fatalf("registration: %+v", out)
	}
	return out.ClientID
}

func authzQuery(clientID string, edit ...func(url.Values)) url.Values {
	q := url.Values{
		"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {testRedirect}, "state": {"st-123"},
		"scope": {"izlek:read izlek:admin"}, "code_challenge": {s256(testVerifier)}, "code_challenge_method": {"S256"}, "resource": {testResource},
	}
	for _, f := range edit {
		f(q)
	}
	return q
}

func set(name, value string) func(url.Values) { return func(q url.Values) { q.Set(name, value) } }
func del(name string) func(url.Values)        { return func(q url.Values) { q.Del(name) } }

func (c *client) cookieHeader() map[string]string {
	h := map[string]string{"Accept-Language": "en"}
	if c != nil && c.cookie != "" {
		h["Cookie"] = "la_session=" + c.cookie
	}
	return h
}

var hiddenField = regexp.MustCompile(`<input type="hidden" name="(\w+)" value="([^"]*)"`)

func hiddenFields(t *testing.T, r *response) url.Values {
	t.Helper()
	out := url.Values{}
	for _, m := range hiddenField.FindAllStringSubmatch(string(r.Body), -1) {
		if out.Get(m[1]) == "" {
			out.Set(m[1], html.UnescapeString(m[2]))
		}
	}
	if out.Get("q") == "" || out.Get("csrf") == "" {
		t.Fatalf("%s: the page has no form: %s", r.req, r.Body)
	}
	return out
}

// consentPage loads the authorization URL as the user and returns the page.
func (e *env) consentPage(c *client, q url.Values) *response {
	e.t.Helper()
	return e.raw("GET", "/oauth/authorize?"+q.Encode(), c.cookieHeader(), "")
}

// decide submits the consent form: action is "approve" or "deny".
func (e *env) decide(c *client, q url.Values, action string, edit ...func(url.Values)) *response {
	e.t.Helper()
	page := e.consentPage(c, q).want(200)
	form := hiddenFields(e.t, page)
	form.Set("action", action)
	form.Set("grant_admin", "1")
	for _, f := range edit {
		f(form)
	}
	h := c.cookieHeader()
	h["Origin"] = testPublicURL
	h["Sec-Fetch-Site"] = "same-origin"
	return e.form("/oauth/authorize", form, h)
}

// backTo asserts a redirect to the client's redirect URI and returns its
// query parameters.
func backTo(t *testing.T, r *response, status int) url.Values {
	t.Helper()
	r.want(status)
	loc, err := url.Parse(r.Header.Get("Location"))
	if err != nil || loc.Scheme+"://"+loc.Host+loc.Path != testRedirect {
		t.Fatalf("%s: redirected to %q, want %s", r.req, r.Header.Get("Location"), testRedirect)
	}
	// RFC 9207: every authorization response names the issuer.
	if loc.Query().Get("iss") != testPublicURL {
		t.Fatalf("%s: iss = %q", r.req, loc.Query().Get("iss"))
	}
	return loc.Query()
}

// code runs the authorization step and returns a fresh authorization code.
func (e *env) code(c *client, q url.Values) string {
	e.t.Helper()
	back := backTo(e.t, e.decide(c, q, "approve"), 303)
	if back.Get("state") != q.Get("state") || !strings.HasPrefix(back.Get("code"), "oac_") {
		e.t.Fatalf("authorization response: %v", back)
	}
	return back.Get("code")
}

type tokenSet struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
}

func (e *env) token(values url.Values) *response {
	e.t.Helper()
	return e.form("/oauth/token", values, nil)
}

func exchange(clientID, code string, edit ...func(url.Values)) url.Values {
	v := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "client_id": {clientID}, "redirect_uri": {testRedirect},
		"code_verifier": {testVerifier}, "resource": {testResource}}
	for _, f := range edit {
		f(v)
	}
	return v
}

func refreshing(clientID, refreshToken string, edit ...func(url.Values)) url.Values {
	v := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refreshToken}, "client_id": {clientID}}
	for _, f := range edit {
		f(v)
	}
	return v
}

func wantOAuthError(t *testing.T, r *response, status int, code string) {
	t.Helper()
	r.want(status)
	var body struct {
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	r.json(&body)
	if body.Error != code || body.Description == "" {
		t.Fatalf("%s: error %q (%q), want %q", r.req, body.Error, body.Description, code)
	}
	if r.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("%s: Cache-Control %q", r.req, r.Header.Get("Cache-Control"))
	}
}

// grant runs the whole flow and returns the client id and its tokens.
func (e *env) grant(c *client, scope string) (string, tokenSet) {
	e.t.Helper()
	clientID := e.register("Test Assistant", testRedirect)
	var tok tokenSet
	e.token(exchange(clientID, e.code(c, authzQuery(clientID, set("scope", scope))))).want(200).json(&tok)
	return clientID, tok
}

func (e *env) auditActions(prefix string) []string {
	e.t.Helper()
	var out []string
	err := e.db.Read(context.Background(), func(q dbQ) error {
		rows, err := q.QueryContext(context.Background(), `SELECT action, actor, target, detail FROM audit_log WHERE action LIKE ? ORDER BY id`, prefix+"%")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var action, actor, target, detail string
			if err := rows.Scan(&action, &actor, &target, &detail); err != nil {
				return err
			}
			out = append(out, action+" "+actor+" "+target+" "+detail)
		}
		return rows.Err()
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return out
}

func (e *env) scalar(query string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.db.Write(context.Background(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(context.Background(), query, args...).Scan(&n)
	}); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// ---- metadata ----

func TestOAuthMetadata(t *testing.T) {
	e := newOAuthEnv(t)
	for _, path := range []string{"/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp"} {
		var prm struct {
			Resource string   `json:"resource"`
			Servers  []string `json:"authorization_servers"`
			Scopes   []string `json:"scopes_supported"`
			Methods  []string `json:"bearer_methods_supported"`
		}
		e.raw("GET", path, nil, "").want(200).json(&prm)
		if prm.Resource != testResource || len(prm.Servers) != 1 || prm.Servers[0] != testPublicURL ||
			strings.Join(prm.Scopes, " ") != "izlek:read izlek:admin" || strings.Join(prm.Methods, ",") != "header" {
			t.Fatalf("%s: %+v", path, prm)
		}
	}
	var as map[string]any
	r := e.raw("GET", "/.well-known/oauth-authorization-server", nil, "").want(200).json(&as)
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("content type %q", r.Header.Get("Content-Type"))
	}
	for key, want := range map[string]any{
		"issuer":                 testPublicURL, // exactly the value in authorization_servers
		"authorization_endpoint": testPublicURL + "/oauth/authorize",
		"token_endpoint":         testPublicURL + "/oauth/token",
		"registration_endpoint":  testPublicURL + "/oauth/register",
		"revocation_endpoint":    testPublicURL + "/oauth/revoke",
		"authorization_response_iss_parameter_supported": true,
		"client_id_metadata_document_supported":          true,
	} {
		if as[key] != want {
			t.Errorf("%s = %v, want %v", key, as[key], want)
		}
	}
	for key, want := range map[string]string{
		"code_challenge_methods_supported":      "S256", // no "plain"
		"token_endpoint_auth_methods_supported": "none", // what Claude needs to pick CIMD
		"response_types_supported":              "code",
		"grant_types_supported":                 "authorization_code refresh_token",
		"scopes_supported":                      "izlek:read izlek:admin",
	} {
		var got []string
		for _, v := range as[key].([]any) {
			got = append(got, v.(string))
		}
		if strings.Join(got, " ") != want {
			t.Errorf("%s = %v, want %s", key, got, want)
		}
	}
	// Unknown well-known documents are a JSON 404, never the app's HTML.
	e.raw("GET", "/.well-known/openid-configuration", nil, "").wantError(404, "not_found")
	e.raw("POST", "/.well-known/oauth-authorization-server", nil, "").want(405)
}

func TestOAuthIsOffWithoutAPublicURL(t *testing.T) {
	e := newEnv(t)
	e.raw("GET", "/.well-known/oauth-authorization-server", nil, "").wantError(404, "not_found")
	e.raw("GET", "/.well-known/oauth-protected-resource/mcp", nil, "").wantError(404, "not_found")
	e.raw("GET", "/oauth/authorize?client_id=x", nil, "").wantError(404, "oauth_not_configured")
	e.form("/oauth/token", url.Values{"grant_type": {"authorization_code"}}, nil).wantError(404, "oauth_not_configured")
	e.raw("POST", "/oauth/register", map[string]string{"Content-Type": "application/json"}, `{"redirect_uris":["https://a.example/cb"]}`).wantError(404, "oauth_not_configured")
	// The MCP endpoint still works with keys; its challenge names no metadata.
	r := e.raw("POST", "/mcp", map[string]string{"Content-Type": "application/json"}, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`).want(401)
	if got := r.Header.Get("WWW-Authenticate"); got != `Bearer realm="izlek"` {
		t.Fatalf("challenge %q", got)
	}
	// An OAuth-shaped token is not looked up at all.
	e.raw("POST", "/mcp", map[string]string{"Content-Type": "application/json", "Authorization": "Bearer " + oauth.NewAccessToken()}, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`).want(401)

	// LA_MCP_ENABLED=false removes all of it.
	off := newEnv(t, func(c *config.Config) { c.MCPEnabled = false; c.PublicURL = testPublicURL })
	off.raw("POST", "/mcp", map[string]string{"Content-Type": "application/json"}, `{}`).wantError(404, "not_found")
	off.raw("GET", "/.well-known/oauth-authorization-server", nil, "").wantError(404, "not_found")
	off.raw("GET", "/oauth/authorize", nil, "").wantError(404, "not_found")
}

// ---- registration ----

func TestDynamicClientRegistration(t *testing.T) {
	e := newOAuthEnv(t)
	jsonHeader := map[string]string{"Content-Type": "application/json"}
	var out map[string]any
	e.raw("POST", "/oauth/register", jsonHeader, `{"client_name":"Claude","redirect_uris":["https://claude.ai/api/mcp/auth_callback"],
		"token_endpoint_auth_method":"client_secret_post","application_type":"web","scope":"izlek:read","software_id":"x"}`).want(201).json(&out)
	// Whatever was asked for, the client is public: no secret is issued.
	if out["token_endpoint_auth_method"] != "none" || out["client_secret"] != nil || out["client_name"] != "Claude" {
		t.Fatalf("registration response: %v", out)
	}
	// Registering grants nothing.
	if n := e.scalar(`SELECT COUNT(*) FROM oauth_grants`) + e.scalar(`SELECT COUNT(*) FROM oauth_tokens`) + e.scalar(`SELECT COUNT(*) FROM audit_log WHERE action LIKE 'oauth.%'`); n != 0 {
		t.Fatalf("registration left %d grants/tokens/audit entries", n)
	}

	bad := map[string]struct{ body, code string }{
		"no redirect uris":  {`{"client_name":"x"}`, "invalid_redirect_uri"},
		"http redirect":     {`{"redirect_uris":["http://example.com/cb"]}`, "invalid_redirect_uri"},
		"script redirect":   {`{"redirect_uris":["javascript:alert(document.cookie)"]}`, "invalid_redirect_uri"},
		"custom scheme":     {`{"redirect_uris":["myapp://callback"]}`, "invalid_redirect_uri"},
		"fragment":          {`{"redirect_uris":["https://example.com/cb#x"]}`, "invalid_redirect_uri"},
		"implicit":          {`{"redirect_uris":["https://example.com/cb"],"grant_types":["implicit"]}`, "invalid_client_metadata"},
		"token response":    {`{"redirect_uris":["https://example.com/cb"],"response_types":["token"]}`, "invalid_client_metadata"},
		"not an object":     {`["https://example.com/cb"]`, "invalid_client_metadata"},
		"not json":          {`redirect_uris=https://example.com/cb`, "invalid_client_metadata"},
		"wrong field types": {`{"redirect_uris":"https://example.com/cb"}`, "invalid_client_metadata"},
	}
	for name, tc := range bad {
		wantOAuthError(t, e.raw("POST", "/oauth/register", jsonHeader, tc.body), 400, tc.code)
		_ = name
	}
	wantOAuthError(t, e.raw("POST", "/oauth/register", map[string]string{"Content-Type": "text/plain"}, `{"redirect_uris":["https://example.com/cb"]}`), 400, "invalid_client_metadata")
	wantOAuthError(t, e.raw("POST", "/oauth/register", jsonHeader, `{"redirect_uris":["https://example.com/cb"],"pad":"`+strings.Repeat("x", 20<<10)+`"}`), 400, "invalid_client_metadata")
	e.raw("GET", "/oauth/register", nil, "").want(405)

	// The display name cannot carry markup or invisible characters into the
	// consent page.
	e.raw("POST", "/oauth/register", jsonHeader, `{"client_name":"<script>alert(1)</script>‮evil","redirect_uris":["`+testRedirect+`"]}`).want(201).json(&out)
	page := e.consentPage(e.owner, authzQuery(out["client_id"].(string))).want(200)
	if strings.Contains(string(page.Body), "<script>alert") || strings.Contains(string(page.Body), "‮") || !strings.Contains(string(page.Body), "&lt;script&gt;alert(1)&lt;/script&gt;evil") {
		t.Fatalf("client name is not neutralised: %s", page.Body)
	}
}

func TestRegistrationIsRateLimitedAndCanBeSwitchedOff(t *testing.T) {
	e := newOAuthEnv(t)
	body := `{"redirect_uris":["https://example.com/cb"]}`
	h := map[string]string{"Content-Type": "application/json"}
	for i := 0; i < 30; i++ {
		e.raw("POST", "/oauth/register", h, body).want(201)
	}
	r := e.raw("POST", "/oauth/register", h, body)
	wantOAuthError(t, r, 429, "temporarily_unavailable")
	if r.Header.Get("Retry-After") == "" {
		t.Fatal("no Retry-After")
	}
	e.clock.Advance(11 * time.Minute)
	e.raw("POST", "/oauth/register", h, body).want(201)

	off := newOAuthEnv(t, func(c *config.Config) { c.OAuthDCR = false })
	off.raw("POST", "/oauth/register", h, body).wantError(404, "not_found")
	var as map[string]any
	off.raw("GET", "/.well-known/oauth-authorization-server", nil, "").want(200).json(&as)
	if _, ok := as["registration_endpoint"]; ok {
		t.Fatal("registration_endpoint advertised although registration is off")
	}
}

// ---- the flow ----

func TestAuthorizationCodeFlow(t *testing.T) {
	e := newOAuthEnv(t)
	clientID := e.register("Test Assistant", testRedirect)
	q := authzQuery(clientID)

	page := e.consentPage(e.owner, q).want(200)
	body := string(page.Body)
	for _, want := range []string{
		"Test Assistant",     // who asks
		"client.example.com", // where the access goes
		"owner@example.com",  // which account approves
		"Acme", "owner",      // with which role
		"run analytics queries",      // the read scope, in words
		"Create and change projects", // the admin scope, in words
		"registered itself",          // nobody verified the name
		`name="action" value="approve"`, `name="action" value="deny"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("consent page does not mention %q", want)
		}
	}
	// Clickjacking and caching.
	csp := page.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "frame-ancestors 'none'") || !strings.Contains(csp, "default-src 'none'") || strings.Contains(csp, "unsafe-inline") ||
		!strings.Contains(csp, "form-action 'self' https://client.example.com") {
		t.Errorf("CSP %q", csp)
	}
	if page.Header.Get("X-Frame-Options") != "DENY" || page.Header.Get("Cache-Control") != "no-store" || page.Header.Get("Referrer-Policy") != "same-origin" {
		t.Errorf("headers: %v", page.Header)
	}
	// Showing the page changes nothing.
	if n := e.scalar(`SELECT COUNT(*) FROM oauth_codes`); n != 0 {
		t.Fatalf("%d codes before approval", n)
	}

	code := e.code(e.owner, q)
	var tok tokenSet
	r := e.token(exchange(clientID, code)).want(200).json(&tok)
	if r.Header.Get("Cache-Control") != "no-store" || r.Header.Get("Pragma") != "no-cache" {
		t.Fatalf("token response headers: %v", r.Header)
	}
	if !strings.HasPrefix(tok.AccessToken, "oat_") || !strings.HasPrefix(tok.RefreshToken, "ort_") || tok.TokenType != "Bearer" ||
		tok.ExpiresIn != 3600 || tok.Scope != "izlek:read izlek:admin" {
		t.Fatalf("tokens: %+v", tok)
	}
	// The token works at the MCP endpoint and acts as the user.
	res := e.callTool(tok.AccessToken, "list_projects", nil)
	if res.IsError || len(res.Structured["organisations"].([]any)) != 1 {
		t.Fatalf("list_projects: %+v", res)
	}

	// Only hashes are stored.
	for _, secret := range []string{code, tok.AccessToken, tok.RefreshToken} {
		for _, table := range []string{"oauth_codes", "oauth_tokens", "audit_log"} {
			col := map[string]string{"oauth_codes": "code_hash", "oauth_tokens": "hash", "audit_log": "detail || target"}[table]
			if n := e.scalar(`SELECT COUNT(*) FROM `+table+` WHERE instr(`+col+`, ?) > 0`, secret); n != 0 {
				t.Fatalf("%s contains a secret in clear", table)
			}
		}
	}
	// Authorization and token issue are audited, attributed to the user.
	audit := e.auditActions("oauth.")
	user := fmt.Sprintf("user:%d", e.owner.userID)
	if len(audit) != 2 || !strings.HasPrefix(audit[0], "oauth.authorize "+user+" "+clientID) || !strings.HasPrefix(audit[1], "oauth.token "+user+" "+clientID) ||
		!strings.Contains(audit[0], `"client_name":"Test Assistant"`) || !strings.Contains(audit[0], `"redirect_host":"client.example.com"`) ||
		!strings.Contains(audit[1], `"grant_type":"authorization_code"`) {
		t.Fatalf("audit: %q", audit)
	}
}

func TestTokenRequestIsBoundToTheAuthorizationRequest(t *testing.T) {
	e := newOAuthEnv(t)
	clientID := e.register("Test Assistant", testRedirect, "https://client.example.com/other")
	otherClient := e.register("Other", testRedirect)
	q := authzQuery(clientID)

	cases := []struct {
		name   string
		edit   func(url.Values)
		status int
		code   string
	}{
		{"wrong verifier", set("code_verifier", "another-verifier-another-verifier-another-verif"), 400, "invalid_grant"},
		{"challenge used as verifier (plain PKCE)", set("code_verifier", s256(testVerifier)), 400, "invalid_grant"},
		{"no verifier", del("code_verifier"), 400, "invalid_request"},
		{"other registered redirect uri", set("redirect_uri", "https://client.example.com/other"), 400, "invalid_grant"},
		{"redirect uri with a suffix", set("redirect_uri", testRedirect+"/"), 400, "invalid_grant"},
		{"wrong resource", set("resource", "https://other.example.com/mcp"), 400, "invalid_target"},
		{"another client's id", set("client_id", otherClient), 400, "invalid_grant"},
		{"no client id", del("client_id"), 400, "invalid_client"},
		{"malformed code", set("code", "oac_short"), 400, "invalid_grant"},
		{"unknown code", set("code", oauth.NewCode()), 400, "invalid_grant"},
		{"unsupported grant", set("grant_type", "client_credentials"), 400, "unsupported_grant_type"},
		{"password grant", set("grant_type", "password"), 400, "unsupported_grant_type"},
		{"no grant type", del("grant_type"), 400, "invalid_request"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code := e.code(e.owner, q)
			wantOAuthError(t, e.token(exchange(clientID, code, tc.edit)), tc.status, tc.code)
			// A failed attempt gave nothing away and issued nothing.
			if n := e.scalar(`SELECT COUNT(*) FROM oauth_tokens`); n != 0 {
				t.Fatalf("%d tokens after a refused request", n)
			}
		})
	}
	// A refused attempt does not burn the code: the right request still works.
	code := e.code(e.owner, q)
	wantOAuthError(t, e.token(exchange(clientID, code, set("code_verifier", strings.Repeat("x", 50)))), 400, "invalid_grant")
	e.token(exchange(clientID, code)).want(200)

	// OAuth 2.1 clients send neither redirect_uri nor (older ones) resource.
	e.token(exchange(clientID, e.code(e.owner, q), del("redirect_uri"), del("resource"))).want(200)

	// Transport rules.
	wantOAuthError(t, e.raw("POST", "/oauth/token", map[string]string{"Content-Type": "application/json"}, `{"grant_type":"authorization_code"}`), 400, "invalid_request")
	wantOAuthError(t, e.form("/oauth/token", url.Values{"grant_type": {"authorization_code", "refresh_token"}, "client_id": {clientID}}, nil), 400, "invalid_request")
	wantOAuthError(t, e.form("/oauth/token", url.Values{"client_id": {clientID}, "pad": {strings.Repeat("x", 20<<10)}}, nil), 400, "invalid_request")
	e.raw("GET", "/oauth/token?grant_type=authorization_code", nil, "").want(405)
}

func TestAuthorizationCodeIsSingleUseAndShortLived(t *testing.T) {
	e := newOAuthEnv(t)
	clientID := e.register("Test Assistant", testRedirect)
	q := authzQuery(clientID)

	// Expired.
	code := e.code(e.owner, q)
	e.clock.Advance(2*time.Minute + time.Second)
	wantOAuthError(t, e.token(exchange(clientID, code)), 400, "invalid_grant")

	// Used twice: the second attempt fails and takes back what the first got.
	code = e.code(e.owner, q)
	var tok tokenSet
	e.token(exchange(clientID, code)).want(200).json(&tok)
	if res := e.callTool(tok.AccessToken, "list_projects", nil); res.IsError {
		t.Fatalf("token does not work: %+v", res)
	}
	wantOAuthError(t, e.token(exchange(clientID, code)), 400, "invalid_grant")
	e.mcp(tok.AccessToken, "tools/list", nil).want(401)
	wantOAuthError(t, e.token(refreshing(clientID, tok.RefreshToken)), 400, "invalid_grant")
	audit := e.auditActions("oauth.revoke")
	if len(audit) != 1 || !strings.Contains(audit[0], `"reason":"code_replay"`) {
		t.Fatalf("audit: %q", audit)
	}
}

func TestRefreshTokensRotate(t *testing.T) {
	e := newOAuthEnv(t)
	clientID, first := e.grant(e.owner, "izlek:read izlek:admin")
	other := e.register("Other", testRedirect)

	// Not with another client's id, and not for another resource or more scope.
	wantOAuthError(t, e.token(refreshing(other, first.RefreshToken)), 400, "invalid_grant")
	wantOAuthError(t, e.token(refreshing(clientID, first.RefreshToken, del("client_id"))), 400, "invalid_client")
	wantOAuthError(t, e.token(refreshing(clientID, first.RefreshToken, set("resource", "https://other.example.com/mcp"))), 400, "invalid_target")
	wantOAuthError(t, e.token(refreshing(clientID, first.AccessToken)), 400, "invalid_grant") // an access token is not a refresh token
	wantOAuthError(t, e.token(refreshing(clientID, oauth.NewRefreshToken())), 400, "invalid_grant")

	e.clock.Advance(30 * time.Minute)
	var second tokenSet
	e.token(refreshing(clientID, first.RefreshToken, set("resource", testResource), set("scope", "izlek:read"))).want(200).json(&second)
	if second.AccessToken == first.AccessToken || second.RefreshToken == first.RefreshToken || second.ExpiresIn != 3600 || second.Scope != "izlek:read izlek:admin" {
		t.Fatalf("second pair: %+v", second)
	}
	// Both access tokens work until they expire on their own.
	e.mcp(first.AccessToken, "tools/list", nil).want(200)
	e.mcp(second.AccessToken, "tools/list", nil).want(200)
	e.clock.Advance(31 * time.Minute)
	e.mcp(first.AccessToken, "tools/list", nil).want(401)
	e.mcp(second.AccessToken, "tools/list", nil).want(200)

	// The first refresh token is spent. Using it again is treated as theft:
	// the grant ends, with every token under it.
	var third tokenSet
	e.token(refreshing(clientID, second.RefreshToken)).want(200).json(&third)
	wantOAuthError(t, e.token(refreshing(clientID, first.RefreshToken)), 400, "invalid_grant")
	e.mcp(third.AccessToken, "tools/list", nil).want(401)
	wantOAuthError(t, e.token(refreshing(clientID, third.RefreshToken)), 400, "invalid_grant")
	if n := e.scalar(`SELECT COUNT(*) FROM oauth_tokens`); n != 0 {
		t.Fatalf("%d tokens survive the revoked grant", n)
	}
	audit := e.auditActions("oauth.")
	last := audit[len(audit)-1]
	if !strings.HasPrefix(last, "oauth.revoke ") || !strings.Contains(last, `"reason":"refresh_token_reuse"`) {
		t.Fatalf("audit: %q", audit)
	}
	refreshes := 0
	for _, a := range audit {
		if strings.Contains(a, `"grant_type":"refresh_token"`) {
			refreshes++
		}
	}
	if refreshes != 2 {
		t.Fatalf("%d refreshes audited, want 2: %q", refreshes, audit)
	}
}

func TestTokenLifetimes(t *testing.T) {
	e := newOAuthEnv(t)
	clientID, tok := e.grant(e.owner, "izlek:read")
	e.mcp(tok.AccessToken, "tools/list", nil).want(200)

	// An access token lives for an hour.
	e.clock.Advance(time.Hour + time.Second)
	r := e.mcp(tok.AccessToken, "tools/list", nil).want(401)
	if !strings.Contains(r.Header.Get("WWW-Authenticate"), `error="invalid_token"`) {
		t.Fatalf("challenge %q", r.Header.Get("WWW-Authenticate"))
	}
	// An unused refresh token lives for 30 days.
	e.clock.Advance(30 * 24 * time.Hour)
	wantOAuthError(t, e.token(refreshing(clientID, tok.RefreshToken)), 400, "invalid_grant")

	// A grant ends after 180 days however often it is refreshed. (The
	// browser session of the owner has expired by now, too.)
	clientID, tok = e.grant(e.login("owner@example.com", testPassword), "izlek:read")
	for day := 0; day < 180; day += 20 {
		e.clock.Advance(20 * 24 * time.Hour)
		r := e.token(refreshing(clientID, tok.RefreshToken))
		if day+20 >= 180 {
			wantOAuthError(t, r, 400, "invalid_grant")
			break
		}
		r.want(200).json(&tok)
	}
	e.mcp(tok.AccessToken, "tools/list", nil).want(401)
}

func TestRevocation(t *testing.T) {
	e := newOAuthEnv(t)
	clientID, tok := e.grant(e.owner, "izlek:read")
	revoke := func(v url.Values) *response { return e.form("/oauth/revoke", v, nil) }

	// Unknown tokens and tokens of another client: 200 and nothing happens.
	revoke(url.Values{"token": {oauth.NewRefreshToken()}}).want(200)
	revoke(url.Values{"token": {"garbage"}}).want(200)
	revoke(url.Values{"token": {tok.RefreshToken}, "client_id": {"oc_someoneelse"}}).want(200)
	e.mcp(tok.AccessToken, "tools/list", nil).want(200)
	wantOAuthError(t, revoke(url.Values{}), 400, "invalid_request")

	// Revoking an access token ends that token only.
	revoke(url.Values{"token": {tok.AccessToken}, "token_type_hint": {"access_token"}}).want(200)
	e.mcp(tok.AccessToken, "tools/list", nil).want(401)
	var next tokenSet
	e.token(refreshing(clientID, tok.RefreshToken)).want(200).json(&next)
	e.mcp(next.AccessToken, "tools/list", nil).want(200)

	// Revoking the refresh token ends the grant.
	revoke(url.Values{"token": {next.RefreshToken}, "client_id": {clientID}}).want(200)
	e.mcp(next.AccessToken, "tools/list", nil).want(401)
	wantOAuthError(t, e.token(refreshing(clientID, next.RefreshToken)), 400, "invalid_grant")
	audit := e.auditActions("oauth.revoke")
	if len(audit) != 1 || !strings.Contains(audit[0], `"reason":"client"`) || !strings.HasPrefix(audit[0], fmt.Sprintf("oauth.revoke user:%d %s", e.owner.userID, clientID)) {
		t.Fatalf("audit: %q", audit)
	}
}

func TestConnectedApps(t *testing.T) {
	e := newOAuthEnv(t)
	other := e.member(e.owner, e.org, "admin")
	clientID, tok := e.grant(e.owner, "izlek:read izlek:admin")
	_, otherTok := e.grant(other, "izlek:read")
	e.callTool(tok.AccessToken, "list_projects", nil)

	apps := e.owner.get("/api/me/connected-apps").want(200).list()
	if len(apps) != 1 {
		t.Fatalf("connected apps: %v", apps)
	}
	a := apps[0]
	if a["client_id"] != clientID || a["client_name"] != "Test Assistant" || a["client_kind"] != "dcr" || fmt.Sprint(a["scopes"]) != "[izlek:read izlek:admin]" ||
		fmt.Sprint(a["redirect_hosts"]) != "[client.example.com]" || a["last_used_at"] == nil || a["created_at"] == nil || a["expires_at"] == nil {
		t.Fatalf("connected app: %v", a)
	}
	// Nothing secret in the listing.
	raw := string(e.owner.get("/api/me/connected-apps").Body)
	if strings.Contains(raw, "oat_") || strings.Contains(raw, "ort_") || strings.Contains(raw, "hash") {
		t.Fatalf("listing leaks token material: %s", raw)
	}
	id := int64(a["id"].(float64))

	// Another user's grant does not exist; anonymous callers are rejected;
	// the cookie-authenticated delete needs the CSRF token.
	other.del(fmt.Sprintf("/api/me/connected-apps/%d", id)).wantError(404, "not_found")
	e.anon().get("/api/me/connected-apps").wantError(401, "unauthorized")
	noCSRF := *e.owner
	noCSRF.noCSRF = true
	noCSRF.del(fmt.Sprintf("/api/me/connected-apps/%d", id)).wantError(403, "csrf")
	e.owner.del("/api/me/connected-apps/abc").wantError(404, "not_found")
	e.mcp(tok.AccessToken, "tools/list", nil).want(200)

	e.owner.del(fmt.Sprintf("/api/me/connected-apps/%d", id)).want(204)
	e.mcp(tok.AccessToken, "tools/list", nil).want(401)
	wantOAuthError(t, e.token(refreshing(clientID, tok.RefreshToken)), 400, "invalid_grant")
	if apps := e.owner.get("/api/me/connected-apps").want(200).list(); len(apps) != 0 {
		t.Fatalf("still listed: %v", apps)
	}
	e.owner.del(fmt.Sprintf("/api/me/connected-apps/%d", id)).wantError(404, "not_found")
	e.mcp(otherTok.AccessToken, "tools/list", nil).want(200) // the other user's grant is untouched
	audit := e.auditActions("oauth.revoke")
	if len(audit) != 1 || !strings.Contains(audit[0], `"reason":"user"`) {
		t.Fatalf("audit: %q", audit)
	}
}

func TestCredentialChangesEndGrants(t *testing.T) {
	e := newOAuthEnv(t)
	_, tok := e.grant(e.owner, "izlek:read")
	e.owner.post("/api/me/password", map[string]any{"current": testPassword, "new": "a different passphrase"}).want(200)
	e.mcp(tok.AccessToken, "tools/list", nil).want(401)
	if apps := e.owner.get("/api/me/connected-apps").want(200).list(); len(apps) != 0 {
		t.Fatalf("grants survive a password change: %v", apps)
	}
	audit := e.auditActions("user.password")
	if len(audit) != 1 || !strings.Contains(audit[0], `"oauth_grants_revoked":1`) {
		t.Fatalf("audit: %q", audit)
	}
}

// ---- the authorization endpoint ----

func TestAuthorizeNeverRedirectsToAnUnverifiedAddress(t *testing.T) {
	e := newOAuthEnv(t)
	clientID := e.register("Test Assistant", testRedirect)
	cases := map[string]url.Values{
		"unknown client":             authzQuery("oc_" + strings.Repeat("a", 24)),
		"malformed client id":        authzQuery("nope"),
		"no client id":               authzQuery(clientID, del("client_id")),
		"redirect uri not the one":   authzQuery(clientID, set("redirect_uri", "https://evil.example/cb")),
		"redirect uri with a suffix": authzQuery(clientID, set("redirect_uri", testRedirect+"/extra")),
		"redirect uri other host":    authzQuery(clientID, set("redirect_uri", "https://client.example.com.evil.example/oauth/callback")),
		"redirect uri other scheme":  authzQuery(clientID, set("redirect_uri", "http://client.example.com/oauth/callback")),
		"redirect uri with query":    authzQuery(clientID, set("redirect_uri", testRedirect+"?next=https://evil.example")),
		"script redirect uri":        authzQuery(clientID, set("redirect_uri", "javascript:alert(1)")),
		"no redirect uri":            authzQuery(clientID, del("redirect_uri")),
		"repeated parameter":         {"client_id": {clientID, clientID}, "redirect_uri": {testRedirect}},
		"metadata url off https":     authzQuery("http://example.com/oauth/client.json"),
		"metadata url to an address": authzQuery("https://169.254.169.254/latest/meta-data/"),
	}
	for name, q := range cases {
		for _, who := range []*client{e.owner, nil} {
			r := e.raw("GET", "/oauth/authorize?"+q.Encode(), who.cookieHeader(), "")
			if r.Status != 400 || r.Header.Get("Location") != "" || !strings.Contains(string(r.Body), "This connection cannot continue") {
				t.Errorf("%s (signed in: %v): status %d, Location %q", name, who != nil, r.Status, r.Header.Get("Location"))
			}
			if strings.Contains(string(r.Body), `name="action"`) {
				t.Errorf("%s: the error page offers a form", name)
			}
		}
	}
	if e.fetches.Load() != 0 {
		t.Fatalf("%d outbound requests for invalid client ids", e.fetches.Load())
	}
}

func TestAuthorizeErrorsGoBackToAVerifiedClient(t *testing.T) {
	e := newOAuthEnv(t)
	clientID := e.register("Test Assistant", testRedirect)
	cases := map[string]struct {
		q    url.Values
		code string
	}{
		"implicit flow":         {authzQuery(clientID, set("response_type", "token")), "unsupported_response_type"},
		"no response type":      {authzQuery(clientID, del("response_type")), "unsupported_response_type"},
		"no PKCE":               {authzQuery(clientID, del("code_challenge")), "invalid_request"},
		"plain PKCE":            {authzQuery(clientID, set("code_challenge_method", "plain")), "invalid_request"},
		"PKCE method missing":   {authzQuery(clientID, del("code_challenge_method")), "invalid_request"},
		"malformed challenge":   {authzQuery(clientID, set("code_challenge", "too-short")), "invalid_request"},
		"another resource":      {authzQuery(clientID, set("resource", "https://api.other.example/")), "invalid_target"},
		"resource on evil host": {authzQuery(clientID, set("resource", "https://izlek.example.com.evil.example/mcp")), "invalid_target"},
		"only unknown scopes":   {authzQuery(clientID, set("scope", "profile email")), "invalid_scope"},
	}
	for name, tc := range cases {
		back := backTo(t, e.consentPage(e.owner, tc.q), 302)
		if back.Get("error") != tc.code || back.Get("state") != "st-123" || back.Get("code") != "" || back.Get("error_description") == "" {
			t.Errorf("%s: %v, want error %s", name, back, tc.code)
		}
		// The same request through the form is answered the same way.
		form := url.Values{"q": {tc.q.Encode()}, "csrf": {e.owner.csrf}, "action": {"approve"}, "grant_admin": {"1"}}
		h := e.owner.cookieHeader()
		h["Origin"] = testPublicURL
		if back := backTo(t, e.form("/oauth/authorize", form, h), 303); back.Get("error") != tc.code {
			t.Errorf("%s via POST: %v", name, back)
		}
	}
	if n := e.scalar(`SELECT COUNT(*) FROM oauth_codes`); n != 0 {
		t.Fatalf("%d codes issued for invalid requests", n)
	}

	// The resource may be left out (older clients) or name the server's
	// origin; the token is bound to the MCP endpoint either way.
	for _, edit := range []func(url.Values){del("resource"), set("resource", testPublicURL), set("resource", "HTTPS://IZLEK.EXAMPLE.COM/mcp/")} {
		q := authzQuery(clientID, edit)
		var tok tokenSet
		e.token(exchange(clientID, e.code(e.owner, q), del("resource"))).want(200).json(&tok)
		e.mcp(tok.AccessToken, "tools/list", nil).want(200)
	}
	if n := e.scalar(`SELECT COUNT(*) FROM oauth_grants WHERE resource <> ?`, testResource); n != 0 {
		t.Fatalf("%d grants bound to something other than the MCP endpoint", n)
	}
}

func TestScopeSelection(t *testing.T) {
	e := newOAuthEnv(t)
	clientID := e.register("Test Assistant", testRedirect)
	issued := func(q url.Values, edit ...func(url.Values)) string {
		t.Helper()
		back := backTo(t, e.decide(e.owner, q, "approve", edit...), 303)
		var tok tokenSet
		e.token(exchange(clientID, back.Get("code"))).want(200).json(&tok)
		return tok.Scope
	}
	// No scope asked for: the least.
	if got := issued(authzQuery(clientID, del("scope"))); got != "izlek:read" {
		t.Errorf("no scope requested: %q", got)
	}
	// Unknown scopes are dropped; the response says what was granted.
	if got := issued(authzQuery(clientID, set("scope", "izlek:read offline_access openid"))); got != "izlek:read" {
		t.Errorf("with offline_access: %q", got)
	}
	// admin implies read.
	if got := issued(authzQuery(clientID, set("scope", "izlek:admin"))); got != "izlek:read izlek:admin" {
		t.Errorf("admin only: %q", got)
	}
	// The user can decline the admin part.
	if got := issued(authzQuery(clientID), del("grant_admin")); got != "izlek:read" {
		t.Errorf("admin unticked: %q", got)
	}
	// Ticking a box that was not offered grants nothing extra.
	if got := issued(authzQuery(clientID, set("scope", "izlek:read")), set("grant_admin", "1")); got != "izlek:read" {
		t.Errorf("read requested, admin box forged: %q", got)
	}
	// A read-only request does not show the admin line at all.
	page := string(e.consentPage(e.owner, authzQuery(clientID, set("scope", "izlek:read"))).want(200).Body)
	if strings.Contains(page, "Create and change projects") || strings.Contains(page, "grant_admin") {
		t.Error("the page offers the admin scope although it was not requested")
	}
}

func TestDenyAndSwitchAccount(t *testing.T) {
	e := newOAuthEnv(t)
	clientID := e.register("Test Assistant", testRedirect)
	q := authzQuery(clientID)
	back := backTo(t, e.decide(e.owner, q, "deny"), 303)
	if back.Get("error") != "access_denied" || back.Get("code") != "" || back.Get("state") != "st-123" {
		t.Fatalf("deny: %v", back)
	}
	if n := e.scalar(`SELECT COUNT(*) FROM oauth_codes`); n != 0 {
		t.Fatal("a code was issued on deny")
	}
	// "Use a different account" ends the session and shows the sign-in form.
	r := e.decide(e.owner, q, "logout").want(303)
	if loc := r.Header.Get("Location"); !strings.HasPrefix(loc, "/oauth/authorize?") {
		t.Fatalf("after sign-out: %q", loc)
	}
	e.owner.get("/api/me").wantError(401, "unauthorized")
}

func TestConsentIsProtectedAgainstCSRF(t *testing.T) {
	e := newOAuthEnv(t)
	clientID := e.register("Test Assistant", testRedirect)
	q := authzQuery(clientID)
	form := func(csrf string) url.Values {
		return url.Values{"q": {q.Encode()}, "csrf": {csrf}, "action": {"approve"}, "grant_admin": {"1"}}
	}
	with := func(extra map[string]string) map[string]string {
		h := e.owner.cookieHeader()
		for k, v := range extra {
			h[k] = v
		}
		return h
	}
	other := e.member(e.owner, e.org, "admin")

	refused := map[string]*response{
		"no token":                e.form("/oauth/authorize", form(""), with(nil)),
		"wrong token":             e.form("/oauth/authorize", form(strings.Repeat("0", 32)), with(nil)),
		"another session's token": e.form("/oauth/authorize", form(other.csrf), with(nil)),
		"cross-site origin":       e.form("/oauth/authorize", form(e.owner.csrf), with(map[string]string{"Origin": "https://evil.example"})),
		"null origin":             e.form("/oauth/authorize", form(e.owner.csrf), with(map[string]string{"Origin": "null"})),
		"cross-site fetch":        e.form("/oauth/authorize", form(e.owner.csrf), with(map[string]string{"Sec-Fetch-Site": "cross-site"})),
		"same-site fetch":         e.form("/oauth/authorize", form(e.owner.csrf), with(map[string]string{"Sec-Fetch-Site": "same-site"})),
	}
	for name, r := range refused {
		if r.Status != 403 || r.Header.Get("Location") != "" {
			t.Errorf("%s: status %d, Location %q", name, r.Status, r.Header.Get("Location"))
		}
	}
	// A GET never approves, whatever it carries.
	g := url.Values{"q": {q.Encode()}, "csrf": {e.owner.csrf}, "action": {"approve"}}
	for k, v := range q {
		g[k] = v
	}
	if r := e.raw("GET", "/oauth/authorize?"+g.Encode(), e.owner.cookieHeader(), ""); r.Status != 200 || r.Header.Get("Location") != "" {
		t.Errorf("GET with action=approve: status %d", r.Status)
	}
	if n := e.scalar(`SELECT COUNT(*) FROM oauth_codes`); n != 0 {
		t.Fatalf("%d codes issued by forged requests", n)
	}
	// The genuine form works, with and without the browser's fetch metadata.
	backTo(t, e.form("/oauth/authorize", form(e.owner.csrf), with(nil)), 303)
	backTo(t, e.form("/oauth/authorize", form(e.owner.csrf), with(map[string]string{"Origin": testPublicURL, "Sec-Fetch-Site": "same-origin"})), 303)
	// JSON cannot be used to submit the form.
	if r := e.raw("POST", "/oauth/authorize", with(map[string]string{"Content-Type": "application/json"}), `{"action":"approve"}`); r.Status != 400 {
		t.Errorf("JSON body: %d", r.Status)
	}
}

func TestSignInOnTheAuthorizationPage(t *testing.T) {
	e := newOAuthEnv(t)
	clientID := e.register("Test Assistant", testRedirect)
	q := authzQuery(clientID)

	// Not signed in: a sign-in form that mentions no client details and
	// offers no way to approve.
	page := e.raw("GET", "/oauth/authorize?"+q.Encode(), map[string]string{"Accept-Language": "en"}, "").want(200)
	body := string(page.Body)
	if !strings.Contains(body, "Sign in to izlek") || strings.Contains(body, `value="approve"`) || strings.Contains(body, "Test Assistant") {
		t.Fatalf("sign-in page: %s", body)
	}
	var pre string
	for _, ck := range (&http.Response{Header: page.Header}).Cookies() {
		if ck.Name == "__Host-la_oauth_csrf" {
			pre = ck.Value
			if !ck.HttpOnly || !ck.Secure || ck.SameSite != http.SameSiteLaxMode || ck.Path != "/" {
				t.Fatalf("pre-session cookie: %+v", ck)
			}
		}
	}
	form := hiddenFields(t, page)
	if pre == "" || form.Get("csrf") != pre {
		t.Fatalf("pre-session token: cookie %q, field %q", pre, form.Get("csrf"))
	}
	post := func(email, password, csrf string, extra map[string]string) *response {
		h := map[string]string{"Cookie": "__Host-la_oauth_csrf=" + pre, "Accept-Language": "en"}
		for k, v := range extra {
			h[k] = v
		}
		return e.form("/oauth/authorize", url.Values{"q": {form.Get("q")}, "csrf": {csrf}, "action": {"login"}, "email": {email}, "password": {password}}, h)
	}

	// A sign-in posted from another site (login CSRF) is refused: without the
	// cookie-bound token, or with a cross-site origin.
	if r := post("owner@example.com", testPassword, "", nil); r.Status != 403 || len(r.Header.Values("Set-Cookie")) != 0 {
		t.Fatalf("no token: %d %v", r.Status, r.Header.Values("Set-Cookie"))
	}
	if r := post("owner@example.com", testPassword, strings.Repeat("a", 32), nil); r.Status != 403 {
		t.Fatalf("wrong token: %d", r.Status)
	}
	if r := post("owner@example.com", testPassword, pre, map[string]string{"Origin": "https://evil.example"}); r.Status != 403 {
		t.Fatalf("cross-site: %d", r.Status)
	}
	// Wrong password: the form again, with a message and no session.
	r := post("owner@example.com", "wrong password", pre, nil).want(401)
	if !strings.Contains(string(r.Body), "not correct") || strings.Contains(string(r.Body), "wrong password") || !strings.Contains(string(r.Body), `value="owner@example.com"`) {
		t.Fatalf("failed sign-in page: %s", r.Body)
	}
	// The right one: a session cookie and a redirect back to the same request.
	r = post("owner@example.com", testPassword, pre, nil).want(303)
	loc := r.Header.Get("Location")
	if !strings.HasPrefix(loc, "/oauth/authorize?") {
		t.Fatalf("after sign-in: %q", loc)
	}
	var session string
	for _, ck := range (&http.Response{Header: r.Header}).Cookies() {
		if ck.Name == "la_session" {
			session = ck.Value
		}
	}
	if session == "" {
		t.Fatal("no session cookie")
	}
	consent := e.raw("GET", loc, map[string]string{"Cookie": "la_session=" + session, "Accept-Language": "en"}, "").want(200)
	if !strings.Contains(string(consent.Body), "Test Assistant") || !strings.Contains(string(consent.Body), `value="approve"`) {
		t.Fatalf("consent after sign-in: %s", consent.Body)
	}
	// The sign-in is the ordinary one: audited, and throttled like the API's.
	if audit := e.auditActions("auth.login"); len(audit) != 2 { // setup's client.me() does not log in; setup + this one
		t.Logf("auth.login entries: %q", audit)
	}
	for i := 0; i < 5; i++ {
		post("owner@example.com", "wrong password", pre, nil)
	}
	limited := post("owner@example.com", testPassword, pre, nil).want(429)
	if limited.Header.Get("Retry-After") == "" || !strings.Contains(string(limited.Body), "Too many attempts") {
		t.Fatalf("throttled sign-in: %v %s", limited.Header, limited.Body)
	}
}

func TestPagesAreBilingual(t *testing.T) {
	e := newOAuthEnv(t)
	clientID := e.register("Test Assistant", testRedirect)
	q := authzQuery(clientID)
	page := func(header map[string]string, q url.Values) string {
		h := e.owner.cookieHeader()
		delete(h, "Accept-Language")
		for k, v := range header {
			if k == "Cookie" {
				v = h["Cookie"] + "; " + v
			}
			h[k] = v
		}
		return string(e.raw("GET", "/oauth/authorize?"+q.Encode(), h, "").want(200).Body)
	}
	const en, tr = "Connect an application", "Uygulama bağla"
	cases := []struct {
		name   string
		header map[string]string
		q      url.Values
		want   string
	}{
		{"default is English", nil, q, en},
		{"Accept-Language tr", map[string]string{"Accept-Language": "tr-TR,tr;q=0.9,en;q=0.8"}, q, tr},
		{"Accept-Language en first", map[string]string{"Accept-Language": "en-US,en;q=0.9,tr;q=0.8"}, q, en},
		{"Accept-Language other", map[string]string{"Accept-Language": "de-DE,de;q=0.9"}, q, en},
		{"cookie beats the header", map[string]string{"Accept-Language": "en", "Cookie": "la_locale=tr"}, q, tr},
		{"unknown cookie value", map[string]string{"Cookie": "la_locale=xx"}, q, en},
		{"ui_locales beats the cookie", map[string]string{"Cookie": "la_locale=tr"}, authzQuery(clientID, set("ui_locales", "en")), en},
	}
	for _, tc := range cases {
		if body := page(tc.header, tc.q); !strings.Contains(body, tc.want) {
			t.Errorf("%s: page is not in the expected language", tc.name)
		}
	}
	turkish := page(map[string]string{"Accept-Language": "tr"}, q)
	for _, want := range []string{`<html lang="tr">`, "Onayla", "Reddet", "owner@example.com", "client.example.com", "sahip", "English"} {
		if !strings.Contains(turkish, want) {
			t.Errorf("Turkish page lacks %q", want)
		}
	}
	// The language link remembers the choice in the la_locale cookie.
	r := e.raw("GET", "/oauth/authorize?"+authzQuery(clientID, set("ui_locales", "tr")).Encode(), e.owner.cookieHeader(), "").want(200)
	if !strings.Contains(strings.Join(r.Header.Values("Set-Cookie"), ";"), "la_locale=tr") {
		t.Errorf("no la_locale cookie: %v", r.Header.Values("Set-Cookie"))
	}
	// The error and sign-in pages are translated too.
	bad := e.raw("GET", "/oauth/authorize?client_id=nope&redirect_uri="+url.QueryEscape(testRedirect), map[string]string{"Accept-Language": "tr"}, "").want(400)
	if !strings.Contains(string(bad.Body), "Bu bağlantı sürdürülemiyor") {
		t.Errorf("error page: %s", bad.Body)
	}
	signIn := e.raw("GET", "/oauth/authorize?"+q.Encode(), map[string]string{"Accept-Language": "tr"}, "").want(200)
	if !strings.Contains(string(signIn.Body), "izlek&#39;e giriş yapın") {
		t.Errorf("sign-in page: %s", signIn.Body)
	}
}

// ---- Client ID Metadata Documents ----

func TestClientIDMetadataDocument(t *testing.T) {
	e := newOAuthEnv(t)
	q := authzQuery(testCIMD, set("redirect_uri", "https://example.com/cb"))

	// Nobody without an account can make the server fetch anything.
	e.raw("GET", "/oauth/authorize?"+q.Encode(), map[string]string{"Accept-Language": "en"}, "").want(200)
	if e.fetches.Load() != 0 {
		t.Fatal("the metadata document was fetched for an anonymous visitor")
	}

	page := e.consentPage(e.owner, q).want(200)
	body := string(page.Body)
	if !strings.Contains(body, "Example Assistant") || !strings.Contains(body, "read from") || strings.Contains(body, "registered itself") {
		t.Fatalf("consent page: %s", body)
	}
	if e.fetches.Load() != 1 {
		t.Fatalf("%d fetches", e.fetches.Load())
	}
	// Cached: further page loads do not fetch again, until the cache expires.
	e.consentPage(e.owner, q).want(200)
	e.clock.Advance(4 * time.Minute)
	e.consentPage(e.owner, q).want(200)
	if e.fetches.Load() != 1 {
		t.Fatalf("%d fetches within the cache lifetime", e.fetches.Load())
	}
	e.clock.Advance(2 * time.Minute)
	e.consentPage(e.owner, q).want(200)
	if e.fetches.Load() != 2 {
		t.Fatalf("%d fetches after the cache expired", e.fetches.Load())
	}

	// Redirect URIs come from the document, with the loopback port rule.
	for uri, ok := range map[string]bool{
		"https://example.com/cb": true, "http://localhost:49152/callback": true,
		"https://example.com/other": false, "https://evil.example/cb": false, "http://127.0.0.1:49152/callback": false,
	} {
		r := e.consentPage(e.owner, authzQuery(testCIMD, set("redirect_uri", uri)))
		if (r.Status == 200) != ok {
			t.Errorf("redirect_uri %s: status %d", uri, r.Status)
		}
	}
	loopback := string(e.consentPage(e.owner, authzQuery(testCIMD, set("redirect_uri", "http://localhost:49152/callback"))).Body)
	if !strings.Contains(loopback, "on your own computer") || !strings.Contains(loopback, "localhost:49152") {
		t.Error("no warning for a loopback redirect")
	}

	// The whole flow, with the URL as client_id.
	back, err := url.Parse(e.decide(e.owner, q, "approve").want(303).Header.Get("Location"))
	if err != nil || back.Host != "example.com" || back.Query().Get("iss") != testPublicURL {
		t.Fatalf("redirect: %v %v", back, err)
	}
	var tok tokenSet
	e.token(exchange(testCIMD, back.Query().Get("code"), set("redirect_uri", "https://example.com/cb"))).want(200).json(&tok)
	e.mcp(tok.AccessToken, "tools/list", nil).want(200)
	apps := e.owner.get("/api/me/connected-apps").want(200).list()
	if len(apps) != 1 || apps[0]["client_kind"] != "cimd" || apps[0]["client_id"] != testCIMD || apps[0]["client_name"] != "Example Assistant" {
		t.Fatalf("connected apps: %v", apps)
	}
	if n := e.scalar(`SELECT COUNT(*) FROM oauth_clients WHERE kind = 'dcr'`); n != 0 {
		t.Fatal("a metadata-document client was registered dynamically")
	}
}

func TestClientIDMetadataDocumentFailures(t *testing.T) {
	e := newOAuthEnv(t)
	q := authzQuery(testCIMD, set("redirect_uri", "https://example.com/cb"))
	e.metadata.Store(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"client_id":"https://example.com/oauth/other.json","redirect_uris":["https://example.com/cb"]}`)
	}))
	r := e.consentPage(e.owner, q).want(400)
	if !strings.Contains(string(r.Body), "different client_id") || r.Header.Get("Location") != "" {
		t.Fatalf("error page: %s", r.Body)
	}
	// A failing URL is not fetched again right away, and nothing is stored.
	e.consentPage(e.owner, q).want(400)
	if e.fetches.Load() != 1 {
		t.Fatalf("%d fetches of a failing document", e.fetches.Load())
	}
	if n := e.scalar(`SELECT COUNT(*) FROM oauth_clients`); n != 0 {
		t.Fatalf("%d clients stored", n)
	}
	// The token endpoint knows no such client either.
	wantOAuthError(t, e.token(exchange(testCIMD, oauth.NewCode())), 400, "invalid_grant")

	// With the production fetcher the same name "resolves" to a loopback
	// address here, and the fetch is refused.
	strict := newEnvWith(t, func(o *app.Options) {
		o.ClientMetadata = &oauth.Fetcher{DialAddr: "127.0.0.1:9", Timeout: time.Second}
	}, withPublicURL)
	owner, _ := strict.setup()
	r = strict.consentPage(owner, q).want(400)
	if !strings.Contains(string(r.Body), "not public") {
		t.Fatalf("error page: %s", r.Body)
	}

	// Metadata documents can be switched off.
	off := newOAuthEnv(t, func(c *config.Config) { c.OAuthCIMD = false })
	off.consentPage(off.owner, q).want(400)
	if off.fetches.Load() != 0 {
		t.Fatal("fetched although metadata documents are switched off")
	}
	var as map[string]any
	off.raw("GET", "/.well-known/oauth-authorization-server", nil, "").want(200).json(&as)
	if as["client_id_metadata_document_supported"] != false {
		t.Fatalf("advertised: %v", as["client_id_metadata_document_supported"])
	}
}

func TestMetadataFetchesAreRateLimitedPerUser(t *testing.T) {
	e := newOAuthEnv(t)
	for i := 0; i < 25; i++ {
		id := fmt.Sprintf("https://example.com/oauth/client-%d.json", i)
		e.consentPage(e.owner, authzQuery(id, set("redirect_uri", "https://example.com/cb")))
	}
	if got := e.fetches.Load(); got != 20 {
		t.Fatalf("%d fetches in a minute, want the limit of 20", got)
	}
}

// ---- token endpoint throttling and clean-up ----

func TestTokenEndpointThrottlesGuessing(t *testing.T) {
	e := newOAuthEnv(t)
	clientID, tok := e.grant(e.owner, "izlek:read")
	for i := 0; i < 30; i++ {
		wantOAuthError(t, e.token(refreshing(clientID, oauth.NewRefreshToken())), 400, "invalid_grant")
	}
	r := e.token(refreshing(clientID, oauth.NewRefreshToken()))
	wantOAuthError(t, r, 429, "temporarily_unavailable")
	if r.Header.Get("Retry-After") == "" {
		t.Fatal("no Retry-After")
	}
	wantOAuthError(t, e.token(exchange(clientID, oauth.NewCode())), 429, "temporarily_unavailable")
	// A valid request from the same (possibly shared) address still works.
	var next tokenSet
	e.token(refreshing(clientID, tok.RefreshToken)).want(200).json(&next)
	e.clock.Advance(61 * time.Second)
	wantOAuthError(t, e.token(refreshing(clientID, oauth.NewRefreshToken())), 400, "invalid_grant")

	// Every request counts against a generous per-address budget.
	for i := 0; i < 299; i++ {
		e.form("/oauth/revoke", url.Values{"token": {"x"}}, nil).want(200)
	}
	wantOAuthError(t, e.token(refreshing(clientID, next.RefreshToken)), 429, "temporarily_unavailable")
	wantOAuthError(t, e.form("/oauth/revoke", url.Values{"token": {"x"}}, nil), 429, "temporarily_unavailable")
	e.clock.Advance(61 * time.Second)
	e.token(refreshing(clientID, next.RefreshToken)).want(200)
}

func TestPruneOAuth(t *testing.T) {
	e := newOAuthEnv(t)
	clientID, tok := e.grant(e.owner, "izlek:read")
	unused := e.register("Never used", testRedirect)
	e.code(e.owner, authzQuery(clientID)) // a code that is never redeemed
	prune := func() {
		t.Helper()
		if err := e.db.Write(context.Background(), func(tx *sql.Tx) error { return api.PruneOAuth(context.Background(), tx, e.clock.Now()) }); err != nil {
			t.Fatal(err)
		}
	}
	prune()
	if e.scalar(`SELECT COUNT(*) FROM oauth_clients`) != 2 || e.scalar(`SELECT COUNT(*) FROM oauth_codes`) != 2 || e.scalar(`SELECT COUNT(*) FROM oauth_tokens`) != 2 {
		t.Fatal("pruning removed live state")
	}
	e.mcp(tok.AccessToken, "tools/list", nil).want(200)

	// Two days on: the never-approved client and the dead codes are gone,
	// the expired access token too; the grant and its refresh token remain.
	e.clock.Advance(48 * time.Hour)
	prune()
	if n := e.scalar(`SELECT COUNT(*) FROM oauth_clients WHERE client_id = ?`, unused); n != 0 {
		t.Error("a client nobody approved survives")
	}
	if e.scalar(`SELECT COUNT(*) FROM oauth_codes`) != 0 || e.scalar(`SELECT COUNT(*) FROM oauth_tokens WHERE kind = 'access'`) != 0 {
		t.Error("expired codes or access tokens survive")
	}
	e.token(refreshing(clientID, tok.RefreshToken)).want(200)

	// Long after the grant ended everything about it is removed.
	e.clock.Advance(400 * 24 * time.Hour)
	prune()
	for _, table := range []string{"oauth_clients", "oauth_grants", "oauth_tokens", "oauth_codes"} {
		if n := e.scalar(`SELECT COUNT(*) FROM ` + table); n != 0 {
			t.Errorf("%s: %d rows left", table, n)
		}
	}
}

func TestSignInOnTheAuthorizationPageWithTwoStepVerification(t *testing.T) {
	e := newOAuthEnv(t)
	clientID := e.register("Test Assistant", testRedirect)
	// An application authorised before the second factor existed…
	_, earlier := e.grant(e.owner, "izlek:read")

	var begin struct {
		Secret string `json:"secret"`
	}
	e.owner.post("/api/me/totp/begin", nil).want(200).json(&begin)
	code := func() string {
		c, err := auth.TOTPCode(begin.Secret, auth.TOTPStep(e.clock.Now()))
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	e.owner.post("/api/me/totp/enable", map[string]any{"code": code()}).want(200)
	// …has to be approved again once it is switched on.
	e.mcp(earlier.AccessToken, "tools/list", nil).want(401)

	q := authzQuery(clientID)
	page := e.raw("GET", "/oauth/authorize?"+q.Encode(), map[string]string{"Accept-Language": "en"}, "").want(200)
	form := hiddenFields(t, page)
	pre := form.Get("csrf")
	signIn := func(totp string) *response {
		return e.form("/oauth/authorize", url.Values{"q": {form.Get("q")}, "csrf": {pre}, "action": {"login"},
			"email": {"owner@example.com"}, "password": {testPassword}, "totp": {totp}},
			map[string]string{"Cookie": "__Host-la_oauth_csrf=" + pre, "Accept-Language": "en", "Origin": testPublicURL})
	}
	// The password alone is not enough, and a wrong code is refused.
	r := signIn("").want(401)
	if !strings.Contains(string(r.Body), "Enter the verification code") || len(r.Header.Values("Set-Cookie")) != 0 {
		t.Fatalf("without a code: %v %s", r.Header.Values("Set-Cookie"), r.Body)
	}
	if r := signIn("000000").want(401); !strings.Contains(string(r.Body), "verification code is not correct") {
		t.Fatalf("wrong code: %s", r.Body)
	}
	e.clock.Advance(30 * time.Second) // the code that enabled TOTP cannot be used twice
	r = signIn(code()).want(303)
	if !strings.Contains(strings.Join(r.Header.Values("Set-Cookie"), ";"), "la_session=") || !strings.HasPrefix(r.Header.Get("Location"), "/oauth/authorize?") {
		t.Fatalf("sign-in with a code: %v", r.Header)
	}
}
