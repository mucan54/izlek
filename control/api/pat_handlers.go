package api

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/mucan54/izlek/control/auth"
	"github.com/mucan54/izlek/control/db"
	"github.com/mucan54/izlek/control/oauth"
)

// Personal access tokens (spec/control.md §14): "pat_…" credentials a user
// creates for clients that can send a header but cannot run a browser flow.
// A token acts as its user — that user's current role in every
// organisation — limited by the token's scopes (the OAuth scopes,
// izlek:read and izlek:admin).

const maxPersonalTokens = 25

// personalTokenBody is the API shape of a token.
type personalTokenBody struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Prefix     string   `json:"prefix"`
	Scopes     []string `json:"scopes"`
	CreatedAt  int64    `json:"created_at"`
	LastUsedAt *int64   `json:"last_used_at"`
	ExpiresAt  *int64   `json:"expires_at"`
}

func personalTokenView(t db.PersonalToken) personalTokenBody {
	out := personalTokenBody{ID: strconv.FormatInt(t.ID, 10), Name: t.Name, Prefix: t.Prefix, Scopes: t.Scopes, CreatedAt: t.CreatedAt}
	if t.LastUsedAt != 0 {
		v := t.LastUsedAt
		out.LastUsedAt = &v
	}
	if t.ExpiresAt != 0 {
		v := t.ExpiresAt
		out.ExpiresAt = &v
	}
	return out
}

// GET /api/me/tokens
func (s *Server) listPersonalTokens(c *reqCtx) (any, error) {
	list, err := db.ListPersonalTokens(c.ctx(), s.db.R(), c.user().ID, c.nowMs())
	if err != nil {
		return nil, err
	}
	out := make([]personalTokenBody, 0, len(list))
	for _, t := range list {
		out = append(out, personalTokenView(t))
	}
	return out, nil
}

// POST /api/me/tokens
func (s *Server) createPersonalToken(c *reqCtx) (any, error) {
	var in struct {
		Name      string   `json:"name"`
		Scopes    []string `json:"scopes"`
		ExpiresIn *int64   `json:"expires_in_days"`
	}
	if err := c.decode(&in); err != nil {
		return nil, err
	}
	name, err := cleanText("name", in.Name, 1, 80)
	if err != nil {
		return nil, err
	}
	badScopes := errBadRequest(`"scopes": izlek:read ya da izlek:read + izlek:admin`)
	param := ""
	for _, sc := range in.Scopes {
		if sc != oauth.ScopeRead && sc != oauth.ScopeAdmin {
			return nil, badScopes
		}
		param += sc + " "
	}
	scopes := oauth.ParseScopes(param) // admin implies read
	if len(scopes) == 0 {
		return nil, badScopes
	}
	var expires int64
	if in.ExpiresIn != nil {
		switch *in.ExpiresIn {
		case 30, 90, 365:
			expires = c.now.Add(time.Duration(*in.ExpiresIn) * 24 * time.Hour).UnixMilli()
		default:
			return nil, errBadRequest(`"expires_in_days": 30, 90, 365 ya da null`)
		}
	}
	secret := auth.NewPersonalToken()
	t := db.PersonalToken{
		UserID: c.user().ID, Name: name, Scopes: scopes, Hash: auth.HashSecret(secret), Prefix: auth.PersonalPrefixOf(secret),
		CreatedAt: c.nowMs(), ExpiresAt: expires,
	}
	err = s.db.Write(c.ctx(), func(tx *sql.Tx) error {
		existing, err := db.ListPersonalTokens(c.ctx(), tx, t.UserID, c.nowMs())
		if err != nil {
			return err
		}
		if len(existing) >= maxPersonalTokens {
			return errCode(http.StatusConflict, "too_many_tokens", "En çok "+strconv.Itoa(maxPersonalTokens)+" kişisel erişim token'ı olabilir; kullanılmayanları iptal edin")
		}
		if t.ID, err = db.CreatePersonalToken(c.ctx(), tx, t); err != nil {
			return err
		}
		// The entry names the token, never its secret or hash.
		detail := map[string]any{"name": name, "scopes": scopes}
		if expires != 0 {
			detail["expires_at"] = expires
		}
		return c.audit(tx, 0, "pat.create", strconv.FormatInt(t.ID, 10), detail)
	})
	if err != nil {
		return nil, err
	}
	// The only time the secret is shown.
	writeJSON(c.w, http.StatusCreated, struct {
		personalTokenBody
		Token string `json:"token"`
	}{personalTokenView(t), secret})
	return nil, nil
}

