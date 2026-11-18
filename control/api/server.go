// Package api is the control plane's HTTP surface (spec/control.md):
// authentication, the management API, the query proxy, the static UI and
// the health/metrics endpoints.
package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/mucan54/izlek/control/auth"
	"github.com/mucan54/izlek/control/config"
	"github.com/mucan54/izlek/control/db"
	"github.com/mucan54/izlek/control/metrics"
)

// Notifier is told about every committed change (the publisher).
type Notifier interface{ Notify() }

type noopNotifier struct{}

func (noopNotifier) Notify() {}

// Options are the dependencies of a Server.
type Options struct {
	Config   config.Config
	DB       *db.DB
	Notifier Notifier
	Hasher   *auth.Hasher
	Logger   *slog.Logger
	Metrics  *metrics.Registry
	Now      func() time.Time
	// QueryTransport overrides the transport used to reach the query
	// service (tests).
	QueryTransport http.RoundTripper
}

// Server implements http.Handler.
type Server struct {
	cfg    config.Config
	db     *db.DB
	notify Notifier
	hasher *auth.Hasher
	log    *slog.Logger
	now    func() time.Time
	reg    *metrics.Registry

	routes []route
	proxy  *queryProxy
	ui     *staticUI

	loginPair *auth.Limiter // failed logins per (IP, e-mail)
	loginIP   *auth.Limiter // failed logins per IP
	reauth    *auth.Limiter // failed password confirmations per user

	mRequests   *metrics.CounterVec
	mDuration   *metrics.Histogram
	mLogins     *metrics.CounterVec
	mProxy      *metrics.CounterVec
	mAudit      *metrics.Counter
	mKeyAuth    *metrics.Counter
	metricsOpen bool
}

// Limits of the login throttle (spec/control.md §1: 5 per minute per IP +
// e-mail). The per-IP limit is a coarse second line against spraying many
// accounts from one address.
const (
	loginPairMax = 5
	loginIPMax   = 50
	loginWindow  = time.Minute
)

