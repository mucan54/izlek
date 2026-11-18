package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/mucan54/izlek/golib/objstore"
)

// Feature-flag rulesets (spec/flags.md §1) are published by the control
// plane as flags/<token>.json. SDKs download them from <flagsHost>/flags/…,
// and flagsHost defaults to the ingestion host, so the node serves the file
// itself: a small in-memory copy per known token, refreshed from the store
// at most every Config.FlagsCache. A CDN in front can cache it further (the
// response carries ETag and Cache-Control).
const (
	rulesetFetchMax = 5 * time.Second
	rulesetMaxAge   = "public, max-age=60"
)

type rulesetEntry struct {
	body    string
	etag    string
	missing bool
	at      time.Time
}

type rulesetCache struct {
	mu sync.Mutex
	m  map[string]rulesetEntry
}

func (c *rulesetCache) get(token string) (rulesetEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[token]
	return e, ok
}

func (c *rulesetCache) put(token string, e rulesetEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = make(map[string]rulesetEntry)
	}
	c.m[token] = e
}

// rulesetToken returns the token of a /flags/<token>.json path.
func rulesetToken(path string) (string, bool) {
	rest, ok := strings.CutPrefix(path, "/flags/")
	if !ok {
		return "", false
	}
	tok, ok := strings.CutSuffix(rest, ".json")
	if !ok || tok == "" || strings.ContainsAny(tok, "/.") {
		return "", false
	}
	return tok, true
}

// handleRuleset answers GET/HEAD /flags/<token>.json. Unknown and disabled
// tokens never reach the store, so the cache is bounded by the token set.
func (s *Server) handleRuleset(r *http.Request, rc *reqCtx) reply {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return reply{status: http.StatusMethodNotAllowed}
	}
	if !s.tokens.Loaded() {
		return reply{status: http.StatusServiceUnavailable, retryAfter: retryTransient}
	}
	token, _ := rulesetToken(r.URL.Path)
	tok, ok := s.tokens.Lookup(token)
	if !ok || tok.Disabled {
		return reply{status: http.StatusNotFound}
	}
	if rc.origin != "" {
		if !tok.OriginAllowed(rc.origin) {
			rc.cors = corsDeny
			return reply{status: http.StatusForbidden}
		}
		rc.cors = corsAllow
	}

	e, cached := s.rulesets.get(token)
	if !cached || rc.recv.Sub(e.at) > s.cfg.FlagsCache {
		ctx, cancel := context.WithTimeout(r.Context(), rulesetFetchMax)
		data, _, err := s.store.Get(ctx, "flags/"+token+".json", nil)
		cancel()
		switch {
		case err == nil:
			sum := sha256.Sum256(data)
			e = rulesetEntry{body: string(data), etag: `"` + hex.EncodeToString(sum[:8]) + `"`, at: rc.recv}
			s.rulesets.put(token, e)
		case errors.Is(err, objstore.ErrNotFound):
			e = rulesetEntry{missing: true, at: rc.recv}
			s.rulesets.put(token, e)
		case cached:
			// Store trouble: keep serving the last copy, retry on the next request.
		default:
			return reply{status: http.StatusServiceUnavailable, retryAfter: retryTransient}
		}
	}
	if e.missing {
		return reply{status: http.StatusNotFound}
	}
	if inm := r.Header.Get("If-None-Match"); inm != "" && etagMatches(inm, e.etag) {
		return reply{status: http.StatusNotModified, etag: e.etag, cacheControl: rulesetMaxAge}
	}
	return reply{status: http.StatusOK, body: e.body, ctype: "application/json", etag: e.etag, cacheControl: rulesetMaxAge}
}

// etagMatches implements the weak comparison of If-None-Match.
func etagMatches(header, etag string) bool {
	for _, c := range strings.Split(header, ",") {
		c = strings.TrimSpace(c)
		if c == "*" || strings.TrimPrefix(c, "W/") == etag {
			return true
		}
	}
	return false
}
