package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/mail"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mucan54/izlek/control/auth"
	"github.com/mucan54/izlek/control/db"
)

// BootstrapInput describes the first user, organisation and project of an
// installation.
type BootstrapInput struct {
	Email    string
	Password string
	Org      string
	Project  string
	// RotateKey forces a new secret key even if the project already has a
	// bootstrap key (whose secret can no longer be shown).
	RotateKey bool
}

// BootstrapResult is what Bootstrap created or found.
type BootstrapResult struct {
	UserID    int64  `json:"user_id"`
	OrgID     int64  `json:"org_id"`
	ProjectID uint32 `json:"project_id"`
	// Token is the project's write token (not a secret; printed every run).
	Token string `json:"token"`
	// Secret is the sk_ key (scopes query + admin). It is only set by the
	// run that created it: the database keeps nothing but its hash.
	Secret string `json:"secret,omitempty"`
	// Created lists what this run created: "user", "org", "membership",
	// "project", "token", "key". Empty on a repeated run.
	Created []string `json:"created"`
}

const (
	bootstrapKeyName   = "bootstrap"
	bootstrapTokenName = "default"
)

// Bootstrap creates (or finds) a user, an organisation the user owns, a
// project in it, a write token and a secret key. It is idempotent: running
// it again with the same arguments changes nothing and creates no
// duplicates. An existing user's password is left alone.
func Bootstrap(ctx context.Context, d *db.DB, hasher *auth.Hasher, in BootstrapInput, now time.Time) (BootstrapResult, error) {
	res := BootstrapResult{Created: []string{}}
	email := strings.ToLower(strings.TrimSpace(in.Email))
	if addr, err := mail.ParseAddress(email); err != nil || addr.Address != email || len(email) > 254 {
		return res, fmt.Errorf("bootstrap: %q is not an e-mail address", in.Email)
	}
	org, project := strings.TrimSpace(in.Org), strings.TrimSpace(in.Project)
	for name, v := range map[string]string{"--org": org, "--project": project} {
		if v == "" || utf8.RuneCountInString(v) > 100 {
			return res, fmt.Errorf("bootstrap: %s must be 1–100 characters", name)
		}
	}
	// Hash before taking the write lock; only used if the user is new.
	var hash string
	if _, err := db.UserByEmail(ctx, d.R(), email); errors.Is(err, db.ErrNotFound) {
		if err := auth.CheckPassword(in.Password); err != nil {
			return res, fmt.Errorf("bootstrap: --password must be at least %d characters", auth.MinPasswordChars)
		}
		if hash, err = hasher.Hash(ctx, in.Password); err != nil {
			return res, err
		}
	} else if err != nil {
		return res, err
	}

	ms := now.UnixMilli()
	actor := db.SystemActor()
	err := d.Write(ctx, func(tx *sql.Tx) error {
		res = BootstrapResult{Created: []string{}}
		user, err := db.UserByEmail(ctx, tx, email)
		if errors.Is(err, db.ErrNotFound) {
			if hash == "" {
				return errors.New("bootstrap: the user disappeared while bootstrapping; run again")
			}
			id, err := db.CreateUser(ctx, tx, email, hash, ms)
			if err != nil {
				return err
			}
			user = db.User{ID: id, Email: email}
			res.Created = append(res.Created, "user")
		} else if err != nil {
			return err
		}
		res.UserID = user.ID

		// The organisation: one of that name the user already belongs to,
		// else a new one.
		orgs, err := db.UserOrgs(ctx, tx, user.ID)
		if err != nil {
			return err
		}
		for _, o := range orgs {
			if o.Name == org {
				res.OrgID = o.ID
				if o.Role != db.RoleOwner {
					return fmt.Errorf("bootstrap: %s is a member of %q but not its owner", email, org)
				}
				break
			}
		}
		if res.OrgID == 0 {
			if res.OrgID, err = db.CreateOrg(ctx, tx, org, ms); err != nil {
				return err
			}
			if err := db.AddMember(ctx, tx, res.OrgID, user.ID, db.RoleOwner, ms); err != nil {
				return err
			}
			res.Created = append(res.Created, "org", "membership")
			if err := db.InsertAudit(ctx, tx, ms, res.OrgID, actor, "bootstrap.org", strconv.FormatInt(res.OrgID, 10),
				map[string]any{"name": org, "owner": email}); err != nil {
				return err
			}
		}

		projects, err := db.ListOrgProjects(ctx, tx, res.OrgID)
		if err != nil {
			return err
		}
		for _, p := range projects {
			if p.Name == project {
				res.ProjectID = p.ID
				break
			}
		}
		if res.ProjectID == 0 {
			p, err := db.CreateProject(ctx, tx, db.Project{OrgID: res.OrgID, Name: project, RetentionDays: 365, ReplayRetentionDays: 30,
				DropKeys: []string{}, MaxConcurrentQueries: 4, ReplaySampleRate: 0.1}, ms)
			if err != nil {
				return err
			}
			res.ProjectID = p.ID
			res.Created = append(res.Created, "project")
			if err := db.InsertAudit(ctx, tx, ms, res.OrgID, actor, "project.create", strconv.FormatUint(uint64(p.ID), 10),
				map[string]any{"name": project}); err != nil {
				return err
			}
		}

		// The write token: the oldest active one, else a new one.
		tokens, err := db.ListTokens(ctx, tx, res.ProjectID)
		if err != nil {
			return err
		}
		for _, t := range tokens {
			if t.Status == "active" {
				res.Token = t.Token
				break
			}
		}
		if res.Token == "" {
			t := db.Token{Token: auth.NewWriteToken(), ProjectID: res.ProjectID, Name: bootstrapTokenName, Status: "active",
				AllowedOrigins: []string{}, Replay: true, CreatedAt: ms}
			if err := db.CreateToken(ctx, tx, t); err != nil {
				return err
			}
			res.Token = t.Token
			res.Created = append(res.Created, "token")
			if err := db.InsertAudit(ctx, tx, ms, res.OrgID, actor, "token.create", t.Token,
				map[string]any{"project_id": res.ProjectID, "name": t.Name}); err != nil {
				return err
			}
		}

		// The secret key: created once (or on demand); it cannot be shown again.
		keys, err := db.ListKeys(ctx, tx, res.ProjectID)
		if err != nil {
			return err
		}
		have := false
		for _, k := range keys {
			if k.Name == bootstrapKeyName {
				have = true
			}
		}
		if !have || in.RotateKey {
			secret := auth.NewSecretKey()
			id, err := db.CreateKey(ctx, tx, db.Key{ProjectID: res.ProjectID, Name: bootstrapKeyName, Scopes: []string{"query", "admin"},
				Hash: auth.HashSecret(secret), Prefix: auth.SecretPrefixOf(secret), CreatedAt: ms})
			if err != nil {
				return err
			}
			res.Secret = secret
			res.Created = append(res.Created, "key")
			if err := db.InsertAudit(ctx, tx, ms, res.OrgID, actor, "key.create", strconv.FormatInt(id, 10),
				map[string]any{"project_id": res.ProjectID, "name": bootstrapKeyName, "scopes": []string{"query", "admin"}}); err != nil {
				return err
			}
		}
		return nil
	})
	return res, err
}