// New builds a server.
func New(opt Options) (*Server, error) {
	if opt.DB == nil {
		return nil, errors.New("api: no database")
	}
	if opt.Logger == nil {
		opt.Logger = slog.New(slog.DiscardHandler)
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.Notifier == nil {
		opt.Notifier = noopNotifier{}
	}
	if opt.Hasher == nil {
		opt.Hasher = auth.NewHasher(auth.DefaultParams, 4)
	}
	if opt.Metrics == nil {
		opt.Metrics = metrics.New()
	}
	s := &Server{
		cfg: opt.Config, db: opt.DB, notify: opt.Notifier, hasher: opt.Hasher, log: opt.Logger, now: opt.Now, reg: opt.Metrics,
		loginPair:   auth.NewLimiter(loginPairMax, loginWindow, opt.Now),
		loginIP:     auth.NewLimiter(loginIPMax, loginWindow, opt.Now),
		reauth:      auth.NewLimiter(loginPairMax, loginWindow, opt.Now),
		metricsOpen: opt.Config.AdminListen == "",
	}
	s.mRequests = s.reg.CounterVec("http_requests_total", "HTTP requests by status code.", "code")
	s.mDuration = s.reg.Histogram("http_request_seconds", "HTTP request duration.", 0.005, 0.025, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60)
	s.mLogins = s.reg.CounterVec("logins_total", "Login attempts by result.", "result")
	s.mProxy = s.reg.CounterVec("query_proxy_requests_total", "Query proxy requests by outcome.", "result")
	s.mAudit = s.reg.Counter("audit_entries_total", "Audit log entries written.")
	s.mKeyAuth = s.reg.Counter("key_auth_failures_total", "Requests with an unknown or insufficient secret key.")
	s.proxy = newQueryProxy(opt.Config, opt.QueryTransport)
	if opt.Config.UIDir != "" {
		ui, err := newStaticUI(opt.Config.UIDir)
		if err != nil {
			return nil, err
		}
		s.ui = ui
	}
	s.registerRoutes()
	return s, nil
}

// Close releases resources held by the server.
func (s *Server) Close() {
	if s.ui != nil {
		s.ui.Close()
	}
}

// ---- routing ----

type authMode uint8

const (
	authNone    authMode = iota // no credentials needed
	authSession                 // session cookie only
	authProject                 // session cookie, or an sk_ key of that project
)

type handlerFunc func(c *reqCtx) (any, error)

type route struct {
	method  string
	pattern string
	parts   []string
	auth    authMode
	h       handlerFunc
}

func (s *Server) handle(method, pattern string, mode authMode, h handlerFunc) {
	s.routes = append(s.routes, route{method: method, pattern: pattern, parts: strings.Split(strings.Trim(pattern, "/"), "/"), auth: mode, h: h})
}

func (rt *route) match(parts []string) (map[string]string, bool) {
	if len(parts) != len(rt.parts) {
		return nil, false
	}
	var params map[string]string
	for i, want := range rt.parts {
		if strings.HasPrefix(want, ":") {
			if parts[i] == "" {
				return nil, false
			}
			if params == nil {
				params = make(map[string]string, 2)
			}
			params[want[1:]] = parts[i]
		} else if want != parts[i] {
			return nil, false
		}
	}
	return params, true
}

func (s *Server) registerRoutes() {
	// §1 authentication
	s.handle("GET", "/api/setup", authNone, s.getSetup)
	s.handle("POST", "/api/setup", authNone, s.postSetup)
	s.handle("POST", "/api/auth/login", authNone, s.login)
	s.handle("POST", "/api/auth/logout", authSession, s.logout)
	s.handle("GET", "/api/me", authSession, s.me)
	s.handle("POST", "/api/me/password", authSession, s.changePassword)
	s.handle("POST", "/api/me/totp/begin", authSession, s.totpBegin)
	s.handle("POST", "/api/me/totp/enable", authSession, s.totpEnable)
	s.handle("POST", "/api/me/totp/disable", authSession, s.totpDisable)
	// §2 organisations
	s.handle("GET", "/api/orgs/:org/members", authSession, s.listMembers)
	s.handle("POST", "/api/orgs/:org/members", authSession, s.addMember)
	s.handle("PATCH", "/api/orgs/:org/members/:user", authSession, s.updateMember)
	s.handle("DELETE", "/api/orgs/:org/members/:user", authSession, s.removeMember)
	s.handle("GET", "/api/orgs/:org/audit", authSession, s.listAudit)
	s.handle("DELETE", "/api/orgs/:org", authSession, s.deleteOrg)
	// §3 projects
	s.handle("GET", "/api/projects", authSession, s.listProjects)
	s.handle("POST", "/api/projects", authSession, s.createProject)
	s.handle("GET", "/api/projects/:pid", authProject, s.getProject)
	s.handle("PATCH", "/api/projects/:pid", authProject, s.updateProject)
	s.handle("DELETE", "/api/projects/:pid", authProject, s.closeProject)
	// §4 tokens and keys
	s.handle("GET", "/api/projects/:pid/tokens", authProject, s.listTokens)
	s.handle("POST", "/api/projects/:pid/tokens", authProject, s.createToken)
	s.handle("PATCH", "/api/projects/:pid/tokens/:token", authProject, s.updateToken)
	s.handle("DELETE", "/api/projects/:pid/tokens/:token", authProject, s.deleteToken)
	s.handle("GET", "/api/projects/:pid/keys", authProject, s.listKeys)
	s.handle("POST", "/api/projects/:pid/keys", authProject, s.createKey)
	s.handle("DELETE", "/api/projects/:pid/keys/:id", authProject, s.deleteKey)
	// §5 flags
	s.handle("GET", "/api/projects/:pid/flags", authProject, s.listFlags)
	s.handle("POST", "/api/projects/:pid/flags", authProject, s.createFlag)
	s.handle("GET", "/api/projects/:pid/flags/:key", authProject, s.getFlag)
	s.handle("PUT", "/api/projects/:pid/flags/:key", authProject, s.putFlag)
	s.handle("DELETE", "/api/projects/:pid/flags/:key", authProject, s.deleteFlag)
	s.handle("GET", "/api/projects/:pid/flags/:key/versions", authProject, s.listFlagVersions)
	s.handle("POST", "/api/projects/:pid/flags/:key/evaluate", authProject, s.evaluateFlag)
	// §6 insights and dashboards
	s.handle("GET", "/api/projects/:pid/insights", authProject, s.listInsights)
	s.handle("POST", "/api/projects/:pid/insights", authProject, s.createInsight)
	s.handle("GET", "/api/projects/:pid/insights/:id", authProject, s.getInsight)
	s.handle("PUT", "/api/projects/:pid/insights/:id", authProject, s.putInsight)
	s.handle("DELETE", "/api/projects/:pid/insights/:id", authProject, s.deleteInsight)
	s.handle("GET", "/api/projects/:pid/dashboards", authProject, s.listDashboards)
	s.handle("POST", "/api/projects/:pid/dashboards", authProject, s.createDashboard)
	s.handle("GET", "/api/projects/:pid/dashboards/:id", authProject, s.getDashboard)
	s.handle("PUT", "/api/projects/:pid/dashboards/:id", authProject, s.putDashboard)
	s.handle("DELETE", "/api/projects/:pid/dashboards/:id", authProject, s.deleteDashboard)
	// §7 deletions
	s.handle("GET", "/api/projects/:pid/deletions", authProject, s.listDeletions)
	s.handle("POST", "/api/projects/:pid/deletions", authProject, s.createDeletion)
	// §8 query proxy
	s.handle("GET", "/api/projects/:pid/q/replay", authProject, s.proxyReplay)
	s.handle("POST", "/api/projects/:pid/q/:endpoint", authProject, s.proxyQuery)
}

// ---- request plumbing ----

// statusWriter records the status code and whether anything was written.
type statusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wrote {
		w.status, w.wrote = code, true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.status, w.wrote = http.StatusOK, true
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func newRequestID(r *http.Request) string {
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
	var raw [8]byte
	_, _ = rand.Read(raw[:])
	return hex.EncodeToString(raw[:])
}

// Content security policies. The UI is a same-origin SPA; API responses are
// data and need nothing at all.
const (
	cspUI  = "default-src 'self'; base-uri 'self'; form-action 'self'; frame-ancestors 'none'"
	cspAPI = "default-src 'none'; frame-ancestors 'none'"
)

func (s *Server) securityHeaders(h http.Header, csp string) {
	h.Set("Content-Security-Policy", csp)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "same-origin")
	h.Set("X-Frame-Options", "DENY")
	if s.cfg.CookieSecure {
		// LA_COOKIE_SECURE=true declares "served behind TLS".
		h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
	}
}

// ServeHTTP is the main (public) handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	sw := &statusWriter{ResponseWriter: w}
	reqID := newRequestID(r)
	sw.Header().Set("X-Request-Id", reqID)
	info := &requestInfo{id: reqID, route: "-"}

	defer func() {
		if rec := recover(); rec != nil {
			if rec == http.ErrAbortHandler {
				panic(rec)
			}
			s.log.Error("panic in handler", "req_id", reqID, "panic", fmt.Sprint(rec), "stack", string(debug.Stack()))
			if !sw.wrote {
				s.securityHeaders(sw.Header(), cspAPI)
				writeError(sw, errInternal())
			}
		}
		elapsed := time.Since(start)
		s.mRequests.With(strconv.Itoa(sw.status)).Inc()
		s.mDuration.Observe(elapsed.Seconds())
		level := slog.LevelInfo
		if info.quiet && sw.status < 500 {
			level = slog.LevelDebug
		}
		if s.log.Enabled(r.Context(), level) {
			s.log.Log(r.Context(), level, "request", "req_id", reqID, "method", r.Method, "route", info.route,
				"status", sw.status, "ms", elapsed.Milliseconds(), "actor", info.actor, "project", info.project, "ip", s.clientIP(r))
		}
	}()

	// No response may be written forever: a client that stops reading is
	// cut off. The query proxy extends this for long-running queries.
	_ = http.NewResponseController(sw).SetWriteDeadline(time.Now().Add(apiWriteTimeout))

	path := r.URL.Path
	switch {
	case path == "/healthz":
		info.route, info.quiet = path, true
		s.plain(sw, http.StatusOK, "ok\n")
	case path == "/readyz":
		info.route, info.quiet = path, true
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := s.db.Ping(ctx); err != nil {
			s.plain(sw, http.StatusServiceUnavailable, "database unavailable\n")
			return
		}
		s.plain(sw, http.StatusOK, "ready\n")
	case path == "/metrics":
		info.route, info.quiet = path, true
		if !s.metricsOpen {
			s.securityHeaders(sw.Header(), cspAPI)
			writeError(sw, errNotFound())
			return
		}
		s.ServeMetrics(sw, r)
	case path == "/api" || strings.HasPrefix(path, "/api/"):
		s.securityHeaders(sw.Header(), cspAPI)
		s.serveAPI(sw, r, info)
	default:
		info.route, info.quiet = "static", true
		if s.ui == nil {
			s.securityHeaders(sw.Header(), cspAPI)
			writeError(sw, errNotFound())
			return
		}
		s.securityHeaders(sw.Header(), cspUI)
		s.ui.serve(sw, r)
	}
}

