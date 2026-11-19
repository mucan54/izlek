package oauth

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestScopes(t *testing.T) {
	for param, want := range map[string][]string{
		"":                                     {},
		"izlek:read":                           {ScopeRead},
		"izlek:admin":                          {ScopeRead, ScopeAdmin}, // admin implies read
		"izlek:admin izlek:read":               {ScopeRead, ScopeAdmin},
		"  izlek:read   offline_access openid": {ScopeRead},
		"offline_access":                       {},
		"IZLEK:ADMIN izlek:admin.extra":        {},
	} {
		if got := ParseScopes(param); !reflect.DeepEqual(got, want) {
			t.Errorf("ParseScopes(%q) = %v, want %v", param, got, want)
		}
	}
	if !HasScope([]string{ScopeAdmin}, ScopeRead) || HasScope([]string{ScopeRead}, ScopeAdmin) || HasScope(nil, ScopeRead) {
		t.Error("scope hierarchy: admin covers read, read does not cover admin")
	}
	if !Subset([]string{ScopeRead}, []string{ScopeRead, ScopeAdmin}) || Subset([]string{ScopeRead, ScopeAdmin}, []string{ScopeRead}) {
		t.Error("Subset")
	}
}

func TestSecretsAreShapedAndDistinct(t *testing.T) {
	a, r, c, id := NewAccessToken(), NewRefreshToken(), NewCode(), NewClientID()
	if !LooksLikeAccessToken(a) || !LooksLikeRefreshToken(r) || !LooksLikeCode(c) || !LooksLikeClientID(id) {
		t.Fatalf("shapes: %q %q %q %q", a, r, c, id)
	}
	// A secret of one kind is never accepted as another.
	if LooksLikeAccessToken(r) || LooksLikeRefreshToken(a) || LooksLikeCode(a) || LooksLikeClientID(c) {
		t.Fatal("kinds are interchangeable")
	}
	if a == NewAccessToken() || len(a) != len(AccessPrefix)+40 {
		t.Fatalf("access token %q", a)
	}
	for _, bad := range []string{"", "oat_", a + "x", a[:len(a)-1], strings.Replace(a, a[5:6], "-", 1), "sk_" + a[4:]} {
		if LooksLikeAccessToken(bad) {
			t.Errorf("%q accepted as an access token", bad)
		}
	}
}

