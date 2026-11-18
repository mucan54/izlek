package api

import (
	"bytes"
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/mucan54/izlek/control/auth"
	"github.com/mucan54/izlek/control/db"
)

// ---- errors ----

// apiError is an error with an HTTP status and the JSON envelope
// {"error": {"code", "message"}} of spec/control.md.
type apiError struct {
	Status     int
	Code       string
	Message    string
	RetryAfter int // seconds; sets Retry-After when > 0
}

func (e *apiError) Error() string { return e.Code + ": " + e.Message }

// User-facing messages are Turkish, like the UI that shows them; clients
// must branch on the stable English code.
func errBadRequest(format string, args ...any) *apiError {
	return &apiError{Status: http.StatusBadRequest, Code: "bad_request", Message: fmt.Sprintf(format, args...)}
}

func errCode(status int, code, message string) *apiError {
	return &apiError{Status: status, Code: code, Message: message}
}

func errUnauthorized() *apiError {
	return errCode(http.StatusUnauthorized, "unauthorized", "Oturum açmanız gerekiyor")
}

func errForbidden() *apiError {
	return errCode(http.StatusForbidden, "forbidden", "Bu işlem için yetkiniz yok")
}

func errNotFound() *apiError {
	return errCode(http.StatusNotFound, "not_found", "Kaynak bulunamadı")
}

func errConflict(message string) *apiError {
	return errCode(http.StatusConflict, "conflict", message)
}

func errInternal() *apiError {
	return errCode(http.StatusInternalServerError, "internal", "Beklenmeyen sunucu hatası")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		status, body = http.StatusInternalServerError, []byte(`{"error":{"code":"internal","message":"Beklenmeyen sunucu hatası"}}`)
	}
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func writeError(w http.ResponseWriter, e *apiError) {
	if e.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(e.RetryAfter))
	}
	writeJSON(w, e.Status, map[string]any{"error": map[string]string{"code": e.Code, "message": e.Message}})
}

// ---- request context ----

type reqCtx struct {
	s      *Server
	w      *statusWriter
	r      *http.Request
	params map[string]string
	now    time.Time
	info   *requestInfo

	// Exactly one of session and key is set on authenticated routes.
	session *db.Session
	key     *db.Key
}

func (c *reqCtx) ctx() context.Context { return c.r.Context() }

func (c *reqCtx) nowMs() int64 { return c.now.UnixMilli() }

// actor is who the audit log attributes the request to.
func (c *reqCtx) actor() db.Actor {
	switch {
	case c.key != nil:
		return db.KeyActor(c.key.ID)
	case c.session != nil:
		return db.UserActor(c.session.User.ID)
	}
	return db.SystemActor()
}

func (c *reqCtx) user() *db.User { return &c.session.User }

const (
	sessionCookie = "la_session"
	csrfHeader    = "X-LA-CSRF"
	sessionTTL    = 30 * 24 * time.Hour
	// A session's expiry is pushed forward at most this often, so that
	// sliding it does not turn every read into a database write.
	sessionRefresh = time.Hour
	maxBodyBytes   = 1 << 20
)

func isSafeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

// crossSite reports a browser-declared cross-site request. Browsers that
// send Sec-Fetch-Site cannot be made to lie about it by a page.
func crossSite(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Sec-Fetch-Site"), "cross-site")
}

func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if len(h) < 7 || !strings.EqualFold(h[:7], "Bearer ") {
		return "", false
	}
	return strings.TrimSpace(h[7:]), true
}

func (c *reqCtx) authenticate(mode authMode) error {
	r := c.r
	if token, ok := bearerToken(r); ok {
		return c.authenticateKey(mode, token)
	}
	if mode == authNone {
		// Unauthenticated mutations (setup, login) cannot carry a CSRF
		// token; refusing declared cross-site requests keeps a hostile page
		// from logging a visitor into an account of its choosing.
		if !isSafeMethod(r.Method) && crossSite(r) {
			return errCode(http.StatusForbidden, "csrf", "CSRF doğrulaması başarısız; sayfayı yenileyip yeniden deneyin")
		}
		return nil
	}
	ck, err := r.Cookie(sessionCookie)
	if err != nil || len(ck.Value) != 64 {
		return errUnauthorized()
	}
	hash := auth.HashSecret(ck.Value)
	sess, err := db.SessionByHash(c.ctx(), c.s.db.R(), hash, c.nowMs())
	if errors.Is(err, db.ErrNotFound) {
		return errUnauthorized()
	}
	if err != nil {
		return err
	}
	if !isSafeMethod(r.Method) {
		got := r.Header.Get(csrfHeader)
		if crossSite(r) || subtle.ConstantTimeCompare([]byte(got), []byte(sess.CSRF)) != 1 {
			return errCode(http.StatusForbidden, "csrf", "CSRF doğrulaması başarısız; sayfayı yenileyip yeniden deneyin")
		}
	}
	c.session = &sess
	c.info.actor = db.UserActor(sess.User.ID).String()
	// Sliding expiry.
	if want := c.now.Add(sessionTTL).UnixMilli(); want-sess.ExpiresAt > sessionRefresh.Milliseconds() {
		if err := c.s.db.Write(c.ctx(), func(tx *sql.Tx) error { return db.ExtendSession(c.ctx(), tx, hash, want) }); err != nil {
			return err
		}
		sess.ExpiresAt = want
		c.s.setSessionCookie(c.w, ck.Value)
	}
	return nil
}

