package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mucan54/izlek/control/auth"
	"github.com/mucan54/izlek/control/db"
	"github.com/mucan54/izlek/control/mcp"
	"github.com/mucan54/izlek/control/oauth"
)

// The MCP endpoint, POST /mcp (spec/control.md §13, docs/mcp.md).
//
// This file is the part in front of the protocol handler: Origin check,
// authentication, and the bridge that lets a tool run an ordinary API
// handler as the caller. A tool never has rules of its own: validation,
// role checks, limits and audit entries are the handler's.

// Limits of the endpoint.
const (
	mcpAuthFailMax = 30  // failed bearer credentials per minute, per address
	mcpCallMax     = 300 // tool calls per minute, per key or grant
	// A tool result goes into a model's context window; a query answer
	// beyond this is useless there and is refused with advice to narrow it.
	mcpMaxQueryBytes = 256 << 10
)

// mcpPrincipal is who an MCP request acts as: an sk_ key (one project, the
// admin role) or a user who authorised an OAuth client (that user's roles,
// limited by the scopes of the grant).
type mcpPrincipal struct {
	key   *db.Key
	user  *db.User
	grant *db.OAuthGrant
}

func (p *mcpPrincipal) hasScope(scope string) bool {
	if p.key != nil {
		return true // only admin-scoped keys get this far
	}
	return oauth.HasScope(p.grant.Scopes, scope)
}

func (p *mcpPrincipal) limiterKey() string {
	if p.key != nil {
		return "key:" + strconv.FormatInt(p.key.ID, 10)
	}
	return "grant:" + strconv.FormatInt(p.grant.ID, 10)
}

type mcpContextKey struct{}

type mcpContext struct {
	principal *mcpPrincipal
	info      *requestInfo
	now       time.Time
}

// originAllowed implements the Origin rule of the Streamable HTTP transport
// (protection against DNS rebinding and against other sites driving the
// endpoint from a browser). Requests without an Origin header are not from
// a browser page. The server's own origin is the configured public URL —
// never the Host header, which a rebinding attacker controls.
func (s *Server) originAllowed(origin string) bool {
	if origin == "" {
		return true
	}
	if s.cfg.PublicURL != "" && strings.EqualFold(origin, s.cfg.PublicURL) {
		return true
	}
	for _, o := range s.cfg.MCPOrigins() {
		if strings.EqualFold(origin, o) {
			return true
		}
	}
	return false
}

// corsPreflight adds CORS headers for an allowed browser origin and answers
// a preflight request; it reports whether the request has been answered.
// The origin is echoed only when it is on the allow-list.
func (s *Server) corsPreflight(w http.ResponseWriter, r *http.Request, methods string) bool {
	origin := r.Header.Get("Origin")
	allowed := origin != "" && s.originAllowed(origin)
	if allowed {
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", origin)
		h.Add("Vary", "Origin")
		h.Set("Access-Control-Expose-Headers", "WWW-Authenticate")
	}
	if r.Method != http.MethodOptions {
		return false
	}
	if !allowed || r.Header.Get("Access-Control-Request-Method") == "" {
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusForbidden)
		return true
	}
	h := w.Header()
	h.Set("Access-Control-Allow-Methods", methods)
	h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Accept, MCP-Protocol-Version, Mcp-Method, Mcp-Name, Mcp-Session-Id")
	h.Set("Access-Control-Max-Age", "600")
	w.WriteHeader(http.StatusNoContent)
	return true
}

// bearerChallenge builds the WWW-Authenticate header that tells a client
// where the protected resource metadata is (RFC 9728 §5.1) and which scopes
// to ask for.
func (s *Server) bearerChallenge(errCode, description string, scopes []string) string {
	var parts []string
	if errCode != "" {
		parts = append(parts, `error="`+errCode+`"`)
		if description != "" {
			parts = append(parts, `error_description="`+description+`"`)
		}
	}
	if s.cfg.OAuthEnabled() {
		parts = append(parts, `scope="`+strings.Join(scopes, " ")+`"`,
			`resource_metadata="`+s.cfg.PublicURL+`/.well-known/oauth-protected-resource/mcp"`)
	} else {
		parts = append(parts, `realm="izlek"`)
	}
	return "Bearer " + strings.Join(parts, ", ")
}