func challengeOf(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func TestPKCE(t *testing.T) {
	// RFC 7636 appendix B.
	const verifier, challenge = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk", "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"
	if !ValidChallenge(challenge) || !VerifyS256(verifier, challenge) {
		t.Fatal("RFC 7636 example rejected")
	}
	if VerifyS256(verifier+"x", challenge) || VerifyS256(verifier, challengeOf("another-verifier-another-verifier-another-ve")) {
		t.Fatal("wrong verifier accepted")
	}
	// The "plain" method is not supported: a verifier equal to the challenge fails.
	if VerifyS256(challenge, challenge) {
		t.Fatal("plain PKCE accepted")
	}
	long := strings.Repeat("a", 129)
	for _, bad := range []string{"", "short", long, strings.Repeat("a", 42) + "!", strings.Repeat("é", 43)} {
		if VerifyS256(bad, challengeOf(bad)) {
			t.Errorf("verifier %q accepted", bad)
		}
	}
	for _, bad := range []string{"", "abc", challenge + "=", challenge[:42], challenge[:42] + "+", strings.Repeat("A", 44)} {
		if ValidChallenge(bad) {
			t.Errorf("challenge %q accepted", bad)
		}
	}
}

func TestValidateRedirectURI(t *testing.T) {
	for _, ok := range []string{
		"https://claude.ai/api/mcp/auth_callback",
		"https://chatgpt.com/connector_platform_oauth_redirect",
		"https://app.example.com:8443/cb?x=1",
		"http://localhost/callback", "http://localhost:3118/callback",
		"http://127.0.0.1:33418/callback", "http://[::1]:9000/cb",
	} {
		if err := ValidateRedirectURI(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{
		"", "/relative", "example.com/cb", "https://",
		"http://example.com/cb",                // plain http off the loopback
		"http://localhost.example.com/cb",      // not localhost
		"http://127.0.0.1.example.com/cb",      // not a loopback literal
		"http://192.168.1.10/cb",               // private is not loopback
		"https://example.com/cb#fragment",      // fragment
		"https://example.com/cb#",              // empty fragment
		"https://user:pw@example.com/cb",       // credentials
		"javascript:alert(1)",                  // script
		"data:text/html,<script>1</script>",    // data
		"cursor://anysphere.cursor/oauth",      // custom scheme
		"https://example.com/cb\r\nSet-Cookie", // control characters
		"https://example.com/c b",              // space
		"https://exämple.com/cb",               // not ASCII
		"https:example.com",                    // opaque
		"https://example.com/" + strings.Repeat("a", MaxRedirectURILen),
	} {
		if err := ValidateRedirectURI(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestMatchRedirectURI(t *testing.T) {
	registered := []string{"https://claude.ai/api/mcp/auth_callback", "http://localhost/callback", "http://127.0.0.1/callback", "https://app.example.com/cb?tenant=1"}
	for uri, want := range map[string]bool{
		"https://claude.ai/api/mcp/auth_callback": true,
		"https://app.example.com/cb?tenant=1":     true,
		// Exact means exact.
		"https://claude.ai/api/mcp/auth_callback/":       false,
		"https://claude.ai/api/mcp/auth_callback?x=1":    false,
		"https://claude.ai/api/mcp/AUTH_CALLBACK":        false,
		"https://CLAUDE.ai/api/mcp/auth_callback":        false,
		"https://claude.ai:443/api/mcp/auth_callback":    false,
		"https://claude.ai.evil.example/api/mcp/auth_cb": false,
		"https://evil.example/api/mcp/auth_callback":     false,
		"https://app.example.com/cb?tenant=2":            false,
		"https://app.example.com/cb":                     false,
		"http://claude.ai/api/mcp/auth_callback":         false,
		// Loopback: any port, nothing else varies (RFC 8252 §7.3).
		"http://localhost:3118/callback":               true,
		"http://127.0.0.1:51234/callback":              true,
		"http://127.0.0.1/callback":                    true,
		"http://localhost:3118/other":                  false,
		"http://localhost:3118/callback?x":             false,
		"http://[::1]:3118/callback":                   false, // not registered
		"https://localhost:3118/callback":              false, // scheme differs
		"http://localhost.evil.example/callback":       false,
		"http://127.0.0.1:51234@evil.example/callback": false,
		"": false,
	} {
		if got := MatchRedirectURI(registered, uri); got != want {
			t.Errorf("MatchRedirectURI(%q) = %v, want %v", uri, got, want)
		}
	}
	// The port exception is for loopback only.
	if MatchRedirectURI([]string{"https://app.example.com/cb"}, "https://app.example.com:8443/cb") {
		t.Error("port ignored for a non-loopback redirect URI")
	}
}

func TestMetadataClean(t *testing.T) {
	name, uri, redirects, err := Metadata{
		ClientName: "  Claude‮​ \t Code\x00  ", ClientURI: "https://claude.ai",
		RedirectURIs:            []string{"http://localhost/callback", "http://localhost/callback", "https://claude.ai/cb"},
		GrantTypes:              []string{"authorization_code", "refresh_token", "urn:ietf:params:oauth:grant-type:jwt-bearer"},
		ResponseTypes:           []string{"code"},
		TokenEndpointAuthMethod: "client_secret_post", // replaced by "none": every client is public
	}.Clean()
	if err != nil {
		t.Fatal(err)
	}
	if name != "Claude Code" || uri != "https://claude.ai" || !reflect.DeepEqual(redirects, []string{"http://localhost/callback", "https://claude.ai/cb"}) {
		t.Fatalf("%q %q %v", name, uri, redirects)
	}
	if n := CleanName(strings.Repeat("x", 500)); len(n) != MaxClientNameLen {
		t.Fatalf("name length %d", len(n))
	}
	if _, uri, _, _ := (Metadata{RedirectURIs: []string{"https://a.example/cb"}, ClientURI: "javascript:alert(1)"}).Clean(); uri != "" {
		t.Fatalf("client_uri %q kept", uri)
	}
	many := make([]string, MaxRedirectURIs+1)
	for i := range many {
		many[i] = fmt.Sprintf("https://a.example/cb%d", i)
	}
	for what, md := range map[string]Metadata{
		"no redirect uris":   {},
		"too many":           {RedirectURIs: many},
		"http redirect":      {RedirectURIs: []string{"http://example.com/cb"}},
		"implicit grant":     {RedirectURIs: []string{"https://a.example/cb"}, GrantTypes: []string{"implicit"}},
		"password grant":     {RedirectURIs: []string{"https://a.example/cb"}, GrantTypes: []string{"password"}},
		"token response":     {RedirectURIs: []string{"https://a.example/cb"}, ResponseTypes: []string{"token"}},
		"one bad among good": {RedirectURIs: []string{"https://a.example/cb", "javascript:alert(1)"}},
	} {
		if _, _, _, err := md.Clean(); err == nil {
			t.Errorf("%s: accepted", what)
		}
	}
	// ChatGPT's published document prefers private_key_jwt but can be public.
	chatgpt := Metadata{TokenEndpointAuthMethod: "private_key_jwt", TokenEndpointAuthMethods: []string{"none", "private_key_jwt"}}
	if !chatgpt.AllowsPublic() || !(Metadata{}).AllowsPublic() || !(Metadata{TokenEndpointAuthMethod: "none"}).AllowsPublic() {
		t.Error("public clients rejected")
	}
	if (Metadata{TokenEndpointAuthMethod: "private_key_jwt"}).AllowsPublic() {
		t.Error("a client that can only use private_key_jwt accepted")
	}
}

func TestSameResource(t *testing.T) {
	const res = "https://izlek.example.com/mcp"
	for other, want := range map[string]bool{
		res:                               true,
		"https://izlek.example.com/mcp/":  true,
		"HTTPS://IZLEK.example.com/mcp":   true,
		"https://izlek.example.com":       false,
		"https://izlek.example.com/mcp2":  false,
		"https://izlek.example.com/MCP":   false,
		"https://other.example.com/mcp":   false,
		"http://izlek.example.com/mcp":    false,
		"https://izlek.example.com/mcp#x": false,
		"https://izlek.example.com/mcp?x": false,
		"izlek.example.com/mcp":           false,
		"":                                false,
	} {
		if got := SameResource(other, res); got != want {
			t.Errorf("SameResource(%q) = %v, want %v", other, got, want)
		}
	}
}

func TestValidateClientIDURL(t *testing.T) {
	for _, ok := range []string{
		"https://chatgpt.com/oauth/client.json",
		"https://claude.ai/oauth/claude-code-client-metadata",
		"https://app.example.com:443/client.json?v=2",
	} {
		if _, err := ValidateClientIDURL(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{
		"http://example.com/client.json",            // not https
		"https://example.com",                       // no path
		"https://example.com/",                      // root path
		"https://example.com/a/../client.json",      // dot segment
		"https://example.com/./client.json",         // dot segment
		"https://example.com/%2e%2e/client.json",    // encoded dot segment
		"https://example.com/client.json#frag",      // fragment
		"https://user:pw@example.com/client.json",   // credentials
		"https://example.com:8443/client.json",      // other port: port scanning
		"https://127.0.0.1/client.json",             // IP literal
		"https://169.254.169.254/latest/meta-data/", // cloud metadata service
		"https://[::1]/client.json",                 // IPv6 literal
		"https://2130706433/client.json",            // 127.0.0.1 as a number
		"https://localhost/client.json",             // local name
		"https://metadata/client.json",              // single-label name
		"https://db.localhost/client.json",          // .localhost
		"https://example.com/client.json\n",         // control character
		"https://exämple.com/client.json",           // not ASCII
		"https://example.com/" + strings.Repeat("a", MaxClientIDLen),
		"ftp://example.com/client.json", "//example.com/client.json", "",
	} {
		if _, err := ValidateClientIDURL(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestPublicAddr(t *testing.T) {
	for addr, want := range map[string]bool{
		"8.8.8.8": true, "160.79.104.10": true, "2606:4700:4700::1111": true,
		"127.0.0.1": false, "127.255.255.254": false, "::1": false,
		"10.0.0.5": false, "172.16.0.1": false, "172.31.255.255": false, "192.168.1.1": false,
		"169.254.169.254": false, // cloud metadata
		"100.64.0.1":      false, // carrier-grade NAT
		"0.0.0.0":         false, "0.1.2.3": false, "255.255.255.255": false, "240.0.0.1": false,
		"224.0.0.1": false, "ff02::1": false,
		"fe80::1": false, "fc00::1": false, "fd12:3456::1": false, "fec0::1": false,
		"::": false, "198.18.0.1": false, "192.0.2.1": false, "203.0.113.9": false, "192.0.0.8": false,
		"::ffff:127.0.0.1":   false, // IPv4-mapped loopback
		"::ffff:10.0.0.1":    false, // IPv4-mapped private
		"::ffff:8.8.8.8":     true,
		"64:ff9b::7f00:1":    false, // NAT64 of 127.0.0.1
		"2002:7f00:1::1":     false, // 6to4 of 127.0.0.1
		"2001:0:4136:e378::": false, // Teredo
		"2001:db8::1":        false,
	} {
		if got := PublicAddr(netip.MustParseAddr(addr)); got != want {
			t.Errorf("PublicAddr(%s) = %v, want %v", addr, got, want)
		}
	}
	if PublicAddr(netip.Addr{}) {
		t.Error("the zero address is public")
	}
	if PublicAddr(netip.MustParseAddr("fe80::1%eth0")) {
		t.Error("zoned link-local address is public")
	}
}

// metadataServer serves documents over TLS for the name example.com (the
// test certificate of net/http/httptest covers it).
type metadataServer struct {
	*httptest.Server
	hits atomic.Int64
}

const testClientID = "https://example.com/oauth/client.json"

func newMetadataServer(t *testing.T, h http.HandlerFunc) *metadataServer {
	t.Helper()
	m := &metadataServer{}
	m.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.hits.Add(1)
		h(w, r)
	}))
	t.Cleanup(m.Close)
	return m
}

// fetcher returns a Fetcher that dials the test server. allowLoopback
// stands in for "the name resolves to a public address".
func (m *metadataServer) fetcher(allowLoopback bool) *Fetcher {
	pool := x509.NewCertPool()
	pool.AddCert(m.Certificate())
	f := &Fetcher{DialAddr: m.Listener.Addr().String(), TLSConfig: &tls.Config{RootCAs: pool}, Timeout: 2 * time.Second}
	if allowLoopback {
		f.AllowAddr = func(netip.Addr) bool { return true }
	}
	return f
}

func serveJSON(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "public, max-age=300")
		fmt.Fprint(w, body)
	}
}

const goodDocument = `{"client_id":"https://example.com/oauth/client.json","client_name":"Example","client_uri":"https://example.com",
	"redirect_uris":["https://example.com/cb","http://localhost/callback"],"grant_types":["authorization_code","refresh_token"],
	"response_types":["code"],"token_endpoint_auth_method":"none"}`

func TestFetchDocument(t *testing.T) {
	var got http.Header
	m := newMetadataServer(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		if r.URL.Path != "/oauth/client.json" || r.Host != "example.com" {
			http.NotFound(w, r)
			return
		}
		serveJSON(goodDocument)(w, r)
	})
	doc, err := m.fetcher(true).Fetch(context.Background(), testClientID)
	if err != nil {
		t.Fatal(err)
	}
	if doc.Metadata.ClientName != "Example" || len(doc.Metadata.RedirectURIs) != 2 || doc.CacheFor != 5*time.Minute {
		t.Fatalf("%+v", doc)
	}
	// The request carries nothing about this server or its users.
	if got.Get("Cookie") != "" || got.Get("Authorization") != "" || got.Get("Accept") != "application/json" || !strings.HasPrefix(got.Get("User-Agent"), "izlek-control") {
		t.Fatalf("request headers: %v", got)
	}
}

func TestFetchRefusesPrivateAddresses(t *testing.T) {
	m := newMetadataServer(t, serveJSON(goodDocument))
	// The production policy: the name "resolves" to 127.0.0.1 here, and the
	// connection is refused before a single byte is sent.
	_, err := m.fetcher(false).Fetch(context.Background(), testClientID)
	if !errors.Is(err, ErrAddrNotAllowed) {
		t.Fatalf("err = %v, want ErrAddrNotAllowed", err)
	}
	if m.hits.Load() != 0 {
		t.Fatal("the server was contacted")
	}
	// The zero Fetcher refuses URLs that name local targets outright.
	for _, id := range []string{"https://localhost/client.json", "https://127.0.0.1/client.json", "https://169.254.169.254/latest/meta-data/iam", "http://example.com/client.json"} {
		if _, err := (&Fetcher{Timeout: time.Second}).Fetch(context.Background(), id); err == nil {
			t.Errorf("%s fetched", id)
		}
	}
}

func TestFetchDoesNotFollowRedirects(t *testing.T) {
	var internalHits atomic.Int64
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		internalHits.Add(1)
		serveJSON(goodDocument)(w, r)
	}))
	defer internal.Close()
	m := newMetadataServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, internal.URL+"/secret", http.StatusFound)
	})
	_, err := m.fetcher(true).Fetch(context.Background(), testClientID)
	if err == nil || !strings.Contains(err.Error(), "302") {
		t.Fatalf("err = %v", err)
	}
	if internalHits.Load() != 0 {
		t.Fatal("the redirect was followed")
	}
}

