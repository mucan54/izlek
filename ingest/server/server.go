package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mucan54/izlek/golib/objstore"
	"github.com/mucan54/izlek/ingest/dedupe"
	"github.com/mucan54/izlek/ingest/geoip"
	"github.com/mucan54/izlek/ingest/metrics"
	"github.com/mucan54/izlek/ingest/ratelimit"
	"github.com/mucan54/izlek/ingest/tokens"
	"github.com/mucan54/izlek/ingest/wal"
	"github.com/mucan54/izlek/ingest/wire"
)

// Retry-After values (seconds) for the retryable statuses.
const (
	retryRateLimited  = 1
	retryBackpressure = 5
	retryTransient    = 1
	retryNoTokens     = 5
)

// Server is one ingestion node.
type Server struct {
	cfg   Config
	log   *slog.Logger
	store objstore.Store

	tokens    *tokens.Registry
	geo       *geoip.DB
	ipLimiter *ratelimit.IPLimiter
	dedupe    *dedupe.Cache
	writer    *wal.Writer
	uploader  *wal.Uploader

	rulesets   rulesetCache
	localBytes atomic.Int64 // bytes of local, not yet uploaded WAL
	maxLocal   int64

	m        serverMetrics
	inflight chan struct{}
	errLog   *ratelimit.Bucket // caps 5xx log lines per second

	shuttingDown atomic.Bool
	reqPrefix    string
	reqSeq       atomic.Uint64

	bgCancel context.CancelFunc
	bgWG     sync.WaitGroup

	mu       sync.Mutex
	httpSrv  *http.Server
	adminSrv *http.Server

	// afterFsync, when set (tests only), runs after a native batch has been
	// fsynced and before the response is written.
	afterFsync func()
}

type serverMetrics struct {
	reg              *metrics.Registry
	requests         *metrics.RequestCounter
	requestSeconds   *metrics.Histogram
	bytesIn          *metrics.Counter
	recordsIn        *metrics.Counter
	fsyncSeconds     *metrics.Histogram
	segmentsUploaded *metrics.Counter
	uploadedBytes    *metrics.Counter
	uploadErrors     *metrics.Counter
	rateLimited      *metrics.Counter
	dupBatches       *metrics.Counter
	walWriteErrors   *metrics.Counter
	truncatedTails   *metrics.Counter
}

