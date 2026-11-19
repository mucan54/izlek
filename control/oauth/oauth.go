// Package oauth holds the protocol rules of the control plane's built-in
// OAuth 2.1 authorization server that do not need the database or HTTP
// handlers: scopes, token formats, PKCE, redirect URI validation and
// matching, client metadata validation (RFC 7591) and the guarded fetch of
// Client ID Metadata Documents.
//
// The server only supports what the MCP authorization specification asks
// for: the authorization code grant with PKCE (S256), public clients, and
// refresh tokens that rotate on every use.
package oauth

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/mucan54/izlek/control/auth"
)

// Scopes. Admin implies read.
const (
	ScopeRead  = "izlek:read"
	ScopeAdmin = "izlek:admin"
)

// AllScopes is what the server advertises, weakest first.
var AllScopes = []string{ScopeRead, ScopeAdmin}

// ParseScopes splits a space-separated scope parameter and keeps the scopes
// this server knows, in canonical order and without duplicates. Unknown
// scopes (offline_access, openid, …) are dropped: the token response states
// what was actually granted.
func ParseScopes(param string) []string {
	var read, admin bool
	for _, s := range strings.Fields(param) {
		switch s {
		case ScopeRead:
			read = true
		case ScopeAdmin:
			admin = true
		}
	}
	out := []string{}
	if read || admin { // admin implies read; keep it explicit in what is granted
		out = append(out, ScopeRead)
	}
	if admin {
		out = append(out, ScopeAdmin)
	}
	return out
}

// HasScope reports whether granted covers want.
func HasScope(granted []string, want string) bool {
	for _, s := range granted {
		if s == want || (want == ScopeRead && s == ScopeAdmin) {
			return true
		}
	}
	return false
}

// Subset reports whether every scope of sub is covered by super.
func Subset(sub, super []string) bool {
	for _, s := range sub {
		if !HasScope(super, s) {
			return false
		}
	}
	return true
}

// ---- secrets ----

// Prefixes make the kind of a credential obvious in configuration files and
// let secret scanners recognise leaked values.
const (
	AccessPrefix  = "oat_"
	RefreshPrefix = "ort_"
	CodePrefix    = "oac_"
	ClientPrefix  = "oc_"

	secretChars = 40 // base62: about 238 bits
	clientChars = 24
)

// NewAccessToken, NewRefreshToken and NewCode return fresh random secrets.
// Only their SHA-256 (auth.HashSecret) is ever stored.
func NewAccessToken() string  { return AccessPrefix + auth.RandomBase62(secretChars) }
func NewRefreshToken() string { return RefreshPrefix + auth.RandomBase62(secretChars) }
func NewCode() string         { return CodePrefix + auth.RandomBase62(secretChars) }

// NewClientID returns the identifier of a dynamically registered client. It
// is not a secret.
func NewClientID() string { return ClientPrefix + auth.RandomBase62(clientChars) }

func shaped(s, prefix string, n int) bool {
	if len(s) != len(prefix)+n || s[:len(prefix)] != prefix {
		return false
	}
	for i := len(prefix); i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') {
			return false
		}
	}
	return true
}

// LooksLikeAccessToken etc. report whether s has the shape of the secret, so
// that malformed input never reaches the database.
func LooksLikeAccessToken(s string) bool  { return shaped(s, AccessPrefix, secretChars) }
func LooksLikeRefreshToken(s string) bool { return shaped(s, RefreshPrefix, secretChars) }
func LooksLikeCode(s string) bool         { return shaped(s, CodePrefix, secretChars) }
func LooksLikeClientID(s string) bool     { return shaped(s, ClientPrefix, clientChars) }

// ---- PKCE (RFC 7636), S256 only ----

func unreserved(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '-' || c == '.' || c == '_' || c == '~') {
			return false
		}
	}
	return true
}