func TestFetchLimits(t *testing.T) {
	big := `{"client_id":"https://example.com/oauth/client.json","redirect_uris":["https://example.com/cb"],"pad":"` + strings.Repeat("x", DefaultFetchMaxBytes) + `"}`
	cases := map[string]struct {
		handler http.HandlerFunc
		want    string
	}{
		"too large": {serveJSON(big), "larger than"},
		"endless body": {func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			for i := 0; i < 1024; i++ {
				if _, err := w.Write([]byte(strings.Repeat(" ", 1024))); err != nil {
					return
				}
			}
		}, "larger than"},
		"slow": {func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
		}, "could not be fetched"},
		"not found":  {http.NotFound, "HTTP 404"},
		"html":       {func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "<html>") }, "application/json"},
		"not json":   {serveJSON("client_id=x"), "not valid JSON"},
		"other id":   {serveJSON(strings.Replace(goodDocument, "client.json", "other.json", 1)), "different client_id"},
		"no id":      {serveJSON(`{"redirect_uris":["https://example.com/cb"]}`), "different client_id"},
		"secret":     {serveJSON(strings.Replace(goodDocument, `"client_name"`, `"client_secret":"s","client_name"`, 1)), "client secret"},
		"jwt only":   {serveJSON(strings.Replace(goodDocument, `"none"`, `"private_key_jwt"`, 1)), "public clients"},
		"no uris":    {serveJSON(`{"client_id":"https://example.com/oauth/client.json"}`), "redirect_uris"},
		"http uri":   {serveJSON(`{"client_id":"https://example.com/oauth/client.json","redirect_uris":["http://evil.example/cb"]}`), "https"},
		"script uri": {serveJSON(`{"client_id":"https://example.com/oauth/client.json","redirect_uris":["javascript:alert(1)"]}`), "redirect"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			m := newMetadataServer(t, tc.handler)
			f := m.fetcher(true)
			f.Timeout = 500 * time.Millisecond
			start := time.Now()
			_, err := f.Fetch(context.Background(), testClientID)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
			if d := time.Since(start); d > 3*time.Second {
				t.Fatalf("took %v", d)
			}
		})
	}
}

