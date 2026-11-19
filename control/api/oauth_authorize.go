package api

import (
	"crypto/subtle"
	"database/sql"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/mucan54/izlek/control/auth"
	"github.com/mucan54/izlek/control/db"
	"github.com/mucan54/izlek/control/oauth"
)

// The authorization endpoint, GET and POST /oauth/authorize.
//
// The request parameters travel through the sign-in and consent forms as one
// hidden field and are validated from scratch on every step, so there is no
// server-side "pending request" that could be confused with another one.
//
// Where an error is shown depends on what is known to be genuine:
//
//   - Until the client and its redirect URI have been verified, problems are
//     shown on a page here. The user is never sent to an address taken from
//     the request on trust (no open redirect).
//   - After that, errors go back to the client's redirect URI as OAuth
//     errors, with state and iss.

// authzRequest is a parsed authorization request.
type authzRequest struct {
	ClientID, RedirectURI, ResponseType, State string
	Scope, CodeChallenge, CodeChallengeMethod  string
	Resource, UILocales                        string
}

var authzParams = []string{"response_type", "client_id", "redirect_uri", "state", "scope", "code_challenge", "code_challenge_method", "resource", "ui_locales"}

// parseAuthz reads the known parameters; unknown ones are ignored
// (RFC 6749 §3.1), repeated ones are an error.
func parseAuthz(v url.Values) (authzRequest, string) {
	for _, name := range authzParams {
		if len(v[name]) > 1 {
			return authzRequest{}, "parameter \"" + name + "\" appears more than once"
		}
	}
	return authzRequest{
		ClientID: v.Get("client_id"), RedirectURI: v.Get("redirect_uri"), ResponseType: v.Get("response_type"), State: v.Get("state"),
		Scope: v.Get("scope"), CodeChallenge: v.Get("code_challenge"), CodeChallengeMethod: v.Get("code_challenge_method"),
		Resource: v.Get("resource"), UILocales: v.Get("ui_locales"),
	}, ""
}

// encode is the canonical query string of the request: what the forms carry.
func (a authzRequest) encode() string {
	v := url.Values{}
	set := func(name, value string) {
		if value != "" {
			v.Set(name, value)
		}
	}
	set("response_type", a.ResponseType)
	set("client_id", a.ClientID)
	set("redirect_uri", a.RedirectURI)
	set("state", a.State)
	set("scope", a.Scope)
	set("code_challenge", a.CodeChallenge)
	set("code_challenge_method", a.CodeChallengeMethod)
	set("resource", a.Resource)
	set("ui_locales", a.UILocales)
	return v.Encode()
}

// Cookie that protects the sign-in form against cross-site submission
// (there is no session to bind a token to yet). With Secure cookies the
// __Host- prefix keeps other subdomains from planting it.
func (s *Server) preSessionCookieName() string {
	if s.cfg.CookieSecure {
		return "__Host-la_oauth_csrf"
	}
	return "la_oauth_csrf"
}

func (s *Server) preSessionToken(w http.ResponseWriter, r *http.Request) string {
	name := s.preSessionCookieName()
	if ck, err := r.Cookie(name); err == nil && len(ck.Value) == 32 {
		return ck.Value
	}
	token := auth.NewCSRFToken()
	http.SetCookie(w, &http.Cookie{Name: name, Value: token, Path: "/", MaxAge: 3600, HttpOnly: true, Secure: s.cfg.CookieSecure, SameSite: http.SameSiteLaxMode})
	return token
}

