// Package config reads the control plane's configuration from the
// environment (spec/control.md §11). The object-store variables
// (LA_STORE_URL, LA_S3_*) are read by golib/objstore.
package config

import (
	"errors"
	"fmt"
	"log/slog"
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
