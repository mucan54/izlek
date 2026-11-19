package oauth

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Client ID Metadata Documents (draft-ietf-oauth-client-id-metadata-document):
// the client_id is an https URL, and the document behind it describes the
// client. Fetching a URL chosen by whoever starts an authorization request is
// a server-side request forgery risk, so the fetch is fenced in:
//
//   - https only, default port only, a DNS name (no IP literal), no
//     credentials, no fragment, no dot segments;
//   - the address actually connected to is checked after DNS resolution, at
//     connect time, and must be a public unicast address — a name that
//     resolves (or re-resolves) to a private address is refused;
//   - no proxy, no redirects, no cookies;
//   - a short overall timeout and a small response size limit;
//   - the response must be 200 application/json and name the same client_id.

// Limits of a metadata fetch.
const (
	DefaultFetchTimeout  = 5 * time.Second
	DefaultFetchMaxBytes = 64 << 10
	MaxClientIDLen       = 2048

	minCacheFor     = time.Minute
	defaultCacheFor = 5 * time.Minute
	maxCacheFor     = time.Hour
)

// IsClientIDURL reports whether a client_id is meant as a metadata document
// URL (as opposed to an identifier issued by POST /oauth/register).
func IsClientIDURL(clientID string) bool {
	return strings.HasPrefix(clientID, "https://") || strings.HasPrefix(clientID, "http://")
}

// ValidateClientIDURL checks the syntactic rules for a metadata document
// URL.
func ValidateClientIDURL(raw string) (*url.URL, error) {
	if len(raw) > MaxClientIDLen {
		return nil, errors.New("client_id URL is too long")
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] <= ' ' || raw[i] >= 0x7f {
			return nil, errors.New("client_id URL must be printable ASCII")
		}
	}
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" {
		return nil, errors.New("client_id is not a valid URL")
	}
	if u.Scheme != "https" {
		return nil, errors.New("client_id URL must use https")
	}
	if u.User != nil {
		return nil, errors.New("client_id URL must not contain credentials")
	}
	if u.Fragment != "" || strings.Contains(raw, "#") {
		return nil, errors.New("client_id URL must not contain a fragment")
	}
	host := u.Hostname()
	if host == "" {
		return nil, errors.New("client_id URL has no host")
	}
	if _, err := netip.ParseAddr(host); err == nil || strings.Contains(host, ":") {
		return nil, errors.New("client_id URL must use a DNS name, not an IP address")
	}
	if strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".localhost") || !strings.Contains(strings.Trim(host, "."), ".") {
		return nil, errors.New("client_id URL must use a public DNS name")
	}
	if port := u.Port(); port != "" && port != "443" {
		return nil, errors.New("client_id URL must use the default https port")
	}
	if u.Path == "" || u.Path == "/" {
		return nil, errors.New("client_id URL must contain a path")
	}
	for _, seg := range strings.Split(u.EscapedPath(), "/") {
		if seg == "." || seg == ".." || strings.EqualFold(seg, "%2e") || strings.EqualFold(seg, "%2e%2e") {
			return nil, errors.New("client_id URL must not contain dot segments")
		}
	}
	return u, nil
}

// reserved are the address ranges a metadata fetch must never reach, on top
// of what netip classifies itself (loopback, private, link-local,
// multicast, unspecified).
var reserved = func() []netip.Prefix {
	var out []netip.Prefix
	for _, p := range []string{
		"0.0.0.0/8",       // "this network"
		"100.64.0.0/10",   // carrier-grade NAT (also used inside clouds)
		"192.0.0.0/24",    // IETF protocol assignments
		"192.0.2.0/24",    // documentation
		"198.18.0.0/15",   // benchmarking
		"198.51.100.0/24", // documentation
		"203.0.113.0/24",  // documentation
		"240.0.0.0/4",     // reserved, includes broadcast
		"64:ff9b::/96",    // NAT64: embeds an IPv4 address
		"64:ff9b:1::/48",  // local-use NAT64
		"2001::/32",       // Teredo: embeds an IPv4 address
		"2001:db8::/32",   // documentation
		"2002::/16",       // 6to4: embeds an IPv4 address
		"fc00::/7",        // unique local
		"fec0::/10",       // site-local (deprecated)
	} {
		out = append(out, netip.MustParsePrefix(p))
	}
	return out
}()

// PublicAddr reports whether addr is a public unicast address: the only
// kind a metadata fetch may connect to.
func PublicAddr(addr netip.Addr) bool {
	addr = addr.Unmap().WithZone("")
	if !addr.IsValid() || addr.IsLoopback() || addr.IsPrivate() || addr.IsUnspecified() || addr.IsMulticast() ||
		addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() || addr.IsInterfaceLocalMulticast() {
		return false
	}
	for _, p := range reserved {
		if p.Contains(addr) {
			return false
		}
	}
	return true
}

// ErrAddrNotAllowed is returned (wrapped) when a fetch would connect to an
// address that is not public.
var ErrAddrNotAllowed = errors.New("oauth: destination address is not allowed")