// DELETE /api/me/tokens/{id}
func (s *Server) revokePersonalToken(c *reqCtx) (any, error) {
	id, ok := parseID(c.params["id"])
	if !ok {
		return nil, errNotFound()
	}
	err := s.db.Write(c.ctx(), func(tx *sql.Tx) error {
		// Someone else's token does not exist as far as this user can tell.
		t, err := db.DeletePersonalToken(c.ctx(), tx, c.user().ID, id)
		if errors.Is(err, db.ErrNotFound) {
			return errNotFound()
		}
		if err != nil {
			return err
		}
		return c.audit(tx, 0, "pat.revoke", strconv.FormatInt(id, 10), map[string]any{"name": t.Name, "reason": "user"})
	})
	return nil, err
}

// personalToken resolves a pat_ secret to a live token and its user. It
// returns nil (and no error) when the secret is unknown or expired.
func (s *Server) personalToken(ctx context.Context, secret string, now time.Time, info *requestInfo) (*db.PersonalToken, *db.User, error) {
	if !auth.LooksLikePersonalToken(secret) {
		return nil, nil, nil
	}
	t, err := db.PersonalTokenByHash(ctx, s.db.R(), auth.HashSecret(secret))
	if errors.Is(err, db.ErrNotFound) || (err == nil && !t.Live(now.UnixMilli())) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	user, err := db.UserByID(ctx, s.db.R(), t.UserID)
	if errors.Is(err, db.ErrNotFound) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	info.actor = db.UserActor(user.ID).String()
	// last_used_at is advisory; one write a minute per token.
	if nowMs := now.UnixMilli(); nowMs-t.LastUsedAt > time.Minute.Milliseconds() {
		if err := s.db.Write(ctx, func(tx *sql.Tx) error { return db.TouchPersonalToken(ctx, tx, t.ID, nowMs) }); err != nil {
			s.log.Warn("cannot record personal token use", "token", t.ID, "err", err.Error())
		}
	}
	return &t, &user, nil
}

// Routes a personal access token may use on the API, besides everything
// under /api/projects/{pid}: the account-level reads a script needs to find
// its way, and creating a project.
var patAccountRoutes = map[string]bool{
	"GET /api/me":        true,
	"GET /api/projects":  true,
	"POST /api/projects": true,
}

// Project routes a personal access token may NOT use although an admin
// could: they destroy data for good, and a token that reaches every
// project of its user should not be able to. They stay with the web UI
// (and, per project, with sk_ keys).
var patDeniedRoutes = map[string]bool{
	"DELETE /api/projects/:pid":         true, // closes the project
	"POST /api/projects/:pid/deletions": true, // deletes people's data
}

// Requests that only read although they are not GETs.
var patReadRoutes = map[string]bool{
	"POST /api/projects/:pid/q/:endpoint":         true,
	"POST /api/projects/:pid/flags/:key/evaluate": true,
}

// authenticatePersonal handles "Authorization: Bearer pat_…" on the API.
// The token acts as its user; role checks are the handlers'. Everything
// about the account itself — sessions, password, second factor, tokens,
// connected applications, organisation members, audit log, organisation
// deletion — is out of its reach.
func (c *reqCtx) authenticatePersonal(rt *route, secret string) error {
	t, user, err := c.s.personalToken(c.ctx(), secret, c.now, c.info)
	if err != nil {
		return err
	}
	if t == nil {
		c.s.mKeyAuth.Inc()
		return errCode(http.StatusUnauthorized, "unauthorized", "Geçersiz, süresi dolmuş ya da iptal edilmiş erişim token'ı")
	}
	name := rt.method + " " + rt.pattern
	if !(rt.auth == authProject && !patDeniedRoutes[name]) && !patAccountRoutes[name] {
		return errCode(http.StatusForbidden, "forbidden", "Kişisel erişim token'ları bu ucu kullanamaz")
	}
	if !patReadRoutes[name] && !isSafeMethod(c.r.Method) && !oauth.HasScope(t.Scopes, oauth.ScopeAdmin) {
		return errCode(http.StatusForbidden, "insufficient_scope", "Bu işlem için token'ın izlek:admin kapsamı gerekir")
	}
	c.session = &db.Session{User: *user}
	c.pat = t
	c.via = &mcpVia{PATID: t.ID, PATName: t.Name}
	return nil
}