func TestFetchVerifiesTLS(t *testing.T) {
	m := newMetadataServer(t, serveJSON(goodDocument))
	f := m.fetcher(true)
	f.TLSConfig = nil // system roots: the test certificate is not trusted
	if _, err := f.Fetch(context.Background(), testClientID); err == nil {
		t.Fatal("an untrusted certificate was accepted")
	}
	// Right certificate, wrong name.
	f = m.fetcher(true)
	if _, err := f.Fetch(context.Background(), "https://other.example.org/oauth/client.json"); err == nil {
		t.Fatal("a certificate for another name was accepted")
	}
}

func TestCacheFor(t *testing.T) {
	for header, want := range map[string]time.Duration{
		"":                           5 * time.Minute,
		"public, max-age=300":        5 * time.Minute,
		"max-age=0":                  time.Minute,
		"max-age=10":                 time.Minute,
		"max-age=86400":              time.Hour,
		"no-store":                   time.Minute,
		"private, no-cache":          time.Minute,
		"max-age=abc":                5 * time.Minute,
		"max-age=-5":                 5 * time.Minute,
		`s-maxage=10, max-age="900"`: 15 * time.Minute,
	} {
		if got := cacheFor(header); got != want {
			t.Errorf("cacheFor(%q) = %v, want %v", header, got, want)
		}
	}
}

