package api

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"math"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mucan54/izlek/control/auth"
	"github.com/mucan54/izlek/control/db"
	"github.com/mucan54/izlek/control/oauth"
)

// The built-in OAuth 2.1 authorization server (spec/control.md §13). It
// exists for one resource, the MCP endpoint, and implements only what the
// MCP authorization specification needs: authorization code + PKCE (S256),
// public clients, rotating refresh tokens, RFC 8707 audience binding,
// RFC 8414 / RFC 9728 metadata, RFC 7591 registration, Client ID Metadata
// Documents, RFC 9207 issuer identification and RFC 7009 revocation.

// Lifetimes.
const (
	oauthCodeTTL    = 2 * time.Minute
	oauthAccessTTL  = time.Hour
	oauthRefreshTTL = 30 * 24 * time.Hour  // unused refresh tokens expire after this
	oauthGrantTTL   = 180 * 24 * time.Hour // after this the user must approve again
	// Ended grants stay visible to PruneOAuth's caller for this long.
	oauthKeepEnded = 30 * 24 * time.Hour

	oauthBodyLimit = 16 << 10
	// A deployment serves a handful of assistants; this many dynamically
	// registered clients means registration is being abused.
	maxDCRClients = 5000
	maxStateLen   = 4096
)

// Rate limits (per client address unless noted).
const (
	oauthRegisterMax    = 30 // registrations per 10 minutes
	oauthRegisterWindow = 10 * time.Minute
	oauthTokenMax       = 300 // token and revocation requests per minute
	oauthTokenFailMax   = 30  // failed token requests per minute
	oauthFetchMax       = 20  // client metadata fetches per minute, per user
)

// take reports whether key may proceed under limiter l, and counts the
// attempt if so.
func take(l *auth.Limiter, key string) (bool, time.Duration) {
	if blocked, wait := l.Blocked(key); blocked {
		return false, wait
	}
	l.Add(key)
	return true, 0
}

func retryAfter(wait time.Duration) string {
	return strconv.Itoa(max(1, int(math.Ceil(wait.Seconds()))))
}

// ---- metadata ----

func writeMetadata(w http.ResponseWriter, r *http.Request, doc any) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeError(w, errCode(http.StatusMethodNotAllowed, "method_not_allowed", "Bu yöntem desteklenmiyor"))
		return
	}
	body, _ := json.Marshal(doc)
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "public, max-age=300")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

// protectedResourceMetadata is the RFC 9728 document of the MCP endpoint.
func (s *Server) protectedResourceMetadata() map[string]any {
	return map[string]any{
		"resource":                 s.cfg.MCPResource(),
		"authorization_servers":    []string{s.cfg.PublicURL},
		"scopes_supported":         oauth.AllScopes,
		"bearer_methods_supported": []string{"header"},
		"resource_name":            "izlek",
	}
}

// authorizationServerMetadata is the RFC 8414 document.
func (s *Server) authorizationServerMetadata() map[string]any {
	base := s.cfg.PublicURL
	doc := map[string]any{
		"issuer":                 base,
		"authorization_endpoint": base + "/oauth/authorize",
		"token_endpoint":         base + "/oauth/token",
		"revocation_endpoint":    base + "/oauth/revoke",
		"scopes_supported":       oauth.AllScopes,

		"response_types_supported":                   []string{"code"},
		"response_modes_supported":                   []string{"query"},
		"grant_types_supported":                      []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":           []string{"S256"},
		"token_endpoint_auth_methods_supported":      []string{"none"},
		"revocation_endpoint_auth_methods_supported": []string{"none"},
		// RFC 9207: every authorization response, errors included, names
		// the issuer.
		"authorization_response_iss_parameter_supported": true,
		"client_id_metadata_document_supported":          s.cfg.OAuthCIMD,
	}
	if s.cfg.OAuthDCR {
		doc["registration_endpoint"] = base + "/oauth/register"
	}
	return doc
}

