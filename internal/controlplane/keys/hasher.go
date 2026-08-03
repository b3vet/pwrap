package keys

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// argon2id parameters tuned for server-side API key verification.
// These are "interactive" settings: fast enough to verify cheaply on the hot /connection path,
// strong enough to resist offline brute-force if the hashes leak.
const (
	argonTime    = 2
	argonMemory  = 64 * 1024 // 64 MiB
	argonThreads = 4
	argonKeyLen  = 32
	argonSaltLen = 16
)

// Hash returns an encoded argon2id hash: $argon2id$v=19$m=...,t=...,p=...$<salt>$<hash>
func Hash(plaintext string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("salt: %w", err)
	}
	sum := argon2.IDKey([]byte(plaintext), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf(
		"$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version,
		argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(sum),
	), nil
}

// Verify returns nil if plaintext matches encoded, ErrMismatch otherwise.
func Verify(plaintext, encoded string) error {
	p, err := parseEncoded(encoded)
	if err != nil {
		return err
	}
	got := argon2.IDKey([]byte(plaintext), p.salt, p.time, p.memory, p.threads, uint32(len(p.hash)))
	if subtle.ConstantTimeCompare(got, p.hash) != 1 {
		return ErrMismatch
	}
	return nil
}

var ErrMismatch = errors.New("argon2id: hash mismatch")

type argonParams struct {
	memory  uint32
	time    uint32
	threads uint8
	salt    []byte
	hash    []byte
}

func parseEncoded(s string) (argonParams, error) {
	var p argonParams
	parts := strings.Split(s, "$")
	// "" | argon2id | v=19 | m=...,t=...,p=... | salt | hash
	if len(parts) != 6 || parts[1] != "argon2id" {
		return p, fmt.Errorf("argon2id: bad format")
	}
	if parts[2] != fmt.Sprintf("v=%d", argon2.Version) {
		return p, fmt.Errorf("argon2id: unsupported version %q", parts[2])
	}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.memory, &p.time, &p.threads); err != nil {
		return p, fmt.Errorf("argon2id: bad params: %w", err)
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return p, fmt.Errorf("argon2id: bad salt: %w", err)
	}
	hash, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return p, fmt.Errorf("argon2id: bad hash: %w", err)
	}
	p.salt, p.hash = salt, hash
	return p, nil
}