// New recovers the local WAL, starts the uploader, the WAL writer and the
// token poller, and returns a server ready to handle requests.
func New(cfg Config, store objstore.Store, log *slog.Logger) (*Server, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	s := &Server{
		cfg:      cfg,
		log:      log,
		store:    store,
		dedupe:   dedupe.New(cfg.DedupeEntries, cfg.DedupeTTL),
		inflight: make(chan struct{}, max(cfg.MaxInflight, 1)),
		errLog:   ratelimit.NewBucket(5, 5),
	}
	var rnd [4]byte
	rand.Read(rnd[:])
	s.reqPrefix = cfg.NodeID + "-" + hex.EncodeToString(rnd[:]) + "-"

	if cfg.GeoDB != "" {
		db, err := geoip.Open(cfg.GeoDB)
		if err != nil {
			return nil, err
		}
		s.geo = db
	}
	if cfg.RateIP > 0 {
		s.ipLimiter = ratelimit.NewIPLimiter(cfg.RateIP, 0)
	}
	s.initMetrics()

	// Crash recovery: seal what the previous process left behind and re-seed
	// the dedupe cache from it, so a batch whose acknowledgement was lost in
	// the crash is recognised when the client retries it.
	now := time.Now()
	rec, err := wal.Recover(cfg.WALDir, now, func(env wal.Envelope, body []byte) {
		if env.Format != wal.FormatNative {
			return
		}
		at := time.UnixMilli(int64(env.RecvTSMs))
		if now.Sub(at) >= cfg.DedupeTTL {
			return
		}
		if f, err := wire.Validate(body); err == nil {
			s.dedupe.Add(dedupe.MakeKey(env.ProjectID, f.BatchID), at)
		}
	}, log)
	if err != nil {
		return nil, fmt.Errorf("wal recovery: %w", err)
	}
	s.localBytes.Store(rec.Bytes)
	s.m.truncatedTails.Add(uint64(rec.TruncatedTails))
	if len(rec.Segments) > 0 {
		log.Info("wal recovery: segments queued for upload", "segments", len(rec.Segments), "bytes", rec.Bytes,
			"truncated_tails", rec.TruncatedTails, "dedupe_seeded", s.dedupe.Len())
	}

	s.maxLocal = cfg.WALMaxLocalBytes
	if s.maxLocal <= 0 {
		s.maxLocal = filesystemSize(cfg.WALDir) / 10 * 7
	}
	if s.maxLocal <= 0 {
		return nil, errors.New("cannot determine the WAL filesystem size; set LA_WAL_MAX_LOCAL_BYTES")
	}

	s.uploader = wal.NewUploader(wal.UploaderOptions{
		Store:      store,
		Workers:    cfg.UploadWorkers,
		LocalBytes: &s.localBytes,
		Logger:     log,
		OnUploaded: func(seg wal.Segment, compressed int64) {
			s.m.segmentsUploaded.Inc()
			s.m.uploadedBytes.Add(uint64(compressed))
			log.Debug("wal segment uploaded", "key", seg.Key(), "bytes", seg.Size, "compressed", compressed)
		},
		OnError: func(wal.Segment, error) { s.m.uploadErrors.Inc() },
	})
	for _, seg := range rec.Segments {
		s.uploader.Enqueue(seg)
	}
	s.uploader.Start()

	s.writer = wal.NewWriter(wal.Options{
		Dir:           cfg.WALDir,
		NodeID:        cfg.NodeID,
		SegmentBytes:  cfg.SegmentBytes,
		SegmentAge:    cfg.SegmentAge,
		FsyncInterval: cfg.FsyncInterval,
		FsyncBytes:    cfg.FsyncBytes,
		StartSeq:      wal.StartSeq(rec.MaxSeq, now),
		OnSealed:      s.uploader.Enqueue,
		OnFsync:       func(d time.Duration, _ int) { s.m.fsyncSeconds.ObserveSeconds(d.Seconds()) },
		LocalBytes:    &s.localBytes,
		Logger:        log,
	})

	s.tokens = tokens.New(tokens.Options{
		Store:       store,
		Key:         cfg.TokensKey,
		CachePath:   cfg.tokensCachePath(),
		DefaultRate: cfg.RateToken,
		Logger:      log,
	})
	ctx, cancel := context.WithCancel(context.Background())
	s.bgCancel = cancel
	lctx, lcancel := context.WithTimeout(ctx, 15*time.Second)
	if err := s.tokens.Load(lctx); err != nil {
		// Not fatal: the node answers 503 (never 401) until a token set
		// arrives, and the poller keeps trying.
		log.Error("token registry: nothing loaded yet; requests get 503 until it is", "err", err)
	}
	lcancel()

	s.bgWG.Add(1)
	go func() {
		defer s.bgWG.Done()
		s.tokens.Run(ctx, cfg.TokensPoll)
	}()
	if s.ipLimiter != nil {
		s.bgWG.Add(1)
		go func() {
			defer s.bgWG.Done()
			t := time.NewTicker(10 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case now := <-t.C:
					s.ipLimiter.Sweep(now)
				}
			}
		}()
	}
	return s, nil
}

