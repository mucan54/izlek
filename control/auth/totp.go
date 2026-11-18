package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // RFC 6238 mandates HMAC-SHA-1 for authenticator apps
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// TOTP parameters (spec/control.md §1): RFC 6238, SHA-1, 30 s, 6 digits.
const (
	TOTPPeriod = 30
	TOTPDigits = 6
	// totpSkew is how many periods before/after the current one are accepted.
	totpSkew = 1
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewTOTPSecret returns a fresh 160-bit secret in base32 (no padding).
func NewTOTPSecret() (string, error) {
	raw := make([]byte, 20)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("auth: %w", err)
	}
	return b32.EncodeToString(raw), nil
}

func decodeTOTPSecret(secret string) ([]byte, error) {
	s := strings.ToUpper(strings.TrimRight(strings.ReplaceAll(secret, " ", ""), "="))
	key, err := b32.DecodeString(s)
	if err != nil || len(key) == 0 {
		return nil, fmt.Errorf("auth: invalid TOTP secret")
	}
	return key, nil
}

// hotp is RFC 4226 with the given number of digits (1–9).
func hotp(key []byte, counter uint64, digits int) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], counter)
	mac := hmac.New(sha1.New, key)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	code := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	mod := uint32(1)
	for i := 0; i < digits; i++ {
		mod *= 10
	}
	return fmt.Sprintf("%0*d", digits, code%mod)
}

// TOTPStep returns the time step of t.
func TOTPStep(t time.Time) int64 { return t.Unix() / TOTPPeriod }

// TOTPCode returns the 6-digit code of secret for a time step.
func TOTPCode(secret string, step int64) (string, error) {
	key, err := decodeTOTPSecret(secret)
	if err != nil {
		return "", err
	}
	return hotp(key, uint64(step), TOTPDigits), nil
}

// VerifyTOTP checks code against secret at time now, accepting one period of
// clock skew either way. Steps at or before lastStep are rejected so a code
// cannot be replayed. It returns the matched step.
func VerifyTOTP(secret, code string, now time.Time, lastStep int64) (int64, bool) {
	key, err := decodeTOTPSecret(secret)
	if err != nil {
		return 0, false
	}
	code = strings.ReplaceAll(strings.TrimSpace(code), " ", "")
	if len(code) != TOTPDigits {
		return 0, false
	}
	cur := TOTPStep(now)
	matched, found := int64(0), false
	for step := cur - totpSkew; step <= cur+totpSkew; step++ {
		if step < 0 {
			continue
		}
		// No early exit: every candidate is computed and compared.
		if subtle.ConstantTimeCompare([]byte(hotp(key, uint64(step), TOTPDigits)), []byte(code)) == 1 && step > lastStep && !found {
			matched, found = step, true
		}
	}
	return matched, found
}

// OTPAuthURL builds the provisioning URI authenticator apps scan.
func OTPAuthURL(issuer, account, secret string) string {
	v := url.Values{}
	v.Set("secret", secret)
	v.Set("issuer", issuer)
	v.Set("algorithm", "SHA1")
	v.Set("digits", fmt.Sprint(TOTPDigits))
	v.Set("period", fmt.Sprint(TOTPPeriod))
	return "otpauth://totp/" + url.PathEscape(issuer+":"+account) + "?" + v.Encode()
}