// serveWellKnown answers /.well-known/*. Nothing here falls through to the
// single-page app: a client probing for metadata must get JSON or a 404,
// never an HTML page with status 200.
func (s *Server) serveWellKnown(w *statusWriter, r *http.Request, info *requestInfo) {
	info.route, info.quiet = "well-known", true
	if !s.cfg.OAuthEnabled() {
		writeError(w, errNotFound())
		return
	}
	if s.corsPreflight(w, r, "GET") {
		return
	}
	switch r.URL.Path {
	case "/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp":
		writeMetadata(w, r, s.protectedResourceMetadata())
	case "/.well-known/oauth-authorization-server":
		writeMetadata(w, r, s.authorizationServerMetadata())
	default:
		writeError(w, errNotFound())
	}
}

// ---- endpoints that speak the OAuth error format ----

func oauthJSON(w http.ResponseWriter, status int, body any) {
	raw, _ := json.Marshal(body)
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	h.Set("Pragma", "no-cache")
	h.Set("Content-Length", strconv.Itoa(len(raw)))
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func oauthFail(w http.ResponseWriter, status int, code, description string) {
	oauthJSON(w, status, map[string]string{"error": code, "error_description": description})
}

// serveOAuth routes /oauth/*.
func (s *Server) serveOAuth(w *statusWriter, r *http.Request, info *requestInfo) {
	info.route = r.URL.Path
	if !s.cfg.OAuthEnabled() {
		info.route = "oauth"
		writeError(w, errCode(http.StatusNotFound, "oauth_not_configured", "OAuth yapılandırılmamış (LA_PUBLIC_URL ayarlanmalı)"))
		return
	}
	switch r.URL.Path {
	case "/oauth/authorize":
		s.oauthAuthorize(w, r, info)
		return
	case "/oauth/token", "/oauth/register", "/oauth/revoke":
	default:
		info.route = "oauth"
		writeError(w, errNotFound())
		return
	}
	if r.URL.Path == "/oauth/register" && !s.cfg.OAuthDCR {
		writeError(w, errNotFound())
		return
	}
	if s.corsPreflight(w, r, "POST") {
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		oauthFail(w, http.StatusMethodNotAllowed, "invalid_request", "use POST")
		return
	}
	switch r.URL.Path {
	case "/oauth/token":
		s.oauthToken(w, r, info)
	case "/oauth/register":
		s.oauthRegister(w, r, info)
	case "/oauth/revoke":
		s.oauthRevoke(w, r, info)
	}
}

// readForm reads a bounded application/x-www-form-urlencoded body. A
// parameter that appears twice is an error (RFC 6749 §3.2).
func readForm(w http.ResponseWriter, r *http.Request) (url.Values, string) {
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/x-www-form-urlencoded" {
		return nil, "the request body must be application/x-www-form-urlencoded"
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, oauthBodyLimit))
	if err != nil {
		return nil, "the request body is too large or could not be read"
	}
	form, err := url.ParseQuery(string(raw))
	if err != nil {
		return nil, "the request body is not valid form data"
	}
	for name, values := range form {
		if len(values) > 1 {
			return nil, "parameter " + strconv.Quote(clipASCII(name, 40)) + " appears more than once"
		}
	}
	return form, ""
}

func clipASCII(s string, n int) string {
	if len(s) > n {
		s = s[:n]
	}
	return strings.Map(func(r rune) rune {
		if r < ' ' || r > '~' {
			return '?'
		}
		return r
	}, s)
}

// ---- dynamic client registration (RFC 7591) ----

