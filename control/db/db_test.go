package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func openTest(t *testing.T) *DB {
	t.Helper()
	d, err := Open(context.Background(), filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func write(t *testing.T, d *DB, fn func(tx *sql.Tx) error) {
	t.Helper()
	if err := d.Write(context.Background(), fn); err != nil {
		t.Fatal(err)
	}
}

func TestOpenConfiguresSQLiteAndMigrates(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "control.db")
	d, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	var mode string
	if err := d.w.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal_mode = %q, %v", mode, err)
	}
	for name, pool := range map[string]*sql.DB{"writer": d.w, "reader": d.r} {
		var fk, busy int
		if err := pool.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fk); err != nil || fk != 1 {
			t.Fatalf("%s: foreign_keys = %d, %v", name, fk, err)
		}
		if err := pool.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&busy); err != nil || busy < 1000 {
			t.Fatalf("%s: busy_timeout = %d, %v", name, busy, err)
		}
	}
	if _, err := d.r.ExecContext(ctx, `INSERT INTO orgs(name, created_at) VALUES ('x', 1)`); err == nil {
		t.Fatal("the read pool accepted a write")
	}
	v, err := d.SchemaVersion(ctx)
	if err != nil || v != len(migrations) {
		t.Fatalf("schema version %d, %v; want %d", v, err, len(migrations))
	}
	for _, table := range []string{"orgs", "users", "memberships", "projects", "tokens", "keys", "flags", "flag_versions",
		"insights", "dashboards", "deletion_requests", "audit_log", "sessions", "publish_state"} {
		var n int
		if err := d.r.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&n); err != nil || n != 1 {
			t.Errorf("table %s missing (%v)", table, err)
		}
	}
	var hasOIDC int
	if err := d.r.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('users') WHERE name = 'oidc_subject'`).Scan(&hasOIDC); err != nil || hasOIDC != 1 {
		t.Errorf("users.oidc_subject missing (%v)", err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("database file mode: %v %v", fi.Mode(), err)
	}
	write(t, d, func(tx *sql.Tx) error {
		_, err := CreateOrg(ctx, tx, "Acme", 1)
		return err
	})
	d.Close()

	// Reopening applies nothing and keeps the data.
	d2, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()
	if name, err := OrgName(ctx, d2.R(), 1); err != nil || name != "Acme" {
		t.Fatalf("after reopen: %q, %v", name, err)
	}
	var applied int
	if err := d2.r.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations`).Scan(&applied); err != nil || applied != len(migrations) {
		t.Fatalf("migrations recorded: %d, %v", applied, err)
	}
	// A database from a newer binary is refused rather than misread.
	if _, err := d2.w.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at) VALUES (999, 0)`); err != nil {
		t.Fatal(err)
	}
	d2.Close()
	if _, err := Open(ctx, path); err == nil {
		t.Fatal("opened a database with a newer schema")
	}
}

func TestForeignKeysAreEnforced(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	err := d.Write(ctx, func(tx *sql.Tx) error {
		return AddMember(ctx, tx, 99, 98, RoleOwner, 1)
	})
	if err == nil {
		t.Fatal("membership of a missing org and user was inserted")
	}
}

func TestWriteRollsBackOnError(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	boom := errors.New("boom")
	err := d.Write(ctx, func(tx *sql.Tx) error {
		if _, err := CreateOrg(ctx, tx, "gone", 1); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if _, err := OrgName(ctx, d.R(), 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rolled-back org is visible: %v", err)
	}
}

func newProject(t *testing.T, d *DB, orgID int64, name string) Project {
	t.Helper()
	var p Project
	write(t, d, func(tx *sql.Tx) error {
		var err error
		p, err = CreateProject(context.Background(), tx, Project{OrgID: orgID, Name: name, RetentionDays: 365, ReplayRetentionDays: 30, MaxConcurrentQueries: 4}, 1)
		return err
	})
	return p
}

func TestProjectIDsStartAtOneAndAreNeverReused(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	var orgID int64
	write(t, d, func(tx *sql.Tx) error {
		var err error
		orgID, err = CreateOrg(ctx, tx, "Acme", 1)
		return err
	})
	a, b := newProject(t, d, orgID, "a"), newProject(t, d, orgID, "b")
	if a.ID != 1 || b.ID != 2 {
		t.Fatalf("ids %d, %d; want 1, 2", a.ID, b.ID)
	}
	// Even if rows vanish (nothing in the product deletes them), ids keep growing.
	if _, err := d.w.ExecContext(ctx, `DELETE FROM projects`); err != nil {
		t.Fatal(err)
	}
	if c := newProject(t, d, orgID, "c"); c.ID != 3 {
		t.Fatalf("id after delete: %d; want 3", c.ID)
	}
	// A failed transaction does not hand its id to the next one either way
	// round: ids stay strictly increasing.
	_ = d.Write(ctx, func(tx *sql.Tx) error {
		if _, err := CreateProject(ctx, tx, Project{OrgID: orgID, Name: "rolled back"}, 1); err != nil {
			return err
		}
		return errors.New("abort")
	})
	if e := newProject(t, d, orgID, "e"); e.ID < 4 {
		t.Fatalf("id after rollback: %d", e.ID)
	}
	// The u32 ceiling is an error, not a wrap-around.
	if _, err := d.w.ExecContext(ctx, `UPDATE sqlite_sequence SET seq = 4294967295 WHERE name = 'projects'`); err != nil {
		t.Fatal(err)
	}
	err := d.Write(ctx, func(tx *sql.Tx) error {
		_, err := CreateProject(ctx, tx, Project{OrgID: orgID, Name: "too many"}, 1)
		return err
	})
	if !errors.Is(err, ErrProjectIDsExhausted) {
		t.Fatalf("at the u32 ceiling: %v", err)
	}
}

func TestCloseProject(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	write(t, d, func(tx *sql.Tx) error {
		_, err := CreateOrg(ctx, tx, "Acme", 1)
		return err
	})
	p := newProject(t, d, 1, "shop")
	write(t, d, func(tx *sql.Tx) error {
		if err := CreateToken(ctx, tx, Token{Token: "pk_a", ProjectID: p.ID, Name: "a", Status: "active", CreatedAt: 1}); err != nil {
			return err
		}
		return CloseProject(ctx, tx, p.ID, 50)
	})
	got, err := ProjectByID(ctx, d.R(), p.ID)
	if err != nil || !got.Closed() || got.RetentionDays != 0 || got.ReplayRetentionDays != 0 {
		t.Fatalf("closed project: %+v, %v", got, err)
	}
	tok, err := TokenOf(ctx, d.R(), p.ID, "pk_a")
	if err != nil || tok.Status != "disabled" {
		t.Fatalf("token after close: %+v, %v", tok, err)
	}
	if open, _ := ListOrgProjects(ctx, d.R(), 1); len(open) != 0 {
		t.Fatalf("closed project still listed: %+v", open)
	}
}

func TestLastOwnerProtection(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	var org, alice, bob int64
	write(t, d, func(tx *sql.Tx) error {
		var err error
		if org, err = CreateOrg(ctx, tx, "Acme", 1); err != nil {
			return err
		}
		if alice, err = CreateUser(ctx, tx, "alice@example.com", "h", 1); err != nil {
			return err
		}
		if bob, err = CreateUser(ctx, tx, "bob@example.com", "h", 1); err != nil {
			return err
		}
		if err := AddMember(ctx, tx, org, alice, RoleOwner, 1); err != nil {
			return err
		}
		return AddMember(ctx, tx, org, bob, RoleAdmin, 1)
	})
	try := func(fn func(tx *sql.Tx) error) error { return d.Write(ctx, fn) }
	if err := try(func(tx *sql.Tx) error { return SetMemberRole(ctx, tx, org, alice, RoleAdmin) }); !errors.Is(err, ErrLastOwner) {
		t.Fatalf("demoting the only owner: %v", err)
	}
	if err := try(func(tx *sql.Tx) error { return RemoveMember(ctx, tx, org, alice) }); !errors.Is(err, ErrLastOwner) {
		t.Fatalf("removing the only owner: %v", err)
	}
	if err := try(func(tx *sql.Tx) error { return SetMemberRole(ctx, tx, org, alice, RoleOwner) }); err != nil {
		t.Fatalf("owner → owner: %v", err)
	}
	if err := try(func(tx *sql.Tx) error { return SetMemberRole(ctx, tx, org, bob, RoleOwner) }); err != nil {
		t.Fatal(err)
	}
	if err := try(func(tx *sql.Tx) error { return SetMemberRole(ctx, tx, org, alice, RoleViewer) }); err != nil {
		t.Fatalf("demoting one of two owners: %v", err)
	}
	if err := try(func(tx *sql.Tx) error { return RemoveMember(ctx, tx, org, bob) }); !errors.Is(err, ErrLastOwner) {
		t.Fatalf("removing the now-only owner: %v", err)
	}
	if err := try(func(tx *sql.Tx) error { return RemoveMember(ctx, tx, org, 12345) }); !errors.Is(err, ErrNotFound) {
		t.Fatalf("removing a stranger: %v", err)
	}
	if err := try(func(tx *sql.Tx) error { return AddMember(ctx, tx, org, bob, RoleViewer, 1) }); !errors.Is(err, ErrConflict) {
		t.Fatalf("adding a member twice: %v", err)
	}
	if _, err := CreateUserTx(d, "alice@example.com"); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate e-mail: %v", err)
	}
}

func CreateUserTx(d *DB, email string) (int64, error) {
	var id int64
	err := d.Write(context.Background(), func(tx *sql.Tx) error {
		var err error
		id, err = CreateUser(context.Background(), tx, email, "h", 1)
		return err
	})
	return id, err
}

func TestActorEncoding(t *testing.T) {
	cases := []struct {
		a       Actor
		storage string
		json    string
	}{
		{UserActor(7), "user:7", `7`},
		{KeyActor(3), "key:3", `"key:3"`},
		{SystemActor(), "system", `"system"`},
	}
	for _, c := range cases {
		if c.a.String() != c.storage {
			t.Errorf("String() = %q, want %q", c.a.String(), c.storage)
		}
		if ParseActor(c.storage) != c.a {
			t.Errorf("ParseActor(%q) = %+v", c.storage, ParseActor(c.storage))
		}
		raw, err := json.Marshal(c.a)
		if err != nil || string(raw) != c.json {
			t.Errorf("json = %s, %v; want %s", raw, err, c.json)
		}
		var back Actor
		if err := json.Unmarshal(raw, &back); err != nil || back != c.a {
			t.Errorf("round trip of %s: %+v, %v", raw, back, err)
		}
	}
	if ParseActor("garbage") != SystemActor() || ParseActor("user:x") != SystemActor() {
		t.Error("unparseable actors must fall back to the system")
	}
}

func TestFlagVersionsContinueAfterDelete(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	write(t, d, func(tx *sql.Tx) error {
		_, err := CreateOrg(ctx, tx, "Acme", 1)
		return err
	})
	p := newProject(t, d, 1, "shop")
	mk := func(salt string) Flag {
		return Flag{Key: "f", Salt: salt, Rules: json.RawMessage(`[]`), Default: json.RawMessage(`null`), UpdatedAt: 5, UpdatedBy: UserActor(1)}
	}
	var f Flag
	write(t, d, func(tx *sql.Tx) error {
		var err error
		if f, err = CreateFlag(ctx, tx, p.ID, mk("s1")); err != nil {
			return err
		}
		if _, err := CreateFlag(ctx, tx, p.ID, mk("zz")); !errors.Is(err, ErrConflict) {
			t.Errorf("duplicate key: %v", err)
		}
		upd := mk("ignored")
		upd.Enabled = true
		f, err = UpdateFlag(ctx, tx, p.ID, upd)
		return err
	})
	if f.Version != 2 || f.Salt != "s1" || !f.Enabled {
		t.Fatalf("after update: %+v (the salt must not change)", f)
	}
	write(t, d, func(tx *sql.Tx) error {
		if err := DeleteFlag(ctx, tx, p.ID, "f"); err != nil {
			return err
		}
		if err := DeleteFlag(ctx, tx, p.ID, "f"); !errors.Is(err, ErrNotFound) {
			t.Errorf("second delete: %v", err)
		}
		var err error
		f, err = CreateFlag(ctx, tx, p.ID, mk("s2"))
		return err
	})
	if f.Version != 3 || f.Salt != "s2" {
		t.Fatalf("recreated flag: %+v; want version 3 with its own salt", f)
	}
	vs, err := ListFlagVersions(ctx, d.R(), p.ID, "f")
	if err != nil || len(vs) != 3 || vs[0].Version != 3 || vs[2].Version != 1 {
		t.Fatalf("history: %+v, %v", vs, err)
	}
}

func TestDeletionTimestampsStrictlyIncreasePerProject(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	write(t, d, func(tx *sql.Tx) error {
		_, err := CreateOrg(ctx, tx, "Acme", 1)
		return err
	})
	p1, p2 := newProject(t, d, 1, "a"), newProject(t, d, 1, "b")
	var got []int64
	write(t, d, func(tx *sql.Tx) error {
		// Same millisecond three times, then the clock steps back.
		for i, now := range []int64{1000, 1000, 1000, 900} {
			del, err := CreateDeletion(ctx, tx, Deletion{ID: "del_" + string(rune('a'+i)), ProjectID: p1.ID, UserIDs: []string{"u"}, CreatedBy: SystemActor()}, now)
			if err != nil {
				return err
			}
			got = append(got, del.TSMillis)
		}
		other, err := CreateDeletion(ctx, tx, Deletion{ID: "del_other", ProjectID: p2.ID, AnonIDs: []string{"x"}, CreatedBy: SystemActor()}, 1000)
		if err != nil {
			return err
		}
		if other.TSMillis != 1000 {
			t.Errorf("another project's timestamps are independent; got %d", other.TSMillis)
		}
		return nil
	})
	want := []int64{1000, 1001, 1002, 1003}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ts_ms = %v, want %v", got, want)
		}
	}
	list, err := ListDeletions(ctx, d.R(), p1.ID)
	if err != nil || len(list) != 4 || list[0].TSMillis != 1003 || list[0].Status != "pending" || list[0].AnonIDs == nil {
		t.Fatalf("list: %+v, %v", list, err)
	}
}

func TestBackupIsAConsistentCopy(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	write(t, d, func(tx *sql.Tx) error {
		if _, err := CreateOrg(ctx, tx, "Acme", 1); err != nil {
			return err
		}
		_, err := CreateUser(ctx, tx, "a@example.com", "hash", 1)
		return err
	})
	dest := filepath.Join(t.TempDir(), "nested", "backup.db")
	if err := d.Backup(ctx, dest); err != nil {
		t.Fatal(err)
	}
	if err := d.Backup(ctx, dest); err == nil {
		t.Fatal("Backup overwrote an existing file")
	}
	if fi, err := os.Stat(dest); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("backup mode: %v, %v", fi, err)
	}
	// Changes after the backup are not in it.
	write(t, d, func(tx *sql.Tx) error {
		_, err := CreateOrg(ctx, tx, "Later", 2)
		return err
	})
	b, err := Open(ctx, dest)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if name, err := OrgName(ctx, b.R(), 1); err != nil || name != "Acme" {
		t.Fatalf("backup content: %q, %v", name, err)
	}
	if _, err := OrgName(ctx, b.R(), 2); !errors.Is(err, ErrNotFound) {
		t.Fatalf("backup contains a later write: %v", err)
	}
	if u, err := UserByEmail(ctx, b.R(), "a@example.com"); err != nil || u.PasswordHash != "hash" {
		t.Fatalf("backup user: %+v, %v", u, err)
	}
}

// Readers and the writer work at the same time (WAL): no SQLITE_BUSY, no
// lost updates.
func TestConcurrentReadsAndWrites(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	const writers, perWriter = 8, 25
	var wg sync.WaitGroup
	errs := make(chan error, writers*2)
	for w := 0; w < writers; w++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				if err := d.Write(ctx, func(tx *sql.Tx) error {
					_, err := CreateOrg(ctx, tx, "org", 1)
					return err
				}); err != nil {
					errs <- err
					return
				}
			}
		}()
		go func() {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				if err := d.Read(ctx, func(q Q) error {
					var n int
					return q.QueryRowContext(ctx, `SELECT COUNT(*) FROM orgs`).Scan(&n)
				}); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	var n int
	if err := d.R().QueryRowContext(ctx, `SELECT COUNT(*) FROM orgs`).Scan(&n); err != nil || n != writers*perWriter {
		t.Fatalf("orgs = %d, %v; want %d", n, err, writers*perWriter)
	}
}
