// Package db is the control plane's SQLite database (docs/design.md §14.1):
// schema, migrations and typed queries. SQLite runs in WAL mode with foreign
// keys on. All writes go through one connection (Write), reads through a
// small pool of read-only connections, so a slow reader never blocks a
// mutation and a mutation never observes a half-applied one.
package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, registered as "sqlite"
)

// Common query errors.
var (
	// ErrNotFound: the row does not exist (or is not visible to the caller).
	ErrNotFound = errors.New("db: not found")
	// ErrConflict: a uniqueness rule would be violated.
	ErrConflict = errors.New("db: conflict")
	// ErrLastOwner: the change would leave an organisation without an owner.
	ErrLastOwner = errors.New("db: an organisation must keep at least one owner")
	// ErrProjectIDsExhausted: the u32 project id space is used up.
	ErrProjectIDsExhausted = errors.New("db: project ids exhausted")
)

// Q is what queries need; *sql.DB, *sql.Tx and *sql.Conn all satisfy it.
type Q interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// DB is an open control database.
type DB struct {
	w    *sql.DB // single writer connection
	r    *sql.DB // read-only pool
	path string
}

func dsn(path string, readOnly bool) string {
	v := url.Values{}
	v.Add("_pragma", "busy_timeout(10000)")
	v.Add("_pragma", "foreign_keys(1)")
	if readOnly {
		v.Add("_pragma", "query_only(1)")
	} else {
		v.Add("_pragma", "journal_mode(WAL)")
		v.Add("_pragma", "synchronous(FULL)")
		// BEGIN IMMEDIATE: a write transaction takes the write lock up front
		// instead of failing with SQLITE_BUSY on its first write.
		v.Set("_txlock", "immediate")
	}
	return "file:" + (&url.URL{Path: path}).EscapedPath() + "?" + v.Encode()
}

// Open opens (creating if needed) the database at path and applies pending
// migrations.
func Open(ctx context.Context, path string) (*DB, error) {
	if path == "" {
		return nil, errors.New("db: empty database path")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("db: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o750); err != nil {
		return nil, fmt.Errorf("db: %w", err)
	}
	w, err := sql.Open("sqlite", dsn(abs, false))
	if err != nil {
		return nil, fmt.Errorf("db: %w", err)
	}
	w.SetMaxOpenConns(1)
	w.SetMaxIdleConns(1)
	w.SetConnMaxLifetime(0)
	d := &DB{w: w, path: abs}
	if err := d.migrate(ctx); err != nil {
		w.Close()
		return nil, err
	}
	// The database file holds password hashes and TOTP secrets.
	for _, suffix := range []string{"", "-wal", "-shm"} {
		_ = os.Chmod(abs+suffix, 0o600)
	}
	r, err := sql.Open("sqlite", dsn(abs, true))
	if err != nil {
		w.Close()
		return nil, fmt.Errorf("db: %w", err)
	}
	r.SetMaxOpenConns(4)
	r.SetMaxIdleConns(4)
	d.r = r
	if err := r.PingContext(ctx); err != nil {
		d.Close()
		return nil, fmt.Errorf("db: %w", err)
	}
	return d, nil
}

// Close closes both pools.
func (d *DB) Close() error {
	var err error
	if d.r != nil {
		err = d.r.Close()
	}
	if werr := d.w.Close(); err == nil {
		err = werr
	}
	return err
}

// Path returns the absolute path of the database file.
func (d *DB) Path() string { return d.path }

// R returns the read-only pool.
func (d *DB) R() *sql.DB { return d.r }

// Ping checks that the database answers.
func (d *DB) Ping(ctx context.Context) error {
	var one int
	return d.r.QueryRowContext(ctx, "SELECT 1").Scan(&one)
}

// Write runs fn in a write transaction. The transaction commits when fn
// returns nil and rolls back otherwise.
func (d *DB) Write(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := d.w.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("db: begin: %w", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("db: commit: %w", err)
	}
	return nil
}

// Read runs fn in a read transaction: everything fn reads comes from one
// consistent snapshot of the database.
func (d *DB) Read(ctx context.Context, fn func(q Q) error) error {
	tx, err := d.r.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("db: begin read: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	return fn(tx)
}

// Backup writes a consistent, compacted copy of the database to dest using
// VACUUM INTO (an online operation: readers and the writer keep working).
// dest must not exist.
func (d *DB) Backup(ctx context.Context, dest string) error {
	abs, err := filepath.Abs(dest)
	if err != nil {
		return fmt.Errorf("db: %w", err)
	}
	if _, err := os.Stat(abs); err == nil {
		return fmt.Errorf("db: backup target %s already exists", abs)
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o750); err != nil {
		return fmt.Errorf("db: %w", err)
	}
	// A dedicated connection: VACUUM cannot run inside a transaction and must
	// not queue behind the single writer for longer than necessary.
	conn, err := sql.Open("sqlite", dsn(d.path, false))
	if err != nil {
		return fmt.Errorf("db: %w", err)
	}
	defer conn.Close()
	conn.SetMaxOpenConns(1)
	if _, err := conn.ExecContext(ctx, "VACUUM INTO ?", abs); err != nil {
		_ = os.Remove(abs)
		return fmt.Errorf("db: backup: %w", err)
	}
	if err := os.Chmod(abs, 0o600); err != nil {
		return fmt.Errorf("db: %w", err)
	}
	return nil
}

func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") || strings.Contains(msg, "PRIMARY KEY constraint failed")
}

// Millis converts a time to unix milliseconds, the timestamp unit of the API.
func Millis(t time.Time) int64 { return t.UnixMilli() }