// authenticateKey handles "Authorization: Bearer sk_…" (spec/control.md §1):
// an admin-scoped key may use the endpoints under its own project and
// nothing else. No cookie, no CSRF token.
func (c *reqCtx) authenticateKey(mode authMode, token string) error {
	deny := func() error {
		c.s.mKeyAuth.Inc()
		return errCode(http.StatusUnauthorized, "unauthorized", "Geçersiz API anahtarı")
	}
	if !auth.LooksLikeSecretKey(token) {
		return deny()
	}
	key, err := db.KeyByHash(c.ctx(), c.s.db.R(), auth.HashSecret(token))
	if errors.Is(err, db.ErrNotFound) {
		return deny()
	}
	if err != nil {
		return err
	}
	c.info.actor = db.KeyActor(key.ID).String()
	if !key.HasScope("admin") {
		c.s.mKeyAuth.Inc()
		return errCode(http.StatusForbidden, "forbidden", "Bu API anahtarının yönetim (admin) kapsamı yok")
	}
	if mode != authProject {
		return errCode(http.StatusForbidden, "forbidden", "API anahtarları yalnızca kendi projelerinin uçlarını kullanabilir")
	}
	// Another project's endpoints do not exist as far as this key can tell.
	if pid, ok := parseProjectID(c.params["pid"]); !ok || pid != key.ProjectID {
		return errNotFound()
	}
	c.key = &key
	// last_used_at is advisory; keep it to one write a minute per key.
	if key.LastUsedAt == nil || c.nowMs()-*key.LastUsedAt > time.Minute.Milliseconds() {
		now := c.nowMs()
		if err := c.s.db.Write(c.ctx(), func(tx *sql.Tx) error { return db.TouchKey(c.ctx(), tx, key.ID, now) }); err != nil {
			c.s.log.Warn("cannot record key use", "key", key.ID, "err", err.Error())
		}
	}
	return nil
}

func (s *Server) setSessionCookie(w http.ResponseWriter, value string) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: value, Path: "/", MaxAge: int(sessionTTL / time.Second),
		HttpOnly: true, Secure: s.cfg.CookieSecure, SameSite: http.SameSiteLaxMode,
	})
}

func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: s.cfg.CookieSecure, SameSite: http.SameSiteLaxMode,
	})
}

// openSession creates a session for userID inside tx and returns the cookie
// value to set once the transaction has committed.
func (c *reqCtx) openSession(tx *sql.Tx, userID int64) (string, error) {
	cookie, hash := auth.NewSessionID()
	err := db.CreateSession(c.ctx(), tx, hash, userID, auth.NewCSRFToken(), c.nowMs(), c.now.Add(sessionTTL).UnixMilli())
	return cookie, err
}

// ---- authorisation ----

func parseProjectID(s string) (uint32, bool) {
	if s == "" || len(s) > 10 || s[0] == '0' {
		return 0, false
	}
	n, err := strconv.ParseUint(s, 10, 32)
	if err != nil || n == 0 {
		return 0, false
	}
	return uint32(n), true
}

