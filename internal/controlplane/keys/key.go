package keys

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"
)

const (
	KeyPrefix    = "pwk_"
	keyBytes     = 32
	PrefixLookup = 8 // chars of the payload stored for O(1) lookup
)

// Generate returns a fresh API key plaintext and the lookup prefix.
// Format: "pwk_<43 base64url chars>". Prefix = first PrefixLookup chars after "pwk_".
func Generate() (plaintext, prefix string, err error) {
	var b [keyBytes]byte
	if _, err = rand.Read(b[:]); err != nil {
		return "", "", fmt.Errorf("rand: %w", err)
	}
	payload := base64.RawURLEncoding.EncodeToString(b[:])
	return KeyPrefix + payload, payload[:PrefixLookup], nil
}

// ParsePrefix extracts the lookup prefix from a plaintext API key, or returns an error
// if the key is malformed.
func ParsePrefix(plaintext string) (string, error) {
	if !strings.HasPrefix(plaintext, KeyPrefix) {
		return "", fmt.Errorf("api key: missing %q prefix", KeyPrefix)
	}
	payload := plaintext[len(KeyPrefix):]
	if len(payload) < PrefixLookup {
		return "", fmt.Errorf("api key: too short")
	}
	return payload[:PrefixLookup], nil
}
