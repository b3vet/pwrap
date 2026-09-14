package admintokens

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/b3vet/pwrap/internal/auth"
)

type ctxKey int

const (
	tokenKey ctxKey = iota
	entryKey
)

// FromContext returns the token that authenticated the request, if any.
func FromContext(ctx context.Context) (Token, bool) {
	t, ok := ctx.Value(tokenKey).(Token)
	return t, ok
}

// auditEntry is filled in by RequireScope and read by Audit after the handler
// returns. Audit has to be the OUTER middleware so it still records a request
// the scope check refuses — but that means it runs before the token is known,
// so the two share this instead of a context value the inner layer cannot
// publish upwards.
type auditEntry struct {
	prefix string
	name   string
	known  bool
}

// RequireScope authenticates an admin token and checks it carries `scope`.
//
// The bootstrap token is deliberately NOT accepted here. It can mint tokens and
// nothing else, so a leaked deployment secret no longer grants the whole
// management API.
func RequireScope(svc *Service, scope Scope) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, err := auth.ExtractBearer(r)
			if err != nil {
				deny(w, http.StatusUnauthorized, "missing bearer token")
				return
			}
			tok, scopes, err := svc.Verify(r.Context(), raw)
			switch {
			case errors.Is(err, ErrInvalid):
				deny(w, http.StatusUnauthorized, "invalid admin token")
				return
			case errors.Is(err, ErrRevoked):
				deny(w, http.StatusForbidden, "admin token revoked or expired")
				return
			case err != nil:
				deny(w, http.StatusInternalServerError, err.Error())
				return
			}
			if !Has(scopes, scope) {
				// Name the missing scope: the operator has to know which grant
				// to add, and the scope set is not a secret from its holder.
				deny(w, http.StatusForbidden, fmt.Sprintf("admin token lacks the %q scope", string(scope)))
				return
			}
			if e, ok := r.Context().Value(entryKey).(*auditEntry); ok {
				e.prefix, e.name, e.known = tok.Prefix, tok.Name, true
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), tokenKey, tok)))
		})
	}
}

// Audit records every mutating admin request, refusals included — a run of
// denials is exactly what an investigation wants to see, so logging only
// successes would hide the interesting case. It must therefore be mounted
// OUTSIDE RequireScope, which short-circuits a refused request before any inner
// middleware runs.
//
// Reads are skipped: they are the bulk of the traffic and the least informative.
func Audit(pool *pgxpool.Pool, logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet || r.Method == http.MethodHead {
				next.ServeHTTP(w, r)
				return
			}
			entry := &auditEntry{}
			r = r.WithContext(context.WithValue(r.Context(), entryKey, entry))
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)

			var prefix, name *string
			if entry.known {
				prefix, name = &entry.prefix, &entry.name
			}
			var projectID *uuid.UUID
			if raw := chi.URLParam(r, "id"); raw != "" {
				if id, err := uuid.Parse(raw); err == nil {
					projectID = &id
				}
			}
			// Detached context: the audit row must still be written when the
			// client has already hung up.
			if _, err := pool.Exec(context.WithoutCancel(r.Context()), `
				INSERT INTO admin_audit_log (token_prefix, token_name, action, method, path, project_id, status)
				VALUES ($1, $2, $3, $4, $5, $6, $7)
			`, prefix, name, r.Method+" "+routePattern(r), r.Method, r.URL.Path, projectID, rec.status); err != nil {
				logger.Warn("admin audit write failed", "err", err, "path", r.URL.Path)
			}
		})
	}
}

// routePattern prefers the matched route ("/v1/projects/{id}") over the concrete
// path, so audit rows group by operation rather than by project id.
func routePattern(r *http.Request) string {
	if rc := chi.RouteContext(r.Context()); rc != nil && rc.RoutePattern() != "" {
		return rc.RoutePattern()
	}
	return r.URL.Path
}

type statusRecorder struct {
	http.ResponseWriter
	status  int
	written bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.written {
		s.status = code
		s.written = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	s.written = true
	return s.ResponseWriter.Write(b)
}

func deny(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
