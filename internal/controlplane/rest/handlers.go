// Package rest issues PostgREST-compatible JWTs to authenticated SDK clients.
package rest

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/berkeucvet/pwrap/internal/auth"
	"github.com/berkeucvet/pwrap/internal/config"
	"github.com/berkeucvet/pwrap/internal/controlplane/authjwt"
	"github.com/berkeucvet/pwrap/internal/controlplane/projects"
)

type Handler struct {
	pool     *pgxpool.Pool
	projects *projects.Service
	signer   *authjwt.Signer
	cfg      config.Config
}

func NewHandler(pool *pgxpool.Pool, ps *projects.Service, signer *authjwt.Signer, cfg config.Config) *Handler {
	return &Handler{pool: pool, projects: ps, signer: signer, cfg: cfg}
}

type issueReq struct {
	UserID     string `json:"user_id,omitempty"`
	TTLSeconds int    `json:"ttl_seconds,omitempty"`
}

type issueResp struct {
	Token     string    `json:"token"`
	URL       string    `json:"url"`
	Role      string    `json:"role"`
	ExpiresAt time.Time `json:"expires_at"`
}

// IssueToken handles POST /v1/projects/{id}/rest/token.
// Authenticated via project API key (APIKeyAuth middleware attaches the project ID).
// Request body is optional — {"user_id": "...", "ttl_seconds": 3600}.
func (h *Handler) IssueToken(w http.ResponseWriter, r *http.Request) {
	if h.signer == nil {
		writeErr(w, http.StatusServiceUnavailable, "REST integration disabled (PWRAP_JWT_SECRET not set)")
		return
	}
	pid, ok := auth.ProjectID(r.Context())
	if !ok {
		writeErr(w, http.StatusUnauthorized, "no project")
		return
	}

	var req issueReq
	if r.Body != http.NoBody {
		_ = json.NewDecoder(r.Body).Decode(&req) // body optional
	}
	ttl := time.Duration(req.TTLSeconds) * time.Second
	if ttl <= 0 {
		ttl = time.Duration(h.cfg.RestTokenTTLSeconds) * time.Second
	}

	proj, err := h.projects.Get(r.Context(), pid)
	if errors.Is(err, projects.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "project not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	token, exp, err := h.signer.Issue(authjwt.IssueOpts{
		Role:      proj.PgRole,
		ProjectID: proj.ID,
		UserID:    req.UserID,
		TTL:       ttl,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, issueResp{
		Token:     token,
		URL:       h.cfg.RestExternalURL,
		Role:      proj.PgRole,
		ExpiresAt: exp,
	})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