// POST /oauth/register. Registration is open: a registered client is only a
// name and a list of redirect URIs, and gets nothing until a signed-in user
// approves it on the consent screen, which shows where the approval goes.
func (s *Server) oauthRegister(w *statusWriter, r *http.Request, info *requestInfo) {
	ip := s.clientIP(r)
	if ok, wait := take(s.oauthRegisterLimit, ip); !ok {
		w.Header().Set("Retry-After", retryAfter(wait))
		oauthFail(w, http.StatusTooManyRequests, "temporarily_unavailable", "too many registrations; try again later")
		return
	}
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		oauthFail(w, http.StatusBadRequest, "invalid_client_metadata", "the request body must be application/json")
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, oauthBodyLimit))
	if err != nil {
		oauthFail(w, http.StatusBadRequest, "invalid_client_metadata", "the request body is too large or could not be read")
		return
	}
	var md oauth.Metadata
	if err := json.Unmarshal(raw, &md); err != nil {
		oauthFail(w, http.StatusBadRequest, "invalid_client_metadata", "the request body is not a JSON object of client metadata")
		return
	}
	name, clientURI, redirectURIs, err := md.Clean()
	if err != nil {
		code := "invalid_client_metadata"
		if strings.HasPrefix(err.Error(), "redirect_uri") || strings.Contains(err.Error(), "redirect URI") {
			code = "invalid_redirect_uri"
		}
		oauthFail(w, http.StatusBadRequest, code, err.Error())
		return
	}
	if name == "" {
		name = "MCP client"
	}
	now := s.now()
	client := db.OAuthClient{
		ClientID: oauth.NewClientID(), Kind: db.OAuthClientDCR, Name: name, ClientURI: clientURI, RedirectURIs: redirectURIs,
		CreatedAt: now.UnixMilli(), LastUsedAt: now.UnixMilli(),
	}
	full := false
	err = s.db.Write(r.Context(), func(tx *sql.Tx) error {
		n, err := db.CountOAuthClients(r.Context(), tx, db.OAuthClientDCR)
		if err != nil {
			return err
		}
		if n >= maxDCRClients {
			// Make room by dropping registrations that were never used.
			if _, err := db.PruneOAuthClients(r.Context(), tx, now.UnixMilli(), now.Add(-time.Hour).UnixMilli(), now.Add(-oauthGrantTTL).UnixMilli()); err != nil {
				return err
			}
			if n, err = db.CountOAuthClients(r.Context(), tx, db.OAuthClientDCR); err != nil {
				return err
			}
			if n >= maxDCRClients {
				full = true
				return nil
			}
		}
		return db.SaveOAuthClient(r.Context(), tx, client)
	})
	if err != nil {
		s.log.Error("client registration failed", "req_id", info.id, "err", err.Error())
		oauthFail(w, http.StatusInternalServerError, "server_error", "registration failed")
		return
	}
	if full {
		w.Header().Set("Retry-After", "3600")
		oauthFail(w, http.StatusTooManyRequests, "temporarily_unavailable", "too many registered clients")
		return
	}
	s.mOAuth.With("register").Inc()
	s.log.Info("oauth client registered", "req_id", info.id, "client_id", client.ClientID, "redirect_hosts", redirectHosts(redirectURIs))
	out := map[string]any{
		"client_id":                  client.ClientID,
		"client_id_issued_at":        now.Unix(),
		"client_name":                client.Name,
		"redirect_uris":              client.RedirectURIs,
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
	}
	if clientURI != "" {
		out["client_uri"] = clientURI
	}
	oauthJSON(w, http.StatusCreated, out)
}

func redirectHosts(uris []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, u := range uris {
		if h := oauth.RedirectHost(u); h != "" && !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	return out
}

// ---- clients ----

// negativeCache remembers client_id URLs whose metadata could not be used,
// so that a broken or hostile URL is not fetched over and over.
type negativeCache struct {
	mu    sync.Mutex
	until map[string]negativeEntry
}

type negativeEntry struct {
	until  time.Time
	reason string
}

func (n *negativeCache) get(key string, now time.Time) (string, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	e, ok := n.until[key]
	if !ok || !now.Before(e.until) {
		delete(n.until, key)
		return "", false
	}
	return e.reason, true
}

func (n *negativeCache) put(key, reason string, now time.Time) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.until == nil {
		n.until = map[string]negativeEntry{}
	}
	if len(n.until) >= 1000 {
		for k, e := range n.until {
			if !now.Before(e.until) {
				delete(n.until, k)
			}
		}
		if len(n.until) >= 1000 {
			return
		}
	}
	n.until[key] = negativeEntry{until: now.Add(time.Minute), reason: reason}
}