// Fetcher downloads Client ID Metadata Documents. The zero value is the
// production configuration; the fields exist so that tests can point it at
// a local TLS server.
type Fetcher struct {
	Timeout  time.Duration
	MaxBytes int64
	// AllowAddr decides whether a resolved address may be connected to
	// (nil: PublicAddr).
	AllowAddr func(netip.Addr) bool
	// DialAddr, when set, is connected to instead of the URL's host
	// ("127.0.0.1:8443"): tests serve documents for a public-looking name
	// from a local listener. AllowAddr still applies to it.
	DialAddr string
	// TLSConfig overrides the TLS client configuration (tests).
	TLSConfig *tls.Config
}

// Document is a validated metadata document.
type Document struct {
	Metadata Metadata
	// CacheFor is how long the document may be reused, from the response's
	// Cache-Control max-age, clamped to [1 minute, 1 hour].
	CacheFor time.Duration
}

func (f *Fetcher) client() *http.Client {
	allow := f.AllowAddr
	if allow == nil {
		allow = PublicAddr
	}
	timeout := f.Timeout
	if timeout <= 0 {
		timeout = DefaultFetchTimeout
	}
	dialer := &net.Dialer{
		Timeout: timeout,
		// Control runs after name resolution, immediately before connect(2),
		// with the address that will really be used. Checking here (rather
		// than resolving first and connecting later) leaves no window for a
		// DNS answer to change between the check and the use.
		Control: func(network, address string, _ syscall.RawConn) error {
			ap, err := netip.ParseAddrPort(address)
			if err != nil || !allow(ap.Addr()) {
				return fmt.Errorf("%w: %s", ErrAddrNotAllowed, address)
			}
			return nil
		},
	}
	tlsCfg := f.TLSConfig
	if tlsCfg == nil {
		tlsCfg = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy: nil, // a proxy would hide the destination address from the check above
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				if f.DialAddr != "" {
					addr = f.DialAddr
				}
				return dialer.DialContext(ctx, network, addr)
			},
			TLSClientConfig:        tlsCfg,
			TLSHandshakeTimeout:    timeout,
			ResponseHeaderTimeout:  timeout,
			DisableKeepAlives:      true,
			MaxResponseHeaderBytes: 16 << 10,
			ForceAttemptHTTP2:      true,
		},
		// A redirect could lead anywhere, including to another client's
		// document; the draft tells servers not to follow them.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// Fetch downloads and validates the metadata document at clientID.
func (f *Fetcher) Fetch(ctx context.Context, clientID string) (*Document, error) {
	u, err := ValidateClientIDURL(clientID)
	if err != nil {
		return nil, err
	}
	timeout := f.Timeout
	if timeout <= 0 {
		timeout = DefaultFetchTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "izlek-control (OAuth client metadata fetch)")
	client := f.client()
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		if errors.Is(err, ErrAddrNotAllowed) {
			return nil, fmt.Errorf("client metadata host resolves to an address that is not public: %w", ErrAddrNotAllowed)
		}
		return nil, errors.New("client metadata document could not be fetched")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("client metadata document answered HTTP %d (redirects are not followed)", resp.StatusCode)
	}
	if mt, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type")); err != nil || (mt != "application/json" && !strings.HasSuffix(mt, "+json")) {
		return nil, errors.New("client metadata document is not served as application/json")
	}
	limit := f.MaxBytes
	if limit <= 0 {
		limit = DefaultFetchMaxBytes
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, errors.New("client metadata document could not be read")
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("client metadata document is larger than %d bytes", limit)
	}
	var md Metadata
	if err := json.Unmarshal(raw, &md); err != nil {
		return nil, errors.New("client metadata document is not valid JSON")
	}
	// The document must name exactly the URL it was fetched from: otherwise
	// any page that can host a JSON file could speak for another client.
	if md.ClientID != clientID {
		return nil, errors.New("client metadata document names a different client_id")
	}
	if md.ClientSecret != "" {
		return nil, errors.New("client metadata document must not contain a client secret")
	}
	if !md.AllowsPublic() {
		return nil, errors.New("client requires an authentication method this server does not support (only public clients, token_endpoint_auth_method \"none\")")
	}
	if _, _, _, err := md.Clean(); err != nil {
		return nil, fmt.Errorf("client metadata document: %w", err)
	}
	return &Document{Metadata: md, CacheFor: cacheFor(resp.Header.Get("Cache-Control"))}, nil
}

// cacheFor reads max-age from a Cache-Control header.
func cacheFor(header string) time.Duration {
	d := defaultCacheFor
	for _, part := range strings.Split(header, ",") {
		part = strings.ToLower(strings.TrimSpace(part))
		if part == "no-store" || part == "no-cache" {
			return minCacheFor
		}
		if v, ok := strings.CutPrefix(part, "max-age="); ok {
			if n, err := strconv.Atoi(strings.Trim(v, `"`)); err == nil && n >= 0 {
				d = time.Duration(n) * time.Second
			}
		}
	}
	return min(max(d, minCacheFor), maxCacheFor)
}