func (s *Server) mcpUnauthorized(w http.ResponseWriter, errCode, description, message string) {
	w.Header().Set("WWW-Authenticate", s.bearerChallenge(errCode, description, oauth.AllScopes))
	mcp.WriteError(w, http.StatusUnauthorized, nil, &mcp.Error{Code: mcp.CodeInvalidRequest, Message: message})
}

// serveMCP is the entry point of POST /mcp.
func (s *Server) serveMCP(w *statusWriter, r *http.Request, info *requestInfo) {
	info.route = "/mcp"
	if origin := r.Header.Get("Origin"); !s.originAllowed(origin) {
		s.mMCP.With("origin_rejected").Inc()
		mcp.WriteError(w, http.StatusForbidden, nil, &mcp.Error{Code: mcp.CodeInvalidRequest, Message: "Origin not allowed"})
		return
	}
	if s.corsPreflight(w, r, "POST") {
		return
	}
	// A tool may wait for the query service.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(s.proxy.timeout + 30*time.Second))

	// Authentication comes before anything is read from the body: a client
	// without credentials must get the 401 that starts the OAuth flow, even
	// for a request this server would otherwise reject.
	now := s.now()
	ip := s.clientIP(r)
	if blocked, wait := s.mcpAuthFailLimit.Blocked(ip); blocked {
		w.Header().Set("Retry-After", retryAfter(wait))
		mcp.WriteError(w, http.StatusTooManyRequests, nil, &mcp.Error{Code: mcp.CodeInvalidRequest, Message: "Too many failed authentication attempts"})
		return
	}
	token, ok := bearerToken(r)
	if !ok || token == "" {
		s.mMCP.With("unauthenticated").Inc()
		s.mcpUnauthorized(w, "", "", "Authentication required: send an OAuth access token or an admin-scoped secret key as a Bearer token")
		return
	}
	principal, status, err := s.mcpAuthenticate(r.Context(), token, now, info)
	if err != nil {
		s.log.Error("mcp authentication failed", "req_id", info.id, "err", err.Error())
		mcp.WriteError(w, http.StatusInternalServerError, nil, &mcp.Error{Code: mcp.CodeInternalError, Message: "Internal error"})
		return
	}
	switch status {
	case http.StatusOK:
	case http.StatusForbidden:
		s.mMCP.With("forbidden").Inc()
		mcp.WriteError(w, http.StatusForbidden, nil, &mcp.Error{Code: mcp.CodeInvalidRequest, Message: "This secret key does not have the admin scope; create a key with the admin scope or connect with OAuth"})
		return
	default:
		s.mcpAuthFailLimit.Add(ip)
		s.mMCP.With("invalid_token").Inc()
		s.mcpUnauthorized(w, "invalid_token", "The access token is invalid, expired or revoked", "The bearer token is invalid, expired or revoked")
		return
	}
	ctx := context.WithValue(r.Context(), mcpContextKey{}, &mcpContext{principal: principal, info: info, now: now})
	s.mcp.ServeHTTP(w, r.WithContext(ctx))
}