// errClientDeferred: the client is described by a metadata document that is
// not cached, and the caller did not allow fetching it.
var errClientDeferred = errors.New("api: client metadata not fetched yet")

// clientError is a problem with the client that is shown to the user (in
// English: it is addressed to whoever configured the client).
type clientError struct{ msg string }

func (e *clientError) Error() string { return e.msg }

// oauthClient resolves a client_id. A registered client comes from the
// database. A metadata document URL is served from the cached row while it
// is fresh and fetched otherwise — but only when fetchFor names a signed-in
// user: nobody without an account here can make this server issue requests.
func (s *Server) oauthClient(ctx context.Context, clientID string, fetchFor int64, now time.Time) (db.OAuthClient, error) {
	if oauth.IsClientIDURL(clientID) {
		if !s.cfg.OAuthCIMD {
			return db.OAuthClient{}, &clientError{"this server does not accept Client ID Metadata Documents; register the client first"}
		}
		if _, err := oauth.ValidateClientIDURL(clientID); err != nil {
			return db.OAuthClient{}, &clientError{err.Error()}
		}
		cached, err := db.OAuthClientByID(ctx, s.db.R(), clientID)
		if err != nil && !errors.Is(err, db.ErrNotFound) {
			return db.OAuthClient{}, err
		}
		if err == nil && cached.Kind == db.OAuthClientCIMD && cached.FreshUntil > now.UnixMilli() {
			return cached, nil
		}
		if fetchFor == 0 {
			return db.OAuthClient{}, errClientDeferred
		}
		if reason, bad := s.cimdNegative.get(clientID, now); bad {
			return db.OAuthClient{}, &clientError{reason}
		}
		if ok, _ := take(s.oauthFetchLimit, "user:"+strconv.FormatInt(fetchFor, 10)); !ok {
			return db.OAuthClient{}, &clientError{"too many client metadata lookups; try again in a minute"}
		}
		doc, err := s.cimd.Fetch(ctx, clientID)
		if err != nil {
			if ctx.Err() != nil {
				return db.OAuthClient{}, ctx.Err()
			}
			s.cimdNegative.put(clientID, err.Error(), now)
			s.log.Warn("client metadata document rejected", "client_id", clipASCII(clientID, 200), "err", err.Error())
			return db.OAuthClient{}, &clientError{err.Error()}
		}
		name, clientURI, redirectURIs, _ := doc.Metadata.Clean()
		if name == "" {
			if u, err := url.Parse(clientID); err == nil {
				name = u.Hostname()
			}
		}
		client := db.OAuthClient{
			ClientID: clientID, Kind: db.OAuthClientCIMD, Name: name, ClientURI: clientURI, RedirectURIs: redirectURIs,
			CreatedAt: now.UnixMilli(), FetchedAt: now.UnixMilli(), FreshUntil: now.Add(doc.CacheFor).UnixMilli(), LastUsedAt: now.UnixMilli(),
		}
		if err := s.db.Write(ctx, func(tx *sql.Tx) error { return db.SaveOAuthClient(ctx, tx, client) }); err != nil {
			return db.OAuthClient{}, err
		}
		return client, nil
	}
	if !oauth.LooksLikeClientID(clientID) {
		return db.OAuthClient{}, &clientError{"unknown client_id"}
	}
	client, err := db.OAuthClientByID(ctx, s.db.R(), clientID)
	if errors.Is(err, db.ErrNotFound) {
		return client, &clientError{"unknown client_id (the registration may have expired; register again)"}
	}
	return client, err
}

// ---- token endpoint ----

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
}

