package db

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"testing"
)

// oauthFixture creates a user and a registered client.
func oauthFixture(t *testing.T, d *DB) (userID int64, client OAuthClient) {
	t.Helper()
	ctx := context.Background()
	client = OAuthClient{ClientID: "oc_test", Kind: OAuthClientDCR, Name: "Assistant", RedirectURIs: []string{"https://a.example/cb"}, CreatedAt: 100, LastUsedAt: 100}
	err := d.Write(ctx, func(tx *sql.Tx) error {
		var err error
		if userID, err = CreateUser(ctx, tx, "u@example.com", "hash", 1); err != nil {
			return err
		}
		return SaveOAuthClient(ctx, tx, client)
	})
	if err != nil {
		t.Fatal(err)
	}
	return userID, client
}

func TestOAuthClients(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	_, client := oauthFixture(t, d)

	got, err := OAuthClientByID(ctx, d.R(), client.ClientID)
	if err != nil || !reflect.DeepEqual(got, client) {
		t.Fatalf("%+v, %v", got, err)
	}
	if _, err := OAuthClientByID(ctx, d.R(), "oc_missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing client: %v", err)
	}
	// Saving again refreshes the description and keeps the creation time.
	doc := OAuthClient{ClientID: "https://a.example/client.json", Kind: OAuthClientCIMD, Name: "Old", RedirectURIs: []string{"https://a.example/cb"}, CreatedAt: 200, FetchedAt: 200, FreshUntil: 500, LastUsedAt: 200}
	refreshed := doc
	refreshed.Name, refreshed.RedirectURIs, refreshed.CreatedAt, refreshed.FetchedAt, refreshed.FreshUntil = "New", []string{"https://a.example/cb2"}, 900, 900, 1200
	err = d.Write(ctx, func(tx *sql.Tx) error {
		if err := SaveOAuthClient(ctx, tx, doc); err != nil {
			return err
		}
		return SaveOAuthClient(ctx, tx, refreshed)
	})
	if err != nil {
		t.Fatal(err)
	}
	got, _ = OAuthClientByID(ctx, d.R(), doc.ClientID)
	if got.Name != "New" || got.CreatedAt != 200 || got.FreshUntil != 1200 || got.RedirectURIs[0] != "https://a.example/cb2" {
		t.Fatalf("after refresh: %+v", got)
	}
	// A metadata document cannot take over a registered client's row.
	hijack := client
	hijack.Kind, hijack.Name = OAuthClientCIMD, "Hijacked"
	if err := d.Write(ctx, func(tx *sql.Tx) error { return SaveOAuthClient(ctx, tx, hijack) }); err != nil {
		t.Fatal(err)
	}
	if got, _ = OAuthClientByID(ctx, d.R(), client.ClientID); got.Name != "Assistant" || got.Kind != OAuthClientDCR {
		t.Fatalf("row replaced across kinds: %+v", got)
	}
	if n, _ := CountOAuthClients(ctx, d.R(), OAuthClientDCR); n != 1 {
		t.Fatalf("dcr clients: %d", n)
	}
}

