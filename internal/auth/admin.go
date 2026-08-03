package auth

import (
	"crypto/subtle"
	"net/http"
)

// AdminOnly guards management endpoints with a shared bootstrap token.
// If the configured token is empty, the middleware refuses ALL requests —
// don't accidentally expose /v1/projects with no auth.
func AdminOnly(token string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if token == "" {
				http.Error(w, `{"error":"admin disabled: PWRAP_BOOTSTRAP_TOKEN unset"}`, http.StatusForbidden)
				return
			}
			got, err := ExtractBearer(r)
			if err != nil {
				http.Error(w, `{"error":"missing bearer"}`, http.StatusUnauthorized)
				return
			}
			if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
				http.Error(w, `{"error":"invalid admin token"}`, http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
