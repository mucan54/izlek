package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"github.com/mucan54/izlek/control/auth"
	"github.com/mucan54/izlek/control/db"
	"github.com/mucan54/izlek/control/publish"
	"github.com/mucan54/izlek/golib/flags"
)

const (
	maxFlagsPerProject = 500
	maxRulesBytes      = 32 << 10 // canonical JSON of one flag's rules
	saltChars          = 8
)

func errInvalidFlag(format string, args ...any) *apiError {
	e := errBadRequest(format, args...)
	e.Code = "invalid_flag"
	return e
}

// flagInput is the writable part of a flag definition. Unknown top-level
// fields (salt, version, updated_at, … of an echoed-back Flag) are ignored;
// inside rules nothing unknown is tolerated.
type flagInput struct {
	Key         *string         `json:"key"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Enabled     bool            `json:"enabled"`
	Rules       json.RawMessage `json:"rules"`
	Default     json.RawMessage `json:"default"`
}

// decodeFlagDefinition parses and validates a flag definition body (the
// JSON object of POST/PUT) and returns the canonical name, description,
// enabled flag, rules and default. It is the single entry point for
// user-supplied flag JSON and is what the fuzz target exercises.
func decodeFlagDefinition(raw []byte) (flagInput, db.Flag, error) {
	var in flagInput
	var f db.Flag
	if err := decodeJSON(raw, &in); err != nil {
		return in, f, err
	}
	var err error
	if f.Name, err = cleanText("name", in.Name, 0, 200); err != nil {
		return in, f, err
	}
	if f.Description, err = cleanText("description", in.Description, 0, 2000); err != nil {
		return in, f, err
	}
	f.Enabled = in.Enabled
	rules, err := flags.ParseRules(in.Rules)
	if err != nil {
		return in, f, errInvalidFlag("Geçersiz flag tanımı: %s", err.Error())
	}
	def, err := flags.ParseDefault(in.Default)
	if err != nil {
		return in, f, errInvalidFlag("Geçersiz flag tanımı: %s", err.Error())
	}
	// Stored and published in canonical form: what was validated is exactly
	// what the SDKs will download.
	if f.Rules, err = json.Marshal(rules); err != nil {
		return in, f, errInvalidFlag("Geçersiz flag tanımı: rules kodlanamadı")
	}
	if len(f.Rules) > maxRulesBytes {
		return in, f, errInvalidFlag("Geçersiz flag tanımı: rules en çok %d bayt olabilir", maxRulesBytes)
	}
	if f.Default, err = json.Marshal(def); err != nil {
		return in, f, errInvalidFlag("Geçersiz flag tanımı: default kodlanamadı")
	}
	return in, f, nil
}

// GET /api/projects/{pid}/flags
func (s *Server) listFlags(c *reqCtx) (any, error) {
	p, err := c.project(db.RoleViewer)
	if err != nil {
		return nil, err
	}
	return db.ListFlags(c.ctx(), s.db.R(), p.ID)
}

// POST /api/projects/{pid}/flags
func (s *Server) createFlag(c *reqCtx) (any, error) {
	p, err := c.project(db.RoleAdmin)
	if err != nil {
		return nil, err
	}
	raw, err := c.rawBody()
	if err != nil {
		return nil, err
	}
	in, f, err := decodeFlagDefinition(raw)
	if err != nil {
		return nil, err
	}
	if in.Key == nil || !flags.ValidKey(*in.Key) {
		return nil, errInvalidFlag(`"key" küçük harf, rakam, "_" ve "-" içerebilir; harf ya da rakamla başlamalıdır (en çok 64 karakter)`)
	}
	f.Key = *in.Key
	// The salt is generated once and never changes: it fixes which bucket
	// every subject falls into for the lifetime of the flag.
	f.Salt = strings.ToLower(auth.RandomBase62(saltChars))
	f.UpdatedAt, f.UpdatedBy = c.nowMs(), c.actor()
	err = s.db.Write(c.ctx(), func(tx *sql.Tx) error {
		existing, err := db.ListFlags(c.ctx(), tx, p.ID)
		if err != nil {
			return err
		}
		if len(existing) >= maxFlagsPerProject {
			return errCode(409, "limit_exceeded", "Bir projede en çok 500 flag olabilir")
		}
		if f, err = db.CreateFlag(c.ctx(), tx, p.ID, f); errors.Is(err, db.ErrConflict) {
			return errConflict(`"` + f.Key + `" anahtarlı bir flag zaten var`)
		} else if err != nil {
			return err
		}
		return c.audit(tx, p.OrgID, "flag.create", f.Key, map[string]any{"project_id": p.ID, "version": f.Version, "enabled": f.Enabled})
	})
	if err != nil {
		return nil, err
	}
	s.notify.Notify()
	return f, nil
}

func (c *reqCtx) flag(minRole string) (db.Project, db.Flag, error) {
	p, err := c.project(minRole)
	if err != nil {
		return p, db.Flag{}, err
	}
	f, err := db.FlagByKey(c.ctx(), c.s.db.R(), p.ID, c.params["key"])
	if errors.Is(err, db.ErrNotFound) {
		return p, f, errNotFound()
	}
	return p, f, err
}

// GET /api/projects/{pid}/flags/{key}
func (s *Server) getFlag(c *reqCtx) (any, error) {
	_, f, err := c.flag(db.RoleViewer)
	if err != nil {
		return nil, err
	}
	return f, nil
}

// PUT /api/projects/{pid}/flags/{key} — replaces the whole definition
// (absent fields take their defaults); key and salt cannot change.
func (s *Server) putFlag(c *reqCtx) (any, error) {
	p, err := c.project(db.RoleAdmin)
	if err != nil {
		return nil, err
	}
	raw, err := c.rawBody()
	if err != nil {
		return nil, err
	}
	in, f, err := decodeFlagDefinition(raw)
	if err != nil {
		return nil, err
	}
	key := c.params["key"]
	if in.Key != nil && *in.Key != key {
		return nil, errInvalidFlag(`"key" değiştirilemez`)
	}
	f.Key = key
	f.UpdatedAt, f.UpdatedBy = c.nowMs(), c.actor()
	err = s.db.Write(c.ctx(), func(tx *sql.Tx) error {
		var err error
		if f, err = db.UpdateFlag(c.ctx(), tx, p.ID, f); errors.Is(err, db.ErrNotFound) {
			return errNotFound()
		} else if err != nil {
			return err
		}
		return c.audit(tx, p.OrgID, "flag.update", f.Key, map[string]any{"project_id": p.ID, "version": f.Version, "enabled": f.Enabled})
	})
	if err != nil {
		return nil, err
	}
	s.notify.Notify()
	return f, nil
}

// DELETE /api/projects/{pid}/flags/{key}
func (s *Server) deleteFlag(c *reqCtx) (any, error) {
	p, err := c.project(db.RoleAdmin)
	if err != nil {
		return nil, err
	}
	key := c.params["key"]
	err = s.db.Write(c.ctx(), func(tx *sql.Tx) error {
		if err := db.DeleteFlag(c.ctx(), tx, p.ID, key); errors.Is(err, db.ErrNotFound) {
			return errNotFound()
		} else if err != nil {
			return err
		}
		return c.audit(tx, p.OrgID, "flag.delete", key, map[string]any{"project_id": p.ID})
	})
	if err != nil {
		return nil, err
	}
	s.notify.Notify()
	return nil, nil
}

// GET /api/projects/{pid}/flags/{key}/versions
func (s *Server) listFlagVersions(c *reqCtx) (any, error) {
	p, f, err := c.flag(db.RoleViewer)
	if err != nil {
		return nil, err
	}
	return db.ListFlagVersions(c.ctx(), s.db.R(), p.ID, f.Key)
}

// EvaluateFlag evaluates a stored flag exactly the way an SDK would: the
// flag goes through the same encoding that is published as
// flags/<token>.json and is then read back by the shared evaluator.
func EvaluateFlag(f db.Flag, props map[string]any, userID, anonID string) (any, error) {
	raw, err := json.Marshal(publish.Ruleset([]db.Flag{f}))
	if err != nil {
		return nil, err
	}
	rs, err := flags.ParseRuleset(raw)
	if err != nil {
		return nil, err
	}
	if len(rs.Flags) != 1 {
		return nil, errors.New("api: ruleset round trip lost the flag")
	}
	return rs.Flags[0].Eval(props, userID, anonID), nil
}

// POST /api/projects/{pid}/flags/{key}/evaluate — server-side trial for the
// UI preview. Read-only, so viewers may use it.
func (s *Server) evaluateFlag(c *reqCtx) (any, error) {
	_, f, err := c.flag(db.RoleViewer)
	if err != nil {
		return nil, err
	}
	var in struct {
		Props  map[string]any `json:"props"`
		UserID flexString     `json:"user_id"`
		AnonID flexString     `json:"anon_id"`
	}
	if err := c.decode(&in); err != nil {
		return nil, err
	}
	value, err := EvaluateFlag(f, in.Props, string(in.UserID), string(in.AnonID))
	if err != nil {
		return nil, err
	}
	return map[string]any{"value": value}, nil
}