// issueTokens stores a fresh access/refresh pair for a grant.
func issueTokens(ctx context.Context, tx *sql.Tx, grantID, grantExpires int64, scopes []string, now time.Time) (tokenResponse, error) {
	access, refresh := oauth.NewAccessToken(), oauth.NewRefreshToken()
	accessExp := min(now.Add(oauthAccessTTL).UnixMilli(), grantExpires)
	refreshExp := min(now.Add(oauthRefreshTTL).UnixMilli(), grantExpires)
	if err := db.CreateOAuthToken(ctx, tx, auth.HashSecret(access), grantID, db.OAuthAccess, now.UnixMilli(), accessExp); err != nil {
		return tokenResponse{}, err
	}
	if err := db.CreateOAuthToken(ctx, tx, auth.HashSecret(refresh), grantID, db.OAuthRefresh, now.UnixMilli(), refreshExp); err != nil {
		return tokenResponse{}, err
	}
	return tokenResponse{
		AccessToken: access, TokenType: "Bearer", ExpiresIn: max(1, (accessExp-now.UnixMilli())/1000),
		RefreshToken: refresh, Scope: strings.Join(scopes, " "),
	}, nil
}

// oauthAudit writes a user-level audit entry about an OAuth client. Tokens
// and codes never appear in it.
func (s *Server) oauthAudit(ctx context.Context, tx *sql.Tx, now time.Time, userID int64, action string, g db.OAuthGrant, extra map[string]any) error {
	detail := map[string]any{"client_name": g.ClientName, "client_kind": g.ClientKind, "scopes": g.Scopes}
	if g.ID != 0 {
		detail["grant"] = g.ID
	}
	for k, v := range extra {
		detail[k] = v
	}
	s.mAudit.Inc()
	return db.InsertAudit(ctx, tx, now.UnixMilli(), 0, db.UserActor(userID), action, g.ClientID, detail)
}

// revokeGrant ends a grant and records why.
func (s *Server) revokeGrant(ctx context.Context, tx *sql.Tx, g db.OAuthGrant, now time.Time, reason string) error {
	revoked, err := db.RevokeOAuthGrant(ctx, tx, g.ID, now.UnixMilli(), reason)
	if err != nil || !revoked {
		return err
	}
	return s.oauthAudit(ctx, tx, now, g.UserID, "oauth.revoke", g, map[string]any{"reason": reason})
}

