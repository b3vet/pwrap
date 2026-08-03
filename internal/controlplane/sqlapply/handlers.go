// Package sqlapply runs user-supplied SQL against a tenant's schema as the tenant role.
// Escape hatch for creating real (non-JSONB) tables that PostgREST can expose.
// Admin-token-authenticated — don't expose this surface to end-user API keys.
package sqlapply

import (
	"context"
	"encoding/json"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/b3vet/pwrap/internal/config"
	"github.com/b3vet/pwrap/internal/controlplane/projects"
	"github.com/b3vet/pwrap/internal/controlplane/tenancy"
)

type Handler struct {
	projects *projects.Service
	cfg      config.Config
}

func NewHandler(ps *projects.Service, cfg config.Config) *Handler {
	return &Handler{projects: ps, cfg: cfg}
}

func (h *Handler) Routes(r chi.Router) {
	r.Post("/projects/{id}/sql", h.apply)
}

type applyResp struct {
	Applied bool `json:"applied"`
}

// apply streams the request body to Postgres as the tenant role. The SDK or CLI
// wraps a file upload in a POST body; we don't enforce size limits at this layer.
func (h *Handler) apply(w http.ResponseWriter, r *http.Request) {
	pid, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad project id")
		return
	}
	role, password, _, err := h.projects.GetCredentials(r.Context(), pid)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	if len(body) == 0 {
		writeErr(w, http.StatusBadRequest, "empty body")
		return
	}

	dsn := tenancy.BuildDSN(h.cfg, role, password)
	tenantPool, err := pgxpool.New(r.Context(), dsn)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "open tenant pool: "+err.Error())
		return
	}
	defer tenantPool.Close()

	// Execute as one batch; pgx's simple query protocol supports multiple statements.
	// Wrap in a tx so a syntactically broken file doesn't leave half the DDL applied.
	err = withTx(r.Context(), tenantPool, func(ctx context.Context, conn *pgxpool.Conn) error {
		_, err := conn.Exec(ctx, string(body))
		return err
	})
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, applyResp{Applied: true})
}

func withTx(ctx context.Context, pool *pgxpool.Pool, fn func(context.Context, *pgxpool.Conn) error) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "BEGIN"); err != nil {
		return err
	}
	if err := fn(ctx, conn); err != nil {
		_, _ = conn.Exec(ctx, "ROLLBACK")
		return err
	}
	_, err = conn.Exec(ctx, "COMMIT")
	return err
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
