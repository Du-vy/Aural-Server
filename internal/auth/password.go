// Package auth holds the credential primitives: Argon2id password hashing and
// the opaque session tokens that let a client resume an identity.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"runtime"
	"strings"

	"golang.org/x/crypto/argon2"
)

// argon2Params are the Argon2id cost parameters. They target roughly 50ms on a
// modest machine, which is a reasonable ceiling for a self-hosted server that
// may be running on a home box or a small VPS.
type argon2Params struct {
	memoryKiB   uint32
	iterations  uint32
	parallelism uint8
	saltLength  uint32
	keyLength   uint32
}

func defaultParams() argon2Params {
	parallelism := runtime.NumCPU()
	if parallelism > 4 {
		parallelism = 4
	}
	if parallelism < 1 {
		parallelism = 1
	}
	return argon2Params{
		memoryKiB:   64 * 1024,
		iterations:  2,
		parallelism: uint8(parallelism),
		saltLength:  16,
		keyLength:   32,
	}
}

// ErrInvalidHash is returned when a stored hash cannot be parsed, which means
// the row was written by a different or corrupted implementation.
var ErrInvalidHash = errors.New("auth: unrecognised password hash format")

// HashPassword derives an Argon2id hash and encodes it, salt and parameters
// included, in the standard PHC string format.
func HashPassword(plain string) (string, error) {
	p := defaultParams()

	salt := make([]byte, p.saltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: read salt: %w", err)
	}

	key := argon2.IDKey([]byte(plain), salt, p.iterations, p.memoryKiB, p.parallelism, p.keyLength)

	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.memoryKiB, p.iterations, p.parallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// VerifyPassword reports whether plain produces encoded. It re-derives with the
// parameters recorded in encoded, so hashes written under older settings keep
// verifying after the defaults change.
func VerifyPassword(encoded, plain string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return false, ErrInvalidHash
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return false, ErrInvalidHash
	}
	if version != argon2.Version {
		return false, fmt.Errorf("%w: version %d", ErrInvalidHash, version)
	}

	var p argon2Params
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.memoryKiB, &p.iterations, &p.parallelism); err != nil {
		return false, ErrInvalidHash
	}

	salt, err := base64.RawStdEncoding.Strict().DecodeString(parts[4])
	if err != nil {
		return false, ErrInvalidHash
	}
	want, err := base64.RawStdEncoding.Strict().DecodeString(parts[5])
	if err != nil {
		return false, ErrInvalidHash
	}

	got := argon2.IDKey([]byte(plain), salt, p.iterations, p.memoryKiB, p.parallelism, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// dummyHash is verified against when a login names an account that does not
// exist, so that a wrong username and a wrong password cost the same time and
// cannot be told apart by an attacker enumerating names.
var dummyHash, _ = HashPassword("aural-timing-equaliser")

// BurnVerify spends the same work VerifyPassword would, and discards it.
func BurnVerify(plain string) {
	_, _ = VerifyPassword(dummyHash, plain)
}