func (s *Server) plain(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

// ServeMetrics writes the Prometheus exposition. It is mounted on the admin
// listener when one is configured, otherwise on the main one.
func (s *Server) ServeMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodHead {
		return
	}
	_ = s.reg.Write(w)
}

// AdminHandler serves /metrics, /healthz and /readyz for the internal
// listener (LA_CONTROL_ADMIN_LISTEN).
func (s *Server) AdminHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/metrics":
			s.ServeMetrics(w, r)
		case "/healthz":
			s.plain(w, http.StatusOK, "ok\n")
		case "/readyz":
			ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
			defer cancel()
			if err := s.db.Ping(ctx); err != nil {
				s.plain(w, http.StatusServiceUnavailable, "database unavailable\n")
				return
			}
			s.plain(w, http.StatusOK, "ready\n")
		default:
			http.NotFound(w, r)
		}
	})
}

type requestInfo struct {
	id      string
	route   string
	actor   string
	project uint32
	quiet   bool
}

// apiWriteTimeout bounds how long an ordinary response may take to write;
// the query proxy sets its own, longer deadline.
const apiWriteTimeout = 60 * time.Second

func (s *Server) serveAPI(w *statusWriter, r *http.Request, info *requestInfo) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	method := r.Method
	if method == http.MethodHead {
		method = http.MethodGet
	}
	var matched *route
	var params map[string]string
	pathKnown := false
	for i := range s.routes {
		rt := &s.routes[i]
		p, ok := rt.match(parts)
		if !ok {
			continue
		}
		pathKnown = true
		if rt.method == method {
			matched, params = rt, p
			break
		}
	}
	if matched == nil {
		if pathKnown {
			writeError(w, &apiError{Status: http.StatusMethodNotAllowed, Code: "method_not_allowed", Message: "Bu yöntem desteklenmiyor"})
		} else {
			writeError(w, errNotFound())
		}
		return
	}
	info.route = matched.pattern

	c := &reqCtx{s: s, w: w, r: r, params: params, now: s.now(), info: info}
	if err := c.authenticate(matched.auth); err != nil {
		c.fail(err)
		return
	}
	out, err := matched.h(c)
	if err != nil {
		c.fail(err)
		return
	}
	if w.wrote { // the handler streamed its own response
		return
	}
	if out == nil {
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// fail writes an error response; unexpected errors are logged and reported
// as an opaque 500.
func (c *reqCtx) fail(err error) {
	var ae *apiError
	if !errors.As(err, &ae) {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			ae = &apiError{Status: http.StatusServiceUnavailable, Code: "unavailable", Message: "İstek tamamlanamadı; yeniden deneyin"}
		} else if errors.Is(err, os.ErrDeadlineExceeded) {
			ae = &apiError{Status: http.StatusRequestTimeout, Code: "timeout", Message: "İstek zaman aşımına uğradı"}
		} else {
			c.s.log.Error("request failed", "req_id", c.info.id, "route", c.info.route, "err", err.Error())
			ae = errInternal()
		}
	}
	if c.w.wrote {
		return
	}
	writeError(c.w, ae)
}
