package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
)

// ---- projects ----

const projectCols = `p.id, p.org_id, p.name, p.retention_days, p.replay_retention_days, p.drop_keys, p.scan_limit_bytes,
	p.max_concurrent_queries, p.replay_sample_rate, p.created_at, COALESCE(p.closed_at, 0)`

func scanProject(row interface{ Scan(...any) error }) (Project, error) {
	var p Project
	var id int64
	var drop string
	err := row.Scan(&id, &p.OrgID, &p.Name, &p.RetentionDays, &p.ReplayRetentionDays, &drop, &p.ScanLimitBytes,
		&p.MaxConcurrentQueries, &p.ReplaySampleRate, &p.CreatedAt, &p.ClosedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrNotFound
	}
	if err != nil {
		return p, err
	}
	p.ID = uint32(id)
	p.DropKeys = stringsFromJSON(drop)
	return p, nil
}

// CreateProject inserts a project with the given settings and returns it
// with its new id. Ids come from an AUTOINCREMENT column: they start at 1,
// only grow and are never handed out twice.
func CreateProject(ctx context.Context, q Q, p Project, now int64) (Project, error) {
	if p.DropKeys == nil {
		p.DropKeys = []string{}
	}
	res, err := q.ExecContext(ctx, `INSERT INTO projects(org_id, name, retention_days, replay_retention_days, drop_keys,
		scan_limit_bytes, max_concurrent_queries, replay_sample_rate, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.OrgID, p.Name, p.RetentionDays, p.ReplayRetentionDays, toJSON(p.DropKeys), p.ScanLimitBytes,
		p.MaxConcurrentQueries, p.ReplaySampleRate, now)
	if err != nil {
		return p, fmt.Errorf("db: create project: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return p, err
	}
	if id < 1 || id > math.MaxUint32 {
		return p, ErrProjectIDsExhausted
	}
	p.ID = uint32(id)
	p.CreatedAt = now
	p.ClosedAt = 0
	return p, nil
}

// ProjectByID returns a project, closed or not.
func ProjectByID(ctx context.Context, q Q, id uint32) (Project, error) {
	return scanProject(q.QueryRowContext(ctx, `SELECT `+projectCols+` FROM projects p WHERE p.id = ?`, id))
}

func listProjects(ctx context.Context, q Q, query string, args ...any) ([]Project, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Project{}
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ListProjectsForUser lists the open projects of every organisation the
// user belongs to.
func ListProjectsForUser(ctx context.Context, q Q, userID int64) ([]Project, error) {
	return listProjects(ctx, q, `SELECT `+projectCols+` FROM projects p
		JOIN memberships m ON m.org_id = p.org_id AND m.user_id = ?
		JOIN orgs o ON o.id = p.org_id AND o.deleted_at IS NULL
		WHERE p.closed_at IS NULL ORDER BY p.id`, userID)
}

// ListOrgProjects lists the open projects of one organisation.
func ListOrgProjects(ctx context.Context, q Q, orgID int64) ([]Project, error) {
	return listProjects(ctx, q, `SELECT `+projectCols+` FROM projects p WHERE p.org_id = ? AND p.closed_at IS NULL ORDER BY p.id`, orgID)
}

// AllProjects lists every project, including closed ones.
func AllProjects(ctx context.Context, q Q) ([]Project, error) {
	return listProjects(ctx, q, `SELECT `+projectCols+` FROM projects p ORDER BY p.id`)
}

// UpdateProject writes the mutable settings of p.
func UpdateProject(ctx context.Context, q Q, p Project) error {
	if p.DropKeys == nil {
		p.DropKeys = []string{}
	}
	_, err := q.ExecContext(ctx, `UPDATE projects SET name = ?, retention_days = ?, replay_retention_days = ?, drop_keys = ?,
		scan_limit_bytes = ?, max_concurrent_queries = ?, replay_sample_rate = ? WHERE id = ?`,
		p.Name, p.RetentionDays, p.ReplayRetentionDays, toJSON(p.DropKeys), p.ScanLimitBytes,
		p.MaxConcurrentQueries, p.ReplaySampleRate, p.ID)
	return err
}

// CloseProject closes a project (spec/control.md §3): retention drops to 0
// so the compactor removes the data, and every token is disabled.
func CloseProject(ctx context.Context, q Q, id uint32, now int64) error {
	if _, err := q.ExecContext(ctx, `UPDATE projects SET closed_at = ?, retention_days = 0, replay_retention_days = 0
		WHERE id = ? AND closed_at IS NULL`, now, id); err != nil {
		return err
	}
	_, err := q.ExecContext(ctx, `UPDATE tokens SET status = 'disabled' WHERE project_id = ?`, id)
	return err
}

// ---- write tokens ----

const tokenCols = `t.token, t.project_id, t.name, t.status, t.allowed_origins, t.rate_limit, t.replay, t.created_at`

func scanToken(row interface{ Scan(...any) error }) (Token, error) {
	var t Token
	var pid int64
	var origins string
	err := row.Scan(&t.Token, &pid, &t.Name, &t.Status, &origins, &t.RateLimit, &t.Replay, &t.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return t, ErrNotFound
	}
	if err != nil {
		return t, err
	}
	t.ProjectID = uint32(pid)
	t.AllowedOrigins = stringsFromJSON(origins)
	return t, nil
}

// CreateToken inserts a write token.
func CreateToken(ctx context.Context, q Q, t Token) error {
	if t.AllowedOrigins == nil {
		t.AllowedOrigins = []string{}
	}
	_, err := q.ExecContext(ctx, `INSERT INTO tokens(token, project_id, name, status, allowed_origins, rate_limit, replay, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, t.Token, t.ProjectID, t.Name, t.Status, toJSON(t.AllowedOrigins), t.RateLimit, boolInt(t.Replay), t.CreatedAt)
	if isUniqueViolation(err) {
		return ErrConflict
	}
	return err
}

func listTokens(ctx context.Context, q Q, query string, args ...any) ([]Token, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Token{}
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ListTokens lists a project's tokens, oldest first.
func ListTokens(ctx context.Context, q Q, projectID uint32) ([]Token, error) {
	return listTokens(ctx, q, `SELECT `+tokenCols+` FROM tokens t WHERE t.project_id = ? ORDER BY t.created_at, t.token`, projectID)
}

// AllTokens lists every token of every project.
func AllTokens(ctx context.Context, q Q) ([]Token, error) {
	return listTokens(ctx, q, `SELECT `+tokenCols+` FROM tokens t ORDER BY t.token`)
}

// TokenOf returns one token of a project.
func TokenOf(ctx context.Context, q Q, projectID uint32, token string) (Token, error) {
	return scanToken(q.QueryRowContext(ctx, `SELECT `+tokenCols+` FROM tokens t WHERE t.project_id = ? AND t.token = ?`, projectID, token))
}

// UpdateToken writes the mutable fields of t.
func UpdateToken(ctx context.Context, q Q, t Token) error {
	if t.AllowedOrigins == nil {
		t.AllowedOrigins = []string{}
	}
	_, err := q.ExecContext(ctx, `UPDATE tokens SET name = ?, status = ?, allowed_origins = ?, rate_limit = ?, replay = ? WHERE token = ?`,
		t.Name, t.Status, toJSON(t.AllowedOrigins), t.RateLimit, boolInt(t.Replay), t.Token)
	return err
}

// DeleteToken removes a token.
func DeleteToken(ctx context.Context, q Q, projectID uint32, token string) error {
	res, err := q.ExecContext(ctx, `DELETE FROM tokens WHERE project_id = ? AND token = ?`, projectID, token)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ---- secret keys ----

const keyCols = `k.id, k.project_id, k.name, k.scopes, k.hash, k.prefix, k.created_at, k.last_used_at`

func scanKey(row interface{ Scan(...any) error }) (Key, error) {
	var k Key
	var pid int64
	var scopes string
	var last sql.NullInt64
	err := row.Scan(&k.ID, &pid, &k.Name, &scopes, &k.Hash, &k.Prefix, &k.CreatedAt, &last)
	if errors.Is(err, sql.ErrNoRows) {
		return k, ErrNotFound
	}
	if err != nil {
		return k, err
	}
	k.ProjectID = uint32(pid)
	k.Scopes = stringsFromJSON(scopes)
	if last.Valid {
		k.LastUsedAt = &last.Int64
	}
	return k, nil
}

// CreateKey inserts a secret key (hash only) and returns its id.
func CreateKey(ctx context.Context, q Q, k Key) (int64, error) {
	res, err := q.ExecContext(ctx, `INSERT INTO keys(project_id, name, scopes, hash, prefix, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		k.ProjectID, k.Name, toJSON(k.Scopes), k.Hash, k.Prefix, k.CreatedAt)
	if isUniqueViolation(err) {
		return 0, ErrConflict
	}
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func listKeys(ctx context.Context, q Q, query string, args ...any) ([]Key, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Key{}
	for rows.Next() {
		k, err := scanKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// ListKeys lists a project's keys, oldest first.
func ListKeys(ctx context.Context, q Q, projectID uint32) ([]Key, error) {
	return listKeys(ctx, q, `SELECT `+keyCols+` FROM keys k WHERE k.project_id = ? ORDER BY k.id`, projectID)
}

// ActiveKeys lists the keys of every open project.
func ActiveKeys(ctx context.Context, q Q) ([]Key, error) {
	return listKeys(ctx, q, `SELECT `+keyCols+` FROM keys k JOIN projects p ON p.id = k.project_id
		WHERE p.closed_at IS NULL ORDER BY k.hash`)
}

// KeyByHash finds a key of an open project by the SHA-256 of its secret.
func KeyByHash(ctx context.Context, q Q, hash string) (Key, error) {
	return scanKey(q.QueryRowContext(ctx, `SELECT `+keyCols+` FROM keys k JOIN projects p ON p.id = k.project_id
		WHERE k.hash = ? AND p.closed_at IS NULL`, hash))
}

// TouchKey records that a key was used.
func TouchKey(ctx context.Context, q Q, id int64, now int64) error {
	_, err := q.ExecContext(ctx, `UPDATE keys SET last_used_at = ? WHERE id = ?`, now, id)
	return err
}

// DeleteKey removes a key and returns the removed row.
func DeleteKey(ctx context.Context, q Q, projectID uint32, id int64) (Key, error) {
	k, err := scanKey(q.QueryRowContext(ctx, `SELECT `+keyCols+` FROM keys k WHERE k.project_id = ? AND k.id = ?`, projectID, id))
	if err != nil {
		return k, err
	}
	_, err = q.ExecContext(ctx, `DELETE FROM keys WHERE id = ?`, id)
	return k, err
}
