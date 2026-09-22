// Package auth hashes and verifies login passwords with argon2id, and mints the
// opaque session tokens that back a logged-in client.
//
// The encoded hash carries its own parameters (see Params.String), so raising
// the cost later is a code change plus a rehash-on-next-login, not a data
// migration: Verify always uses the parameters found in the stored string, not
// the constants compiled in today.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// ErrMalformedHash means the stored string is not an argon2id encoding this
// package can read. It is a corrupt-data signal, never "wrong password".
var ErrMalformedHash = errors.New("auth: malformed password hash")

// Params are the argon2id cost parameters. Defaults follow RFC 9106's second
// recommended option (64 MiB, t=3, p=2) — enough to make offline cracking
// expensive without making a login noticeably slow on a small VPS.
type Params struct {
	Memory  uint32 // KiB
	Time    uint32 // iterations
	Threads uint8  // parallelism
	SaltLen uint32 // bytes
	KeyLen  uint32 // bytes
}

// DefaultParams is what new hashes are written with.
var DefaultParams = Params{Memory: 64 * 1024, Time: 3, Threads: 2, SaltLen: 16, KeyLen: 32}

const hashPrefix = "$argon2id$"

// Hash derives an encoded argon2id hash of password using DefaultParams.
func Hash(password string) (string, error) { return HashWith(password, DefaultParams) }

// HashWith is Hash with explicit parameters (tests use cheap ones — the default
// 64 MiB per call turns a table-driven test into a memory hog).
func HashWith(password string, p Params) (string, error) {
	salt := make([]byte, p.SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: read salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, p.Time, p.Memory, p.Threads, p.KeyLen)
	return fmt.Sprintf("%sv=%d$m=%d,t=%d,p=%d$%s$%s",
		hashPrefix, argon2.Version, p.Memory, p.Time, p.Threads,
		b64(salt), b64(key),
	), nil
}

// Verify reports whether password matches the encoded hash. The comparison is
// constant-time, and the parameters come from encoded — so a hash written with
// older, cheaper settings still verifies after DefaultParams is raised.
func Verify(encoded, password string) (bool, error) {
	p, salt, want, err := decode(encoded)
	if err != nil {
		return false, err
	}
	got := argon2.IDKey([]byte(password), salt, p.Time, p.Memory, p.Threads, p.KeyLen)
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// dummyHash is verified against when the submitted email has no user, so that
// "no such account" costs the same time as "wrong password". Without it the
// login endpoint answers unknown emails measurably faster and becomes a user
// enumeration oracle. Generated once at init from a random password nobody
// holds; the comparison therefore always fails.
var dummyHash = mustDummyHash()

func mustDummyHash() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("auth: read random: " + err.Error())
	}
	h, err := Hash(string(b))
	if err != nil {
		panic("auth: dummy hash: " + err.Error())
	}
	return h
}

// VerifyDummy burns the same work Verify would, and always reports false. Call
// it on the no-such-user branch of a login handler.
func VerifyDummy(password string) bool {
	ok, _ := Verify(dummyHash, password)
	return ok
}

// decode parses "$argon2id$v=19$m=65536,t=3,p=2$<salt>$<hash>".
func decode(encoded string) (Params, []byte, []byte, error) {
	var p Params
	if !strings.HasPrefix(encoded, hashPrefix) {
		return p, nil, nil, ErrMalformedHash
	}
	// Leading "" from the first $, then: argon2id, v=…, m=…,t=…,p=…, salt, hash.
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 {
		return p, nil, nil, ErrMalformedHash
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return p, nil, nil, ErrMalformedHash
	}
	if version != argon2.Version {
		return p, nil, nil, fmt.Errorf("%w: unsupported version %d", ErrMalformedHash, version)
	}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.Memory, &p.Time, &p.Threads); err != nil {
		return p, nil, nil, ErrMalformedHash
	}
	salt, err := base64.RawStdEncoding.Strict().DecodeString(parts[4])
	if err != nil {
		return p, nil, nil, ErrMalformedHash
	}
	key, err := base64.RawStdEncoding.Strict().DecodeString(parts[5])
	if err != nil {
		return p, nil, nil, ErrMalformedHash
	}
	if p.Memory == 0 || p.Time == 0 || p.Threads == 0 || len(salt) == 0 || len(key) == 0 {
		return p, nil, nil, ErrMalformedHash
	}
	p.SaltLen, p.KeyLen = uint32(len(salt)), uint32(len(key))
	return p, salt, key, nil
}

func b64(b []byte) string { return base64.RawStdEncoding.EncodeToString(b) }

// NewSessionToken mints an opaque session token: 32 bytes from crypto/rand,
// base64url. It is shown to the client once and never stored — see TokenHash.
func NewSessionToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("auth: read token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// TokenHash is what the session table stores. A plain SHA-256 is right here
// (unlike for passwords): the token is 256 bits of uniform randomness, so there
// is no dictionary to slow an attacker down against.
func TokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return base64.RawStdEncoding.EncodeToString(sum[:])
}
