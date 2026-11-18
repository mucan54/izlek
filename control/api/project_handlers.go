package api

import (
	"database/sql"
	"errors"
	"math"
	"strconv"

	"github.com/mucan54/izlek/control/auth"
	"github.com/mucan54/izlek/control/db"
)

// Defaults of a new project.
const (
	defaultRetentionDays       = 365
	defaultReplayRetentionDays = 30
	defaultMaxConcurrent       = 4
	defaultReplaySampleRate    = 0.1
	defaultTokenName           = "default"

	maxTokensPerProject = 100
	maxKeysPerProject   = 50
	maxSafeInteger      = 1<<53 - 1 // largest integer JSON consumers handle exactly
)

// GET /api/projects
func (s *Server) listProjects(c *reqCtx) (any, error) {
	return db.ListProjectsForUser(c.ctx(), s.db.R(), c.user().ID)
}

// projectWithToken is the response of POST /api/projects: the project plus
// the write token that was created with it.
type projectWithToken struct {
	db.Project
	Token string `json:"token"`
}

func newToken(projectID uint32, name string, now int64) db.Token {
	return db.Token{Token: auth.NewWriteToken(), ProjectID: projectID, Name: name, Status: "active", AllowedOrigins: []string{}, CreatedAt: now}
}

// POST /api/projects
func (s *Server) createProject(c *reqCtx) (any, error) {
	var in struct {
		OrgID flexInt `json:"org_id"`
		Name  string  `json:"name"`
	}
	if err := c.decode(&in); err != nil {
		return nil, err
	}
	if in.OrgID < 1 {
		return nil, errBadRequest(`"org_id" zorunludur`)
	}
	if err := c.orgByID(int64(in.OrgID), db.RoleAdmin); err != nil {
		return nil, err
	}
	name, err := cleanText("name", in.Name, 1, 100)
	if err != nil {
		return nil, err
	}
	var out projectWithToken
	err = s.db.Write(c.ctx(), func(tx *sql.Tx) error {
		p, err := db.CreateProject(c.ctx(), tx, db.Project{
			OrgID: int64(in.OrgID), Name: name, RetentionDays: defaultRetentionDays, ReplayRetentionDays: defaultReplayRetentionDays,
			DropKeys: []string{}, MaxConcurrentQueries: defaultMaxConcurrent, ReplaySampleRate: defaultReplaySampleRate,
		}, c.nowMs())
		if err != nil {
			return err
		}
		t := newToken(p.ID, defaultTokenName, c.nowMs())
		if err := db.CreateToken(c.ctx(), tx, t); err != nil {
			return err
		}
		pid := strconv.FormatUint(uint64(p.ID), 10)
		if err := c.audit(tx, p.OrgID, "project.create", pid, map[string]any{"name": name}); err != nil {
			return err
		}
		if err := c.audit(tx, p.OrgID, "token.create", t.Token, map[string]any{"project_id": p.ID, "name": t.Name}); err != nil {
			return err
		}
		out = projectWithToken{Project: p, Token: t.Token}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.notify.Notify()
	return out, nil
}

// GET /api/projects/{pid}
func (s *Server) getProject(c *reqCtx) (any, error) {
	return c.project(db.RoleViewer)
}

// PATCH /api/projects/{pid}
func (s *Server) updateProject(c *reqCtx) (any, error) {
	p, err := c.project(db.RoleAdmin)
	if err != nil {
		return nil, err
	}
	var in struct {
		Name                 *string   `json:"name"`
		RetentionDays        *int64    `json:"retention_days"`
		ReplayRetentionDays  *int64    `json:"replay_retention_days"`
		DropKeys             *[]string `json:"drop_keys"`
		ScanLimitBytes       *int64    `json:"scan_limit_bytes"`
		MaxConcurrentQueries *int64    `json:"max_concurrent_queries"`
		ReplaySampleRate     *float64  `json:"replay_sample_rate"`
	}
	if err := c.decode(&in); err != nil {
		return nil, err
	}
	changed := map[string]any{}
	if in.Name != nil {
		if p.Name, err = cleanText("name", *in.Name, 1, 100); err != nil {
			return nil, err
		}
		changed["name"] = p.Name
	}
	if in.RetentionDays != nil {
		// 0 is reserved for closed projects.
		if err := intInRange("retention_days", *in.RetentionDays, 1, 3650); err != nil {
			return nil, err
		}
		p.RetentionDays = int(*in.RetentionDays)
		changed["retention_days"] = p.RetentionDays
	}
	if in.ReplayRetentionDays != nil {
		if err := intInRange("replay_retention_days", *in.ReplayRetentionDays, 1, 3650); err != nil {
			return nil, err
		}
		p.ReplayRetentionDays = int(*in.ReplayRetentionDays)
		changed["replay_retention_days"] = p.ReplayRetentionDays
	}
	if in.DropKeys != nil {
		if p.DropKeys, err = cleanStrings("drop_keys", *in.DropKeys, 500, 200); err != nil {
			return nil, err
		}
		changed["drop_keys"] = p.DropKeys
	}
	if in.ScanLimitBytes != nil {
		if err := intInRange("scan_limit_bytes", *in.ScanLimitBytes, 0, maxSafeInteger); err != nil {
			return nil, err
		}
		p.ScanLimitBytes = *in.ScanLimitBytes
		changed["scan_limit_bytes"] = p.ScanLimitBytes
	}
	if in.MaxConcurrentQueries != nil {
		if err := intInRange("max_concurrent_queries", *in.MaxConcurrentQueries, 1, 64); err != nil {
			return nil, err
		}
		p.MaxConcurrentQueries = int(*in.MaxConcurrentQueries)
		changed["max_concurrent_queries"] = p.MaxConcurrentQueries
	}
	if in.ReplaySampleRate != nil {
		r := *in.ReplaySampleRate
		if math.IsNaN(r) || r < 0 || r > 1 {
			return nil, errBadRequest(`"replay_sample_rate" 0 ile 1 arasında olmalıdır`)
		}
		p.ReplaySampleRate = r
		changed["replay_sample_rate"] = r
	}
	err = s.db.Write(c.ctx(), func(tx *sql.Tx) error {
		// The project may have been closed since it was read.
		cur, err := db.ProjectByID(c.ctx(), tx, p.ID)
		if err != nil || cur.Closed() {
			return errNotFound()
		}
		if err := db.UpdateProject(c.ctx(), tx, p); err != nil {
			return err
		}
		return c.audit(tx, p.OrgID, "project.update", strconv.FormatUint(uint64(p.ID), 10), changed)
	})
	if err != nil {
		return nil, err
	}
	s.notify.Notify()
	return p, nil
}

// DELETE /api/projects/{pid} — closes the project.
func (s *Server) closeProject(c *reqCtx) (any, error) {
	p, err := c.project(db.RoleAdmin)
	if err != nil {
		return nil, err
	}
	err = s.db.Write(c.ctx(), func(tx *sql.Tx) error {
		if err := db.CloseProject(c.ctx(), tx, p.ID, c.nowMs()); err != nil {
			return err
		}
		return c.audit(tx, p.OrgID, "project.close", strconv.FormatUint(uint64(p.ID), 10), map[string]any{"name": p.Name})
	})
	if err != nil {
		return nil, err
	}
	s.notify.Notify()
	return nil, nil
}

// ---- write tokens ----

// GET /api/projects/{pid}/tokens
func (s *Server) listTokens(c *reqCtx) (any, error) {
	p, err := c.project(db.RoleViewer)
	if err != nil {
		return nil, err
	}
	return db.ListTokens(c.ctx(), s.db.R(), p.ID)
}

func checkRateLimit(v int64) error { return intInRange("rate_limit", v, 0, 1_000_000) }

// POST /api/projects/{pid}/tokens
func (s *Server) createToken(c *reqCtx) (any, error) {
	p, err := c.project(db.RoleAdmin)
	if err != nil {
		return nil, err
	}
	var in struct {
		Name           string   `json:"name"`
		AllowedOrigins []string `json:"allowed_origins"`
		RateLimit      *int64   `json:"rate_limit"`
		Replay         bool     `json:"replay"`
	}
	if err := c.decode(&in); err != nil {
		return nil, err
	}
	t := newToken(p.ID, "", c.nowMs())
	if t.Name, err = cleanText("name", in.Name, 1, 100); err != nil {
		return nil, err
	}
	if t.AllowedOrigins, err = cleanOrigins(in.AllowedOrigins); err != nil {
		return nil, err
	}
	if in.RateLimit != nil {
		if err := checkRateLimit(*in.RateLimit); err != nil {
			return nil, err
		}
		t.RateLimit = int(*in.RateLimit)
	}
	t.Replay = in.Replay
	err = s.db.Write(c.ctx(), func(tx *sql.Tx) error {
		existing, err := db.ListTokens(c.ctx(), tx, p.ID)
		if err != nil {
			return err
		}
		if len(existing) >= maxTokensPerProject {
			return errCode(409, "limit_exceeded", "Bir projede en çok "+strconv.Itoa(maxTokensPerProject)+" token olabilir")
		}
		if err := db.CreateToken(c.ctx(), tx, t); err != nil {
			return err
		}
		return c.audit(tx, p.OrgID, "token.create", t.Token, map[string]any{"project_id": p.ID, "name": t.Name})
	})
	if err != nil {
		return nil, err
	}
	s.notify.Notify()
	return t, nil
}

// PATCH /api/projects/{pid}/tokens/{token}
func (s *Server) updateToken(c *reqCtx) (any, error) {
	p, err := c.project(db.RoleAdmin)
	if err != nil {
		return nil, err
	}
	var in struct {
		Name           *string   `json:"name"`
		Status         *string   `json:"status"`
		AllowedOrigins *[]string `json:"allowed_origins"`
		RateLimit      *int64    `json:"rate_limit"`
		Replay         *bool     `json:"replay"`
	}
	if err := c.decode(&in); err != nil {
		return nil, err
	}
	var out db.Token
	err = s.db.Write(c.ctx(), func(tx *sql.Tx) error {
		t, err := db.TokenOf(c.ctx(), tx, p.ID, c.params["token"])
		if errors.Is(err, db.ErrNotFound) {
			return errNotFound()
		}
		if err != nil {
			return err
		}
		changed := map[string]any{"project_id": p.ID}
		if in.Name != nil {
			if t.Name, err = cleanText("name", *in.Name, 1, 100); err != nil {
				return err
			}
			changed["name"] = t.Name
		}
		if in.Status != nil {
			if *in.Status != "active" && *in.Status != "disabled" {
				return errBadRequest(`"status": active | disabled`)
			}
			t.Status = *in.Status
			changed["status"] = t.Status
		}
		if in.AllowedOrigins != nil {
			if t.AllowedOrigins, err = cleanOrigins(*in.AllowedOrigins); err != nil {
				return err
			}
			changed["allowed_origins"] = t.AllowedOrigins
		}
		if in.RateLimit != nil {
			if err := checkRateLimit(*in.RateLimit); err != nil {
				return err
			}
			t.RateLimit = int(*in.RateLimit)
			changed["rate_limit"] = t.RateLimit
		}
		if in.Replay != nil {
			t.Replay = *in.Replay
			changed["replay"] = t.Replay
		}
		if err := db.UpdateToken(c.ctx(), tx, t); err != nil {
			return err
		}
		out = t
		return c.audit(tx, p.OrgID, "token.update", t.Token, changed)
	})
	if err != nil {
		return nil, err
	}
	s.notify.Notify()
	return out, nil
}

// DELETE /api/projects/{pid}/tokens/{token}
func (s *Server) deleteToken(c *reqCtx) (any, error) {
	p, err := c.project(db.RoleAdmin)
	if err != nil {
		return nil, err
	}
	token := c.params["token"]
	err = s.db.Write(c.ctx(), func(tx *sql.Tx) error {
		if err := db.DeleteToken(c.ctx(), tx, p.ID, token); errors.Is(err, db.ErrNotFound) {
			return errNotFound()
		} else if err != nil {
			return err
		}
		return c.audit(tx, p.OrgID, "token.delete", token, map[string]any{"project_id": p.ID})
	})
	if err != nil {
		return nil, err
	}
	s.notify.Notify()
	return nil, nil
}

// ---- secret keys ----

// GET /api/projects/{pid}/keys
func (s *Server) listKeys(c *reqCtx) (any, error) {
	p, err := c.project(db.RoleAdmin)
	if err != nil {
		return nil, err
	}
	return db.ListKeys(c.ctx(), s.db.R(), p.ID)
}

// keyWithSecret is the response of POST …/keys: the only time the secret
// is ever shown.
type keyWithSecret struct {
	db.Key
	Secret string `json:"secret"`
}

func cleanScopes(in []string) ([]string, error) {
	var query, admin bool
	for _, sc := range in {
		switch sc {
		case "query":
			query = true
		case "admin":
			admin = true
		default:
			return nil, errBadRequest(`"scopes": query ve/veya admin`)
		}
	}
	out := []string{}
	if query {
		out = append(out, "query")
	}
	if admin {
		out = append(out, "admin")
	}
	if len(out) == 0 {
		return nil, errBadRequest(`"scopes": query ve/veya admin`)
	}
	return out, nil
}

// POST /api/projects/{pid}/keys
func (s *Server) createKey(c *reqCtx) (any, error) {
	p, err := c.project(db.RoleAdmin)
	if err != nil {
		return nil, err
	}
	var in struct {
		Name   string   `json:"name"`
		Scopes []string `json:"scopes"`
	}
	if err := c.decode(&in); err != nil {
		return nil, err
	}
	name, err := cleanText("name", in.Name, 1, 100)
	if err != nil {
		return nil, err
	}
	scopes, err := cleanScopes(in.Scopes)
	if err != nil {
		return nil, err
	}
	secret := auth.NewSecretKey()
	k := db.Key{ProjectID: p.ID, Name: name, Scopes: scopes, Hash: auth.HashSecret(secret), Prefix: auth.SecretPrefixOf(secret), CreatedAt: c.nowMs()}
	err = s.db.Write(c.ctx(), func(tx *sql.Tx) error {
		existing, err := db.ListKeys(c.ctx(), tx, p.ID)
		if err != nil {
			return err
		}
		if len(existing) >= maxKeysPerProject {
			return errCode(409, "limit_exceeded", "Bir projede en çok "+strconv.Itoa(maxKeysPerProject)+" gizli anahtar olabilir")
		}
		if k.ID, err = db.CreateKey(c.ctx(), tx, k); err != nil {
			return err
		}
		// The audit entry names the key, never its secret or hash.
		return c.audit(tx, p.OrgID, "key.create", strconv.FormatInt(k.ID, 10), map[string]any{"project_id": p.ID, "name": name, "scopes": scopes})
	})
	if err != nil {
		return nil, err
	}
	s.notify.Notify()
	return keyWithSecret{Key: k, Secret: secret}, nil
}

// DELETE /api/projects/{pid}/keys/{id}
func (s *Server) deleteKey(c *reqCtx) (any, error) {
	p, err := c.project(db.RoleAdmin)
	if err != nil {
		return nil, err
	}
	id, ok := parseID(c.params["id"])
	if !ok {
		return nil, errNotFound()
	}
	err = s.db.Write(c.ctx(), func(tx *sql.Tx) error {
		k, err := db.DeleteKey(c.ctx(), tx, p.ID, id)
		if errors.Is(err, db.ErrNotFound) {
			return errNotFound()
		}
		if err != nil {
			return err
		}
		return c.audit(tx, p.OrgID, "key.delete", strconv.FormatInt(id, 10), map[string]any{"project_id": p.ID, "name": k.Name})
	})
	if err != nil {
		return nil, err
	}
	s.notify.Notify()
	return nil, nil
}
