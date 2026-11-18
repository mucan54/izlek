// Package auth holds the control plane's credential primitives: argon2id
// password hashing, RFC 6238 TOTP, random identifiers and the login rate
// limiter. Nothing here touches HTTP or the database.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// Password policy (spec/control.md §1).
const (
	MinPasswordChars = 10
	MaxPasswordBytes = 1024
)

// ErrWeakPassword is returned for passwords outside the policy.
var ErrWeakPassword = errors.New("auth: password must be at least 10 characters")

// CheckPassword applies the password policy.
func CheckPassword(password string) error {
	if !utf8.ValidString(password) || utf8.RuneCountInString(password) < MinPasswordChars || len(password) > MaxPasswordBytes {
		return ErrWeakPassword
	}
	return nil
}

// Params are argon2id cost parameters.
type Params struct {
	MemoryKiB uint32
	Time      uint32
	Threads   uint8
}

// DefaultParams is the production cost: m = 64 MiB, t = 3, p = 1.
var DefaultParams = Params{MemoryKiB: 64 * 1024, Time: 3, Threads: 1}

const (
	saltLen = 16
	keyLen  = 32
)

// Hasher hashes and verifies passwords. Each hash needs Params.MemoryKiB of
// memory, so the number of concurrent computations is bounded; callers queue.
type Hasher struct {
	params Params
	sem    chan struct{}
	dummy  string
}

// NewHasher returns a hasher running at most concurrency computations at once.
func NewHasher(p Params, concurrency int) *Hasher {
	if p.MemoryKiB == 0 || p.Time == 0 || p.Threads == 0 {
		p = DefaultParams
	}
	if concurrency < 1 {
		concurrency = 1
	}
	h := &Hasher{params: p, sem: make(chan struct{}, concurrency)}
	// A well-formed hash of an unguessable password, verified when the
	// account does not exist so that both cases cost the same.
	salt := make([]byte, saltLen)
	key := make([]byte, keyLen)
	_, _ = rand.Read(salt)
	_, _ = rand.Read(key)
	h.dummy = encodeHash(p, salt, key)
	return h
}

func (h *Hasher) acquire(ctx context.Context) error {
	select {
	case h.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func encodeHash(p Params, salt, key []byte) string {
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, p.MemoryKiB, p.Time, p.Threads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key))
}

// Hash returns the PHC-format argon2id hash of password.
func (h *Hasher) Hash(ctx context.Context, password string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: %w", err)
	}
	if err := h.acquire(ctx); err != nil {
		return "", err
	}
	defer func() { <-h.sem }()
	key := argon2.IDKey([]byte(password), salt, h.params.Time, h.params.MemoryKiB, h.params.Threads, keyLen)
	return encodeHash(h.params, salt, key), nil
}

func decodeHash(encoded string) (Params, []byte, []byte, error) {
	var p Params
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return p, nil, nil, errors.New("auth: not an argon2id hash")
	}
	if parts[2] != "v="+strconv.Itoa(argon2.Version) {
		return p, nil, nil, errors.New("auth: unsupported argon2 version")
	}
	for _, kv := range strings.Split(parts[3], ",") {
		k, v, ok := strings.Cut(kv, "=")
		n, err := strconv.ParseUint(v, 10, 32)
		if !ok || err != nil {
			return p, nil, nil, errors.New("auth: malformed argon2 parameters")
		}
		switch k {
		case "m":
			p.MemoryKiB = uint32(n)
		case "t":
			p.Time = uint32(n)
		case "p":
			if n > 255 {
				return p, nil, nil, errors.New("auth: malformed argon2 parameters")
			}
			p.Threads = uint8(n)
		default:
			return p, nil, nil, errors.New("auth: malformed argon2 parameters")
		}
	}
	// Bounds keep a corrupted row from turning a login into a denial of service.
	if p.MemoryKiB < 8 || p.MemoryKiB > 1<<21 || p.Time < 1 || p.Time > 64 || p.Threads < 1 || p.Threads > 16 {
		return p, nil, nil, errors.New("auth: argon2 parameters out of range")
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) < 8 {
		return p, nil, nil, errors.New("auth: malformed argon2 salt")
	}
	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(key) < 16 || len(key) > 128 {
		return p, nil, nil, errors.New("auth: malformed argon2 hash")
	}
	return p, salt, key, nil
}

// Verify reports whether password matches encoded. rehash is true when the
// password is correct but was hashed with weaker parameters than the current
// ones; the caller should store a fresh hash.
func (h *Hasher) Verify(ctx context.Context, encoded, password string) (ok, rehash bool, err error) {
	p, salt, want, err := decodeHash(encoded)
	if err != nil {
		return false, false, err
	}
	if len(password) > MaxPasswordBytes {
		return false, false, nil
	}
	if err := h.acquire(ctx); err != nil {
		return false, false, err
	}
	got := argon2.IDKey([]byte(password), salt, p.Time, p.MemoryKiB, p.Threads, uint32(len(want)))
	<-h.sem
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return false, false, nil
	}
	weaker := p.MemoryKiB < h.params.MemoryKiB || p.Time < h.params.Time
	return true, weaker, nil
}

// VerifyDummy burns the time of one verification. Call it when there is no
// account to check against, so response time does not reveal which e-mail
// addresses exist.
func (h *Hasher) VerifyDummy(ctx context.Context, password string) {
	_, _, _ = h.Verify(ctx, h.dummy, password)
}