// ValidChallenge reports whether c can be an S256 code challenge: the
// unpadded base64url encoding of 32 bytes.
func ValidChallenge(c string) bool {
	if len(c) != 43 {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(c)
	return err == nil && len(raw) == sha256.Size
}

// VerifyS256 checks a code verifier against the stored challenge in
// constant time.
func VerifyS256(verifier, challenge string) bool {
	if len(verifier) < 43 || len(verifier) > 128 || !unreserved(verifier) {
		return false
	}
	sum := sha256.Sum256([]byte(verifier))
	got := base64.RawURLEncoding.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(got), []byte(challenge)) == 1
}

// ---- redirect URIs ----

// Limits on client metadata.
const (
	MaxRedirectURIs   = 20
	MaxRedirectURILen = 2048
	MaxClientNameLen  = 100
)

// isLoopbackHost reports whether host is "localhost" or a loopback literal.
func isLoopbackHost(host string) bool {
	host = strings.ToLower(strings.Trim(host, "[]"))
	if host == "localhost" {
		return true
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		return ip.Unmap().IsLoopback()
	}
	return false
}

// ValidateRedirectURI accepts an absolute https URI, or an http URI whose
// host is the loopback interface (native clients, RFC 8252 §7.3). Fragments
// and credentials are refused; so is every other scheme — in particular
// javascript:, data: and custom schemes, which a browser would follow
// without the user seeing where the authorization code goes.
func ValidateRedirectURI(raw string) error {
	if raw == "" || len(raw) > MaxRedirectURILen {
		return errors.New("redirect URI is empty or too long")
	}
	for _, r := range raw {
		if r <= ' ' || r == 0x7f || r > unicode.MaxASCII {
			return errors.New("redirect URI must be printable ASCII without spaces")
		}
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Opaque != "" {
		return errors.New("redirect URI must be an absolute URL")
	}
	if u.User != nil {
		return errors.New("redirect URI must not contain credentials")
	}
	if u.Fragment != "" || strings.Contains(raw, "#") {
		return errors.New("redirect URI must not contain a fragment")
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !isLoopbackHost(u.Hostname()) {
			return errors.New("redirect URI must use https (http is accepted for localhost only)")
		}
	default:
		return errors.New("redirect URI must use https, or http on localhost")
	}
	if u.Hostname() == "" {
		return errors.New("redirect URI has no host")
	}
	return nil
}

// IsLoopbackRedirect reports whether a (valid) redirect URI points at the
// user's own machine.
func IsLoopbackRedirect(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "http" && isLoopbackHost(u.Hostname())
}

// RedirectHost is the host shown on the consent screen.
func RedirectHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Host
}

// MatchRedirectURI reports whether requested is one of the registered
// redirect URIs. Comparison is exact string equality, with the single
// exception RFC 8252 §7.3 requires: for an http loopback URI the port is
// ignored, because native clients listen on an ephemeral port.
func MatchRedirectURI(registered []string, requested string) bool {
	if ValidateRedirectURI(requested) != nil {
		return false
	}
	for _, reg := range registered {
		if reg == requested {
			return true
		}
	}
	if !IsLoopbackRedirect(requested) {
		return false
	}
	want, _ := url.Parse(requested)
	for _, reg := range registered {
		if !IsLoopbackRedirect(reg) {
			continue
		}
		have, err := url.Parse(reg)
		if err != nil {
			continue
		}
		if strings.EqualFold(have.Hostname(), want.Hostname()) && have.EscapedPath() == want.EscapedPath() && have.RawQuery == want.RawQuery {
			return true
		}
	}
	return false
}

// ---- client metadata (RFC 7591 §2) ----