// mcpAuthenticate resolves a bearer credential. The returned status is 200,
// 401 (not a valid credential) or 403 (a valid key without the admin scope).
func (s *Server) mcpAuthenticate(ctx context.Context, token string, now time.Time, info *requestInfo) (*mcpPrincipal, int, error) {
	switch {
	case auth.LooksLikeSecretKey(token):
		key, err := s.adminKey(ctx, token, info)
		var ae *apiError
		if errors.As(err, &ae) {
			return nil, ae.Status, nil
		}
		if err != nil {
			return nil, 0, err
		}
		s.touchKey(ctx, key, now)
		return &mcpPrincipal{key: key}, http.StatusOK, nil

	case oauth.LooksLikeAccessToken(token) && s.cfg.OAuthEnabled():
		tok, err := db.OAuthTokenByHash(ctx, s.db.R(), auth.HashSecret(token), db.OAuthAccess)
		if errors.Is(err, db.ErrNotFound) {
			return nil, http.StatusUnauthorized, nil
		}
		if err != nil {
			return nil, 0, err
		}
		g := tok.Grant
		// RFC 8707: only tokens issued for this resource are accepted. A
		// token minted while the server had another public URL is not.
		if tok.ExpiresAt <= now.UnixMilli() || !g.Live(now.UnixMilli()) || !oauth.SameResource(g.Resource, s.cfg.MCPResource()) {
			return nil, http.StatusUnauthorized, nil
		}
		user, err := db.UserByID(ctx, s.db.R(), g.UserID)
		if errors.Is(err, db.ErrNotFound) {
			return nil, http.StatusUnauthorized, nil
		}
		if err != nil {
			return nil, 0, err
		}
		info.actor = db.UserActor(user.ID).String()
		if now.UnixMilli()-g.LastUsedAt > time.Minute.Milliseconds() {
			if err := s.db.Write(ctx, func(tx *sql.Tx) error { return db.TouchOAuthGrant(ctx, tx, g.ID, now.UnixMilli()) }); err != nil {
				s.log.Warn("cannot record grant use", "grant", g.ID, "err", err.Error())
			}
		}
		return &mcpPrincipal{user: &user, grant: &g}, http.StatusOK, nil
	}
	return nil, http.StatusUnauthorized, nil
}

// ---- the tool backend ----

// tool is one MCP tool: its public definition and how it runs.
type tool struct {
	def mcp.Tool
	// scope a grant needs: oauth.ScopeRead or oauth.ScopeAdmin.
	scope string
	// userOnly tools need a user (they span organisations) and are not
	// offered to sk_ keys, which belong to one project.
	userOnly bool
	run      func(tc *toolCall, args json.RawMessage) (*toolResult, error)
}

// toolResult is what a tool produced: Data becomes structuredContent and,
// unless Text is set, its JSON is also the text block.
type toolResult struct {
	Data any
	Text string
}

// toolFailure is a failure of the tool itself that the model can act on: it
// is reported as a result with isError, not as a protocol error.
type toolFailure struct{ msg string }

func (e *toolFailure) Error() string { return e.msg }

func failf(format string, args ...any) error { return &toolFailure{fmt.Sprintf(format, args...)} }

type mcpBackend struct{ s *Server }

func (b mcpBackend) available(p *mcpPrincipal, t *tool) bool {
	return p.hasScope(t.scope) && !(t.userOnly && p.key != nil)
}

// Tools lists what the caller's credential can use. A read-only grant does
// not see the tools that change things.
func (b mcpBackend) Tools(ctx context.Context) []mcp.Tool {
	mc := ctx.Value(mcpContextKey{}).(*mcpContext)
	out := make([]mcp.Tool, 0, len(b.s.tools))
	for _, t := range b.s.tools {
		if b.available(mc.principal, t) {
			out = append(out, t.def)
		}
	}
	return out
}

// English explanations of the API's stable error codes: tool results are
// read by a model, the API's own messages are written for the Turkish UI.
var toolErrorText = map[string]string{
	"bad_request":            "The arguments are not valid.",
	"invalid_flag":           "The flag definition is not valid.",
	"forbidden":              "The caller's role does not allow this.",
	"not_found":              "Not found: the project (or the named item in it) does not exist or is not visible to the caller.",
	"conflict":               "It already exists.",
	"limit_exceeded":         "A limit on the number of items has been reached.",
	"too_large":              "The request is too large.",
	"query_not_configured":   "The control plane is not connected to a query service (LA_QUERY_INTERNAL_SECRET is not set), so analytics tools are unavailable.",
	"upstream_unavailable":   "The query service cannot be reached.",
	"upstream_timeout":       "The query service did not answer in time; narrow the date range or simplify the query.",
	"upstream_auth":          "The query service rejected the control plane's internal credential; an operator has to fix the configuration.",
	"unavailable":            "The request was interrupted; try again.",
	"last_owner":             "An organisation must keep at least one owner.",
	"invalid_email":          "The e-mail address is not valid.",
	"unsupported_media_type": "The request body must be JSON.",
}

