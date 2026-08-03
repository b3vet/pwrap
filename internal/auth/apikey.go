package auth

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/berkeucvet/pwrap/internal/controlplane/keys"
)

type ctxKey int

const (
	ctxProjectID ctxKey = iota
	ctxKeyID
)

// APIKeyAuth middleware verifies a project API key and attaches the project ID to the context.
func APIKeyAuth(svc *keys.Service) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			plaintext, err := ExtractBearer(r)
			if err != nil {
				http.Error(w, `{"error":"missing api key"}`, http.StatusUnauthorized)
				return
			}
			projectID, keyID, err := svc.Verify(r.Context(), plaintext)
			if err != nil {
				code := http.StatusUnauthorized
				if errors.Is(err, keys.ErrRevoked) || errors.Is(err, keys.ErrExpired) {
					code = http.StatusForbidden
				}
				http.Error(w, `{"error":"invalid api key"}`, code)
				return
			}
			ctx := context.WithValue(r.Context(), ctxProjectID, projectID)
			ctx = context.WithValue(ctx, ctxKeyID, keyID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func ProjectID(ctx context.Context) (uuid.UUID, bool) {
	v, ok := ctx.Value(ctxProjectID).(uuid.UUID)
	return v, ok
}

func KeyID(ctx context.Context) (uuid.UUID, bool) {
	v, ok := ctx.Value(ctxKeyID).(uuid.UUID)
	return v, ok
}