func sameToken(a, b string) bool {
	return a != "" && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// sameOriginPost reports whether a form submission comes from this server's
// own pages, as far as the browser tells: Origin (when sent) must be the
// public origin and Sec-Fetch-Site (when sent) must be same-origin.
func (s *Server) sameOriginPost(r *http.Request) bool {
	if o := r.Header.Get("Origin"); o != "" && !strings.EqualFold(o, s.cfg.PublicURL) {
		return false
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && !strings.EqualFold(site, "same-origin") {
		return false
	}
	return true
}

// redirectBack sends the user agent to the client's (verified) redirect URI
// with the given response parameters plus state and iss (RFC 9207).
func (s *Server) redirectBack(w http.ResponseWriter, r *http.Request, a authzRequest, params url.Values) {
	u, err := url.Parse(a.RedirectURI)
	if err != nil {
		http.Error(w, "invalid redirect URI", http.StatusBadRequest)
		return
	}
	q := u.Query()
	for k, v := range params {
		q[k] = v
	}
	if a.State != "" {
		q.Set("state", a.State)
	}
	q.Set("iss", s.cfg.PublicURL)
	u.RawQuery = q.Encode()
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
	// 303 after a form submission, so the browser follows with GET.
	status := http.StatusFound
	if r.Method == http.MethodPost {
		status = http.StatusSeeOther
	}
	h.Set("Location", u.String())
	w.WriteHeader(status)
}

func (s *Server) redirectError(w http.ResponseWriter, r *http.Request, a authzRequest, code, description string) {
	s.mOAuth.With("authorize_" + code).Inc()
	s.redirectBack(w, r, a, url.Values{"error": {code}, "error_description": {description}})
}

func (s *Server) oauthAuthorize(w *statusWriter, r *http.Request, info *requestInfo) {
	now := s.now()
	ctx := r.Context()

	var form url.Values
	var query url.Values
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		query = r.URL.Query()
	case http.MethodPost:
		var problem string
		if form, problem = readForm(w, r); problem != "" {
			s.renderPage(w, http.StatusBadRequest, pageData{T: pickPageLocale(r, ""), Kind: "error", ErrorDetail: problem})
			return
		}
		var err error
		if query, err = url.ParseQuery(form.Get("q")); err != nil {
			s.renderPage(w, http.StatusBadRequest, pageData{T: pickPageLocale(r, ""), Kind: "error", ErrorDetail: "the form is incomplete"})
			return
		}
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	a, problem := parseAuthz(query)
	T := pickPageLocale(r, a.UILocales)
	if tags := strings.Fields(a.UILocales); len(tags) > 0 && localeFor(tags[0]) != nil {
		// An explicit choice (the language link) is remembered.
		http.SetCookie(w, &http.Cookie{Name: localeCookie, Value: T.Lang, Path: "/", MaxAge: 365 * 24 * 3600, Secure: s.cfg.CookieSecure, SameSite: http.SameSiteLaxMode})
	}
	page := pageData{T: T, Query: a.encode()}
	other := a
	other.UILocales = T.OtherLangTag
	page.LanguageLink = "/oauth/authorize?" + other.encode()

	fatal := func(status int, detail string) {
		s.mOAuth.With("authorize_rejected").Inc()
		p := page
		p.Kind, p.ErrorDetail, p.LanguageLink = "error", detail, ""
		s.renderPage(w, status, p)
	}
	if problem != "" {
		fatal(http.StatusBadRequest, problem)
		return
	}
	// What can be checked without looking anything up.
	if a.ClientID == "" {
		fatal(http.StatusBadRequest, "client_id is missing")
		return
	}
	if len(a.ClientID) > oauth.MaxClientIDLen || len(a.State) > maxStateLen {
		fatal(http.StatusBadRequest, "the request is too long")
		return
	}
	if a.RedirectURI == "" {
		fatal(http.StatusBadRequest, "redirect_uri is missing")
		return
	}
	if err := oauth.ValidateRedirectURI(a.RedirectURI); err != nil {
		fatal(http.StatusBadRequest, err.Error())
		return
	}
	if r.Method == http.MethodPost && !s.sameOriginPost(r) {
		fatal(http.StatusForbidden, "cross-site form submission refused")
		return
	}

	sess, _, err := s.sessionFromCookie(r, now)
	if err != nil {
		s.log.Error("session lookup failed", "req_id", info.id, "err", err.Error())
		fatal(http.StatusInternalServerError, "")
		return
	}
	action := form.Get("action")
	toSelf := func() {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Location", "/oauth/authorize?"+a.encode())
		w.WriteHeader(http.StatusSeeOther)
	}

	// ---- not signed in: the sign-in form ----
	if sess == nil {
		// A registered client can be looked up without any request leaving
		// this server; a broken link is better reported before asking for a
		// password. Metadata documents are only fetched for signed-in users.
		if client, err := s.oauthClient(ctx, a.ClientID, 0, now); err == nil {
			if !oauth.MatchRedirectURI(client.RedirectURIs, a.RedirectURI) {
				fatal(http.StatusBadRequest, "redirect_uri is not registered for this client")
				return
			}
		} else if ce := (*clientError)(nil); errors.As(err, &ce) {
			fatal(http.StatusBadRequest, ce.msg)
			return
		} else if !errors.Is(err, errClientDeferred) {
			s.log.Error("client lookup failed", "req_id", info.id, "err", err.Error())
			fatal(http.StatusInternalServerError, "")
			return
		}
		login := page
		login.Kind = "login"
		login.CSRF = s.preSessionToken(w, r)
		status := http.StatusOK
		if action == "login" {
			login.Email = clipASCII(form.Get("email"), 254)
			ck, _ := r.Cookie(s.preSessionCookieName())
			switch {
			case ck == nil || !sameToken(form.Get("csrf"), ck.Value):
				login.Notice, status = T.ErrForm, http.StatusForbidden
			default:
				cookie, err := s.passwordLogin(ctx, s.clientIP(r), now, form.Get("email"), form.Get("password"), form.Get("totp"))
				if err == nil {
					s.setSessionCookie(w, cookie)
					toSelf()
					return
				}
				var ae *apiError
				if !errors.As(err, &ae) {
					s.log.Error("sign-in failed", "req_id", info.id, "err", err.Error())
					ae = errInternal()
				}
				status = ae.Status
				switch ae.Code {
				case "invalid_credentials":
					login.Notice = T.ErrCredentials
				case "totp_required":
					login.Notice = T.ErrTOTPRequired
				case "invalid_totp":
					login.Notice = T.ErrTOTP
				case "rate_limited":
					login.Notice = T.ErrRateLimited
					w.Header().Set("Retry-After", retryAfter(time.Duration(ae.RetryAfter)*time.Second))
				default:
					login.Notice = T.ErrInternal
				}
			}
		} else if r.Method == http.MethodPost {
			// The session ended between showing the consent page and the click.
			login.Notice = T.ErrForm
		}
		// The redirect origin is listed for the form because signing in
		// leads, through this server's own redirects, to wherever the
		// request ends up; the server only ever sends the user to a
		// redirect URI it has verified.
		s.renderPage(w, status, login, originOf(a.RedirectURI))
		return
	}
	info.actor = db.UserActor(sess.User.ID).String()

	// ---- signed in ----
	if r.Method == http.MethodPost && !sameToken(form.Get("csrf"), sess.CSRF) {
		if action == "login" {
			// Signed in meanwhile (another tab): just show the consent page.
			toSelf()
			return
		}
		fatal(http.StatusForbidden, "the form has expired; reload the page and try again")
		return
	}
	if action == "logout" {
		if err := s.db.Write(ctx, func(tx *sql.Tx) error { return db.DeleteSession(ctx, tx, sess.IDHash) }); err != nil {
			s.log.Error("sign-out failed", "req_id", info.id, "err", err.Error())
			fatal(http.StatusInternalServerError, "")
			return
		}
		s.clearSessionCookie(w)
		toSelf()
		return
	}

	client, err := s.oauthClient(ctx, a.ClientID, sess.User.ID, now)
	if err != nil {
		var ce *clientError
		if errors.As(err, &ce) {
			fatal(http.StatusBadRequest, ce.msg)
			return
		}
		s.log.Error("client lookup failed", "req_id", info.id, "err", err.Error())
		fatal(http.StatusInternalServerError, "")
		return
	}
	if !oauth.MatchRedirectURI(client.RedirectURIs, a.RedirectURI) {
		fatal(http.StatusBadRequest, "redirect_uri is not registered for this client")
		return
	}

	// The client and its redirect URI are genuine: from here on, errors are
	// the client's to handle.
	if a.ResponseType != "code" {
		s.redirectError(w, r, a, "unsupported_response_type", "only response_type=code is supported")
		return
	}
	if a.CodeChallenge == "" {
		s.redirectError(w, r, a, "invalid_request", "code_challenge is required (PKCE)")
		return
	}
	if a.CodeChallengeMethod != "S256" {
		s.redirectError(w, r, a, "invalid_request", "code_challenge_method must be S256")
		return
	}
	if !oauth.ValidChallenge(a.CodeChallenge) {
		s.redirectError(w, r, a, "invalid_request", "code_challenge is not a valid S256 challenge")
		return
	}
	// RFC 8707: the token is for the MCP endpoint and nothing else. A
	// request that names no resource can only mean that one; a request that
	// names another is refused.
	resource := s.cfg.MCPResource()
	if a.Resource != "" && !oauth.SameResource(a.Resource, resource) && !oauth.SameResource(a.Resource, s.cfg.PublicURL) {
		s.redirectError(w, r, a, "invalid_target", "this server only issues tokens for "+resource)
		return
	}
	scopes := oauth.ParseScopes(a.Scope)
	if len(scopes) == 0 {
		if strings.TrimSpace(a.Scope) != "" {
			s.redirectError(w, r, a, "invalid_scope", "none of the requested scopes is known; supported: "+strings.Join(oauth.AllScopes, " "))
			return
		}
		scopes = []string{oauth.ScopeRead} // no scope asked for: the least
	}

	switch action {
	case "deny":
		s.mOAuth.With("authorize_denied").Inc()
		s.redirectBack(w, r, a, url.Values{"error": {"access_denied"}, "error_description": {"the user denied the request"}})
		return
	case "approve":
		if form.Get("grant_admin") != "1" {
			scopes = []string{oauth.ScopeRead}
		}
		code := oauth.NewCode()
		err := s.db.Write(ctx, func(tx *sql.Tx) error {
			if err := db.CreateOAuthCode(ctx, tx, db.OAuthCode{
				Hash: auth.HashSecret(code), UserID: sess.User.ID, ClientID: client.ClientID, RedirectURI: a.RedirectURI,
				CodeChallenge: a.CodeChallenge, Scopes: scopes, Resource: resource,
				CreatedAt: now.UnixMilli(), ExpiresAt: now.Add(oauthCodeTTL).UnixMilli(),
			}); err != nil {
				return err
			}
			if err := db.TouchOAuthClient(ctx, tx, client.ClientID, now.UnixMilli()); err != nil {
				return err
			}
			return s.oauthAudit(ctx, tx, now, sess.User.ID, "oauth.authorize",
				db.OAuthGrant{ClientID: client.ClientID, ClientName: client.Name, ClientKind: client.Kind, Scopes: scopes},
				map[string]any{"redirect_host": oauth.RedirectHost(a.RedirectURI)})
		})
		if err != nil {
			s.log.Error("cannot store authorization code", "req_id", info.id, "err", err.Error())
			s.redirectError(w, r, a, "server_error", "the authorization could not be recorded")
			return
		}
		s.mOAuth.With("authorize_approved").Inc()
		s.redirectBack(w, r, a, url.Values{"code": {code}})
		return
	}

	// ---- the consent page ----
	orgs, err := db.UserOrgs(ctx, s.db.R(), sess.User.ID)
	if err != nil {
		s.log.Error("cannot list organisations", "req_id", info.id, "err", err.Error())
		fatal(http.StatusInternalServerError, "")
		return
	}
	consent := page
	consent.Kind, consent.CSRF, consent.Email = "consent", sess.CSRF, sess.User.Email
	consent.ClientName = client.Name
	consent.RedirectHost = oauth.RedirectHost(a.RedirectURI)
	consent.Loopback = oauth.IsLoopbackRedirect(a.RedirectURI)
	consent.Unverified = client.Kind == db.OAuthClientDCR
	if client.Kind == db.OAuthClientCIMD {
		if u, err := url.Parse(client.ClientID); err == nil {
			consent.MetadataHost = u.Host
			if ru, err := url.Parse(a.RedirectURI); err == nil && !consent.Loopback {
				consent.HostMismatch = !strings.EqualFold(u.Hostname(), ru.Hostname())
			}
		}
	}
	consent.WantsAdmin = oauth.HasScope(scopes, oauth.ScopeAdmin)
	for _, o := range orgs {
		role := T.Roles[o.Role]
		if role == "" {
			role = o.Role
		}
		consent.Orgs = append(consent.Orgs, orgLine{Name: o.Name, Role: role})
	}
	s.renderPage(w, http.StatusOK, consent, originOf(a.RedirectURI))
}