// Metadata is the subset of client metadata this server reads, from a
// registration request or from a Client ID Metadata Document.
type Metadata struct {
	ClientID                 string   `json:"client_id,omitempty"`
	ClientName               string   `json:"client_name,omitempty"`
	ClientURI                string   `json:"client_uri,omitempty"`
	RedirectURIs             []string `json:"redirect_uris"`
	GrantTypes               []string `json:"grant_types,omitempty"`
	ResponseTypes            []string `json:"response_types,omitempty"`
	TokenEndpointAuthMethod  string   `json:"token_endpoint_auth_method,omitempty"`
	TokenEndpointAuthMethods []string `json:"token_endpoint_auth_methods_supported,omitempty"`
	ClientSecret             string   `json:"client_secret,omitempty"`
}

// CleanName makes a client-supplied display name safe to show: control and
// formatting characters are dropped, whitespace is collapsed and the length
// is bounded. Markup is harmless (pages escape it) but invisible characters
// could be used to imitate another name.
func CleanName(name string) string {
	var b strings.Builder
	space := false
	n := 0
	for _, r := range name {
		if r == utf8.RuneError || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Co, r) {
			continue
		}
		if unicode.IsSpace(r) {
			space = b.Len() > 0
			continue
		}
		if space {
			b.WriteByte(' ')
			n++
			space = false
		}
		b.WriteRune(r)
		if n++; n >= MaxClientNameLen {
			break
		}
	}
	return b.String()
}

// Clean validates client metadata and returns what the server keeps: the
// display name, the client's home page and its redirect URIs. Every client
// is treated as a public client whatever authentication method it asks for;
// metadata that can only work with a method this server does not implement
// is refused.
func (m Metadata) Clean() (name, clientURI string, redirectURIs []string, err error) {
	if len(m.RedirectURIs) == 0 {
		return "", "", nil, errors.New("redirect_uris is required")
	}
	if len(m.RedirectURIs) > MaxRedirectURIs {
		return "", "", nil, fmt.Errorf("at most %d redirect_uris are accepted", MaxRedirectURIs)
	}
	seen := map[string]bool{}
	for _, u := range m.RedirectURIs {
		if err := ValidateRedirectURI(u); err != nil {
			return "", "", nil, fmt.Errorf("redirect_uris: %q: %w", truncate(u, 80), err)
		}
		if !seen[u] {
			seen[u] = true
			redirectURIs = append(redirectURIs, u)
		}
	}
	for _, g := range m.GrantTypes {
		// Other grant types a client lists for other servers are ignored;
		// only these two are ever served.
		if g == "implicit" || g == "password" {
			return "", "", nil, fmt.Errorf("grant type %q is not supported", g)
		}
	}
	for _, r := range m.ResponseTypes {
		if r != "code" {
			return "", "", nil, fmt.Errorf("response type %q is not supported", truncate(r, 40))
		}
	}
	name = CleanName(m.ClientName)
	if u, perr := url.Parse(m.ClientURI); perr == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && len(m.ClientURI) <= MaxRedirectURILen {
		clientURI = m.ClientURI
	}
	return name, clientURI, redirectURIs, nil
}

// AllowsPublic reports whether the metadata permits token requests without
// client authentication: no method named (RFC 7591 would default to
// client_secret_basic, but neither MCP nor CIMD clients mean that), "none",
// or "none" among the methods the client says it can use.
func (m Metadata) AllowsPublic() bool {
	if m.TokenEndpointAuthMethod == "" || m.TokenEndpointAuthMethod == "none" {
		return true
	}
	for _, method := range m.TokenEndpointAuthMethods {
		if method == "none" {
			return true
		}
	}
	return false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// SameResource compares two RFC 8707 resource identifiers: scheme and host
// case-insensitively, the rest exactly, ignoring one trailing slash.
func SameResource(a, b string) bool {
	norm := func(s string) (string, bool) {
		u, err := url.Parse(s)
		if err != nil || u.Scheme == "" || u.Host == "" || u.Fragment != "" || u.User != nil {
			return "", false
		}
		return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host) + strings.TrimRight(u.EscapedPath(), "/") + "?" + u.RawQuery, true
	}
	na, ok1 := norm(a)
	nb, ok2 := norm(b)
	return ok1 && ok2 && na == nb
}
