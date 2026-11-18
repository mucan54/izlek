package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// ---- flags ----

const flagCols = `f.key, f.name, f.description, f.enabled, f.salt, f.rules, f.default_value, f.version, f.updated_at, f.updated_by`

func scanFlag(row interface{ Scan(...any) error }) (Flag, error) {
	var f Flag
	var rules, def, by string
	err := row.Scan(&f.Key, &f.Name, &f.Description, &f.Enabled, &f.Salt, &rules, &def, &f.Version, &f.UpdatedAt, &by)
	if errors.Is(err, sql.ErrNoRows) {
		return f, ErrNotFound
	}
	if err != nil {
		return f, err
	}
	f.Rules, f.Default, f.UpdatedBy = json.RawMessage(rules), json.RawMessage(def), ParseActor(by)
	return f, nil
}

func recordFlagVersion(ctx context.Context, q Q, projectID uint32, f Flag) error {
	_, err := q.ExecContext(ctx, `INSERT INTO flag_versions(project_id, key, version, ts, actor, flag) VALUES (?, ?, ?, ?, ?, ?)`,
		projectID, f.Key, f.Version, f.UpdatedAt, f.UpdatedBy.String(), toJSON(f))
	return err
}

// CreateFlag inserts a flag and its first history entry. The version
// continues after any earlier history of the same key. ErrConflict if the
// key exists. f.Salt, f.UpdatedAt and f.UpdatedBy must be set by the caller.
func CreateFlag(ctx context.Context, q Q, projectID uint32, f Flag) (Flag, error) {
	var exists int
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM flags WHERE project_id = ? AND key = ?`, projectID, f.Key).Scan(&exists); err != nil {
		return f, err
	}
	if exists > 0 {
		return f, ErrConflict
	}
	var last int64
	if err := q.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM flag_versions WHERE project_id = ? AND key = ?`, projectID, f.Key).Scan(&last); err != nil {
		return f, err
	}
	f.Version = last + 1
	_, err := q.ExecContext(ctx, `INSERT INTO flags(project_id, key, name, description, enabled, salt, rules, default_value, version, updated_at, updated_by)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, projectID, f.Key, f.Name, f.Description, boolInt(f.Enabled), f.Salt,
		string(f.Rules), string(f.Default), f.Version, f.UpdatedAt, f.UpdatedBy.String())
	if isUniqueViolation(err) {
		return f, ErrConflict
	}
	if err != nil {
		return f, fmt.Errorf("db: create flag: %w", err)
	}
	return f, recordFlagVersion(ctx, q, projectID, f)
}

// FlagByKey returns one flag.
func FlagByKey(ctx context.Context, q Q, projectID uint32, key string) (Flag, error) {
	return scanFlag(q.QueryRowContext(ctx, `SELECT `+flagCols+` FROM flags f WHERE f.project_id = ? AND f.key = ?`, projectID, key))
}

// ListFlags lists a project's flags ordered by key.
func ListFlags(ctx context.Context, q Q, projectID uint32) ([]Flag, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+flagCols+` FROM flags f WHERE f.project_id = ? ORDER BY f.key`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Flag{}
	for rows.Next() {
		f, err := scanFlag(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// UpdateFlag replaces the definition of a flag (everything but key and
// salt), bumps its version and appends a history entry. f.UpdatedAt and
// f.UpdatedBy must be set by the caller.
func UpdateFlag(ctx context.Context, q Q, projectID uint32, f Flag) (Flag, error) {
	cur, err := FlagByKey(ctx, q, projectID, f.Key)
	if err != nil {
		return f, err
	}
	f.Salt = cur.Salt
	f.Version = cur.Version + 1
	if _, err := q.ExecContext(ctx, `UPDATE flags SET name = ?, description = ?, enabled = ?, rules = ?, default_value = ?,
		version = ?, updated_at = ?, updated_by = ? WHERE project_id = ? AND key = ?`,
		f.Name, f.Description, boolInt(f.Enabled), string(f.Rules), string(f.Default), f.Version, f.UpdatedAt, f.UpdatedBy.String(),
		projectID, f.Key); err != nil {
		return f, fmt.Errorf("db: update flag: %w", err)
	}
	return f, recordFlagVersion(ctx, q, projectID, f)
}

// DeleteFlag removes a flag; its history is kept.
func DeleteFlag(ctx context.Context, q Q, projectID uint32, key string) error {
	res, err := q.ExecContext(ctx, `DELETE FROM flags WHERE project_id = ? AND key = ?`, projectID, key)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ListFlagVersions returns the history of a flag key, newest first.
func ListFlagVersions(ctx context.Context, q Q, projectID uint32, key string) ([]FlagVersion, error) {
	rows, err := q.QueryContext(ctx, `SELECT version, ts, actor, flag FROM flag_versions WHERE project_id = ? AND key = ? ORDER BY version DESC`, projectID, key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FlagVersion{}
	for rows.Next() {
		var v FlagVersion
		var actor, flag string
		if err := rows.Scan(&v.Version, &v.TS, &actor, &flag); err != nil {
			return nil, err
		}
		v.Actor, v.Flag = ParseActor(actor), json.RawMessage(flag)
		out = append(out, v)
	}
	return out, rows.Err()
}

// ---- insights ----

const insightCols = `i.id, i.name, i.kind, i.query, i.created_by, i.updated_at`

func scanInsight(row interface{ Scan(...any) error }) (Insight, error) {
	var i Insight
	var query, by string
	err := row.Scan(&i.ID, &i.Name, &i.Kind, &query, &by, &i.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return i, ErrNotFound
	}
	if err != nil {
		return i, err
	}
	i.Query, i.CreatedBy = json.RawMessage(query), ParseActor(by)
	return i, nil
}

// CreateInsight inserts a saved query.
func CreateInsight(ctx context.Context, q Q, projectID uint32, i Insight, now int64) (Insight, error) {
	res, err := q.ExecContext(ctx, `INSERT INTO insights(project_id, name, kind, query, created_by, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		projectID, i.Name, i.Kind, string(i.Query), i.CreatedBy.String(), now, now)
	if err != nil {
		return i, fmt.Errorf("db: create insight: %w", err)
	}
	i.ID, err = res.LastInsertId()
	i.UpdatedAt = now
	return i, err
}

