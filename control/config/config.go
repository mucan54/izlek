// Package config reads the control plane's configuration from the
// environment (spec/control.md §11). The object-store variables
// (LA_STORE_URL, LA_S3_*) are read by golib/objstore.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the control plane configuration.
type Config struct {
	Listen      string // LA_CONTROL_LISTEN
	AdminListen string // LA_CONTROL_ADMIN_LISTEN ("" = /metrics on the main listener)
	DBPath      string // LA_CONTROL_DB

	QueryURL       string // LA_QUERY_URL
	InternalSecret string // LA_QUERY_INTERNAL_SECRET
	QueryTimeout   time.Duration

	UIDir          string // LA_UI_DIR ("" = no static UI)
	CookieSecure   bool   // LA_COOKIE_SECURE
	TrustedProxies int    // LA_TRUSTED_PROXIES

	PublishDebounce time.Duration // LA_CONTROL_PUBLISH_DEBOUNCE_MS
	RepublishEvery  time.Duration // LA_CONTROL_REPUBLISH_SECONDS
	DeletionCheck   time.Duration // LA_CONTROL_DELETION_CHECK_SECONDS

	LogLevel slog.Level // LA_LOG_LEVEL

	// PublicURL is the control plane's own public origin (LA_PUBLIC_URL),
	// e.g. "https://izlek.example.com": no path, no trailing slash. It is
	// the OAuth issuer and the base of the MCP resource identifier; OAuth
	// stays switched off while it is empty.
	PublicURL string
	// PublicURLIgnored is set instead of PublicURL when LA_PUBLIC_URL was a
	// plain-http address on a non-loopback host: the reason, for the start-up log.
	PublicURLIgnored string
	// PublicIngestURL is where SDKs send events (LA_PUBLIC_INGEST_URL).
	// Only used to fill in setup snippets.
	PublicIngestURL string
	// MCPEnabled serves the MCP endpoint at /mcp (LA_MCP_ENABLED).
	MCPEnabled bool
	// MCPAllowedOrigins is the normalised, comma-separated list of extra
	// browser origins that may call /mcp and the OAuth endpoints
	// (LA_MCP_ALLOWED_ORIGINS). Kept as a string so Config stays comparable.
	MCPAllowedOrigins string
	// OAuthDCR and OAuthCIMD select how OAuth clients may introduce
	// themselves (LA_OAUTH_CLIENT_REGISTRATION = "cimd,dcr").
	OAuthDCR  bool
	OAuthCIMD bool
}

// OAuthEnabled reports whether the built-in authorization server is on.
func (c Config) OAuthEnabled() bool { return c.MCPEnabled && c.PublicURL != "" }

// MCPResource is the canonical resource identifier of the MCP endpoint
// (RFC 8707 / RFC 9728); empty while PublicURL is not set.
func (c Config) MCPResource() string {
	if c.PublicURL == "" {
		return ""
	}
	return c.PublicURL + "/mcp"
}

// MCPOrigins returns the extra allowed browser origins.
func (c Config) MCPOrigins() []string {
	if c.MCPAllowedOrigins == "" {
		return nil
	}
	return strings.Split(c.MCPAllowedOrigins, ",")
}

// Defaults returns the configuration used when no variable is set.
func Defaults() Config {
	return Config{
		Listen:          ":8090",
		DBPath:          "/var/lib/la/control.db",
		QueryURL:        "http://127.0.0.1:8081",
		QueryTimeout:    120 * time.Second,
		CookieSecure:    true,
		PublishDebounce: 250 * time.Millisecond,
		RepublishEvery:  5 * time.Minute,
		DeletionCheck:   60 * time.Second,
		LogLevel:        slog.LevelInfo,
		MCPEnabled:      true,
		OAuthDCR:        true,
		OAuthCIMD:       true,
	}
}

// FromEnv reads the process environment.
func FromEnv() (Config, error) { return FromEnvFunc(os.Getenv) }

