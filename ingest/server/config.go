// Package server is the ingestion HTTP service: the native /v1/e hot path,
// replay upload, the PostHog compatibility adapter, health and metrics.
package server

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Config is the ingestion configuration. See FromEnv for the environment
// variable behind every field and its default.
type Config struct {
	Listen      string // LA_LISTEN
	AdminListen string // LA_ADMIN_LISTEN ("" = /metrics on the main listener)
	NodeID      string // LA_NODE_ID

	WALDir           string        // LA_WAL_DIR
	SegmentBytes     int64         // LA_WAL_SEGMENT_BYTES
	SegmentAge       time.Duration // LA_WAL_SEGMENT_SECONDS
	FsyncInterval    time.Duration // LA_WAL_FSYNC_MS
	FsyncBytes       int           // LA_WAL_FSYNC_BYTES
	WALMaxLocalBytes int64         // LA_WAL_MAX_LOCAL_BYTES (0 = 70 % of the WAL filesystem)
	UploadWorkers    int           // LA_WAL_UPLOAD_CONCURRENCY

	TokensKey   string        // LA_TOKENS_KEY
	TokensPoll  time.Duration // LA_TOKENS_POLL_SECONDS
	FlagsCache  time.Duration // LA_FLAGS_CACHE_SECONDS: how long a served ruleset copy is reused
	TokensCache string        // LA_TOKENS_CACHE

	CompatPostHog  bool   // LA_COMPAT_POSTHOG
	GeoDB          string // LA_GEO_DB
	TrustedProxies int    // LA_TRUSTED_PROXIES

	RateToken float64 // LA_RATE_TOKEN (default requests/s per token; 0 = off)
	RateIP    float64 // LA_RATE_IP (requests/s per client IP; 0 = off)

	DedupeEntries int           // LA_DEDUPE_ENTRIES
	DedupeTTL     time.Duration // LA_DEDUPE_TTL_SECONDS
	MaxInflight   int           // LA_MAX_INFLIGHT

	DrainDelay            time.Duration // LA_DRAIN_SECONDS
	ShutdownUploadTimeout time.Duration // LA_SHUTDOWN_UPLOAD_SECONDS

	LogLevel slog.Level // LA_LOG_LEVEL
}

// DefaultConfig returns the defaults of design §6.9.
func DefaultConfig() Config {
	return Config{
		Listen:                ":8080",
		NodeID:                "ing-01",
		WALDir:                "/var/lib/la/wal",
		SegmentBytes:          64 << 20,
		SegmentAge:            10 * time.Second,
		FsyncInterval:         2 * time.Millisecond,
		FsyncBytes:            1 << 20,
		UploadWorkers:         2,
		TokensKey:             "control/tokens.json",
		TokensPoll:            30 * time.Second,
		FlagsCache:            30 * time.Second,
		CompatPostHog:         true,
		RateToken:             2000,
		RateIP:                50,
		DedupeEntries:         100_000,
		DedupeTTL:             10 * time.Minute,
		MaxInflight:           1024,
		DrainDelay:            5 * time.Second,
		ShutdownUploadTimeout: 60 * time.Second,
		LogLevel:              slog.LevelInfo,
	}
}

var nodeIDRe = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)

// FromEnv builds the configuration from the process environment.
func FromEnv() (Config, error) { return FromEnvFunc(os.Getenv) }

// FromEnvFunc is FromEnv with an injectable lookup. Unset or empty variables
// keep their default; malformed values are reported together.
func FromEnvFunc(getenv func(string) string) (Config, error) {
	c := DefaultConfig()
	var errs []error
	str := func(name string, dst *string) {
		if v := getenv(name); v != "" {
			*dst = v
		}
	}
	num := func(name string, min float64, set func(float64)) {
		v := getenv(name)
		if v == "" {
			return
		}
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f < min || f != f {
			errs = append(errs, fmt.Errorf("%s=%q: want a number >= %v", name, v, min))
			return
		}
		set(f)
	}
	seconds := func(name string, min float64, dst *time.Duration) {
		num(name, min, func(f float64) { *dst = time.Duration(f * float64(time.Second)) })
	}

	str("LA_LISTEN", &c.Listen)
	str("LA_ADMIN_LISTEN", &c.AdminListen)
	str("LA_NODE_ID", &c.NodeID)
	str("LA_WAL_DIR", &c.WALDir)
	num("LA_WAL_SEGMENT_BYTES", 1, func(f float64) { c.SegmentBytes = int64(f) })
	seconds("LA_WAL_SEGMENT_SECONDS", 0.001, &c.SegmentAge)
	num("LA_WAL_FSYNC_MS", 0, func(f float64) { c.FsyncInterval = time.Duration(f * float64(time.Millisecond)) })
	num("LA_WAL_FSYNC_BYTES", 1, func(f float64) { c.FsyncBytes = int(f) })
	num("LA_WAL_MAX_LOCAL_BYTES", 0, func(f float64) { c.WALMaxLocalBytes = int64(f) })
	num("LA_WAL_UPLOAD_CONCURRENCY", 1, func(f float64) { c.UploadWorkers = int(f) })
	str("LA_TOKENS_KEY", &c.TokensKey)
	seconds("LA_TOKENS_POLL_SECONDS", 0.001, &c.TokensPoll)
	seconds("LA_FLAGS_CACHE_SECONDS", 0.001, &c.FlagsCache)
	str("LA_TOKENS_CACHE", &c.TokensCache)
	if v := getenv("LA_COMPAT_POSTHOG"); v != "" {
		b, err := strconv.ParseBool(strings.ToLower(v))
		if err != nil {
			errs = append(errs, fmt.Errorf("LA_COMPAT_POSTHOG=%q: want true or false", v))
		}
		c.CompatPostHog = b
	}
	str("LA_GEO_DB", &c.GeoDB)
	num("LA_TRUSTED_PROXIES", 0, func(f float64) { c.TrustedProxies = int(f) })
	num("LA_RATE_TOKEN", 0, func(f float64) { c.RateToken = f })
	num("LA_RATE_IP", 0, func(f float64) { c.RateIP = f })
	num("LA_DEDUPE_ENTRIES", 1, func(f float64) { c.DedupeEntries = int(f) })
	seconds("LA_DEDUPE_TTL_SECONDS", 0, &c.DedupeTTL)
	num("LA_MAX_INFLIGHT", 1, func(f float64) { c.MaxInflight = int(f) })
	seconds("LA_DRAIN_SECONDS", 0, &c.DrainDelay)
	seconds("LA_SHUTDOWN_UPLOAD_SECONDS", 0, &c.ShutdownUploadTimeout)
	if v := getenv("LA_LOG_LEVEL"); v != "" {
		if err := c.LogLevel.UnmarshalText([]byte(v)); err != nil {
			errs = append(errs, fmt.Errorf("LA_LOG_LEVEL=%q: want debug, info, warn or error", v))
		}
	}
	if err := c.validate(); err != nil {
		errs = append(errs, err)
	}
	return c, errors.Join(errs...)
}

func (c *Config) validate() error {
	if !nodeIDRe.MatchString(c.NodeID) {
		return fmt.Errorf("LA_NODE_ID=%q: must match [a-z0-9-]{1,32}", c.NodeID)
	}
	if c.WALDir == "" {
		return errors.New("LA_WAL_DIR must not be empty")
	}
	return nil
}

func (c *Config) tokensCachePath() string {
	if c.TokensCache != "" {
		return c.TokensCache
	}
	return filepath.Join(c.WALDir, "tokens.json")
}