// POST /oauth/token
func (s *Server) oauthToken(w *statusWriter, r *http.Request, info *requestInfo) {
	ip := s.clientIP(r)
	if ok, wait := take(s.oauthTokenLimit, ip); !ok {
		w.Header().Set("Retry-After", retryAfter(wait))
		oauthFail(w, http.StatusTooManyRequests, "temporarily_unavailable", "too many requests")
		return
	}
	form, problem := readForm(w, r)
	if problem != "" {
		oauthFail(w, http.StatusBadRequest, "invalid_request", problem)
		return
	}
	// deny answers a failed request. Guesses at codes and tokens are counted
	// against the address, and an address over the limit is told to slow
	// down. Requests that are valid are never refused on that account:
	// assistants share addresses, and one of them misbehaving must not keep
	// the others from refreshing their tokens.
	deny := func(code, description string) {
		s.mOAuth.With("token_" + code).Inc()
		if code == "invalid_grant" || code == "invalid_client" {
			if blocked, wait := s.oauthTokenFailLimit.Blocked(ip); blocked {
				w.Header().Set("Retry-After", retryAfter(wait))
				oauthFail(w, http.StatusTooManyRequests, "temporarily_unavailable", "too many failed requests")
				return
			}
			s.oauthTokenFailLimit.Add(ip)
		}
		oauthFail(w, http.StatusBadRequest, code, description)
	}
	clientID := form.Get("client_id")
	if clientID == "" {
		deny("invalid_client", "client_id is required")
		return
	}
	if len(clientID) > oauth.MaxClientIDLen {
		deny("invalid_client", "unknown client")
		return
	}
	now := s.now()
	ctx := r.Context()

	var out tokenResponse
	var failCode, failDesc string
	fail := func(code, desc string) error { failCode, failDesc = code, desc; return nil }

	switch form.Get("grant_type") {
	case "authorization_code":
		code := form.Get("code")
		if !oauth.LooksLikeCode(code) {
			deny("invalid_grant", "the authorization code is not valid")
			return
		}
		hash := auth.HashSecret(code)
		err := s.db.Write(ctx, func(tx *sql.Tx) error {
			row, err := db.OAuthCodeByHash(ctx, tx, hash)
			if errors.Is(err, db.ErrNotFound) {
				return fail("invalid_grant", "the authorization code is not valid")
			}
			if err != nil {
				return err
			}
			if subtle.ConstantTimeCompare([]byte(row.ClientID), []byte(clientID)) != 1 {
				return fail("invalid_grant", "the authorization code was issued to another client")
			}
			if row.UsedAt != 0 {
				// A code is redeemed once. A second attempt means it leaked:
				// whatever the first redemption obtained is withdrawn.
				if row.GrantID != 0 {
					if g, err := db.OAuthGrantByID(ctx, tx, row.GrantID); err == nil {
						if err := s.revokeGrant(ctx, tx, g, now, "code_replay"); err != nil {
							return err
						}
					} else if !errors.Is(err, db.ErrNotFound) {
						return err
					}
				}
				return fail("invalid_grant", "the authorization code has already been used")
			}
			if row.ExpiresAt <= now.UnixMilli() {
				return fail("invalid_grant", "the authorization code has expired")
			}
			// OAuth 2.1 no longer sends redirect_uri here; a client that
			// does must repeat the value of the authorization request.
			if ru, sent := form["redirect_uri"]; sent && ru[0] != row.RedirectURI {
				return fail("invalid_grant", "redirect_uri does not match the authorization request")
			}
			if res := form.Get("resource"); res != "" && !oauth.SameResource(res, row.Resource) {
				return fail("invalid_target", "resource does not match the authorization request")
			}
			verifier := form.Get("code_verifier")
			if verifier == "" {
				return fail("invalid_request", "code_verifier is required")
			}
			if !oauth.VerifyS256(verifier, row.CodeChallenge) {
				return fail("invalid_grant", "code_verifier does not match the code challenge")
			}
			client, err := db.OAuthClientByID(ctx, tx, row.ClientID)
			if err != nil {
				return err
			}
			grant := db.OAuthGrant{
				UserID: row.UserID, ClientID: row.ClientID, Scopes: row.Scopes, Resource: row.Resource,
				CreatedAt: now.UnixMilli(), ExpiresAt: now.Add(oauthGrantTTL).UnixMilli(),
				ClientName: client.Name, ClientKind: client.Kind,
			}
			if grant.ID, err = db.CreateOAuthGrant(ctx, tx, grant); err != nil {
				return err
			}
			if fresh, err := db.RedeemOAuthCode(ctx, tx, hash, grant.ID, now.UnixMilli()); err != nil {
				return err
			} else if !fresh {
				return errors.New("api: authorization code redeemed twice inside one transaction")
			}
			if out, err = issueTokens(ctx, tx, grant.ID, grant.ExpiresAt, grant.Scopes, now); err != nil {
				return err
			}
			if err := db.TouchOAuthClient(ctx, tx, row.ClientID, now.UnixMilli()); err != nil {
				return err
			}
			return s.oauthAudit(ctx, tx, now, row.UserID, "oauth.token", grant, map[string]any{"grant_type": "authorization_code"})
		})
		if err != nil {
			s.log.Error("token request failed", "req_id", info.id, "err", err.Error())
			oauthFail(w, http.StatusInternalServerError, "server_error", "the token request failed")
			return
		}

	case "refresh_token":
		refresh := form.Get("refresh_token")
		if !oauth.LooksLikeRefreshToken(refresh) {
			deny("invalid_grant", "the refresh token is not valid")
			return
		}
		hash := auth.HashSecret(refresh)
		err := s.db.Write(ctx, func(tx *sql.Tx) error {
			tok, err := db.OAuthTokenByHash(ctx, tx, hash, db.OAuthRefresh)
			if errors.Is(err, db.ErrNotFound) {
				return fail("invalid_grant", "the refresh token is not valid")
			}
			if err != nil {
				return err
			}
			g := tok.Grant
			if subtle.ConstantTimeCompare([]byte(g.ClientID), []byte(clientID)) != 1 {
				return fail("invalid_grant", "the refresh token was issued to another client")
			}
			if tok.UsedAt != 0 {
				// Refresh tokens rotate: each is valid once. One that comes
				// back after it was exchanged is either a client that lost a
				// response or a thief; the two cannot be told apart, so the
				// whole grant ends and the user has to approve again.
				if err := s.revokeGrant(ctx, tx, g, now, "refresh_token_reuse"); err != nil {
					return err
				}
				return fail("invalid_grant", "the refresh token has already been used; access has been revoked")
			}
			if !g.Live(now.UnixMilli()) || tok.ExpiresAt <= now.UnixMilli() {
				return fail("invalid_grant", "the refresh token has expired or was revoked")
			}
			if res := form.Get("resource"); res != "" && !oauth.SameResource(res, g.Resource) {
				return fail("invalid_target", "resource does not match the grant")
			}
			if sc := form.Get("scope"); sc != "" && !oauth.Subset(oauth.ParseScopes(sc), g.Scopes) {
				return fail("invalid_scope", "the requested scope exceeds what was granted")
			}
			if fresh, err := db.RotateOAuthRefresh(ctx, tx, hash, now.UnixMilli()); err != nil {
				return err
			} else if !fresh {
				return errors.New("api: refresh token rotated twice inside one transaction")
			}
			// Access tokens issued earlier are left to expire: a client with
			// several workers may still be using one.
			if out, err = issueTokens(ctx, tx, g.ID, g.ExpiresAt, g.Scopes, now); err != nil {
				return err
			}
			if err := db.TouchOAuthGrant(ctx, tx, g.ID, now.UnixMilli()); err != nil {
				return err
			}
			if err := db.TouchOAuthClient(ctx, tx, g.ClientID, now.UnixMilli()); err != nil {
				return err
			}
			return s.oauthAudit(ctx, tx, now, g.UserID, "oauth.token", g, map[string]any{"grant_type": "refresh_token"})
		})
		if err != nil {
			s.log.Error("token request failed", "req_id", info.id, "err", err.Error())
			oauthFail(w, http.StatusInternalServerError, "server_error", "the token request failed")
			return
		}

	case "":
		deny("invalid_request", "grant_type is required")
		return
	default:
		deny("unsupported_grant_type", "only authorization_code and refresh_token are supported")
		return
	}
	if failCode != "" {
		deny(failCode, failDesc)
		return
	}
	s.mOAuth.With("token_issued").Inc()
	oauthJSON(w, http.StatusOK, out)
}

