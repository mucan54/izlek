package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ---- users ----

// CountUsers returns the number of users (0 = first-run setup is open).
func CountUsers(ctx context.Context, q Q) (int, error) {
	var n int
	err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

// CreateUser inserts a user. ErrConflict if the e-mail is taken.
func CreateUser(ctx context.Context, q Q, email, passwordHash string, now int64) (int64, error) {
	res, err := q.ExecContext(ctx, `INSERT INTO users(email, password_hash, created_at) VALUES (?, ?, ?)`, email, passwordHash, now)
	if isUniqueViolation(err) {
		return 0, ErrConflict
	}
	if err != nil {
		return 0, fmt.Errorf("db: create user: %w", err)
	}
	return res.LastInsertId()
}

const userCols = `u.id, u.email, u.password_hash, COALESCE(u.totp_secret, ''), COALESCE(u.totp_pending, ''), u.totp_enabled, u.totp_last_step, u.created_at`

func scanUser(row interface{ Scan(...any) error }, extra ...any) (User, error) {
	var u User
	dest := append([]any{&u.ID, &u.Email, &u.PasswordHash, &u.TOTPSecret, &u.TOTPPending, &u.TOTPEnabled, &u.TOTPLastStep, &u.CreatedAt}, extra...)
	err := row.Scan(dest...)
	if errors.Is(err, sql.ErrNoRows) {
		return u, ErrNotFound
	}
	return u, err
}

// UserByEmail looks a user up by (already normalised) e-mail.
func UserByEmail(ctx context.Context, q Q, email string) (User, error) {
	return scanUser(q.QueryRowContext(ctx, `SELECT `+userCols+` FROM users u WHERE u.email = ?`, email))
}

// UserByID looks a user up by id.
func UserByID(ctx context.Context, q Q, id int64) (User, error) {
	return scanUser(q.QueryRowContext(ctx, `SELECT `+userCols+` FROM users u WHERE u.id = ?`, id))
}

// SetPassword replaces a user's password hash.
func SetPassword(ctx context.Context, q Q, userID int64, hash string) error {
	_, err := q.ExecContext(ctx, `UPDATE users SET password_hash = ? WHERE id = ?`, hash, userID)
	return err
}

// SetTOTPPending stores the secret of an enrolment that is not yet confirmed.
func SetTOTPPending(ctx context.Context, q Q, userID int64, secret string) error {
	_, err := q.ExecContext(ctx, `UPDATE users SET totp_pending = ? WHERE id = ?`, secret, userID)
	return err
}

// EnableTOTP promotes the pending secret; step is the time step of the code
// that confirmed it (it cannot be used again).
func EnableTOTP(ctx context.Context, q Q, userID int64, step int64) error {
	_, err := q.ExecContext(ctx, `UPDATE users SET totp_secret = totp_pending, totp_pending = NULL, totp_enabled = 1, totp_last_step = ?
		WHERE id = ? AND totp_pending IS NOT NULL`, step, userID)
	return err
}

// DisableTOTP removes every TOTP secret of the user.
func DisableTOTP(ctx context.Context, q Q, userID int64) error {
	_, err := q.ExecContext(ctx, `UPDATE users SET totp_secret = NULL, totp_pending = NULL, totp_enabled = 0, totp_last_step = 0 WHERE id = ?`, userID)
	return err
}

// AdvanceTOTPStep records a used time step. It reports false when that step
// (or a later one) was already used, i.e. the code is a replay.
func AdvanceTOTPStep(ctx context.Context, q Q, userID int64, step int64) (bool, error) {
	res, err := q.ExecContext(ctx, `UPDATE users SET totp_last_step = ? WHERE id = ? AND totp_last_step < ?`, step, userID, step)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// ---- sessions ----

// CreateSession stores a session by the SHA-256 of its cookie value.
func CreateSession(ctx context.Context, q Q, idHash string, userID int64, csrf string, now, expires int64) error {
	_, err := q.ExecContext(ctx, `INSERT INTO sessions(id_hash, user_id, csrf, created_at, expires_at) VALUES (?, ?, ?, ?, ?)`,
		idHash, userID, csrf, now, expires)
	return err
}

// SessionByHash returns a session that has not expired at now.
func SessionByHash(ctx context.Context, q Q, idHash string, now int64) (Session, error) {
	var s Session
	s.IDHash = idHash
	u, err := scanUser(q.QueryRowContext(ctx, `SELECT `+userCols+`, s.csrf, s.created_at, s.expires_at
		FROM sessions s JOIN users u ON u.id = s.user_id WHERE s.id_hash = ? AND s.expires_at > ?`, idHash, now),
		&s.CSRF, &s.CreatedAt, &s.ExpiresAt)
	s.User = u
	return s, err
}

// ExtendSession slides the expiry of a session.
func ExtendSession(ctx context.Context, q Q, idHash string, expires int64) error {
	_, err := q.ExecContext(ctx, `UPDATE sessions SET expires_at = ? WHERE id_hash = ?`, expires, idHash)
	return err
}

// DeleteSession removes one session.
func DeleteSession(ctx context.Context, q Q, idHash string) error {
	_, err := q.ExecContext(ctx, `DELETE FROM sessions WHERE id_hash = ?`, idHash)
	return err
}

// DeleteOtherSessions removes every session of the user except keepHash.
func DeleteOtherSessions(ctx context.Context, q Q, userID int64, keepHash string) error {
	_, err := q.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ? AND id_hash <> ?`, userID, keepHash)
	return err
}

// DeleteExpiredSessions removes sessions past their expiry.
func DeleteExpiredSessions(ctx context.Context, q Q, now int64) (int64, error) {
	res, err := q.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= ?`, now)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// CountSessions returns the number of live sessions.
func CountSessions(ctx context.Context, q Q, now int64) (int64, error) {
	var n int64
	err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions WHERE expires_at > ?`, now).Scan(&n)
	return n, err
}

// ---- organisations and memberships ----

// CreateOrg inserts an organisation.
func CreateOrg(ctx context.Context, q Q, name string, now int64) (int64, error) {
	res, err := q.ExecContext(ctx, `INSERT INTO orgs(name, created_at) VALUES (?, ?)`, name, now)
	if err != nil {
		return 0, fmt.Errorf("db: create org: %w", err)
	}
	return res.LastInsertId()
}

// OrgName returns the name of a live organisation.
func OrgName(ctx context.Context, q Q, orgID int64) (string, error) {
	var name string
	err := q.QueryRowContext(ctx, `SELECT name FROM orgs WHERE id = ? AND deleted_at IS NULL`, orgID).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return name, err
}

// UserOrgs lists the organisations a user belongs to.
func UserOrgs(ctx context.Context, q Q, userID int64) ([]OrgRole, error) {
	rows, err := q.QueryContext(ctx, `SELECT o.id, o.name, m.role FROM memberships m JOIN orgs o ON o.id = m.org_id
		WHERE m.user_id = ? AND o.deleted_at IS NULL ORDER BY o.id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []OrgRole{}
	for rows.Next() {
		var o OrgRole
		if err := rows.Scan(&o.ID, &o.Name, &o.Role); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// RoleIn returns the user's role in a live organisation; ErrNotFound when
// the user is not a member (or the organisation does not exist).
func RoleIn(ctx context.Context, q Q, orgID, userID int64) (string, error) {
	var role string
	err := q.QueryRowContext(ctx, `SELECT m.role FROM memberships m JOIN orgs o ON o.id = m.org_id
		WHERE m.org_id = ? AND m.user_id = ? AND o.deleted_at IS NULL`, orgID, userID).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return role, err
}

// ListMembers lists the members of an organisation, oldest first.
func ListMembers(ctx context.Context, q Q, orgID int64) ([]Member, error) {
	rows, err := q.QueryContext(ctx, `SELECT m.user_id, u.email, m.role, m.created_at FROM memberships m JOIN users u ON u.id = m.user_id
		WHERE m.org_id = ? ORDER BY m.created_at, m.user_id`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Member{}
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.UserID, &m.Email, &m.Role, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// MemberOf returns one membership.
func MemberOf(ctx context.Context, q Q, orgID, userID int64) (Member, error) {
	var m Member
	err := q.QueryRowContext(ctx, `SELECT m.user_id, u.email, m.role, m.created_at FROM memberships m JOIN users u ON u.id = m.user_id
		WHERE m.org_id = ? AND m.user_id = ?`, orgID, userID).Scan(&m.UserID, &m.Email, &m.Role, &m.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return m, ErrNotFound
	}
	return m, err
}

// AddMember inserts a membership. ErrConflict if it exists.
func AddMember(ctx context.Context, q Q, orgID, userID int64, role string, now int64) error {
	_, err := q.ExecContext(ctx, `INSERT INTO memberships(org_id, user_id, role, created_at) VALUES (?, ?, ?, ?)`, orgID, userID, role, now)
	if isUniqueViolation(err) {
		return ErrConflict
	}
	return err
}

func otherOwners(ctx context.Context, q Q, orgID, userID int64) (int, error) {
	var n int
	err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM memberships WHERE org_id = ? AND role = 'owner' AND user_id <> ?`, orgID, userID).Scan(&n)
	return n, err
}

// SetMemberRole changes a member's role. ErrLastOwner when it would demote
// the only owner. Must run inside Write so the check and the update are atomic.
func SetMemberRole(ctx context.Context, q Q, orgID, userID int64, role string) error {
	cur, err := MemberOf(ctx, q, orgID, userID)
	if err != nil {
		return err
	}
	if cur.Role == RoleOwner && role != RoleOwner {
		n, err := otherOwners(ctx, q, orgID, userID)
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrLastOwner
		}
	}
	_, err = q.ExecContext(ctx, `UPDATE memberships SET role = ? WHERE org_id = ? AND user_id = ?`, role, orgID, userID)
	return err
}

// RemoveMember deletes a membership. ErrLastOwner when it is the only owner.
func RemoveMember(ctx context.Context, q Q, orgID, userID int64) error {
	cur, err := MemberOf(ctx, q, orgID, userID)
	if err != nil {
		return err
	}
	if cur.Role == RoleOwner {
		n, err := otherOwners(ctx, q, orgID, userID)
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrLastOwner
		}
	}
	_, err = q.ExecContext(ctx, `DELETE FROM memberships WHERE org_id = ? AND user_id = ?`, orgID, userID)
	return err
}

// DeleteOrg soft-deletes an organisation: its projects are closed, its
// memberships removed. Project rows stay (ids are never reused and the data
// plane must learn the zero retention). Returns the ids of the projects that
// were closed by this call.
func DeleteOrg(ctx context.Context, q Q, orgID int64, now int64) ([]uint32, error) {
	rows, err := q.QueryContext(ctx, `SELECT id FROM projects WHERE org_id = ? AND closed_at IS NULL ORDER BY id`, orgID)
	if err != nil {
		return nil, err
	}
	var ids []uint32
	for rows.Next() {
		var id uint32
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, id := range ids {
		if err := CloseProject(ctx, q, id, now); err != nil {
			return nil, err
		}
	}
	if _, err := q.ExecContext(ctx, `DELETE FROM memberships WHERE org_id = ?`, orgID); err != nil {
		return nil, err
	}
	if _, err := q.ExecContext(ctx, `UPDATE orgs SET deleted_at = ? WHERE id = ?`, now, orgID); err != nil {
		return nil, err
	}
	return ids, nil
}

// ---- audit log ----

// InsertAudit appends an audit entry. orgID 0 means "not tied to an
// organisation". detail must be JSON-encodable and must not contain secrets.
func InsertAudit(ctx context.Context, q Q, now int64, orgID int64, actor Actor, action, target string, detail any) error {
	var org any
	if orgID != 0 {
		org = orgID
	}
	if detail == nil {
		detail = struct{}{}
	}
	_, err := q.ExecContext(ctx, `INSERT INTO audit_log(ts, org_id, actor, action, target, detail) VALUES (?, ?, ?, ?, ?, ?)`,
		now, org, actor.String(), action, target, toJSON(detail))
	if err != nil {
		return fmt.Errorf("db: audit: %w", err)
	}
	return nil
}

// ListAudit returns the audit entries visible to an organisation, newest
// first: entries of the organisation itself plus the user-level entries
// (login, password, TOTP) of its current members. before (an entry id, 0 =
// from the newest) pages backwards.
func ListAudit(ctx context.Context, q Q, orgID int64, limit int, before int64) ([]AuditEntry, error) {
	query := `SELECT a.id, a.ts, a.actor, a.action, a.target, a.detail FROM audit_log a
		WHERE (a.org_id = ?1 OR (a.org_id IS NULL AND a.actor IN
			(SELECT 'user:' || m.user_id FROM memberships m WHERE m.org_id = ?1)))`
	args := []any{orgID}
	if before > 0 {
		query += ` AND a.id < ?2`
		args = append(args, before)
	} else {
		args = append(args, nil)
	}
	query += ` ORDER BY a.id DESC LIMIT ?3`
	args = append(args, limit)
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AuditEntry{}
	for rows.Next() {
		var e AuditEntry
		var actor, detail string
		if err := rows.Scan(&e.ID, &e.TS, &actor, &e.Action, &e.Target, &detail); err != nil {
			return nil, err
		}
		e.Actor = ParseActor(actor)
		e.Detail = []byte(detail)
		out = append(out, e)
	}
	return out, rows.Err()
}