func (s *Server) initMetrics() {
	r := metrics.NewRegistry("la_ingest_")
	lat := []float64{0.0005, 0.001, 0.002, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}
	s.m.reg = r
	s.m.requests = r.Requests("requests_total", "HTTP responses by status code and payload format.")
	s.m.requestSeconds = r.Histogram("request_seconds", "Request handling time (receipt to response), seconds.", lat...)
	s.m.bytesIn = r.Counter("bytes_in_total", "Decoded payload bytes written to the WAL.")
	s.m.recordsIn = r.Counter("records_in_total", "Records (events, replay chunks) written to the WAL.")
	s.m.fsyncSeconds = r.Histogram("fsync_seconds", "Duration of WAL group fsyncs, seconds.",
		0.00025, 0.0005, 0.001, 0.002, 0.004, 0.008, 0.016, 0.032, 0.064, 0.128, 0.256, 0.512, 1, 2)
	r.Gauge("wal_local_bytes", "Bytes of WAL on local disk not yet uploaded.", func() float64 { return float64(s.localBytes.Load()) })
	r.Gauge("wal_local_limit_bytes", "Backpressure threshold for wal_local_bytes.", func() float64 { return float64(s.maxLocal) })
	r.Gauge("wal_upload_lag_seconds", "Age of the oldest sealed segment not yet uploaded (0 if none).", func() float64 {
		if n, oldest := s.uploader.Pending(); n > 0 {
			return max(0, time.Since(oldest).Seconds())
		}
		return 0
	})
	r.Gauge("wal_segments_pending", "Sealed segments waiting for upload.", func() float64 {
		n, _ := s.uploader.Pending()
		return float64(n)
	})
	s.m.segmentsUploaded = r.Counter("segments_uploaded_total", "WAL segments uploaded to object storage.")
	s.m.uploadedBytes = r.Counter("segments_uploaded_bytes_total", "Compressed bytes of uploaded WAL segments.")
	s.m.uploadErrors = r.Counter("segment_upload_errors_total", "Failed WAL segment upload attempts.")
	s.m.rateLimited = r.Counter("rate_limited_total", "Requests rejected with 429.")
	s.m.dupBatches = r.Counter("dup_batches_total", "Native batches acknowledged without writing (batch_id seen before).")
	s.m.walWriteErrors = r.Counter("wal_write_errors_total", "Requests failed because the WAL write or fsync failed.")
	s.m.truncatedTails = r.Counter("wal_truncated_tails_total", "Local segments whose torn tail was cut during recovery.")
	r.Gauge("tokens_age_seconds", "Seconds since the token set was last confirmed against object storage (-1: never).",
		func() float64 { return s.tokens.AgeSeconds() })
	r.Gauge("tokens_loaded", "Number of write tokens in effect.", func() float64 { return float64(s.tokens.Count()) })
	r.Gauge("tokens_refresh_failures", "Failed token refresh attempts since start.", func() float64 { return float64(s.tokens.Failures()) })
	r.Gauge("inflight_requests", "Requests currently being handled.", func() float64 { return float64(len(s.inflight)) })
	r.Gauge("ready", "1 if /readyz would answer 200.", func() float64 {
		if len(s.notReady()) == 0 {
			return 1
		}
		return 0
	})
}

// Handler returns the public HTTP handler.
func (s *Server) Handler() http.Handler { return http.HandlerFunc(s.route) }

// AdminHandler returns the handler for the internal listener (/metrics,
// /healthz, /readyz).
func (s *Server) AdminHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/metrics":
			s.handleMetrics(w, r)
		case "/healthz":
			s.handleHealthz(w, r)
		case "/readyz":
			s.handleReadyz(w, r)
		default:
			http.NotFound(w, r)
		}
	})
}

func (s *Server) route(w http.ResponseWriter, r *http.Request) {
	switch p := r.URL.Path; p {
	case "/v1/e":
		s.serve(w, r, metrics.FormatNative, s.handleEvents)
	case "/v1/r":
		s.serve(w, r, metrics.FormatReplay, s.handleReplay)
	case "/healthz":
		s.handleHealthz(w, r)
	case "/readyz":
		s.handleReadyz(w, r)
	case "/metrics":
		if s.cfg.AdminListen != "" {
			s.serve(w, r, metrics.FormatOther, notFound)
			return
		}
		s.handleMetrics(w, r)
	default:
		if _, ok := rulesetToken(p); ok {
			s.serve(w, r, metrics.FormatFlags, s.handleRuleset)
			return
		}
		if s.cfg.CompatPostHog {
			switch strings.TrimSuffix(p, "/") {
			case "/batch", "/i/v0/e", "/e", "/capture":
				s.serve(w, r, metrics.FormatPostHog, s.handlePostHog)
				return
			case "/decide", "/flags":
				s.serve(w, r, metrics.FormatFlags, s.handleFlags)
				return
			}
			// posthog-js fetches /array/<token>/config on every init before it
			// falls back to /flags; answer it with the same empty config.
			if rest, ok := strings.CutPrefix(p, "/array/"); ok && r.Method == http.MethodGet {
				if tok, tail, _ := strings.Cut(rest, "/"); tok != "" && tail == "config" {
					s.serve(w, r, metrics.FormatFlags, s.handleFlags)
					return
				}
			}
		}
		s.serve(w, r, metrics.FormatOther, notFound)
	}
}