// POST /oauth/revoke (RFC 7009). Revoking a refresh token ends the whole
// grant; revoking an access token ends that token. The answer is 200
// whether or not the token existed.
func (s *Server) oauthRevoke(w *statusWriter, r *http.Request, info *requestInfo) {
	if ok, wait := take(s.oauthTokenLimit, s.clientIP(r)); !ok {
		w.Header().Set("Retry-After", retryAfter(wait))
		oauthFail(w, http.StatusTooManyRequests, "temporarily_unavailable", "too many requests")
		return
	}
	form, problem := readForm(w, r)
	if problem != "" {
		oauthFail(w, http.StatusBadRequest, "invalid_request", problem)
		return
	}
	token := form.Get("token")
	if token == "" {
		oauthFail(w, http.StatusBadRequest, "invalid_request", "token is required")
		return
	}
	now := s.now()
	ctx := r.Context()
	clientID := form.Get("client_id")
	err := s.db.Write(ctx, func(tx *sql.Tx) error {
		hash := auth.HashSecret(token)
		switch {
		case oauth.LooksLikeRefreshToken(token):
			tok, err := db.OAuthTokenByHash(ctx, tx, hash, db.OAuthRefresh)
			if errors.Is(err, db.ErrNotFound) || (err == nil && clientID != "" && clientID != tok.Grant.ClientID) {
				return nil
			}
			if err != nil {
				return err
			}
			return s.revokeGrant(ctx, tx, tok.Grant, now, "client")
		case oauth.LooksLikeAccessToken(token):
			tok, err := db.OAuthTokenByHash(ctx, tx, hash, db.OAuthAccess)
			if errors.Is(err, db.ErrNotFound) || (err == nil && clientID != "" && clientID != tok.Grant.ClientID) {
				return nil
			}
			if err != nil {
				return err
			}
			_, err = db.DeleteOAuthToken(ctx, tx, hash)
			return err
		}
		return nil
	})
	if err != nil {
		s.log.Error("revocation failed", "req_id", info.id, "err", err.Error())
		oauthFail(w, http.StatusInternalServerError, "server_error", "the revocation failed")
		return
	}
	s.mOAuth.With("revoke").Inc()
	oauthJSON(w, http.StatusOK, struct{}{})
}

