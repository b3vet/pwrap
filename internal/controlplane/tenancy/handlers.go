package tenancy

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/berkeucvet/pwrap/internal/auth"
	"github.com/berkeucvet/pwrap/internal/controlplane/projects"
)

type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// Connection handles POST /v1/connection. The request is authenticated with a project API key
// (APIKeyAuth middleware attaches the project ID to the context), so the endpoint takes no body
// or path params — the key *is* the authorization to mint the DSN for its project.
func (h *Handler) Connection(w http.ResponseWriter, r *http.Request) {
	pid, ok := auth.ProjectID(r.Context())
	if !ok {
		writeErr(w, http.StatusUnauthorized, "no project")
		return
	}
	conn, err := h.svc.Exchange(r.Context(), pid)
	switch {
	case errors.Is(err, projects.ErrNotFound):
		writeErr(w, http.StatusNotFound, "project not found")
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err.Error())
	default:
		writeJSON(w, http.StatusOK, conn)
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