// reply is what a handler wants sent.
type reply struct {
	status       int
	retryAfter   int    // seconds; > 0 adds Retry-After
	body         string // small static body
	ctype        string
	etag         string // adds ETag
	cacheControl string // adds Cache-Control
}

func notFound(*http.Request, *reqCtx) reply { return reply{status: http.StatusNotFound} }

// CORS decision for the response to a request that carried an Origin.
const (
	corsUndecided = iota // no token resolved: fall back to "would any token accept it"
	corsAllow
	corsDeny
)

// reqCtx carries per-request facts between serve and the handler.
type reqCtx struct {
	recv   time.Time
	origin string
	cors   int
	reqID  string
}

func (rc *reqCtx) recvMs() uint64 { return uint64(rc.recv.UnixMilli()) }

// serve wraps a handler with request ids, the concurrency limit, CORS,
// metrics and logging.
func (s *Server) serve(w http.ResponseWriter, r *http.Request, format int, h func(*http.Request, *reqCtx) reply) {
	rc := reqCtx{recv: time.Now(), origin: r.Header.Get("Origin"), reqID: s.requestID(r)}
	hdr := w.Header()
	hdr.Set("X-Request-Id", rc.reqID)

	var rep reply
	if r.Method == http.MethodOptions {
		rep = s.preflight(w, r, &rc, format)
	} else {
		select {
		case s.inflight <- struct{}{}:
			func() {
				defer func() { <-s.inflight }() // also on an aborting panic
				rep = h(r, &rc)
			}()
		default:
			rep = reply{status: http.StatusServiceUnavailable, retryAfter: retryTransient}
		}
	}

	if rc.origin != "" {
		hdr.Add("Vary", "Origin")
		allowed := rc.cors == corsAllow || (rc.cors == corsUndecided && s.tokens.AnyTokenAllowsOrigin(rc.origin))
		if allowed {
			hdr.Set("Access-Control-Allow-Origin", rc.origin)
			hdr.Set("Access-Control-Allow-Credentials", "true")
			// Lets browser SDKs read the retry hint on 429/503.
			hdr.Set("Access-Control-Expose-Headers", "Retry-After, ETag")
		}
	}
	if rep.retryAfter > 0 {
		hdr.Set("Retry-After", strconv.Itoa(rep.retryAfter))
	}
	if rep.etag != "" {
		hdr.Set("ETag", rep.etag)
	}
	if rep.cacheControl != "" {
		hdr.Set("Cache-Control", rep.cacheControl)
	}
	switch {
	case rep.status == http.StatusNotModified: // a 304 carries no entity headers
	case rep.body != "":
		hdr.Set("Content-Type", rep.ctype)
		hdr.Set("Content-Length", strconv.Itoa(len(rep.body)))
	default:
		hdr.Set("Content-Length", "0")
	}
	w.WriteHeader(rep.status)
	if rep.body != "" && r.Method != http.MethodHead {
		w.Write([]byte(rep.body))
	}

	elapsed := time.Since(rc.recv)
	s.m.requests.Inc(format, rep.status)
	s.m.requestSeconds.ObserveSeconds(elapsed.Seconds())
	// Request logs never contain payload content (design §16.5).
	switch {
	case rep.status >= 500:
		if s.errLog.Allow(rc.recv) {
			s.log.Warn("request failed", "request_id", rc.reqID, "method", r.Method, "path", r.URL.Path,
				"status", rep.status, "duration_ms", float64(elapsed.Microseconds())/1000)
		}
	case s.log.Enabled(r.Context(), slog.LevelDebug):
		s.log.Debug("request", "request_id", rc.reqID, "method", r.Method, "path", r.URL.Path,
			"status", rep.status, "duration_ms", float64(elapsed.Microseconds())/1000)
	}
}

