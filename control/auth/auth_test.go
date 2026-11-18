package auth

import (
	"context"
	"encoding/base32"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

var fast = Params{MemoryKiB: 64, Time: 1, Threads: 1}

func TestPasswordPolicy(t *testing.T) {
	for _, ok := range []string{"0123456789", "çok gizli şifre", strings.Repeat("x", MaxPasswordBytes)} {
		if err := CheckPassword(ok); err != nil {
			t.Errorf("CheckPassword(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", "short", "123456789", strings.Repeat("x", MaxPasswordBytes+1), "abcdefghi\xff"} {
		if err := CheckPassword(bad); err == nil {
			t.Errorf("CheckPassword(%d bytes) accepted", len(bad))
		}
	}
	// Ten characters, not ten bytes.
	if err := CheckPassword("şşşşş"); err == nil {
		t.Error("five two-byte characters accepted as ten")
	}
}

func TestHashAndVerify(t *testing.T) {
	ctx := context.Background()
	h := NewHasher(fast, 2)
	enc, err := h.Hash(ctx, "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(enc, "$argon2id$v=19$m=64,t=1,p=1$") {
		t.Fatalf("unexpected encoding %q", enc)
	}
	enc2, _ := h.Hash(ctx, "correct horse battery")
	if enc == enc2 {
		t.Fatal("two hashes of one password are identical (no salt?)")
	}
	ok, rehash, err := h.Verify(ctx, enc, "correct horse battery")
	if err != nil || !ok || rehash {
		t.Fatalf("Verify = %v, %v, %v", ok, rehash, err)
	}
	ok, _, err = h.Verify(ctx, enc, "correct horse batterx")
	if err != nil || ok {
		t.Fatalf("wrong password: %v, %v", ok, err)
	}
	// A stronger hasher asks for a rehash of the weaker hash.
	strong := NewHasher(Params{MemoryKiB: 128, Time: 2, Threads: 1}, 1)
	ok, rehash, err = strong.Verify(ctx, enc, "correct horse battery")
	if err != nil || !ok || !rehash {
		t.Fatalf("stronger hasher: %v, %v, %v", ok, rehash, err)
	}
	for _, bad := range []string{"", "plain", "$argon2i$v=19$m=64,t=1,p=1$c2FsdHNhbHQ$aGFzaGhhc2hoYXNoaGFzaA",
		"$argon2id$v=18$m=64,t=1,p=1$c2FsdHNhbHQ$aGFzaGhhc2hoYXNoaGFzaA", "$argon2id$v=19$m=4294967295,t=1,p=1$c2FsdHNhbHQ$aGFzaGhhc2hoYXNoaGFzaA",
		"$argon2id$v=19$m=64,t=1$c2FsdHNhbHQ$aGFzaGhhc2hoYXNoaGFzaA", "$argon2id$v=19$m=64,t=1,p=1$!!$aGFzaGhhc2hoYXNoaGFzaA"} {
		if ok, _, err := h.Verify(ctx, bad, "x"); ok || err == nil {
			t.Errorf("Verify(%q) = %v, %v; want an error", bad, ok, err)
		}
	}
	h.VerifyDummy(ctx, "anything") // must not panic or block
}

func TestDefaultParamsMeetTheSpec(t *testing.T) {
	// spec/control.md §1: argon2id, m = 64 MiB, t = 3, p = 1 or stronger.
	if DefaultParams.MemoryKiB < 64*1024 || DefaultParams.Time < 3 || DefaultParams.Threads < 1 {
		t.Fatalf("DefaultParams too weak: %+v", DefaultParams)
	}
}

func TestHasherBoundsConcurrency(t *testing.T) {
	h := NewHasher(fast, 1)
	h.sem <- struct{}{} // occupy the only slot
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := h.Hash(ctx, "0123456789"); err == nil {
		t.Fatal("Hash did not wait for a free slot")
	}
	<-h.sem
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := h.Hash(context.Background(), "0123456789"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
}

// RFC 6238 appendix B, SHA-1 rows. The RFC lists 8-digit codes; the 6-digit
// codes used here are their last six digits (same truncation, smaller modulus).
func TestTOTPRFC6238Vectors(t *testing.T) {
	key := []byte("12345678901234567890")
	secret := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(key)
	vectors := []struct {
		unix  int64
		eight string
	}{
		{59, "94287082"}, {1111111109, "07081804"}, {1111111111, "14050471"},
		{1234567890, "89005924"}, {2000000000, "69279037"}, {20000000000, "65353130"},
	}
	for _, v := range vectors {
		step := v.unix / TOTPPeriod
		if got := hotp(key, uint64(step), 8); got != v.eight {
			t.Errorf("T=%d: 8-digit code %s, want %s", v.unix, got, v.eight)
		}
		six, err := TOTPCode(secret, step)
		if err != nil {
			t.Fatal(err)
		}
		if six != v.eight[2:] {
			t.Errorf("T=%d: 6-digit code %s, want %s", v.unix, six, v.eight[2:])
		}
		now := time.Unix(v.unix, 0)
		if got, ok := VerifyTOTP(secret, six, now, 0); !ok || got != step {
			t.Errorf("T=%d: VerifyTOTP = %d, %v", v.unix, got, ok)
		}
		// One period of skew either way is accepted, two are not.
		if _, ok := VerifyTOTP(secret, six, now.Add(TOTPPeriod*time.Second), 0); !ok {
			t.Errorf("T=%d: code of the previous period rejected", v.unix)
		}
		if _, ok := VerifyTOTP(secret, six, now.Add(-TOTPPeriod*time.Second), 0); !ok {
			t.Errorf("T=%d: code of the next period rejected", v.unix)
		}
		if _, ok := VerifyTOTP(secret, six, now.Add(2*TOTPPeriod*time.Second), 0); ok {
			t.Errorf("T=%d: code two periods old accepted", v.unix)
		}
		// Replay: a step that was already used is refused.
		if _, ok := VerifyTOTP(secret, six, now, step); ok {
			t.Errorf("T=%d: replayed code accepted", v.unix)
		}
	}
}

func TestTOTPInputs(t *testing.T) {
	secret, err := NewTOTPSecret()
	if err != nil {
		t.Fatal(err)
	}
	if len(secret) != 32 {
		t.Fatalf("secret %q: want 32 base32 characters (160 bits)", secret)
	}
	now := time.Unix(1_700_000_000, 0)
	code, _ := TOTPCode(secret, TOTPStep(now))
	if _, ok := VerifyTOTP(secret, " "+code[:3]+" "+code[3:]+" ", now, 0); !ok {
		t.Error("code with spaces rejected")
	}
	if _, ok := VerifyTOTP(strings.ToLower(secret), code, now, 0); !ok {
		t.Error("lower-case secret rejected")
	}
	for _, bad := range []string{"", "12345", "1234567", "abcdef", code + "0"} {
		if bad == code {
			continue
		}
		if _, ok := VerifyTOTP(secret, bad, now, 0); ok {
			t.Errorf("code %q accepted", bad)
		}
	}
	if _, ok := VerifyTOTP("not base32 !!", code, now, 0); ok {
		t.Error("invalid secret accepted")
	}
	u, err := url.Parse(OTPAuthURL("İzlek", "a@example.com", secret))
	if err != nil || u.Scheme != "otpauth" || u.Host != "totp" {
		t.Fatalf("otpauth url: %v %v", u, err)
	}
	q := u.Query()
	if q.Get("secret") != secret || q.Get("algorithm") != "SHA1" || q.Get("digits") != "6" || q.Get("period") != "30" || q.Get("issuer") != "İzlek" {
		t.Errorf("otpauth parameters: %v", q)
	}
	if u.Path != "/İzlek:a@example.com" {
		t.Errorf("otpauth label %q", u.Path)
	}
}

func TestIDs(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		pk, sk := NewWriteToken(), NewSecretKey()
		if !LooksLikeWriteToken(pk) || len(pk) != 35 || !LooksLikeSecretKey(sk) || len(sk) != 35 {
			t.Fatalf("bad shapes: %q %q", pk, sk)
		}
		if seen[pk] || seen[sk] {
			t.Fatal("duplicate id")
		}
		seen[pk], seen[sk] = true, true
	}
	for _, bad := range []string{"", "sk_", "pk_" + strings.Repeat("a", 32), "sk_" + strings.Repeat("a", 31), "sk_" + strings.Repeat("a", 31) + "-", "sk_" + strings.Repeat("a", 33)} {
		if LooksLikeSecretKey(bad) {
			t.Errorf("LooksLikeSecretKey(%q)", bad)
		}
	}
	// Every base62 character shows up (no off-by-one in the alphabet).
	chars := map[rune]bool{}
	for _, r := range RandomBase62(20000) {
		chars[r] = true
	}
	if len(chars) != 62 {
		t.Errorf("RandomBase62 used %d distinct characters", len(chars))
	}
	if got := SecretPrefixOf("sk_0123456789abcd"); got != "sk_…abcd" {
		t.Errorf("SecretPrefixOf = %q", got)
	}
	if HashSecret("abc") != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Error("HashSecret is not hex SHA-256")
	}
	cookie, hash := NewSessionID()
	if len(cookie) != 64 || HashSecret(cookie) != hash {
		t.Errorf("session id: %q %q", cookie, hash)
	}
	if len(NewCSRFToken()) != 32 {
		t.Error("csrf token length")
	}
}

func TestULID(t *testing.T) {
	t0 := time.UnixMilli(1_795_046_400_000)
	a, b := NewULID(t0), NewULID(t0.Add(time.Millisecond))
	if len(a) != 26 || len(b) != 26 {
		t.Fatalf("lengths %d %d", len(a), len(b))
	}
	for _, r := range a + b {
		if !strings.ContainsRune(crockford, r) {
			t.Fatalf("character %q outside Crockford base32", r)
		}
	}
	if a[:10] >= b[:10] {
		t.Errorf("time prefix does not sort: %s !< %s", a[:10], b[:10])
	}
	if NewULID(t0)[:10] != a[:10] {
		t.Error("same millisecond, different time prefix")
	}
	if NewULID(t0) == a {
		t.Error("no randomness")
	}
	// 48-bit timestamp → the first character is at most '7'.
	if a[0] > '7' {
		t.Errorf("first character %q", a[0])
	}
}

func TestLimiter(t *testing.T) {
	now := time.Unix(1000, 0)
	l := NewLimiter(5, time.Minute, func() time.Time { return now })
	for i := 0; i < 5; i++ {
		if blocked, _ := l.Blocked("k"); blocked {
			t.Fatalf("blocked after %d events", i)
		}
		l.Add("k")
		now = now.Add(time.Second)
	}
	blocked, wait := l.Blocked("k")
	if !blocked || wait != 55*time.Second {
		t.Fatalf("after 5 events: blocked=%v wait=%v, want true 55s", blocked, wait)
	}
	if blocked, _ := l.Blocked("other"); blocked {
		t.Fatal("limit leaked to another key")
	}
	now = now.Add(55 * time.Second) // the first event leaves the window
	if blocked, _ := l.Blocked("k"); blocked {
		t.Fatal("still blocked after the window slid")
	}
	l.Add("k")
	if blocked, _ := l.Blocked("k"); !blocked {
		t.Fatal("not blocked again at 5 events in the window")
	}
	now = now.Add(2 * time.Minute)
	if blocked, _ := l.Blocked("k"); blocked {
		t.Fatal("blocked after everything expired")
	}
	if l.Len() != 0 {
		t.Fatalf("expired key still tracked (%d)", l.Len())
	}
	// Bounded memory.
	l.maxKeys = 10
	for i := 0; i < 50; i++ {
		l.Add(strings.Repeat("x", i+1))
	}
	if l.Len() > 10 {
		t.Fatalf("limiter grew to %d keys", l.Len())
	}
}
