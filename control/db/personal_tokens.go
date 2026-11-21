package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// PersonalToken is a personal access token of a user. The secret itself is
// not stored, only its SHA-256.
type PersonalToken struct {
	ID         int64
	UserID     int64
	Name       string
	Scopes     []string
	Hash       string
	Prefix     string
	CreatedAt  int64
	ExpiresAt  int64 // 0 = never
	LastUsedAt int64 // 0 = never
}

// Live reports whether the token has not expired at now.
func (t *PersonalToken) Live(now int64) bool { return t.ExpiresAt == 0 || t.ExpiresAt > now }

const personalTokenCols = `id, user_id, name, scopes, hash, prefix, created_at, expires_at, last_used_at`

func scanPersonalToken(row interface{ Scan(...any) error }) (PersonalToken, error) {
	var t PersonalToken
	var scopes string
	var expires, used sql.NullInt64
	err := row.Scan(&t.ID, &t.UserID, &t.Name, &scopes, &t.Hash, &t.Prefix, &t.CreatedAt, &expires, &used)
	if errors.Is(err, sql.ErrNoRows) {
		return t, ErrNotFound
	}
	t.Scopes, t.ExpiresAt, t.LastUsedAt = stringsFromJSON(scopes), expires.Int64, used.Int64
	return t, err
}

// CreatePersonalToken stores a token and returns its id.
func CreatePersonalToken(ctx context.Context, q Q, t PersonalToken) (int64, error) {
	var expires any
	if t.ExpiresAt != 0 {
		expires = t.ExpiresAt
	}
	res, err := q.ExecContext(ctx, `INSERT INTO personal_tokens(user_id, name, scopes, hash, prefix, created_at, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		t.UserID, t.Name, toJSON(t.Scopes), t.Hash, t.Prefix, t.CreatedAt, expires)
	if err != nil {
		return 0, fmt.Errorf("db: create personal token: %w", err)
	}
	return res.LastInsertId()
}

// PersonalTokenByHash looks a token up by the hash of its secret, expired
// or not.
func PersonalTokenByHash(ctx context.Context, q Q, hash string) (PersonalToken, error) {
	return scanPersonalToken(q.QueryRowContext(ctx, `SELECT `+personalTokenCols+` FROM personal_tokens WHERE hash = ?`, hash))
}

// ListPersonalTokens lists a user's tokens that have not expired, newest
// first.
func ListPersonalTokens(ctx context.Context, q Q, userID, now int64) ([]PersonalToken, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+personalTokenCols+` FROM personal_tokens
		WHERE user_id = ? AND (expires_at IS NULL OR expires_at > ?) ORDER BY id DESC`, userID, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PersonalToken{}
	for rows.Next() {
		t, err := scanPersonalToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// DeletePersonalToken removes one token of a user and returns it;
// ErrNotFound when the user has no such token.
func DeletePersonalToken(ctx context.Context, q Q, userID, id int64) (PersonalToken, error) {
	t, err := scanPersonalToken(q.QueryRowContext(ctx, `SELECT `+personalTokenCols+` FROM personal_tokens WHERE id = ? AND user_id = ?`, id, userID))
	if err != nil {
		return t, err
	}
	_, err = q.ExecContext(ctx, `DELETE FROM personal_tokens WHERE id = ?`, id)
	return t, err
}

// DeleteUserPersonalTokens removes every token of a user (password change,
// second factor switched on) and returns how many there were.
func DeleteUserPersonalTokens(ctx context.Context, q Q, userID int64) (int64, error) {
	res, err := q.ExecContext(ctx, `DELETE FROM personal_tokens WHERE user_id = ?`, userID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// TouchPersonalToken records that a token was used.
func TouchPersonalToken(ctx context.Context, q Q, id, now int64) error {
	_, err := q.ExecContext(ctx, `UPDATE personal_tokens SET last_used_at = ? WHERE id = ?`, now, id)
	return err
}

// PrunePersonalTokens removes expired tokens.
func PrunePersonalTokens(ctx context.Context, q Q, now int64) (int64, error) {
	res, err := q.ExecContext(ctx, `DELETE FROM personal_tokens WHERE expires_at IS NOT NULL AND expires_at <= ?`, now)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