// The metadata documents the two assistants publish, copied on 2026-11-19
// from https://claude.ai/oauth/claude-code-client-metadata and
// https://chatgpt.com/oauth/client.json. They must keep being acceptable.
func TestPublishedClientDocuments(t *testing.T) {
	docs := map[string]struct {
		body      string
		name      string
		redirects []string
	}{
		"https://claude.ai/oauth/claude-code-client-metadata": {
			`{"client_id":"https://claude.ai/oauth/claude-code-client-metadata","client_name":"Claude Code","client_uri":"https://claude.ai","redirect_uris":["http://localhost/callback","http://127.0.0.1/callback"],"grant_types":["authorization_code","refresh_token"],"response_types":["code"],"token_endpoint_auth_method":"none"}`,
			"Claude Code", []string{"http://localhost:3118/callback", "http://127.0.0.1:50123/callback"},
		},
		"https://chatgpt.com/oauth/client.json": {
			`{"client_id":"https://chatgpt.com/oauth/client.json","client_uri":"https://chatgpt.com/","redirect_uris":["https://chatgpt.com/connector_platform_oauth_redirect"],"token_endpoint_auth_method":"private_key_jwt","token_endpoint_auth_methods_supported":["none","private_key_jwt"],"grant_types":["authorization_code","refresh_token","urn:ietf:params:oauth:grant-type:jwt-bearer"],"response_types":["code"],"client_name":"ChatGPT","logo_uri":"https://persistent.oaistatic.com/sonic/misc/openai-logo.png","token_endpoint_auth_signing_alg":"RS256","jwks_uri":"https://chatgpt.com/oauth/jwks.json"}`,
			"ChatGPT", []string{"https://chatgpt.com/connector_platform_oauth_redirect"},
		},
	}
	for id, doc := range docs {
		if _, err := ValidateClientIDURL(id); err != nil {
			t.Errorf("%s: %v", id, err)
		}
		var md Metadata
		if err := json.Unmarshal([]byte(doc.body), &md); err != nil {
			t.Fatal(err)
		}
		name, _, registered, err := md.Clean()
		if err != nil || name != doc.name || md.ClientID != id || !md.AllowsPublic() {
			t.Errorf("%s: name %q, public %v, err %v", id, name, md.AllowsPublic(), err)
		}
		for _, uri := range doc.redirects {
			if !MatchRedirectURI(registered, uri) {
				t.Errorf("%s: redirect URI %s is not matched", id, uri)
			}
		}
		if MatchRedirectURI(registered, "https://evil.example/callback") || MatchRedirectURI(registered, "http://localhost.evil.example/callback") {
			t.Errorf("%s: a foreign redirect URI is matched", id)
		}
	}
}
