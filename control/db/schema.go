package db

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// migrations are applied in order, each in its own transaction. Never edit
// an entry that has shipped; append a new one.
var migrations = []string{
	// 1: initial schema (docs/design.md §14.1 + sessions + publish state).
	`
CREATE TABLE orgs (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  name       TEXT    NOT NULL,
  created_at INTEGER NOT NULL
);

CREATE TABLE users (
  id             INTEGER PRIMARY KEY AUTOINCREMENT,
  email          TEXT    NOT NULL UNIQUE,
  password_hash  TEXT    NOT NULL,
  totp_secret    TEXT,
  totp_pending   TEXT,
  totp_enabled   INTEGER NOT NULL DEFAULT 0,
  totp_last_step INTEGER NOT NULL DEFAULT 0,
  oidc_subject   TEXT UNIQUE,
  created_at     INTEGER NOT NULL
);

CREATE TABLE memberships (
  org_id     INTEGER NOT NULL REFERENCES orgs(id)  ON DELETE CASCADE,
  user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  role       TEXT    NOT NULL CHECK (role IN ('owner','admin','member','viewer')),
  created_at INTEGER NOT NULL,
  PRIMARY KEY (org_id, user_id)
);
CREATE INDEX memberships_user ON memberships(user_id);

CREATE TABLE sessions (
  id_hash    TEXT    PRIMARY KEY,
  user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  csrf       TEXT    NOT NULL,
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL
);
CREATE INDEX sessions_user ON sessions(user_id);
CREATE INDEX sessions_expiry ON sessions(expires_at);

-- AUTOINCREMENT: ids only ever grow, even after rows are deleted, which is
-- what "project ids are never reused" needs. Projects are never deleted
-- anyway; closing sets closed_at. org_id has no ON DELETE action on purpose:
-- an organisation with projects is soft-deleted, never removed.
CREATE TABLE projects (
  id                     INTEGER PRIMARY KEY AUTOINCREMENT,
  org_id                 INTEGER NOT NULL REFERENCES orgs(id),
  name                   TEXT    NOT NULL,
  retention_days         INTEGER NOT NULL,
  replay_retention_days  INTEGER NOT NULL,
  drop_keys              TEXT    NOT NULL DEFAULT '[]',
  scan_limit_bytes       INTEGER NOT NULL DEFAULT 0,
  max_concurrent_queries INTEGER NOT NULL,
  replay_sample_rate     REAL    NOT NULL,
  created_at             INTEGER NOT NULL,
  closed_at              INTEGER
);
CREATE INDEX projects_org ON projects(org_id);

CREATE TABLE tokens (
  token           TEXT    PRIMARY KEY,
  project_id      INTEGER NOT NULL REFERENCES projects(id),
  name            TEXT    NOT NULL,
  status          TEXT    NOT NULL CHECK (status IN ('active','disabled')),
  allowed_origins TEXT    NOT NULL DEFAULT '[]',
  rate_limit      INTEGER NOT NULL DEFAULT 0,
  replay          INTEGER NOT NULL DEFAULT 0,
  created_at      INTEGER NOT NULL
);
CREATE INDEX tokens_project ON tokens(project_id);

-- Secret keys: only the SHA-256 of the secret is stored.
CREATE TABLE keys (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  project_id   INTEGER NOT NULL REFERENCES projects(id),
  name         TEXT    NOT NULL,
  scopes       TEXT    NOT NULL,
  hash         TEXT    NOT NULL UNIQUE,
  prefix       TEXT    NOT NULL,
  created_at   INTEGER NOT NULL,
  last_used_at INTEGER
);
CREATE INDEX keys_project ON keys(project_id);

CREATE TABLE flags (
  project_id    INTEGER NOT NULL REFERENCES projects(id),
  key           TEXT    NOT NULL,
  name          TEXT    NOT NULL DEFAULT '',
  description   TEXT    NOT NULL DEFAULT '',
  enabled       INTEGER NOT NULL DEFAULT 0,
  salt          TEXT    NOT NULL,
  rules         TEXT    NOT NULL DEFAULT '[]',
  default_value TEXT    NOT NULL DEFAULT 'null',
  version       INTEGER NOT NULL,
  updated_at    INTEGER NOT NULL,
  updated_by    TEXT    NOT NULL,
  PRIMARY KEY (project_id, key)
);

-- Append-only history. Rows outlive the flag: a key that is deleted and
-- created again continues its version numbering.
CREATE TABLE flag_versions (
  project_id INTEGER NOT NULL REFERENCES projects(id),
  key        TEXT    NOT NULL,
  version    INTEGER NOT NULL,
  ts         INTEGER NOT NULL,
  actor      TEXT    NOT NULL,
  flag       TEXT    NOT NULL,
  PRIMARY KEY (project_id, key, version)
);

CREATE TABLE insights (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  project_id INTEGER NOT NULL REFERENCES projects(id),
  name       TEXT    NOT NULL,
  kind       TEXT    NOT NULL CHECK (kind IN ('trends','funnel','retention','sql')),
  query      TEXT    NOT NULL,
  created_by TEXT    NOT NULL,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE INDEX insights_project ON insights(project_id);

CREATE TABLE dashboards (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  project_id INTEGER NOT NULL REFERENCES projects(id),
  name       TEXT    NOT NULL,
  items      TEXT    NOT NULL DEFAULT '[]',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE INDEX dashboards_project ON dashboards(project_id);

-- ts_ms is the tombstone timestamp: strictly increasing per project so the
-- compactor's "tombstone_ts >= ts_ms" watermark can never skip a request.
CREATE TABLE deletion_requests (
  id           TEXT    PRIMARY KEY,
  project_id   INTEGER NOT NULL REFERENCES projects(id),
  ts_ms        INTEGER NOT NULL,
  user_ids     TEXT    NOT NULL DEFAULT '[]',
  anon_ids     TEXT    NOT NULL DEFAULT '[]',
  status       TEXT    NOT NULL CHECK (status IN ('pending','done')),
  note         TEXT    NOT NULL DEFAULT '',
  created_at   INTEGER NOT NULL,
  created_by   TEXT    NOT NULL,
  published_at INTEGER,
  done_at      INTEGER,
  UNIQUE (project_id, ts_ms)
);
CREATE INDEX deletion_status ON deletion_requests(status);

-- org_id is NULL for events that belong to a user rather than an
-- organisation (login, password change, TOTP).
CREATE TABLE audit_log (
  id     INTEGER PRIMARY KEY AUTOINCREMENT,
  ts     INTEGER NOT NULL,
  org_id INTEGER,
  actor  TEXT    NOT NULL,
  action TEXT    NOT NULL,
  target TEXT    NOT NULL DEFAULT '',
  detail TEXT    NOT NULL DEFAULT '{}'
);
CREATE INDEX audit_org ON audit_log(org_id, id);
CREATE INDEX audit_actor ON audit_log(actor, id);

-- One row per published object family: the last version handed out and a
-- hash of the content it was handed out for (spec/control.md §9).
CREATE TABLE publish_state (
  name         TEXT    PRIMARY KEY,
  version      INTEGER NOT NULL,
  hash         TEXT    NOT NULL,
  generated_at INTEGER NOT NULL
);
`,
	// 2: soft delete for organisations.
	`ALTER TABLE orgs ADD COLUMN deleted_at INTEGER;`,
	// 3: OAuth 2.1 authorization server for the MCP endpoint
	// (spec/control.md §13). Authorization codes, access tokens and refresh
	// tokens are stored as SHA-256 only, like session cookies and sk_ keys.
	`
-- A client is either registered dynamically (RFC 7591, kind 'dcr', random
-- client_id) or described by a Client ID Metadata Document (kind 'cimd',
-- client_id is the document's HTTPS URL; the row is a cache of the last
-- successful fetch). Every client is public: there are no client secrets.
CREATE TABLE oauth_clients (
  client_id     TEXT    PRIMARY KEY,
  kind          TEXT    NOT NULL CHECK (kind IN ('dcr','cimd')),
  name          TEXT    NOT NULL,
  client_uri    TEXT    NOT NULL DEFAULT '',
  redirect_uris TEXT    NOT NULL,
  created_at    INTEGER NOT NULL,
  fetched_at    INTEGER NOT NULL DEFAULT 0,
  fresh_until   INTEGER NOT NULL DEFAULT 0,
  last_used_at  INTEGER NOT NULL
);
CREATE INDEX oauth_clients_kind ON oauth_clients(kind, last_used_at);

-- One row per approval on the consent screen: "this client may act as this
-- user with these scopes". Tokens hang off the grant, so revoking it (or
-- deleting the user) ends every token at once.
CREATE TABLE oauth_grants (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id       INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  client_id     TEXT    NOT NULL REFERENCES oauth_clients(client_id) ON DELETE CASCADE,
  scopes        TEXT    NOT NULL,
  resource      TEXT    NOT NULL,
  created_at    INTEGER NOT NULL,
  expires_at    INTEGER NOT NULL,
  last_used_at  INTEGER,
  revoked_at    INTEGER,
  revoke_reason TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX oauth_grants_user ON oauth_grants(user_id);
CREATE INDEX oauth_grants_client ON oauth_grants(client_id);

-- One-time authorization codes. grant_id is set when the code is redeemed,
-- so that a second redemption can revoke what the first one obtained.
CREATE TABLE oauth_codes (
  code_hash      TEXT    PRIMARY KEY,
  user_id        INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  client_id      TEXT    NOT NULL REFERENCES oauth_clients(client_id) ON DELETE CASCADE,
  redirect_uri   TEXT    NOT NULL,
  code_challenge TEXT    NOT NULL,
  scopes         TEXT    NOT NULL,
  resource       TEXT    NOT NULL,
  created_at     INTEGER NOT NULL,
  expires_at     INTEGER NOT NULL,
  used_at        INTEGER,
  grant_id       INTEGER REFERENCES oauth_grants(id) ON DELETE SET NULL
);
CREATE INDEX oauth_codes_expiry ON oauth_codes(expires_at);

-- used_at marks a refresh token that has been rotated: presenting it again
-- is treated as theft and revokes the grant.
CREATE TABLE oauth_tokens (
  hash       TEXT    PRIMARY KEY,
  grant_id   INTEGER NOT NULL REFERENCES oauth_grants(id) ON DELETE CASCADE,
  kind       TEXT    NOT NULL CHECK (kind IN ('access','refresh')),
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  used_at    INTEGER
);
CREATE INDEX oauth_tokens_grant ON oauth_tokens(grant_id);
CREATE INDEX oauth_tokens_expiry ON oauth_tokens(expires_at);
`,
}

func (d *DB) migrate(ctx context.Context) error {
	if _, err := d.w.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
  version    INTEGER PRIMARY KEY,
  applied_at INTEGER NOT NULL)`); err != nil {
		return fmt.Errorf("db: migrations table: %w", err)
	}
	var current int
	if err := d.w.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		return fmt.Errorf("db: read schema version: %w", err)
	}
	if current > len(migrations) {
		return fmt.Errorf("db: database schema version %d is newer than this binary (%d)", current, len(migrations))
	}
	for v := current + 1; v <= len(migrations); v++ {
		err := d.Write(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, migrations[v-1]); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at) VALUES (?, ?)`, v, time.Now().UnixMilli())
			return err
		})
		if err != nil {
			return fmt.Errorf("db: migration %d: %w", v, err)
		}
	}
	return nil
}

// SchemaVersion returns the number of applied migrations.
func (d *DB) SchemaVersion(ctx context.Context) (int, error) {
	var v int
	err := d.r.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&v)
	return v, err
}