// FromEnvFunc is FromEnv with an injectable lookup. On error the returned
// Config still carries a usable log level.
func FromEnvFunc(getenv func(string) string) (Config, error) {
	c := Defaults()
	var errs []error
	str := func(name string, dst *string) {
		if v := getenv(name); v != "" {
			*dst = v
		}
	}
	boolean := func(name string, dst *bool) {
		v := strings.ToLower(strings.TrimSpace(getenv(name)))
		switch v {
		case "":
		case "1", "true", "yes", "on":
			*dst = true
		case "0", "false", "no", "off":
			*dst = false
		default:
			errs = append(errs, fmt.Errorf("%s: want true or false, got %q", name, v))
		}
	}
	integer := func(name string, lo, hi int) (int, bool) {
		v := strings.TrimSpace(getenv(name))
		if v == "" {
			return 0, false
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < lo || n > hi {
			errs = append(errs, fmt.Errorf("%s: want an integer between %d and %d, got %q", name, lo, hi, v))
			return 0, false
		}
		return n, true
	}

	if v := getenv("LA_LOG_LEVEL"); v != "" {
		if err := c.LogLevel.UnmarshalText([]byte(v)); err != nil {
			errs = append(errs, fmt.Errorf("LA_LOG_LEVEL: want debug, info, warn or error, got %q", v))
		}
	}
	str("LA_CONTROL_LISTEN", &c.Listen)
	str("LA_CONTROL_ADMIN_LISTEN", &c.AdminListen)
	str("LA_CONTROL_DB", &c.DBPath)
	str("LA_QUERY_URL", &c.QueryURL)
	str("LA_QUERY_INTERNAL_SECRET", &c.InternalSecret)
	str("LA_UI_DIR", &c.UIDir)
	boolean("LA_COOKIE_SECURE", &c.CookieSecure)
	if n, ok := integer("LA_TRUSTED_PROXIES", 0, 16); ok {
		c.TrustedProxies = n
	}
	if n, ok := integer("LA_CONTROL_QUERY_TIMEOUT_SECONDS", 1, 3600); ok {
		c.QueryTimeout = time.Duration(n) * time.Second
	}
	// The contract allows at most 2 s between a change and its publication.
	if n, ok := integer("LA_CONTROL_PUBLISH_DEBOUNCE_MS", 0, 2000); ok {
		c.PublishDebounce = time.Duration(n) * time.Millisecond
	}
	if n, ok := integer("LA_CONTROL_REPUBLISH_SECONDS", 1, 86400); ok {
		c.RepublishEvery = time.Duration(n) * time.Second
	}
	if n, ok := integer("LA_CONTROL_DELETION_CHECK_SECONDS", 1, 86400); ok {
		c.DeletionCheck = time.Duration(n) * time.Second
	}

	boolean("LA_MCP_ENABLED", &c.MCPEnabled)
	if v := strings.TrimSpace(getenv("LA_PUBLIC_URL")); v != "" {
		origin, err := publicOrigin(v, true)
		switch {
		case err == nil:
			c.PublicURL = origin
		case errors.Is(err, errPlainHTTP):
			// A hosting platform may hand over a plain-http address before TLS
			// is set up. Only OAuth needs this setting, so the control plane
			// starts without OAuth and says so, rather than not starting.
			c.PublicURLIgnored = err.Error()
		default:
			errs = append(errs, fmt.Errorf("LA_PUBLIC_URL: %w", err))
		}
	}
	if v := strings.TrimSpace(getenv("LA_PUBLIC_INGEST_URL")); v != "" {
		u, err := url.Parse(v)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			errs = append(errs, fmt.Errorf("LA_PUBLIC_INGEST_URL: want http(s)://host[:port][/path], got %q", v))
		}
		c.PublicIngestURL = strings.TrimRight(v, "/")
	}
	if v := strings.TrimSpace(getenv("LA_MCP_ALLOWED_ORIGINS")); v != "" {
		var origins []string
		for _, item := range strings.Split(v, ",") {
			if item = strings.TrimSpace(item); item == "" {
				continue
			}
			origin, err := publicOrigin(item, false)
			if err != nil {
				errs = append(errs, fmt.Errorf("LA_MCP_ALLOWED_ORIGINS: %w", err))
				continue
			}
			origins = append(origins, origin)
		}
		c.MCPAllowedOrigins = strings.Join(origins, ",")
	}
	if v := strings.ToLower(strings.TrimSpace(getenv("LA_OAUTH_CLIENT_REGISTRATION"))); v != "" {
		c.OAuthDCR, c.OAuthCIMD = false, false
		for _, item := range strings.Split(v, ",") {
			switch strings.TrimSpace(item) {
			case "dcr":
				c.OAuthDCR = true
			case "cimd":
				c.OAuthCIMD = true
			case "none", "":
			default:
				errs = append(errs, fmt.Errorf("LA_OAUTH_CLIENT_REGISTRATION: want a comma-separated list of cimd, dcr (or none), got %q", v))
			}
		}
	}

	if c.QueryURL != "" {
		u, err := url.Parse(c.QueryURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			errs = append(errs, fmt.Errorf("LA_QUERY_URL: want http(s)://host[:port], got %q", c.QueryURL))
		}
		c.QueryURL = strings.TrimRight(c.QueryURL, "/")
	}
	if c.UIDir != "" {
		if fi, err := os.Stat(c.UIDir); err != nil || !fi.IsDir() {
			errs = append(errs, fmt.Errorf("LA_UI_DIR: %q is not a directory", c.UIDir))
		}
	}
	return c, errors.Join(errs...)
}

// publicOrigin validates and normalises a browser-style origin
// (scheme://host[:port], lower-cased, no path). With requireTLS, plain http
// is accepted for loopback hosts only: an OAuth issuer that is reachable
// over the network must be served over HTTPS.
// errPlainHTTP: a public URL that would carry OAuth tokens without TLS.
var errPlainHTTP = errors.New("must use https (plain http is accepted for localhost only)")

func publicOrigin(v string, requireTLS bool) (string, error) {
	u, err := url.Parse(strings.TrimRight(v, "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil ||
		u.Path != "" || u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(u.Host, "*, ") {
		return "", fmt.Errorf("want http(s)://host[:port] without a path, got %q", v)
	}
	if requireTLS && u.Scheme == "http" && !IsLoopbackHost(u.Hostname()) {
		return "", fmt.Errorf("%q: %w", v, errPlainHTTP)
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host), nil
}

// IsLoopbackHost reports whether host (without port) names this machine:
// "localhost" or a loopback IP literal.
func IsLoopbackHost(host string) bool {
	host = strings.ToLower(strings.Trim(host, "[]"))
	if host == "localhost" {
		return true
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		return ip.Unmap().IsLoopback()
	}
	return false
}