// requestID returns the caller's X-Request-Id if it is sane, else a new one.
func (s *Server) requestID(r *http.Request) string {
	if id := r.Header.Get("X-Request-Id"); id != "" && len(id) <= 64 {
		ok := true
		for i := 0; i < len(id); i++ {
			c := id[i]
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '-' || c == '_' || c == '.') {
				ok = false
				break
			}
		}
		if ok {
			return id
		}
	}
	var b [48]byte
	out := append(b[:0], s.reqPrefix...)
	out = strconv.AppendUint(out, s.reqSeq.Add(1), 36)
	return string(out)
}

// preflight answers a CORS preflight (design §6.8). A preflight carries no
// Authorization header, so unless the token is in the URL (?t=) the origin
// is accepted if any active token would accept it; the actual request is
// then checked against its own token.
func (s *Server) preflight(w http.ResponseWriter, r *http.Request, rc *reqCtx, format int) reply {
	if rc.origin == "" {
		return reply{status: http.StatusNoContent}
	}
	allowed := false
	switch {
	case format == metrics.FormatFlags:
		allowed = true
	case format == metrics.FormatOther:
	default:
		if t := queryParam(r.URL.RawQuery, "t"); t != "" {
			if tok, ok := s.tokens.Lookup(t); ok && !tok.Disabled {
				allowed = tok.OriginAllowed(rc.origin)
			}
		} else {
			allowed = s.tokens.AnyTokenAllowsOrigin(rc.origin)
		}
	}
	if !allowed {
		rc.cors = corsDeny
		return reply{status: http.StatusForbidden}
	}
	rc.cors = corsAllow
	hdr := w.Header()
	hdr.Add("Vary", "Access-Control-Request-Headers")
	hdr.Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS")
	if req := r.Header.Get("Access-Control-Request-Headers"); req != "" {
		hdr.Set("Access-Control-Allow-Headers", req)
	} else {
		hdr.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Content-Encoding")
	}
	hdr.Set("Access-Control-Max-Age", "86400")
	return reply{status: http.StatusNoContent}
}

// bearerToken extracts the write token from "Authorization: Bearer pk_…",
// falling back to the ?t= query parameter (sendBeacon cannot set headers).
func bearerToken(r *http.Request) string {
	if a := r.Header.Get("Authorization"); len(a) > 7 && strings.EqualFold(a[:7], "Bearer ") {
		return strings.TrimSpace(a[7:])
	}
	return queryParam(r.URL.RawQuery, "t")
}

// queryParam returns the first value of key in a raw query string. The
// common case (no escapes) does not allocate.
func queryParam(rawQuery, key string) string {
	for rawQuery != "" {
		var pair string
		pair, rawQuery, _ = strings.Cut(rawQuery, "&")
		k, v, _ := strings.Cut(pair, "=")
		if k != key {
			continue
		}
		if strings.ContainsAny(v, "%+") {
			if u, err := url.QueryUnescape(v); err == nil {
				return u
			}
			return ""
		}
		return v
	}
	return ""
}

// authenticate resolves a token string. The second result is the error
// reply to send when ok is false.
func (s *Server) authenticate(tokenStr string, rc *reqCtx) (tok *tokens.Token, rep reply, ok bool) {
	if !s.tokens.Loaded() {
		// No token set yet: we cannot tell valid from invalid, so this must
		// be a retryable error, not a 401 that makes SDKs drop data.
		return nil, reply{status: http.StatusServiceUnavailable, retryAfter: retryNoTokens}, false
	}
	tok, found := s.tokens.Lookup(tokenStr)
	if !found {
		return nil, reply{status: http.StatusUnauthorized}, false
	}
	if tok.Disabled {
		return nil, reply{status: http.StatusForbidden}, false
	}
	if rc.origin != "" {
		if !tok.OriginAllowed(rc.origin) {
			rc.cors = corsDeny
			return nil, reply{status: http.StatusForbidden}, false
		}
		rc.cors = corsAllow
	}
	return tok, reply{}, true
}