func errorResult(text string) *mcp.CallResult {
	return &mcp.CallResult{Content: []mcp.Content{mcp.Text(text)}, IsError: true}
}

// Call runs a tool.
func (b mcpBackend) Call(ctx context.Context, name string, args json.RawMessage) (*mcp.CallResult, error) {
	s := b.s
	mc := ctx.Value(mcpContextKey{}).(*mcpContext)
	p := mc.principal
	t := s.toolIndex[name]
	if t == nil || (t.userOnly && p.key != nil) {
		return nil, &mcp.Error{Code: mcp.CodeInvalidParams, Message: "Unknown tool: " + clipASCII(name, 80)}
	}
	if !p.hasScope(t.scope) {
		// The step-up signal of the MCP authorization spec: the client may
		// ask the user for a grant with the missing scope.
		s.mMCP.With("insufficient_scope").Inc()
		return nil, &mcp.HTTPError{
			Status: http.StatusForbidden,
			Header: http.Header{"Www-Authenticate": {s.bearerChallenge("insufficient_scope", "This tool needs the "+t.scope+" scope", []string{oauth.ScopeRead, t.scope})}},
			Err:    &mcp.Error{Code: mcp.CodeInvalidRequest, Message: "Insufficient scope: " + name + " needs " + t.scope},
		}
	}
	mc.info.route = "mcp:" + name
	if ok, wait := take(s.mcpCallLimit, p.limiterKey()); !ok {
		return errorResult(fmt.Sprintf("Rate limit reached (%d tool calls per minute). Wait %s seconds and try again.", mcpCallMax, retryAfter(wait))), nil
	}
	tc := &toolCall{s: s, ctx: ctx, p: p, tool: t, info: mc.info, now: mc.now}
	res, err := t.run(tc, args)
	if err != nil {
		var tf *toolFailure
		var ae *apiError
		switch {
		case errors.As(err, &tf):
			return errorResult(tf.msg), nil
		case errors.As(err, &ae):
			if ae.Status >= 500 && ae.Code == "internal" {
				return errorResult("The server failed to carry out the request (internal error). Nothing may have been changed; check before retrying."), nil
			}
			text := toolErrorText[ae.Code]
			if text == "" {
				text = "The request failed."
			}
			return errorResult(fmt.Sprintf("%s (error code %q; server message: %s)", text, ae.Code, ae.Message)), nil
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			return errorResult("The request was cancelled or timed out before it finished."), nil
		}
		s.log.Error("mcp tool failed", "req_id", mc.info.id, "tool", name, "err", err.Error())
		return errorResult("The server failed to carry out the request (internal error). Nothing may have been changed; check before retrying."), nil
	}
	text := res.Text
	if text == "" {
		raw, err := json.Marshal(res.Data)
		if err != nil {
			return nil, err
		}
		text = string(raw)
	}
	return &mcp.CallResult{Content: []mcp.Content{mcp.Text(text)}, StructuredContent: res.Data}, nil
}

// ---- running API handlers for a tool ----

// toolCall is one running tool invocation.
type toolCall struct {
	s    *Server
	ctx  context.Context
	p    *mcpPrincipal
	tool *tool
	info *requestInfo
	now  time.Time
}

// discardWriter is the response writer of a handler that runs for a tool:
// handlers return their result, they do not write it.
type discardWriter struct{ h http.Header }

func (d discardWriter) Header() http.Header       { return d.h }
func (discardWriter) Write(b []byte) (int, error) { return len(b), nil }
func (discardWriter) WriteHeader(int)             {}

// request builds the context an API handler runs in: the same caller, the
// given route parameters and JSON body. Audit entries the handler writes
// say which tool (and which OAuth client) the change came through.
func (tc *toolCall) request(method string, params map[string]string, body any) (*reqCtx, error) {
	var rd io.Reader = http.NoBody
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(raw)
	}
	r, err := http.NewRequestWithContext(tc.ctx, method, "/mcp", rd)
	if err != nil {
		return nil, err
	}
	r.Header.Set("Content-Type", "application/json")
	c := &reqCtx{
		s: tc.s, w: &statusWriter{ResponseWriter: discardWriter{h: http.Header{}}}, r: r, params: params, now: tc.now, info: tc.info,
		via: &mcpVia{Tool: tc.tool.def.Name},
	}
	if tc.p.key != nil {
		c.key = tc.p.key
	} else {
		// The user the grant acts for. There is no cookie session behind it;
		// handlers that need one (password, sign-out) are not tools.
		c.session = &db.Session{User: *tc.p.user}
		c.via.ClientID, c.via.ClientName, c.via.GrantID = tc.p.grant.ClientID, tc.p.grant.ClientName, tc.p.grant.ID
	}
	return c, nil
}

