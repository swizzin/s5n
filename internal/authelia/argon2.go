// Package authelia is root's login: the pure decisions box makes for Authelia,
// and the bytes it writes from them. Everything here is a function of strings and
// a chart's values, so it is tested without a cluster; the cluster half is
// internal/login.
//
// The parameter set, digest syntax and required configuration keys were read from
// Authelia v4.39.28 itself (schema/authentication.go, schema/types.go,
// validator/*.go), not its documentation, because the validator is what refuses
// to start.
package authelia

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Argon2 is one password hashing parameter set. Authelia embeds these values
// in every digest, so a digest box writes is self-describing: Authelia
// re-derives the key from the row, never from this configuration.
type Argon2 struct {
	// Variant is the algorithm name in the digest: argon2id here.
	Variant string
	// Iterations is parameter t.
	Iterations int
	// Memory is parameter m, in KiB.
	Memory int
	// Parallelism is parameter p.
	Parallelism int
	// KeyLength is the derived key's length in bytes.
	KeyLength int
	// SaltLength is the salt's length in bytes.
	SaltLength int
}

// DefaultArgon2 is Authelia's own default (internal/configuration/schema/
// authentication.go, DefaultPasswordConfig), written into the configuration box
// generates: identical values let Authelia rehash a box row on a later login.
var DefaultArgon2 = Argon2{
	Variant:     "argon2id",
	Iterations:  3,
	Memory:      64 * 1024,
	Parallelism: 4,
	KeyLength:   32,
	SaltLength:  16,
}

// DigestPattern is Authelia's own JSON-schema pattern for a password digest
// (internal/configuration/schema/types.go), narrowed to the algorithms box writes.
// Authelia validates the whole file on load, so one malformed row breaks every
// login.
var DigestPattern = regexp.MustCompile(`^\$argon2(id|i|d)\$v=[0-9]+\$m=[0-9]+,t=[0-9]+,p=[0-9]+\$[A-Za-z0-9+/]+\$[A-Za-z0-9+/]+$`)

// ErrBadDigest says a row is not a digest box can verify or Authelia would
// accept: the caller never writes one of these.
var ErrBadDigest = errors.New("not an argon2 digest")

// HashPassword returns Authelia's PHC string for a password: the format its own
// go-crypt encoder writes, both halves base64 standard alphabet without padding.
//
// The salt comes from crypto/rand. A password is never logged or stored: only this
// digest is.
func HashPassword(password string) (string, error) {
	return hashWith(DefaultArgon2, password)
}

func hashWith(p Argon2, password string) (string, error) {
	if p.Variant != "argon2id" {
		return "", fmt.Errorf("hash: variant %q: box writes argon2id only", p.Variant)
	}
	if p.SaltLength < 8 {
		return "", fmt.Errorf("hash: salt length %d is too short", p.SaltLength)
	}
	salt := make([]byte, p.SaltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("hash: salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, uint32(p.Iterations), uint32(p.Memory), uint8(p.Parallelism), uint32(p.KeyLength))
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.Memory, p.Iterations, p.Parallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

// VerifyPassword checks a password against a digest, reading the parameters out of
// the digest exactly as Authelia does. It exists so a test can prove the format
// with Authelia's own template vector: a hash that Authelia cannot verify would be
// a login that never works.
func VerifyPassword(digest, password string) (bool, error) {
	params, salt, want, err := decodeDigest(digest)
	if err != nil {
		return false, err
	}
	got := argon2.IDKey([]byte(password), salt, uint32(params.Iterations), uint32(params.Memory), uint8(params.Parallelism), uint32(len(want)))
	// Constant time is not required (the hash dominates), but comparing in one
	// loop keeps this honest for any future caller.
	var diff byte
	for i := range got {
		diff |= got[i] ^ want[i]
	}
	return diff == 0, nil
}

// decodeDigest parses `$argon2id$v=19$m=65536,t=3,p=4$salt$key`.
func decodeDigest(digest string) (Argon2, []byte, []byte, error) {
	if !DigestPattern.MatchString(digest) {
		return Argon2{}, nil, nil, fmt.Errorf("%w: %q", ErrBadDigest, maskDigest(digest))
	}
	parts := strings.Split(digest, "$")
	// "", "argon2id", "v=19", "m=..,t=..,p=..", salt, key
	if len(parts) != 6 {
		return Argon2{}, nil, nil, fmt.Errorf("%w: %d fields", ErrBadDigest, len(parts))
	}
	p := Argon2{Variant: strings.TrimPrefix(parts[1], "argon2")}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return Argon2{}, nil, nil, fmt.Errorf("%w: version: %v", ErrBadDigest, err)
	}
	if version != argon2.Version {
		return Argon2{}, nil, nil, fmt.Errorf("%w: argon2 version %d, box writes %d", ErrBadDigest, version, argon2.Version)
	}
	for _, field := range strings.Split(parts[3], ",") {
		key, value, ok := strings.Cut(field, "=")
		if !ok {
			return Argon2{}, nil, nil, fmt.Errorf("%w: %q is not key=value", ErrBadDigest, field)
		}
		n, err := strconv.Atoi(value)
		if err != nil {
			return Argon2{}, nil, nil, fmt.Errorf("%w: %s: %v", ErrBadDigest, key, err)
		}
		switch key {
		case "m":
			p.Memory = n
		case "t":
			p.Iterations = n
		case "p":
			p.Parallelism = n
		default:
			return Argon2{}, nil, nil, fmt.Errorf("%w: unknown parameter %q", ErrBadDigest, key)
		}
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return Argon2{}, nil, nil, fmt.Errorf("%w: salt: %v", ErrBadDigest, err)
	}
	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return Argon2{}, nil, nil, fmt.Errorf("%w: key: %v", ErrBadDigest, err)
	}
	if len(salt) == 0 || len(key) == 0 {
		return Argon2{}, nil, nil, fmt.Errorf("%w: empty salt or key", ErrBadDigest)
	}
	p.SaltLength = len(salt)
	p.KeyLength = len(key)
	return p, salt, key, nil
}

// maskDigest is what an error may quote: the algorithm only. A digest is not a
// secret, but half of one in a message is noise nobody asked for.
func maskDigest(digest string) string {
	if i := strings.IndexByte(digest, '$'); i >= 0 {
		if j := strings.LastIndexByte(digest, '$'); j > i {
			return digest[:i] + "…"
		}
	}
	return "…"
}
