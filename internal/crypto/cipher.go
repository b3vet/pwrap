// Package crypto provides envelope encryption for sensitive at-rest fields
// in the pwrapd control-plane DB (currently: projects.pg_password).
//
// Design:
//
//   - AES-256-GCM. Authenticated, no separate MAC needed.
//   - 32-byte key from PWRAP_ENCRYPTION_KEY (base64-encoded). Operator-managed.
//   - Versioned envelope on the wire so we can rotate algorithms later:
//     "v1:<base64(nonce||ciphertext||tag)>"
//   - Plaintext fallback: Decrypt detects un-prefixed values and returns them as-is.
//     Lets pwrapd upgrade an existing DB without a manual migration step (the
//     startup re-encrypt pass rewrites them in place).
//
// If PWRAP_ENCRYPTION_KEY is unset, NewNoop() returns a cipher that passes values
// through verbatim. pwrapd logs a loud warning at startup so production deployers
// don't ship with no key.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Cipher encrypts/decrypts at-rest field values.
type Cipher interface {
	// Encrypt returns the envelope-encoded ciphertext for plaintext.
	// An empty input returns "" (never encrypt-and-pad nothing).
	Encrypt(plaintext string) (string, error)
	// Decrypt returns the plaintext for an envelope-encoded value.
	// If the value has no recognised envelope prefix it's returned verbatim —
	// supports forward compatibility with pre-encryption data.
	Decrypt(stored string) (string, error)
	// Enabled reports whether this cipher actually encrypts. Used so callers
	// can detect "no key configured" without sniffing types.
	Enabled() bool
}

const envelopePrefix = "v1:"

// NewFromEnvKey constructs a cipher from a raw 32-byte AES-256 key.
// Use NewFromBase64Key when reading from PWRAP_ENCRYPTION_KEY.
func NewFromKey(key []byte) (Cipher, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("crypto: key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("crypto: aes cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("crypto: gcm: %w", err)
	}
	return &aesGCM{gcm: gcm}, nil
}

// NewFromBase64Key reads PWRAP_ENCRYPTION_KEY style input. Empty input is an
// error — call NewNoop when the operator deliberately runs without encryption.
func NewFromBase64Key(s string) (Cipher, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, errors.New("crypto: encryption key is empty")
	}
	key, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		// Be forgiving: accept URL-safe base64 too.
		key, err = base64.RawURLEncoding.DecodeString(s)
	}
	if err != nil {
		return nil, fmt.Errorf("crypto: decode key: %w", err)
	}
	return NewFromKey(key)
}

// NewNoop returns a cipher that passes values through. Useful for tests and
// for the "no PWRAP_ENCRYPTION_KEY configured" path — see comment at file top.
func NewNoop() Cipher { return noop{} }

type aesGCM struct{ gcm cipher.AEAD }

func (a *aesGCM) Encrypt(plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	nonce := make([]byte, a.gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("crypto: nonce: %w", err)
	}
	ct := a.gcm.Seal(nil, nonce, []byte(plaintext), nil)
	out := make([]byte, 0, len(nonce)+len(ct))
	out = append(out, nonce...)
	out = append(out, ct...)
	return envelopePrefix + base64.StdEncoding.EncodeToString(out), nil
}

func (a *aesGCM) Decrypt(stored string) (string, error) {
	if stored == "" {
		return "", nil
	}
	if !strings.HasPrefix(stored, envelopePrefix) {
		// Forward compat: treat as legacy plaintext.
		return stored, nil
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(stored, envelopePrefix))
	if err != nil {
		return "", fmt.Errorf("crypto: decode ciphertext: %w", err)
	}
	ns := a.gcm.NonceSize()
	if len(raw) < ns+1 {
		return "", errors.New("crypto: ciphertext too short")
	}
	nonce, ct := raw[:ns], raw[ns:]
	pt, err := a.gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", fmt.Errorf("crypto: open: %w", err)
	}
	return string(pt), nil
}

func (a *aesGCM) Enabled() bool { return true }

type noop struct{}

func (noop) Encrypt(s string) (string, error) { return s, nil }
func (noop) Decrypt(s string) (string, error) {
	// In case a real cipher previously wrote a v1: envelope and the operator
	// later removed the key: we have no way to recover. Return an explicit
	// error so the caller knows the DSN handoff can't proceed.
	if strings.HasPrefix(s, envelopePrefix) {
		return "", errors.New("crypto: stored value is encrypted but no key is configured")
	}
	return s, nil
}
func (noop) Enabled() bool { return false }

// IsEnvelope reports whether the stored value uses a known encryption envelope.
// Used by the startup re-encryption sweep.
func IsEnvelope(s string) bool { return strings.HasPrefix(s, envelopePrefix) }
