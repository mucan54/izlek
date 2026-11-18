package api

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"

	"github.com/mucan54/izlek/control/auth"
	"github.com/mucan54/izlek/control/db"
)

func errLastOwner(message string) *apiError {
	return errCode(http.StatusConflict, "last_owner", message)
}

func checkRole(role string) error {
	if !db.ValidRole(role) {
		return errBadRequest(`"role": owner | admin | member | viewer`)
	}
	return nil
}

// GET /api/orgs/{org}/members
func (s *Server) listMembers(c *reqCtx) (any, error) {
	orgID, err := c.org(db.RoleViewer)
	if err != nil {
		return nil, err
	}
	return db.ListMembers(c.ctx(), s.db.R(), orgID)
}

// POST /api/orgs/{org}/members — creates the user if the address is new
// (the first password is set by the owner; there is no e-mail in v1).
func (s *Server) addMember(c *reqCtx) (any, error) {
	orgID, err := c.org(db.RoleOwner)
	if err != nil {
		return nil, err
	}
	var in struct {
		Email    string `json:"email"`
		Role     string `json:"role"`
		Password string `json:"password"`
	}
	if err := c.decode(&in); err != nil {
		return nil, err
	}
	email, err := normalizeEmail(in.Email)
	if err != nil {
		return nil, err
	}
	if err := checkRole(in.Role); err != nil {
		return nil, err
	}
	// Hash outside the transaction (it takes ~100 ms and 64 MiB); whether it
	// is needed is re-checked under the write lock.
	var hash string
	if _, err := db.UserByEmail(c.ctx(), s.db.R(), email); errors.Is(err, db.ErrNotFound) {
		if auth.CheckPassword(in.Password) != nil {
			return nil, errWeakPassword()
		}
		if hash, err = s.hasher.Hash(c.ctx(), in.Password); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	var member db.Member
	err = s.db.Write(c.ctx(), func(tx *sql.Tx) error {
		created := false
		user, err := db.UserByEmail(c.ctx(), tx, email)
		if errors.Is(err, db.ErrNotFound) {
			if hash == "" { // deleted between the check and the lock; cannot happen in v1 (users are never deleted)
				return errConflict("Kullanıcı durumu değişti; yeniden deneyin")
			}
			id, err := db.CreateUser(c.ctx(), tx, email, hash, c.nowMs())
			if err != nil {
				return err
			}
			user, created = db.User{ID: id, Email: email}, true
		} else if err != nil {
			return err
		}
		if err := db.AddMember(c.ctx(), tx, orgID, user.ID, in.Role, c.nowMs()); errors.Is(err, db.ErrConflict) {
			return errConflict("Bu kullanıcı zaten üye")
		} else if err != nil {
			return err
		}
		member = db.Member{UserID: user.ID, Email: email, Role: in.Role, CreatedAt: c.nowMs()}
		return c.audit(tx, orgID, "member.add", strconv.FormatInt(user.ID, 10),
			map[string]any{"email": email, "role": in.Role, "user_created": created})
	})
	if err != nil {
		return nil, err
	}
	return member, nil
}

func (c *reqCtx) memberID() (int64, error) {
	id, ok := parseID(c.params["user"])
	if !ok {
		return 0, errNotFound()
	}
	return id, nil
}

// PATCH /api/orgs/{org}/members/{user_id}
func (s *Server) updateMember(c *reqCtx) (any, error) {
	orgID, err := c.org(db.RoleOwner)
	if err != nil {
		return nil, err
	}
	userID, err := c.memberID()
	if err != nil {
		return nil, err
	}
	var in struct {
		Role string `json:"role"`
	}
	if err := c.decode(&in); err != nil {
		return nil, err
	}
	if err := checkRole(in.Role); err != nil {
		return nil, err
	}
	var member db.Member
	err = s.db.Write(c.ctx(), func(tx *sql.Tx) error {
		before, err := db.MemberOf(c.ctx(), tx, orgID, userID)
		if errors.Is(err, db.ErrNotFound) {
			return errNotFound()
		}
		if err != nil {
			return err
		}
		if err := db.SetMemberRole(c.ctx(), tx, orgID, userID, in.Role); errors.Is(err, db.ErrLastOwner) {
			return errLastOwner("Son sahibin rolü düşürülemez")
		} else if err != nil {
			return err
		}
		member = before
		member.Role = in.Role
		return c.audit(tx, orgID, "member.update", strconv.FormatInt(userID, 10), map[string]any{"role": in.Role, "previous_role": before.Role})
	})
	if err != nil {
		return nil, err
	}
	return member, nil
}

// DELETE /api/orgs/{org}/members/{user_id}
func (s *Server) removeMember(c *reqCtx) (any, error) {
	orgID, err := c.org(db.RoleOwner)
	if err != nil {
		return nil, err
	}
	userID, err := c.memberID()
	if err != nil {
		return nil, err
	}
	err = s.db.Write(c.ctx(), func(tx *sql.Tx) error {
		before, err := db.MemberOf(c.ctx(), tx, orgID, userID)
		if errors.Is(err, db.ErrNotFound) {
			return errNotFound()
		}
		if err != nil {
			return err
		}
		if err := db.RemoveMember(c.ctx(), tx, orgID, userID); errors.Is(err, db.ErrLastOwner) {
			return errLastOwner("Son sahip çıkarılamaz")
		} else if err != nil {
			return err
		}
		return c.audit(tx, orgID, "member.remove", strconv.FormatInt(userID, 10), map[string]any{"email": before.Email, "role": before.Role})
	})
	return nil, err
}

// GET /api/orgs/{org}/audit?limit=&before=
func (s *Server) listAudit(c *reqCtx) (any, error) {
	orgID, err := c.org(db.RoleAdmin)
	if err != nil {
		return nil, err
	}
	q := c.r.URL.Query()
	limit := 50
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return nil, errBadRequest(`"limit" pozitif bir tam sayı olmalıdır`)
		}
		limit = min(n, 500)
	}
	var before int64
	if v := q.Get("before"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 1 {
			return nil, errBadRequest(`"before" bir kayıt kimliği olmalıdır`)
		}
		before = n
	}
	return db.ListAudit(c.ctx(), s.db.R(), orgID, limit, before)
}

// DELETE /api/orgs/{org} — the role table of spec/control.md §1 lists
// "org silme" (owner). Projects are closed, memberships removed.
func (s *Server) deleteOrg(c *reqCtx) (any, error) {
	orgID, err := c.org(db.RoleOwner)
	if err != nil {
		return nil, err
	}
	err = s.db.Write(c.ctx(), func(tx *sql.Tx) error {
		name, err := db.OrgName(c.ctx(), tx, orgID)
		if errors.Is(err, db.ErrNotFound) {
			return errNotFound()
		}
		if err != nil {
			return err
		}
		// Written before the memberships disappear, attributed to the org.
		closed, err := db.DeleteOrg(c.ctx(), tx, orgID, c.nowMs())
		if err != nil {
			return err
		}
		if closed == nil {
			closed = []uint32{}
		}
		return c.audit(tx, orgID, "org.delete", strconv.FormatInt(orgID, 10), map[string]any{"name": name, "closed_projects": closed})
	})
	if err != nil {
		return nil, err
	}
	s.notify.Notify()
	return nil, nil
}
