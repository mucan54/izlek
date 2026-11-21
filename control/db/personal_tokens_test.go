package db

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

func TestPersonalTokens(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	var alice, bob, forever, short, bobs int64
	write(t, d, func(tx *sql.Tx) error {
		var err error
		if alice, err = CreateUser(ctx, tx, "a@example.com", "h", 1); err != nil {
			return err
		}
		if bob, err = CreateUser(ctx, tx, "b@example.com", "h", 1); err != nil {
			return err
		}
		if forever, err = CreatePersonalToken(ctx, tx, PersonalToken{UserID: alice, Name: "forever", Scopes: []string{"izlek:read"}, Hash: "h1", Prefix: "pat_…aaaa", CreatedAt: 10}); err != nil {
			return err
		}
		if short, err = CreatePersonalToken(ctx, tx, PersonalToken{UserID: alice, Name: "short", Scopes: []string{"izlek:read", "izlek:admin"}, Hash: "h2", Prefix: "pat_…bbbb", CreatedAt: 11, ExpiresAt: 100}); err != nil {
			return err
		}
		bobs, err = CreatePersonalToken(ctx, tx, PersonalToken{UserID: bob, Name: "bob", Scopes: []string{"izlek:read"}, Hash: "h3", Prefix: "pat_…cccc", CreatedAt: 12})
		return err
	})
	// The same secret cannot be stored twice.
	if err := d.Write(ctx, func(tx *sql.Tx) error {
		_, err := CreatePersonalToken(ctx, tx, PersonalToken{UserID: bob, Name: "dup", Scopes: []string{}, Hash: "h1", CreatedAt: 1})
		return err
	}); err == nil {
		t.Fatal("duplicate hash accepted")
	}

	got, err := PersonalTokenByHash(ctx, d.R(), "h2")
	if err != nil || got.ID != short || got.UserID != alice || got.Name != "short" || len(got.Scopes) != 2 || got.ExpiresAt != 100 || got.LastUsedAt != 0 || !got.Live(99) || got.Live(100) {
		t.Fatalf("%+v, %v", got, err)
	}
	if f, _ := PersonalTokenByHash(ctx, d.R(), "h1"); !f.Live(1<<60) || f.ExpiresAt != 0 {
		t.Fatalf("a token without expiry expires: %+v", f)
	}
	if _, err := PersonalTokenByHash(ctx, d.R(), "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown hash: %v", err)
	}

	// Newest first; expired tokens are not listed.
	list, err := ListPersonalTokens(ctx, d.R(), alice, 50)
	if err != nil || len(list) != 2 || list[0].ID != short || list[1].ID != forever {
		t.Fatalf("%+v, %v", list, err)
	}
	if list, _ = ListPersonalTokens(ctx, d.R(), alice, 100); len(list) != 1 || list[0].ID != forever {
		t.Fatalf("at expiry: %+v", list)
	}

	write(t, d, func(tx *sql.Tx) error { return TouchPersonalToken(ctx, tx, forever, 77) })
	if f, _ := PersonalTokenByHash(ctx, d.R(), "h1"); f.LastUsedAt != 77 {
		t.Fatalf("last used: %+v", f)
	}

	// A token is deleted by its owner only.
	write(t, d, func(tx *sql.Tx) error {
		if _, err := DeletePersonalToken(ctx, tx, alice, bobs); !errors.Is(err, ErrNotFound) {
			t.Errorf("deleting another user's token: %v", err)
		}
		gone, err := DeletePersonalToken(ctx, tx, bob, bobs)
		if err != nil || gone.Name != "bob" {
			t.Errorf("delete: %+v, %v", gone, err)
		}
		if _, err := DeletePersonalToken(ctx, tx, bob, bobs); !errors.Is(err, ErrNotFound) {
			t.Errorf("deleting twice: %v", err)
		}
		return nil
	})

	// Pruning removes what has expired and nothing else.
	write(t, d, func(tx *sql.Tx) error {
		if n, err := PrunePersonalTokens(ctx, tx, 99); err != nil || n != 0 {
			t.Errorf("prune before expiry: %d, %v", n, err)
		}
		if n, err := PrunePersonalTokens(ctx, tx, 100); err != nil || n != 1 {
			t.Errorf("prune at expiry: %d, %v", n, err)
		}
		return nil
	})
	write(t, d, func(tx *sql.Tx) error {
		if n, err := DeleteUserPersonalTokens(ctx, tx, alice); err != nil || n != 1 {
			t.Errorf("delete all: %d, %v", n, err)
		}
		return nil
	})
	var left int
	if err := d.R().QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_tokens`).Scan(&left); err != nil || left != 0 {
		t.Fatalf("%d rows left, %v", left, err)
	}
}
