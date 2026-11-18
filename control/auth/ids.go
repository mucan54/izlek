package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"time"
)

const base62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// RandomBase62 returns n uniformly random base62 characters (5.95 bits each).
func RandomBase62(n int) string {
	out := make([]byte, 0, n)
	buf := make([]byte, n+n/2+8)
	for len(out) < n {
		if _, err := rand.Read(buf); err != nil {
			panic("auth: system randomness unavailable: " + err.Error())
		}
		for _, b := range buf {
			// Rejection sampling: 248 = 4 * 62, so b % 62 is unbiased below it.
			if b < 248 {
				out = append(out, base62[b%62])
				if len(out) == n {
					break
				}
			}
		}
	}
	return string(out)
}

// Token and key formats (spec/control.md §4): prefix + 32 base62 characters
// (≈ 190 bits).
const (
	TokenPrefix  = "pk_"
	SecretPrefix = "sk_"
	idChars      = 32
)

// NewWriteToken returns a fresh pk_ token.
func NewWriteToken() string { return TokenPrefix + RandomBase62(idChars) }

// NewSecretKey returns a fresh sk_ secret.
func NewSecretKey() string { return SecretPrefix + RandomBase62(idChars) }

// LooksLikeSecretKey reports whether s has the shape of an sk_ secret.
func LooksLikeSecretKey(s string) bool { return shaped(s, SecretPrefix) }

// LooksLikeWriteToken reports whether s has the shape of a pk_ token.
func LooksLikeWriteToken(s string) bool { return shaped(s, TokenPrefix) }

func shaped(s, prefix string) bool {
	if len(s) != len(prefix)+idChars || s[:len(prefix)] != prefix {
		return false
	}
	for i := len(prefix); i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') {
			return false
		}
	}
	return true
}

// SecretPrefixOf is the display form of a secret key: "sk_…" + last four
// characters.
func SecretPrefixOf(secret string) string {
	if len(secret) < 4 {
		return SecretPrefix + "…"
	}
	return SecretPrefix + "…" + secret[len(secret)-4:]
}

// HashSecret is the hex SHA-256 under which secrets (session cookies, sk_
// keys) are stored and published.
func HashSecret(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// NewSessionID returns a 256-bit random cookie value and its stored hash.
func NewSessionID() (cookie, hash string) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		panic("auth: system randomness unavailable: " + err.Error())
	}
	cookie = hex.EncodeToString(raw)
	return cookie, HashSecret(cookie)
}

// NewCSRFToken returns a 128-bit random token bound to a session.
func NewCSRFToken() string {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		panic("auth: system randomness unavailable: " + err.Error())
	}
	return hex.EncodeToString(raw)
}

const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// NewULID returns a 26-character ULID: 48 bits of millisecond time followed
// by 80 random bits, Crockford base32. Ids sort by creation time.
func NewULID(now time.Time) string {
	var raw [16]byte
	ms := uint64(now.UnixMilli())
	binary.BigEndian.PutUint64(raw[:8], ms<<16)
	if _, err := rand.Read(raw[6:]); err != nil {
		panic("auth: system randomness unavailable: " + err.Error())
	}
	// 128 bits → 26 characters of 5 bits (the first carries only 3).
	out := make([]byte, 26)
	var acc uint
	var bits uint
	pos := 25
	for i := 15; i >= 0; i-- {
		acc |= uint(raw[i]) << bits
		bits += 8
		for bits >= 5 {
			out[pos] = crockford[acc&31]
			pos--
			acc >>= 5
			bits -= 5
		}
	}
	out[0] = crockford[acc&31]
	return string(out)
}
