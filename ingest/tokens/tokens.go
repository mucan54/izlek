// Package tokens is the ingestion token registry (design §6.7,
// spec/manifest.md §8): an in-memory map of write tokens loaded from
// control/tokens.json in object storage, refreshed by conditional GET, with
// a last-good copy on local disk so ingestion keeps working when the control
// plane or the store is unavailable.
package tokens

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mucan54/izlek/golib/objstore"
	"github.com/mucan54/izlek/ingest/ratelimit"
)

// Token is one entry of control/tokens.json.
type Token struct {
	ProjectID uint32
	Disabled  bool
	Replay    bool
	// Origins is the Origin allow-list (lower-cased, no trailing slash);
	// empty means "no Origin check".
	Origins []string
	// Limiter is the per-token request bucket (nil when rate limiting is off).
	Limiter *ratelimit.Bucket
}

// OriginAllowed reports whether a request with the given Origin header value
// may use this token.
func (t *Token) OriginAllowed(origin string) bool {
	if len(t.Origins) == 0 {
		return true
	}
	origin = normalizeOrigin(origin)
	for _, o := range t.Origins {
		if o == origin {
			return true
		}
	}
	return false
}

func normalizeOrigin(o string) string {
	return strings.TrimRight(strings.ToLower(strings.TrimSpace(o)), "/")
}

type snapshot struct {
	version   int64
	tokens    map[string]*Token
	origins   map[string]struct{} // union of all allow-lists
	anyOrigin bool                // some active token has no allow-list
}

// Options configures a Registry.
type Options struct {
	Store     objstore.Store
	Key       string // object key (default control/tokens.json)
	CachePath string // last-good copy on local disk ("" = none)
	// DefaultRate is the per-token request rate used when a token has no
	// rate_limit of its own; <= 0 disables per-token rate limiting for
	// those tokens.
	DefaultRate float64
	Logger      *slog.Logger
	Now         func() time.Time
}

// Registry holds the current token set.
type Registry struct {
	opt      Options
	cur      atomic.Pointer[snapshot]
	lastOK   atomic.Int64 // unix nanoseconds of the last successful store check
	failures atomic.Uint64

	mu   sync.Mutex // serializes Refresh
	etag string
}

// New returns an empty registry; call Load (or Refresh) to fill it.
func New(opt Options) *Registry {
	if opt.Key == "" {
		opt.Key = "control/tokens.json"
	}
	if opt.Logger == nil {
		opt.Logger = slog.New(slog.DiscardHandler)
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	return &Registry{opt: opt}
}

// Loaded reports whether any token set (from the store or the disk cache)
// is in effect. Until then requests cannot be authenticated and must be
// answered 503, never 401.
func (r *Registry) Loaded() bool { return r.cur.Load() != nil }

// Lookup returns the token entry for a write token.
func (r *Registry) Lookup(token string) (*Token, bool) {
	s := r.cur.Load()
	if s == nil {
		return nil, false
	}
	t, ok := s.tokens[token]
	return t, ok
}

// AnyTokenAllowsOrigin reports whether at least one active token could
// accept a request from origin. It answers CORS preflights, which carry no
// token.
func (r *Registry) AnyTokenAllowsOrigin(origin string) bool {
	s := r.cur.Load()
	if s == nil {
		return false
	}
	if s.anyOrigin {
		return true
	}
	_, ok := s.origins[normalizeOrigin(origin)]
	return ok
}

// Version returns the "version" field of the token set in effect (0 if none).
func (r *Registry) Version() int64 {
	if s := r.cur.Load(); s != nil {
		return s.version
	}
	return 0
}

// Count returns the number of tokens in effect.
func (r *Registry) Count() int {
	if s := r.cur.Load(); s != nil {
		return len(s.tokens)
	}
	return 0
}

// AgeSeconds is the time since the token set was last confirmed against the
// store (a 200 or a 304). After a start from the disk cache it is the age of
// the cache file. -1 means nothing has ever been loaded.
func (r *Registry) AgeSeconds() float64 {
	last := r.lastOK.Load()
	if last == 0 {
		return -1
	}
	return max(0, r.opt.Now().Sub(time.Unix(0, last)).Seconds())
}

// Failures returns the number of failed refresh attempts.
func (r *Registry) Failures() uint64 { return r.failures.Load() }

// Load performs the startup sequence: fetch from the store; if that fails,
// fall back to the last-good disk copy. It returns an error only when
// neither source yields a valid token set (the registry then stays empty and
// Run keeps retrying).
func (r *Registry) Load(ctx context.Context) error {
	err := r.Refresh(ctx)
	if err == nil {
		return nil
	}
	if r.opt.CachePath == "" {
		return err
	}
	raw, rerr := os.ReadFile(r.opt.CachePath)
	if rerr != nil {
		return fmt.Errorf("%w (and no last-good copy: %v)", err, rerr)
	}
	snap, perr := r.parse(raw)
	if perr != nil {
		return fmt.Errorf("%w (and last-good copy is invalid: %v)", err, perr)
	}
	r.cur.Store(snap)
	if fi, serr := os.Stat(r.opt.CachePath); serr == nil {
		r.lastOK.Store(fi.ModTime().UnixNano())
	}
	r.opt.Logger.Warn("token registry: store unavailable, using last-good copy",
		"path", r.opt.CachePath, "tokens", len(snap.tokens), "version", snap.version, "err", err)
	return nil
}

// Run polls the store every interval until ctx is done.
func (r *Registry) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			cctx, cancel := context.WithTimeout(ctx, min(interval, 30*time.Second))
			if err := r.Refresh(cctx); err != nil && ctx.Err() == nil {
				r.opt.Logger.Warn("token registry: refresh failed, keeping current set",
					"age_s", int(r.AgeSeconds()), "err", err)
			}
			cancel()
		}
	}
}