// ---- connected applications of the signed-in user ----

type connectedApp struct {
	ID            int64    `json:"id"`
	ClientID      string   `json:"client_id"`
	ClientName    string   `json:"client_name"`
	ClientKind    string   `json:"client_kind"`
	ClientURI     string   `json:"client_uri"`
	RedirectHosts []string `json:"redirect_hosts"`
	Scopes        []string `json:"scopes"`
	CreatedAt     int64    `json:"created_at"`
	LastUsedAt    *int64   `json:"last_used_at"`
	ExpiresAt     int64    `json:"expires_at"`
}

// GET /api/me/connected-apps
func (s *Server) listConnectedApps(c *reqCtx) (any, error) {
	grants, err := db.ListOAuthGrants(c.ctx(), s.db.R(), c.user().ID, c.nowMs())
	if err != nil {
		return nil, err
	}
	out := make([]connectedApp, 0, len(grants))
	for _, g := range grants {
		app := connectedApp{
			ID: g.ID, ClientID: g.ClientID, ClientName: g.ClientName, ClientKind: g.ClientKind, ClientURI: g.ClientURI,
			RedirectHosts: redirectHosts(g.RedirectURIs), Scopes: g.Scopes, CreatedAt: g.CreatedAt, ExpiresAt: g.ExpiresAt,
		}
		if g.LastUsedAt != 0 {
			used := g.LastUsedAt
			app.LastUsedAt = &used
		}
		out = append(out, app)
	}
	return out, nil
}

// DELETE /api/me/connected-apps/{id}
func (s *Server) revokeConnectedApp(c *reqCtx) (any, error) {
	id, ok := parseID(c.params["id"])
	if !ok {
		return nil, errNotFound()
	}
	err := s.db.Write(c.ctx(), func(tx *sql.Tx) error {
		g, err := db.OAuthGrantByID(c.ctx(), tx, id)
		// Someone else's grant does not exist as far as this user can tell.
		if errors.Is(err, db.ErrNotFound) || (err == nil && (g.UserID != c.user().ID || !g.Live(c.nowMs()))) {
			return errNotFound()
		}
		if err != nil {
			return err
		}
		return s.revokeGrant(c.ctx(), tx, g, c.now, "user")
	})
	return nil, err
}

// PruneOAuth deletes OAuth state that can no longer be used: expired codes
// and tokens, grants that ended a while ago, and registered clients nobody
// uses (never approved within a day, or idle for longer than a grant
// lives). The application runs it periodically.
func PruneOAuth(ctx context.Context, tx *sql.Tx, now time.Time) error {
	if err := db.PruneOAuth(ctx, tx, now.UnixMilli(), oauthKeepEnded.Milliseconds()); err != nil {
		return err
	}
	if _, err := db.PrunePersonalTokens(ctx, tx, now.UnixMilli()); err != nil {
		return err
	}
	_, err := db.PruneOAuthClients(ctx, tx, now.UnixMilli(), now.Add(-24*time.Hour).UnixMilli(), now.Add(-oauthGrantTTL).UnixMilli())
	return err
}
