package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// OAuth client kinds.
const (
	OAuthClientDCR  = "dcr"  // registered through POST /oauth/register (RFC 7591)
	OAuthClientCIMD = "cimd" // client_id is the URL of a Client ID Metadata Document
)

// OAuth token kinds.
const (
	OAuthAccess  = "access"
	OAuthRefresh = "refresh"
)

// OAuthClient is a registered (or cached) OAuth client. Clients are public:
// there is no client secret.
type OAuthClient struct {
	ClientID     string
	Kind         string
	Name         string
	ClientURI    string
	RedirectURIs []string
	CreatedAt    int64
	FetchedAt    int64 // cimd: time of the last successful fetch
	FreshUntil   int64 // cimd: the cached document may be used until then
	LastUsedAt   int64
}

const oauthClientCols = `client_id, kind, name, client_uri, redirect_uris, created_at, fetched_at, fresh_until, last_used_at`

func scanOAuthClient(row interface{ Scan(...any) error }) (OAuthClient, error) {
	var c OAuthClient
	var uris string
	err := row.Scan(&c.ClientID, &c.Kind, &c.Name, &c.ClientURI, &uris, &c.CreatedAt, &c.FetchedAt, &c.FreshUntil, &c.LastUsedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	c.RedirectURIs = stringsFromJSON(uris)
	return c, err
}

// SaveOAuthClient inserts a client or, for an existing client_id, replaces
// its descriptive fields (a refreshed metadata document). The creation time
// of an existing row is kept.
func SaveOAuthClient(ctx context.Context, q Q, c OAuthClient) error {
	_, err := q.ExecContext(ctx, `INSERT INTO oauth_clients(`+oauthClientCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(client_id) DO UPDATE SET name = excluded.name, client_uri = excluded.client_uri,
			redirect_uris = excluded.redirect_uris, fetched_at = excluded.fetched_at, fresh_until = excluded.fresh_until,
			last_used_at = excluded.last_used_at
		WHERE oauth_clients.kind = excluded.kind`,
		c.ClientID, c.Kind, c.Name, c.ClientURI, toJSON(c.RedirectURIs), c.CreatedAt, c.FetchedAt, c.FreshUntil, c.LastUsedAt)
	if err != nil {
		return fmt.Errorf("db: save oauth client: %w", err)
	}
	return nil
}

// OAuthClientByID looks a client up.
func OAuthClientByID(ctx context.Context, q Q, clientID string) (OAuthClient, error) {
	return scanOAuthClient(q.QueryRowContext(ctx, `SELECT `+oauthClientCols+` FROM oauth_clients WHERE client_id = ?`, clientID))
}

// TouchOAuthClient records that a client was used.
func TouchOAuthClient(ctx context.Context, q Q, clientID string, now int64) error {
	_, err := q.ExecContext(ctx, `UPDATE oauth_clients SET last_used_at = ? WHERE client_id = ?`, now, clientID)
	return err
}

// CountOAuthClients returns the number of clients of one kind.
func CountOAuthClients(ctx context.Context, q Q, kind string) (int, error) {
	var n int
	err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM oauth_clients WHERE kind = ?`, kind).Scan(&n)
	return n, err
}

// PruneOAuthClients deletes clients that hold no live grant and were last
// used before the cut-off: neverUsedBefore applies to clients that never
// obtained a grant at all, idleBefore to the rest. A pruned dynamically
// registered client simply has to register again.
func PruneOAuthClients(ctx context.Context, q Q, now, neverUsedBefore, idleBefore int64) (int64, error) {
	res, err := q.ExecContext(ctx, `DELETE FROM oauth_clients WHERE
		NOT EXISTS (SELECT 1 FROM oauth_grants g WHERE g.client_id = oauth_clients.client_id AND g.revoked_at IS NULL AND g.expires_at > ?1)
		AND NOT EXISTS (SELECT 1 FROM oauth_codes c WHERE c.client_id = oauth_clients.client_id AND c.expires_at > ?1)
		AND ((last_used_at < ?2 AND NOT EXISTS (SELECT 1 FROM oauth_grants g WHERE g.client_id = oauth_clients.client_id))
		     OR last_used_at < ?3)`, now, neverUsedBefore, idleBefore)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// OAuthCode is a pending authorization code (stored by hash).
type OAuthCode struct {
	Hash          string
	UserID        int64
	ClientID      string
	RedirectURI   string
	CodeChallenge string
	Scopes        []string
	Resource      string
	CreatedAt     int64
	ExpiresAt     int64
	UsedAt        int64 // 0 = not redeemed yet
	GrantID       int64 // 0 = none yet
}

// CreateOAuthCode stores a new authorization code.
func CreateOAuthCode(ctx context.Context, q Q, c OAuthCode) error {
	_, err := q.ExecContext(ctx, `INSERT INTO oauth_codes(code_hash, user_id, client_id, redirect_uri, code_challenge, scopes, resource, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, c.Hash, c.UserID, c.ClientID, c.RedirectURI, c.CodeChallenge, toJSON(c.Scopes), c.Resource, c.CreatedAt, c.ExpiresAt)
	if err != nil {
		return fmt.Errorf("db: create oauth code: %w", err)
	}
	return nil
}

// OAuthCodeByHash returns a code whether or not it is still usable; the
// caller decides (expiry, reuse).
func OAuthCodeByHash(ctx context.Context, q Q, hash string) (OAuthCode, error) {
	c := OAuthCode{Hash: hash}
	var scopes string
	var used, grant sql.NullInt64
	err := q.QueryRowContext(ctx, `SELECT user_id, client_id, redirect_uri, code_challenge, scopes, resource, created_at, expires_at, used_at, grant_id
		FROM oauth_codes WHERE code_hash = ?`, hash).Scan(&c.UserID, &c.ClientID, &c.RedirectURI, &c.CodeChallenge, &scopes, &c.Resource,
		&c.CreatedAt, &c.ExpiresAt, &used, &grant)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	c.Scopes, c.UsedAt, c.GrantID = stringsFromJSON(scopes), used.Int64, grant.Int64
	return c, err
}

// RedeemOAuthCode marks a code as used and links it to the grant created
// from it. It reports false when the code had already been used: the check
// and the update are one statement, so two racing redemptions cannot both
// succeed.
func RedeemOAuthCode(ctx context.Context, q Q, hash string, grantID, now int64) (bool, error) {
	res, err := q.ExecContext(ctx, `UPDATE oauth_codes SET used_at = ?, grant_id = ? WHERE code_hash = ? AND used_at IS NULL`, now, grantID, hash)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// OAuthGrant is one approval: a client acting as a user.
type OAuthGrant struct {
	ID           int64
	UserID       int64
	ClientID     string
	Scopes       []string
	Resource     string
	CreatedAt    int64
	ExpiresAt    int64
	LastUsedAt   int64 // 0 = never
	RevokedAt    int64 // 0 = live
	RevokeReason string

	// Joined from oauth_clients.
	ClientName   string
	ClientKind   string
	ClientURI    string
	RedirectURIs []string
}

// Live reports whether the grant can still be used at now.
func (g *OAuthGrant) Live(now int64) bool { return g.RevokedAt == 0 && g.ExpiresAt > now }

// HasScope reports whether the grant carries scope.
func (g *OAuthGrant) HasScope(scope string) bool {
	for _, s := range g.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

// CreateOAuthGrant inserts a grant and returns its id.
func CreateOAuthGrant(ctx context.Context, q Q, g OAuthGrant) (int64, error) {
	res, err := q.ExecContext(ctx, `INSERT INTO oauth_grants(user_id, client_id, scopes, resource, created_at, expires_at) VALUES (?, ?, ?, ?, ?, ?)`,
		g.UserID, g.ClientID, toJSON(g.Scopes), g.Resource, g.CreatedAt, g.ExpiresAt)
	if err != nil {
		return 0, fmt.Errorf("db: create oauth grant: %w", err)
	}
	return res.LastInsertId()
}

const oauthGrantCols = `g.id, g.user_id, g.client_id, g.scopes, g.resource, g.created_at, g.expires_at, g.last_used_at, g.revoked_at, g.revoke_reason,
	c.name, c.kind, c.client_uri, c.redirect_uris`

func scanOAuthGrant(row interface{ Scan(...any) error }, extra ...any) (OAuthGrant, error) {
	var g OAuthGrant
	var scopes, uris string
	var used, revoked sql.NullInt64
	dest := append([]any{&g.ID, &g.UserID, &g.ClientID, &scopes, &g.Resource, &g.CreatedAt, &g.ExpiresAt, &used, &revoked, &g.RevokeReason,
		&g.ClientName, &g.ClientKind, &g.ClientURI, &uris}, extra...)
	err := row.Scan(dest...)
	if errors.Is(err, sql.ErrNoRows) {
		return g, ErrNotFound
	}
	g.Scopes, g.RedirectURIs = stringsFromJSON(scopes), stringsFromJSON(uris)
	g.LastUsedAt, g.RevokedAt = used.Int64, revoked.Int64
	return g, err
}

// OAuthGrantByID returns a grant (live or not).
func OAuthGrantByID(ctx context.Context, q Q, id int64) (OAuthGrant, error) {
	return scanOAuthGrant(q.QueryRowContext(ctx, `SELECT `+oauthGrantCols+` FROM oauth_grants g JOIN oauth_clients c ON c.client_id = g.client_id WHERE g.id = ?`, id))
}

// ListOAuthGrants lists a user's live grants, newest first.
func ListOAuthGrants(ctx context.Context, q Q, userID, now int64) ([]OAuthGrant, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+oauthGrantCols+` FROM oauth_grants g JOIN oauth_clients c ON c.client_id = g.client_id
		WHERE g.user_id = ? AND g.revoked_at IS NULL AND g.expires_at > ? ORDER BY g.id DESC`, userID, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []OAuthGrant{}
	for rows.Next() {
		g, err := scanOAuthGrant(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// RevokeOAuthGrant ends a grant: it is marked revoked and every token issued
// under it is deleted. It reports false when the grant was not live.
func RevokeOAuthGrant(ctx context.Context, q Q, id, now int64, reason string) (bool, error) {
	res, err := q.ExecContext(ctx, `UPDATE oauth_grants SET revoked_at = ?, revoke_reason = ? WHERE id = ? AND revoked_at IS NULL`, now, reason, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if _, err := q.ExecContext(ctx, `DELETE FROM oauth_tokens WHERE grant_id = ?`, id); err != nil {
		return false, err
	}
	return n == 1, nil
}

// RevokeUserOAuthGrants ends every live grant of a user (password change,
// second factor switched on). It returns how many were revoked.
func RevokeUserOAuthGrants(ctx context.Context, q Q, userID, now int64, reason string) (int64, error) {
	if _, err := q.ExecContext(ctx, `DELETE FROM oauth_tokens WHERE grant_id IN (SELECT id FROM oauth_grants WHERE user_id = ?)`, userID); err != nil {
		return 0, err
	}
	if _, err := q.ExecContext(ctx, `DELETE FROM oauth_codes WHERE user_id = ?`, userID); err != nil {
		return 0, err
	}
	res, err := q.ExecContext(ctx, `UPDATE oauth_grants SET revoked_at = ?, revoke_reason = ? WHERE user_id = ? AND revoked_at IS NULL`, now, reason, userID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// TouchOAuthGrant records that a grant was used.
func TouchOAuthGrant(ctx context.Context, q Q, id, now int64) error {
	_, err := q.ExecContext(ctx, `UPDATE oauth_grants SET last_used_at = ? WHERE id = ?`, now, id)
	return err
}

// CreateOAuthToken stores the hash of a new access or refresh token.
func CreateOAuthToken(ctx context.Context, q Q, hash string, grantID int64, kind string, now, expires int64) error {
	_, err := q.ExecContext(ctx, `INSERT INTO oauth_tokens(hash, grant_id, kind, created_at, expires_at) VALUES (?, ?, ?, ?, ?)`, hash, grantID, kind, now, expires)
	if err != nil {
		return fmt.Errorf("db: create oauth token: %w", err)
	}
	return nil
}

// OAuthToken is a stored token joined with its grant.
type OAuthToken struct {
	Kind      string
	ExpiresAt int64
	UsedAt    int64 // refresh tokens: 0 = not rotated yet
	Grant     OAuthGrant
}

// OAuthTokenByHash returns a token of the given kind with its grant. Expiry,
// rotation and revocation are left to the caller, which needs to tell these
// cases apart.
func OAuthTokenByHash(ctx context.Context, q Q, hash, kind string) (OAuthToken, error) {
	t := OAuthToken{Kind: kind}
	var used sql.NullInt64
	g, err := scanOAuthGrant(q.QueryRowContext(ctx, `SELECT `+oauthGrantCols+`, t.expires_at, t.used_at
		FROM oauth_tokens t JOIN oauth_grants g ON g.id = t.grant_id JOIN oauth_clients c ON c.client_id = g.client_id
		WHERE t.hash = ? AND t.kind = ?`, hash, kind), &t.ExpiresAt, &used)
	t.Grant, t.UsedAt = g, used.Int64
	return t, err
}

// RotateOAuthRefresh marks a refresh token as used. It reports false when it
// had been used before (the caller treats that as replay).
func RotateOAuthRefresh(ctx context.Context, q Q, hash string, now int64) (bool, error) {
	res, err := q.ExecContext(ctx, `UPDATE oauth_tokens SET used_at = ? WHERE hash = ? AND kind = 'refresh' AND used_at IS NULL`, now, hash)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// DeleteOAuthAccessTokens removes the access tokens of a grant (they are
// replaced on every refresh).
func DeleteOAuthAccessTokens(ctx context.Context, q Q, grantID int64) error {
	_, err := q.ExecContext(ctx, `DELETE FROM oauth_tokens WHERE grant_id = ? AND kind = 'access'`, grantID)
	return err
}

// DeleteOAuthToken removes one token by hash; it reports whether it existed.
func DeleteOAuthToken(ctx context.Context, q Q, hash string) (bool, error) {
	res, err := q.ExecContext(ctx, `DELETE FROM oauth_tokens WHERE hash = ?`, hash)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// PruneOAuth removes what can no longer be used: authorization codes a day
// past their expiry (until then a second redemption is still recognised as
// a replay), expired access tokens, refresh tokens past their expiry
// (rotated ones are kept until then for the same reason), and grants that
// ended more than keepEndedFor milliseconds ago.
func PruneOAuth(ctx context.Context, q Q, now, keepEndedFor int64) error {
	const day = 24 * 60 * 60 * 1000
	for _, stmt := range []struct {
		query string
		args  []any
	}{
		{`DELETE FROM oauth_codes WHERE expires_at <= ?`, []any{now - day}},
		{`DELETE FROM oauth_tokens WHERE expires_at <= ?`, []any{now}},
		{`DELETE FROM oauth_grants WHERE (revoked_at IS NOT NULL AND revoked_at <= ?) OR expires_at <= ?`, []any{now - keepEndedFor, now - keepEndedFor}},
	} {
		if _, err := q.ExecContext(ctx, stmt.query, stmt.args...); err != nil {
			return fmt.Errorf("db: prune oauth: %w", err)
		}
	}
	return nil
}