// Refresh does one conditional GET. A 304 only refreshes the age; a new,
// valid document replaces the token set and the disk copy; anything else
// (store error, missing object, invalid document) keeps the current set and
// returns an error.
func (r *Registry) Refresh(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	raw, obj, err := r.opt.Store.Get(ctx, r.opt.Key, &objstore.GetOptions{IfNoneMatch: r.etag})
	if errors.Is(err, objstore.ErrNotModified) {
		r.lastOK.Store(r.opt.Now().UnixNano())
		return nil
	}
	if err != nil {
		r.failures.Add(1)
		return fmt.Errorf("tokens: fetch %s: %w", r.opt.Key, err)
	}
	snap, err := r.parse(raw)
	if err != nil {
		r.failures.Add(1)
		return fmt.Errorf("tokens: %s: %w", r.opt.Key, err)
	}
	r.cur.Store(snap)
	r.etag = obj.ETag
	r.lastOK.Store(r.opt.Now().UnixNano())
	r.opt.Logger.Info("token registry: loaded", "tokens", len(snap.tokens), "version", snap.version)
	if r.opt.CachePath != "" {
		if err := writeFileAtomic(r.opt.CachePath, raw); err != nil {
			r.opt.Logger.Error("token registry: cannot write last-good copy", "path", r.opt.CachePath, "err", err)
		}
	}
	return nil
}

type fileDoc struct {
	Version int64                 `json:"version"`
	Tokens  map[string]*fileToken `json:"tokens"`
}

type fileToken struct {
	ProjectID      int64    `json:"project_id"`
	Status         string   `json:"status"`
	RateLimit      float64  `json:"rate_limit"`
	AllowedOrigins []string `json:"allowed_origins"`
	Replay         bool     `json:"replay"`
}

func (r *Registry) parse(raw []byte) (*snapshot, error) {
	var doc fileDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("invalid json: %w", err)
	}
	if doc.Tokens == nil {
		return nil, errors.New(`missing "tokens" object`)
	}
	prev := r.cur.Load()
	snap := &snapshot{version: doc.Version, tokens: make(map[string]*Token, len(doc.Tokens)), origins: map[string]struct{}{}}
	for name, ft := range doc.Tokens {
		if ft == nil || name == "" {
			return nil, fmt.Errorf("token %q: empty entry", name)
		}
		if ft.ProjectID < 1 || ft.ProjectID > 1<<32-1 {
			return nil, fmt.Errorf("token %q: project_id %d out of range", name, ft.ProjectID)
		}
		t := &Token{ProjectID: uint32(ft.ProjectID), Replay: ft.Replay}
		switch ft.Status {
		case "active":
		case "disabled":
			t.Disabled = true
		default:
			// Unknown status: fail closed for this token only.
			r.opt.Logger.Warn("token registry: unknown status, treating token as disabled", "status", ft.Status)
			t.Disabled = true
		}
		for _, o := range ft.AllowedOrigins {
			if o = normalizeOrigin(o); o != "" {
				t.Origins = append(t.Origins, o)
				if !t.Disabled {
					snap.origins[o] = struct{}{}
				}
			}
		}
		if len(t.Origins) == 0 && !t.Disabled {
			snap.anyOrigin = true
		}
		rate := ft.RateLimit
		if rate <= 0 {
			rate = r.opt.DefaultRate
		}
		if rate > 0 {
			// Keep the bucket across reloads so a reload is not a free refill.
			if prev != nil {
				if old := prev.tokens[name]; old != nil && old.Limiter != nil && old.Limiter.Rate() == rate {
					t.Limiter = old.Limiter
				}
			}
			if t.Limiter == nil {
				t.Limiter = ratelimit.NewBucket(rate, 0)
			}
		}
		snap.tokens[name] = t
	}
	return snap, nil
}

func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}