func (s *Server) clientIP(r *http.Request) netip.Addr {
	return geoip.ClientIP(r, s.cfg.TrustedProxies)
}

func (s *Server) country(ip netip.Addr) [2]byte {
	if s.geo == nil {
		return geoip.Unknown
	}
	return s.geo.Country(ip)
}

func (s *Server) ipLimited(ip netip.Addr, now time.Time) bool {
	if s.ipLimiter != nil && !s.ipLimiter.Allow(ip, now) {
		s.m.rateLimited.Inc()
		return true
	}
	return false
}

func (s *Server) tokenLimited(tok *tokens.Token, now time.Time) bool {
	if tok.Limiter != nil && !tok.Limiter.Allow(now) {
		s.m.rateLimited.Inc()
		return true
	}
	return false
}

// overloaded implements backpressure (design §6.5): too much WAL is waiting
// on local disk for upload.
func (s *Server) overloaded() bool { return s.localBytes.Load() > s.maxLocal }

func envFlags(rc *reqCtx) uint8 {
	if rc.origin != "" {
		return wal.FlagWeb
	}
	return 0
}

// handleEvents is the native hot path, POST /v1/e (design §6.1).
func (s *Server) handleEvents(r *http.Request, rc *reqCtx) reply {
	if r.Method != http.MethodPost {
		return reply{status: http.StatusMethodNotAllowed}
	}
	// 1. token
	tok, rep, ok := s.authenticate(bearerToken(r), rc)
	if !ok {
		return rep
	}
	// 2. rate limits
	ip := s.clientIP(r)
	if s.tokenLimited(tok, rc.recv) || s.ipLimited(ip, rc.recv) {
		return reply{status: http.StatusTooManyRequests, retryAfter: retryRateLimited}
	}
	if s.overloaded() {
		return reply{status: http.StatusServiceUnavailable, retryAfter: retryBackpressure}
	}
	// 3-4. bounded read, gunzip
	bp, err := readBody(r, wal.EntryPrefixLen, wire.MaxBatchBytes)
	if err != nil {
		return bodyErrorReply(err)
	}
	defer putBuf(bp)
	entry := *bp
	body := entry[wal.EntryPrefixLen:]
	// 5. frame
	frame, err := wire.Validate(body)
	if err != nil {
		if errors.Is(err, wire.ErrTooManyRecords) {
			return reply{status: http.StatusRequestEntityTooLarge}
		}
		return reply{status: http.StatusBadRequest}
	}
	if frame.Records == 0 {
		return reply{status: http.StatusOK} // valid empty batch: nothing to write
	}
	// 6. idempotency
	key := dedupe.MakeKey(tok.ProjectID, frame.BatchID)
	if s.dedupe.Seen(key, rc.recv) {
		s.m.dupBatches.Inc()
		return reply{status: http.StatusOK}
	}
	// 7. WAL + fsync
	if err := wal.FinishEntry(entry, wal.Envelope{
		Format:    wal.FormatNative,
		ProjectID: tok.ProjectID,
		RecvTSMs:  rc.recvMs(),
		Country:   s.country(ip),
		Flags:     envFlags(rc),
	}); err != nil {
		return reply{status: http.StatusRequestEntityTooLarge}
	}
	if err := s.writer.Append(entry); err != nil {
		s.m.walWriteErrors.Inc()
		return reply{status: http.StatusServiceUnavailable, retryAfter: retryTransient}
	}
	// Only a durable batch may be remembered: a failed write must stay
	// retryable.
	s.dedupe.Add(key, rc.recv)
	s.m.bytesIn.Add(uint64(len(body)))
	s.m.recordsIn.Add(uint64(frame.Records))
	if s.afterFsync != nil {
		s.afterFsync()
	}
	// 8.
	return reply{status: http.StatusOK}
}

func bodyErrorReply(err error) reply {
	if errors.Is(err, errTooLarge) {
		return reply{status: http.StatusRequestEntityTooLarge}
	}
	return reply{status: http.StatusBadRequest}
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte("ok\n"))
}