func TestOAuthCodeIsRedeemedOnce(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	userID, client := oauthFixture(t, d)
	code := OAuthCode{Hash: "h1", UserID: userID, ClientID: client.ClientID, RedirectURI: "https://a.example/cb", CodeChallenge: "c", Scopes: []string{"izlek:read"}, Resource: "https://izlek.example/mcp", CreatedAt: 10, ExpiresAt: 130}
	var grantID int64
	err := d.Write(ctx, func(tx *sql.Tx) error {
		if err := CreateOAuthCode(ctx, tx, code); err != nil {
			return err
		}
		var err error
		grantID, err = CreateOAuthGrant(ctx, tx, OAuthGrant{UserID: userID, ClientID: client.ClientID, Scopes: code.Scopes, Resource: code.Resource, CreatedAt: 20, ExpiresAt: 1000})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := OAuthCodeByHash(ctx, d.R(), "h1")
	if err != nil || !reflect.DeepEqual(got, code) {
		t.Fatalf("%+v, %v", got, err)
	}
	var first, second bool
	err = d.Write(ctx, func(tx *sql.Tx) error {
		var err error
		if first, err = RedeemOAuthCode(ctx, tx, "h1", grantID, 30); err != nil {
			return err
		}
		second, err = RedeemOAuthCode(ctx, tx, "h1", grantID, 31)
		return err
	})
	if err != nil || !first || second {
		t.Fatalf("redeem: first %v, second %v, err %v", first, second, err)
	}
	if got, _ = OAuthCodeByHash(ctx, d.R(), "h1"); got.UsedAt != 30 || got.GrantID != grantID {
		t.Fatalf("after redeem: %+v", got)
	}
	if _, err := OAuthCodeByHash(ctx, d.R(), "h2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing code: %v", err)
	}
}

func TestOAuthGrantsAndTokens(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	userID, client := oauthFixture(t, d)
	var g1, g2 int64
	err := d.Write(ctx, func(tx *sql.Tx) error {
		var err error
		if g1, err = CreateOAuthGrant(ctx, tx, OAuthGrant{UserID: userID, ClientID: client.ClientID, Scopes: []string{"izlek:read", "izlek:admin"}, Resource: "r", CreatedAt: 10, ExpiresAt: 1000}); err != nil {
			return err
		}
		if g2, err = CreateOAuthGrant(ctx, tx, OAuthGrant{UserID: userID, ClientID: client.ClientID, Scopes: []string{"izlek:read"}, Resource: "r", CreatedAt: 11, ExpiresAt: 1000}); err != nil {
			return err
		}
		for hash, spec := range map[string]struct {
			grant int64
			kind  string
		}{"a1": {g1, OAuthAccess}, "r1": {g1, OAuthRefresh}, "a2": {g2, OAuthAccess}, "r2": {g2, OAuthRefresh}} {
			if err := CreateOAuthToken(ctx, tx, hash, spec.grant, spec.kind, 10, 500); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := OAuthTokenByHash(ctx, d.R(), "a1", OAuthAccess)
	if err != nil || tok.Grant.ID != g1 || tok.Grant.ClientName != "Assistant" || tok.Grant.ClientKind != OAuthClientDCR || !tok.Grant.HasScope("izlek:admin") ||
		tok.ExpiresAt != 500 || !tok.Grant.Live(999) || tok.Grant.Live(1000) {
		t.Fatalf("%+v, %v", tok, err)
	}
	// The kind is part of the lookup: an access token is not a refresh token.
	if _, err := OAuthTokenByHash(ctx, d.R(), "a1", OAuthRefresh); !errors.Is(err, ErrNotFound) {
		t.Fatalf("access token found as refresh token: %v", err)
	}
	if _, err := OAuthTokenByHash(ctx, d.R(), "r1", OAuthAccess); !errors.Is(err, ErrNotFound) {
		t.Fatalf("refresh token found as access token: %v", err)
	}

	// Rotation happens once.
	var first, second bool
	err = d.Write(ctx, func(tx *sql.Tx) error {
		var err error
		if first, err = RotateOAuthRefresh(ctx, tx, "r1", 50); err != nil {
			return err
		}
		if second, err = RotateOAuthRefresh(ctx, tx, "r1", 51); err != nil {
			return err
		}
		// An access token cannot be "rotated".
		if ok, err := RotateOAuthRefresh(ctx, tx, "a1", 52); err != nil || ok {
			t.Errorf("access token rotated: %v %v", ok, err)
		}
		return TouchOAuthGrant(ctx, tx, g1, 60)
	})
	if err != nil || !first || second {
		t.Fatalf("rotate: %v %v %v", first, second, err)
	}
	if tok, _ = OAuthTokenByHash(ctx, d.R(), "r1", OAuthRefresh); tok.UsedAt != 50 || tok.Grant.LastUsedAt != 60 {
		t.Fatalf("after rotation: %+v", tok)
	}

	list, err := ListOAuthGrants(ctx, d.R(), userID, 100)
	if err != nil || len(list) != 2 || list[0].ID != g2 || list[1].ID != g1 {
		t.Fatalf("list: %+v, %v", list, err)
	}

	// Revoking a grant removes its tokens and nothing else.
	var revoked, again bool
	err = d.Write(ctx, func(tx *sql.Tx) error {
		var err error
		if revoked, err = RevokeOAuthGrant(ctx, tx, g1, 70, "user"); err != nil {
			return err
		}
		again, err = RevokeOAuthGrant(ctx, tx, g1, 71, "user")
		return err
	})
	if err != nil || !revoked || again {
		t.Fatalf("revoke: %v %v %v", revoked, again, err)
	}
	for _, hash := range []string{"a1", "r1"} {
		for _, kind := range []string{OAuthAccess, OAuthRefresh} {
			if _, err := OAuthTokenByHash(ctx, d.R(), hash, kind); !errors.Is(err, ErrNotFound) {
				t.Fatalf("token %s survives its grant", hash)
			}
		}
	}
	if _, err := OAuthTokenByHash(ctx, d.R(), "a2", OAuthAccess); err != nil {
		t.Fatalf("another grant's token is gone: %v", err)
	}
	g, _ := OAuthGrantByID(ctx, d.R(), g1)
	if g.RevokedAt != 70 || g.RevokeReason != "user" || g.Live(80) {
		t.Fatalf("revoked grant: %+v", g)
	}
	if list, _ = ListOAuthGrants(ctx, d.R(), userID, 100); len(list) != 1 || list[0].ID != g2 {
		t.Fatalf("list after revoke: %+v", list)
	}
	// Expired grants are not listed either.
	if list, _ = ListOAuthGrants(ctx, d.R(), userID, 1000); len(list) != 0 {
		t.Fatalf("expired grant listed: %+v", list)
	}

	// Ending everything of a user.
	var n int64
	if err := d.Write(ctx, func(tx *sql.Tx) error {
		var err error
		n, err = RevokeUserOAuthGrants(ctx, tx, userID, 90, "password_change")
		return err
	}); err != nil || n != 1 {
		t.Fatalf("revoke all: %d, %v", n, err)
	}
	var tokens int
	if err := d.R().QueryRowContext(ctx, `SELECT COUNT(*) FROM oauth_tokens`).Scan(&tokens); err != nil || tokens != 0 {
		t.Fatalf("%d tokens left, %v", tokens, err)
	}
}
