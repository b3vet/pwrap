package auth

import (
	"errors"
	"net/http"
	"strings"
)

var ErrNoBearer = errors.New("missing bearer token")

// ExtractBearer returns the bearer token from the Authorization header,
// or ErrNoBearer if absent/malformed.
func ExtractBearer(r *http.Request) (string, error) {
	h := r.Header.Get("Authorization")
	if h == "" {
		return "", ErrNoBearer
	}
	const prefix = "Bearer "
	if !strings.HasPrefix(h, prefix) {
		return "", ErrNoBearer
	}
	tok := strings.TrimSpace(h[len(prefix):])
	if tok == "" {
		return "", ErrNoBearer
	}
	return tok, nil
}