// notReady lists the reasons /readyz fails (empty when ready).
func (s *Server) notReady() []string {
	var why []string
	if s.shuttingDown.Load() {
		why = append(why, "shutting_down")
	}
	if s.overloaded() {
		why = append(why, "wal_backpressure")
	}
	if !s.tokens.Loaded() {
		why = append(why, "tokens_not_loaded")
	}
	return why
}

func (s *Server) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if why := s.notReady(); len(why) > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(retryBackpressure))
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintf(w, "{\"ready\":false,\"reasons\":[\"%s\"]}\n", strings.Join(why, `","`))
		return
	}
	w.Write([]byte("{\"ready\":true}\n"))
}

func (s *Server) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	s.m.reg.Write(w)
}

func newHTTPServer(h http.Handler) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
}

// ListenAndServe binds the configured listeners and serves until Shutdown.
func (s *Server) ListenAndServe() error {
	ln, err := net.Listen("tcp", s.cfg.Listen)
	if err != nil {
		return err
	}
	var admin net.Listener
	if s.cfg.AdminListen != "" {
		if admin, err = net.Listen("tcp", s.cfg.AdminListen); err != nil {
			ln.Close()
			return err
		}
	}
	return s.Serve(ln, admin)
}

// Serve serves on ln (and the optional admin listener) until Shutdown. It
// returns nil after a clean shutdown.
func (s *Server) Serve(ln, admin net.Listener) error {
	s.mu.Lock()
	s.httpSrv = newHTTPServer(s.Handler())
	srv := s.httpSrv
	if admin != nil {
		s.adminSrv = newHTTPServer(s.AdminHandler())
		adminSrv := s.adminSrv
		go func() {
			if err := adminSrv.Serve(admin); err != nil && !errors.Is(err, http.ErrServerClosed) {
				s.log.Error("admin listener failed", "err", err)
			}
		}()
	}
	s.mu.Unlock()
	s.log.Info("ingestion listening", "addr", ln.Addr().String(), "node_id", s.cfg.NodeID, "wal_dir", s.cfg.WALDir,
		"wal_max_local_bytes", s.maxLocal, "compat_posthog", s.cfg.CompatPostHog)
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Shutdown performs the graceful stop of design §6.10: fail /readyz, wait
// for the load balancer to take the node out, finish in-flight requests,
// seal the active segment, upload everything, stop. If the uploads cannot
// finish within the configured timeout the remaining segments stay on local
// disk (they are uploaded on the next start) and an error is returned.
func (s *Server) Shutdown(ctx context.Context) error {
	if !s.shuttingDown.CompareAndSwap(false, true) {
		return nil
	}
	s.log.Info("shutdown: readiness failed, draining", "delay_ms", s.cfg.DrainDelay.Milliseconds())
	select {
	case <-time.After(s.cfg.DrainDelay):
	case <-ctx.Done():
	}
	s.mu.Lock()
	srv, admin := s.httpSrv, s.adminSrv
	s.mu.Unlock()
	if srv != nil {
		if err := srv.Shutdown(ctx); err != nil {
			s.log.Warn("shutdown: open requests did not finish in time", "err", err)
			srv.Close()
		}
	}
	// No handler is running any more: seal the active segment.
	s.writer.Close()
	dctx, cancel := context.WithTimeout(ctx, s.cfg.ShutdownUploadTimeout)
	err := s.uploader.Drain(dctx)
	cancel()
	s.uploader.Stop()
	s.bgCancel()
	s.bgWG.Wait()
	if admin != nil {
		admin.Close()
	}
	if n, _ := s.uploader.Pending(); n > 0 {
		s.log.Error("shutdown: segments left on local disk; they are uploaded on next start",
			"segments", n, "bytes", s.localBytes.Load())
		return fmt.Errorf("shutdown: %d wal segment(s) not uploaded: %w", n, err)
	}
	s.log.Info("shutdown: complete, local wal empty")
	return nil
}

// crash stops the server the way kill -9 would, as far as the WAL is
// concerned: nothing is flushed, sealed or uploaded. Tests only.
func (s *Server) crash() {
	s.shuttingDown.Store(true)
	s.writer.Abort()
	s.uploader.Stop()
	s.bgCancel()
	s.bgWG.Wait()
}