func parseID(s string) (int64, bool) {
	if s == "" || len(s) > 18 {
		return 0, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 1 {
		return 0, false
	}
	return n, true
}

// project resolves the :pid of the route and checks that the caller holds
// at least minRole on it. Missing, closed and foreign projects all answer
// 404, so the existence of other organisations' projects does not leak.
func (c *reqCtx) project(minRole string) (db.Project, error) {
	pid, ok := parseProjectID(c.params["pid"])
	if !ok {
		return db.Project{}, errNotFound()
	}
	p, err := db.ProjectByID(c.ctx(), c.s.db.R(), pid)
	if errors.Is(err, db.ErrNotFound) || (err == nil && p.Closed()) {
		return p, errNotFound()
	}
	if err != nil {
		return p, err
	}
	c.info.project = p.ID
	if c.key != nil {
		if c.key.ProjectID != p.ID {
			return p, errNotFound()
		}
		// An admin-scoped key acts with the admin role on its project.
		if db.RoleRank(db.RoleAdmin) < db.RoleRank(minRole) {
			return p, errForbidden()
		}
		return p, nil
	}
	role, err := db.RoleIn(c.ctx(), c.s.db.R(), p.OrgID, c.user().ID)
	if errors.Is(err, db.ErrNotFound) {
		return p, errNotFound()
	}
	if err != nil {
		return p, err
	}
	if db.RoleRank(role) < db.RoleRank(minRole) {
		return p, errForbidden()
	}
	return p, nil
}

// org resolves the :org of the route (or a given id) with a minimum role.
func (c *reqCtx) orgByID(orgID int64, minRole string) error {
	role, err := db.RoleIn(c.ctx(), c.s.db.R(), orgID, c.user().ID)
	if errors.Is(err, db.ErrNotFound) {
		return errNotFound()
	}
	if err != nil {
		return err
	}
	if db.RoleRank(role) < db.RoleRank(minRole) {
		return errForbidden()
	}
	return nil
}

func (c *reqCtx) org(minRole string) (int64, error) {
	id, ok := parseID(c.params["org"])
	if !ok {
		return 0, errNotFound()
	}
	return id, c.orgByID(id, minRole)
}

// ---- bodies ----

// decode reads the JSON object of the request body into v. Unknown fields
// are ignored (clients may echo back whole resources); an empty body is {}.
func (c *reqCtx) decode(v any) error {
	raw, err := c.rawBody()
	if err != nil {
		return err
	}
	return decodeJSON(raw, v)
}

func (c *reqCtx) rawBody() ([]byte, error) {
	r := c.r
	raw, err := io.ReadAll(http.MaxBytesReader(c.w, r.Body, maxBodyBytes))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return nil, errCode(http.StatusRequestEntityTooLarge, "too_large", "İstek gövdesi çok büyük")
		}
		return nil, errBadRequest("İstek gövdesi okunamadı")
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return []byte("{}"), nil
	}
	// Browsers can only send application/json cross-origin after a CORS
	// preflight, which this server never approves. Key-authenticated
	// clients are not browsers and are spared the check.
	if c.key == nil {
		mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || mt != "application/json" {
			return nil, errCode(http.StatusUnsupportedMediaType, "unsupported_media_type", "İstek gövdesi application/json olmalıdır")
		}
	}
	return raw, nil
}

func decodeJSON(raw []byte, v any) error {
	if len(raw) == 0 || raw[0] != '{' {
		return errBadRequest("İstek gövdesi bir JSON nesnesi olmalıdır")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(v); err != nil {
		var te *json.UnmarshalTypeError
		if errors.As(err, &te) {
			if te.Field != "" {
				return errBadRequest("%q alanının türü hatalı", te.Field)
			}
			return errBadRequest("İstek gövdesindeki bir alanın türü hatalı")
		}
		var ae *apiError
		if errors.As(err, &ae) {
			return ae
		}
		return errBadRequest("Geçersiz JSON")
	}
	if dec.More() {
		return errBadRequest("Geçersiz JSON")
	}
	return nil
}

// flexInt accepts a JSON number or a decimal string: ids travel as either
// in the UI's types.
type flexInt int64

func (f *flexInt) UnmarshalJSON(data []byte) error {
	s := strings.Trim(string(data), `"`)
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return &json.UnmarshalTypeError{Value: "non-integer id", Field: ""}
	}
	*f = flexInt(n)
	return nil
}

// flexString accepts a JSON string or number (ids and TOTP codes typed as
// numbers).
type flexString string

func (f *flexString) UnmarshalJSON(data []byte) error {
	if len(data) > 0 && data[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		*f = flexString(s)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(data, &n); err != nil {
		return &json.UnmarshalTypeError{Value: "non-string", Field: ""}
	}
	*f = flexString(n.String())
	return nil
}

// ---- client address ----

// clientIP returns the address used for rate limiting. With
// LA_TRUSTED_PROXIES = N > 0 it is the Nth entry from the right of
// X-Forwarded-For (entries further left are client-supplied); otherwise the
// TCP peer. IPv6 addresses are reduced to their /64.
func (s *Server) clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if n := s.cfg.TrustedProxies; n > 0 {
		var entries []string
		for _, h := range r.Header.Values("X-Forwarded-For") {
			for _, e := range strings.Split(h, ",") {
				if e = strings.TrimSpace(e); e != "" {
					entries = append(entries, e)
				}
			}
		}
		if len(entries) >= n {
			host = entries[len(entries)-n]
		}
	}
	addr, err := netip.ParseAddr(strings.Trim(host, "[]"))
	if err != nil {
		return host
	}
	addr = addr.Unmap()
	if addr.Is6() {
		if p, err := addr.Prefix(64); err == nil {
			return p.Addr().String()
		}
	}
	return addr.String()
}

// audit appends an audit entry inside a write transaction.
func (c *reqCtx) audit(tx *sql.Tx, orgID int64, action, target string, detail any) error {
	c.s.mAudit.Inc()
	return db.InsertAudit(c.ctx(), tx, c.nowMs(), orgID, c.actor(), action, target, detail)
}