// InsightByID returns one insight of a project.
func InsightByID(ctx context.Context, q Q, projectID uint32, id int64) (Insight, error) {
	return scanInsight(q.QueryRowContext(ctx, `SELECT `+insightCols+` FROM insights i WHERE i.project_id = ? AND i.id = ?`, projectID, id))
}

// ListInsights lists a project's insights.
func ListInsights(ctx context.Context, q Q, projectID uint32) ([]Insight, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+insightCols+` FROM insights i WHERE i.project_id = ? ORDER BY i.id`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Insight{}
	for rows.Next() {
		i, err := scanInsight(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// UpdateInsight replaces name, kind and query.
func UpdateInsight(ctx context.Context, q Q, projectID uint32, i Insight, now int64) (Insight, error) {
	res, err := q.ExecContext(ctx, `UPDATE insights SET name = ?, kind = ?, query = ?, updated_at = ? WHERE project_id = ? AND id = ?`,
		i.Name, i.Kind, string(i.Query), now, projectID, i.ID)
	if err != nil {
		return i, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return i, ErrNotFound
	}
	return InsightByID(ctx, q, projectID, i.ID)
}

// DeleteInsight removes an insight and every dashboard tile showing it.
func DeleteInsight(ctx context.Context, q Q, projectID uint32, id int64, now int64) error {
	res, err := q.ExecContext(ctx, `DELETE FROM insights WHERE project_id = ? AND id = ?`, projectID, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	dashboards, err := ListDashboards(ctx, q, projectID)
	if err != nil {
		return err
	}
	for _, d := range dashboards {
		kept := make([]DashboardItem, 0, len(d.Items))
		for _, it := range d.Items {
			if it.InsightID != id {
				kept = append(kept, it)
			}
		}
		if len(kept) == len(d.Items) {
			continue
		}
		if _, err := q.ExecContext(ctx, `UPDATE dashboards SET items = ?, updated_at = ? WHERE id = ?`, toJSON(kept), now, d.ID); err != nil {
			return err
		}
	}
	return nil
}

// InsightIDs returns the set of insight ids of a project.
func InsightIDs(ctx context.Context, q Q, projectID uint32) (map[int64]bool, error) {
	rows, err := q.QueryContext(ctx, `SELECT id FROM insights WHERE project_id = ?`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// ---- dashboards ----

func scanDashboard(row interface{ Scan(...any) error }) (Dashboard, error) {
	var d Dashboard
	var items string
	err := row.Scan(&d.ID, &d.Name, &items, &d.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return d, ErrNotFound
	}
	if err != nil {
		return d, err
	}
	d.Items = []DashboardItem{}
	_ = json.Unmarshal([]byte(items), &d.Items)
	if d.Items == nil {
		d.Items = []DashboardItem{}
	}
	return d, nil
}

// CreateDashboard inserts a dashboard.
func CreateDashboard(ctx context.Context, q Q, projectID uint32, d Dashboard, now int64) (Dashboard, error) {
	if d.Items == nil {
		d.Items = []DashboardItem{}
	}
	res, err := q.ExecContext(ctx, `INSERT INTO dashboards(project_id, name, items, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		projectID, d.Name, toJSON(d.Items), now, now)
	if err != nil {
		return d, fmt.Errorf("db: create dashboard: %w", err)
	}
	d.ID, err = res.LastInsertId()
	d.UpdatedAt = now
	return d, err
}

// DashboardByID returns one dashboard of a project.
func DashboardByID(ctx context.Context, q Q, projectID uint32, id int64) (Dashboard, error) {
	return scanDashboard(q.QueryRowContext(ctx, `SELECT id, name, items, updated_at FROM dashboards WHERE project_id = ? AND id = ?`, projectID, id))
}

// ListDashboards lists a project's dashboards.
func ListDashboards(ctx context.Context, q Q, projectID uint32) ([]Dashboard, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, name, items, updated_at FROM dashboards WHERE project_id = ? ORDER BY id`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Dashboard{}
	for rows.Next() {
		d, err := scanDashboard(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// UpdateDashboard replaces name and items.
func UpdateDashboard(ctx context.Context, q Q, projectID uint32, d Dashboard, now int64) (Dashboard, error) {
	if d.Items == nil {
		d.Items = []DashboardItem{}
	}
	res, err := q.ExecContext(ctx, `UPDATE dashboards SET name = ?, items = ?, updated_at = ? WHERE project_id = ? AND id = ?`,
		d.Name, toJSON(d.Items), now, projectID, d.ID)
	if err != nil {
		return d, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return d, ErrNotFound
	}
	d.UpdatedAt = now
	return d, nil
}

// DeleteDashboard removes a dashboard.
func DeleteDashboard(ctx context.Context, q Q, projectID uint32, id int64) error {
	res, err := q.ExecContext(ctx, `DELETE FROM dashboards WHERE project_id = ? AND id = ?`, projectID, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ---- deletion requests ----

const deletionCols = `d.id, d.project_id, d.ts_ms, d.user_ids, d.anon_ids, d.status, d.note, d.created_at, d.created_by, COALESCE(d.published_at, 0)`

func scanDeletion(row interface{ Scan(...any) error }) (Deletion, error) {
	var d Deletion
	var pid int64
	var users, anons, by string
	err := row.Scan(&d.ID, &pid, &d.TSMillis, &users, &anons, &d.Status, &d.Note, &d.CreatedAt, &by, &d.PublishedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return d, ErrNotFound
	}
	if err != nil {
		return d, err
	}
	d.ProjectID = uint32(pid)
	d.UserIDs, d.AnonIDs, d.CreatedBy = stringsFromJSON(users), stringsFromJSON(anons), ParseActor(by)
	return d, nil
}

// CreateDeletion inserts a deletion request. Its tombstone timestamp is now,
// pushed forward if needed so that it is strictly greater than every earlier
// tombstone of the project (the engine's per-day watermark relies on that).
func CreateDeletion(ctx context.Context, q Q, d Deletion, now int64) (Deletion, error) {
	var last int64
	if err := q.QueryRowContext(ctx, `SELECT COALESCE(MAX(ts_ms), 0) FROM deletion_requests WHERE project_id = ?`, d.ProjectID).Scan(&last); err != nil {
		return d, err
	}
	d.TSMillis = max(now, last+1)
	d.CreatedAt = now
	d.Status = "pending"
	d.PublishedAt = 0
	if d.UserIDs == nil {
		d.UserIDs = []string{}
	}
	if d.AnonIDs == nil {
		d.AnonIDs = []string{}
	}
	_, err := q.ExecContext(ctx, `INSERT INTO deletion_requests(id, project_id, ts_ms, user_ids, anon_ids, status, note, created_at, created_by)
		VALUES (?, ?, ?, ?, ?, 'pending', ?, ?, ?)`, d.ID, d.ProjectID, d.TSMillis, toJSON(d.UserIDs), toJSON(d.AnonIDs), d.Note, now, d.CreatedBy.String())
	if isUniqueViolation(err) {
		return d, ErrConflict
	}
	if err != nil {
		return d, fmt.Errorf("db: create deletion: %w", err)
	}
	return d, nil
}

func listDeletions(ctx context.Context, q Q, query string, args ...any) ([]Deletion, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Deletion{}
	for rows.Next() {
		d, err := scanDeletion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ListDeletions lists a project's deletion requests, newest first.
func ListDeletions(ctx context.Context, q Q, projectID uint32) ([]Deletion, error) {
	return listDeletions(ctx, q, `SELECT `+deletionCols+` FROM deletion_requests d WHERE d.project_id = ? ORDER BY d.ts_ms DESC`, projectID)
}

// DeletionsSince lists every deletion request created at or after since,
// ordered by project and tombstone timestamp (the order tombstones must be
// written in).
func DeletionsSince(ctx context.Context, q Q, since int64) ([]Deletion, error) {
	return listDeletions(ctx, q, `SELECT `+deletionCols+` FROM deletion_requests d WHERE d.created_at >= ? ORDER BY d.project_id, d.ts_ms`, since)
}

// PendingDeletions lists the requests whose status is still "pending".
func PendingDeletions(ctx context.Context, q Q) ([]Deletion, error) {
	return listDeletions(ctx, q, `SELECT `+deletionCols+` FROM deletion_requests d WHERE d.status = 'pending' ORDER BY d.project_id, d.ts_ms`)
}

// MarkDeletionPublished records that the tombstone object exists.
func MarkDeletionPublished(ctx context.Context, q Q, id string, now int64) error {
	_, err := q.ExecContext(ctx, `UPDATE deletion_requests SET published_at = ? WHERE id = ? AND published_at IS NULL`, now, id)
	return err
}

// MarkDeletionDone flips a request to "done".
func MarkDeletionDone(ctx context.Context, q Q, id string, now int64) error {
	_, err := q.ExecContext(ctx, `UPDATE deletion_requests SET status = 'done', done_at = ? WHERE id = ? AND status = 'pending'`, now, id)
	return err
}

// ---- publish state ----

// GetPublishState returns the stored state of a published object family
// (zero value if it was never published).
func GetPublishState(ctx context.Context, q Q, name string) (PublishState, error) {
	var s PublishState
	err := q.QueryRowContext(ctx, `SELECT version, hash, generated_at FROM publish_state WHERE name = ?`, name).Scan(&s.Version, &s.Hash, &s.GeneratedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return PublishState{}, nil
	}
	return s, err
}

// SetPublishState stores the state of a published object family.
func SetPublishState(ctx context.Context, q Q, name string, s PublishState) error {
	_, err := q.ExecContext(ctx, `INSERT INTO publish_state(name, version, hash, generated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET version = excluded.version, hash = excluded.hash, generated_at = excluded.generated_at`,
		name, s.Version, s.Hash, s.GeneratedAt)
	return err
}

// AllFlags returns the flags of every open project, grouped by project and
// ordered by key.
func AllFlags(ctx context.Context, q Q) (map[uint32][]Flag, error) {
	rows, err := q.QueryContext(ctx, `SELECT f.project_id, `+flagCols+` FROM flags f JOIN projects p ON p.id = f.project_id
		WHERE p.closed_at IS NULL ORDER BY f.project_id, f.key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uint32][]Flag{}
	for rows.Next() {
		var pid int64
		var f Flag
		var rules, def, by string
		if err := rows.Scan(&pid, &f.Key, &f.Name, &f.Description, &f.Enabled, &f.Salt, &rules, &def, &f.Version, &f.UpdatedAt, &by); err != nil {
			return nil, err
		}
		f.Rules, f.Default, f.UpdatedBy = json.RawMessage(rules), json.RawMessage(def), ParseActor(by)
		out[uint32(pid)] = append(out[uint32(pid)], f)
	}
	return out, rows.Err()
}