// invoke runs an API handler as the caller and returns its result.
func (tc *toolCall) invoke(h handlerFunc, method string, params map[string]string, body any) (any, error) {
	c, err := tc.request(method, params, body)
	if err != nil {
		return nil, err
	}
	return h(c)
}

// projectID turns the optional project_id argument into the route
// parameter. A key only has its own project; a user who can see exactly one
// project does not have to name it.
func (tc *toolCall) projectID(arg *int64) (string, error) {
	if arg != nil {
		if *arg < 1 || *arg > 1<<32-1 {
			return "", failf("project_id must be a positive integer (see list_projects).")
		}
		return strconv.FormatInt(*arg, 10), nil
	}
	if tc.p.key != nil {
		return strconv.FormatUint(uint64(tc.p.key.ProjectID), 10), nil
	}
	projects, err := db.ListProjectsForUser(tc.ctx, tc.s.db.R(), tc.p.user.ID)
	if err != nil {
		return "", err
	}
	switch len(projects) {
	case 1:
		return strconv.FormatUint(uint64(projects[0].ID), 10), nil
	case 0:
		return "", failf("There is no project yet. Create one with create_project.")
	}
	return "", failf("project_id is required: this account can see %d projects. Call list_projects and pass the id of the one you mean.", len(projects))
}

// decodeArgs reads tool arguments strictly: a misspelled argument is an
// error the model can correct, not something to ignore silently.
func decodeArgs(args json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(args))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var te *json.UnmarshalTypeError
		switch {
		case errors.As(err, &te) && te.Field != "":
			return failf("Argument %q has the wrong type (expected %s).", te.Field, te.Type.String())
		case strings.HasPrefix(err.Error(), "json: unknown field "):
			return failf("Unknown argument %s. Check the tool's input schema.", strings.TrimPrefix(err.Error(), "json: unknown field "))
		}
		return failf("The arguments are not valid: %s", strings.TrimPrefix(err.Error(), "json: "))
	}
	return nil
}

// query sends a request to the query service through the same checks as
// the query proxy of the API (project visible, viewer role) and returns the
// decoded JSON answer.
func (tc *toolCall) query(pid, endpoint string, body any) (any, error) {
	c, err := tc.request(http.MethodPost, map[string]string{"pid": pid}, nil)
	if err != nil {
		return nil, err
	}
	p, err := c.project(db.RoleViewer)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	resp, release, err := tc.s.upstream(c, p.ID, http.MethodPost, "/q/"+endpoint, raw)
	if err != nil {
		return nil, err
	}
	defer release()
	defer resp.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(resp.Body, mcpMaxQueryBytes+1))
	if err != nil {
		return nil, failf("The answer of the query service could not be read; try again.")
	}
	if resp.StatusCode != http.StatusOK {
		var envelope struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(answer, &envelope)
		if envelope.Error.Code == "" {
			return nil, failf("The query service answered HTTP %d.", resp.StatusCode)
		}
		return nil, failf("The query was rejected (HTTP %d, %s): %s", resp.StatusCode, envelope.Error.Code, clipText(envelope.Error.Message, 1000))
	}
	if len(answer) > mcpMaxQueryBytes {
		return nil, failf("The result is larger than %d KB, too much to be useful here. Narrow the date range, add filters, lower \"limit\", or aggregate (for SQL: GROUP BY and LIMIT).", mcpMaxQueryBytes>>10)
	}
	var out any
	dec := json.NewDecoder(bytes.NewReader(answer))
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil {
		return nil, failf("The query service returned something that is not JSON.")
	}
	return out, nil
}

func clipText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "") + "…"
}
