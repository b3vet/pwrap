package branches

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/b3vet/pwrap/internal/controlplane/projects"
)

type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

func (h *Handler) Routes(r chi.Router) {
	r.Post("/projects/{id}/branches", h.create)
	r.Get("/projects/{id}/branches", h.list)
	r.Post("/projects/{id}/sync", h.sync)
}

type createReq struct {
	Name       string   `json:"name"`
	WithData   bool     `json:"with_data"`
	CopyTables []string `json:"copy_tables,omitempty"`
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	parentID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad project id")
		return
	}
	var req createReq
	if r.Body != http.NoBody {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}
	// createReq is field-for-field identical to CreateOpts; the conversion is
	// checked at compile time, so a drift in either struct breaks the build.
	child, err := h.svc.Create(r.Context(), parentID, CreateOpts(req))
	switch {
	case errors.Is(err, projects.ErrNotFound):
		writeErr(w, http.StatusNotFound, "parent not found")
	case errors.Is(err, projects.ErrAlreadyExists):
		writeErr(w, http.StatusConflict, "branch name already taken")
	case errors.Is(err, projects.ErrInvalidName):
		writeErr(w, http.StatusBadRequest, "invalid name")
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err.Error())
	default:
		writeJSON(w, http.StatusCreated, child)
	}
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	parentID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad project id")
		return
	}
	children, err := h.svc.List(r.Context(), parentID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if children == nil {
		children = []projects.Project{}
	}
	writeJSON(w, http.StatusOK, children)
}

type syncReq struct {
	Tables   []string `json:"tables,omitempty"`
	Truncate bool     `json:"truncate"`
}

// sync handles POST /v1/projects/{id}/sync on a CHILD project — copies parent data
// over. The path is on the child project for symmetry with the rest of /v1/projects/{id}/*.
func (h *Handler) sync(w http.ResponseWriter, r *http.Request) {
	childID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad project id")
		return
	}
	var req syncReq
	if r.Body != http.NoBody {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}
	if err := h.svc.Sync(r.Context(), childID, SyncOpts{
		Tables:             req.Tables,
		TruncateBeforeCopy: req.Truncate,
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
