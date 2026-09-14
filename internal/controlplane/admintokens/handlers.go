package admintokens

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// Routes mounts the token-management endpoints. These sit behind the bootstrap
// token, not behind a scope: minting is the one thing the root credential can
// still do, and a scoped token must not be able to widen its own privileges by
// issuing itself a better one.
func (h *Handler) Routes(r chi.Router) {
	r.Post("/admin/tokens", h.issue)
	r.Get("/admin/tokens", h.list)
	r.Delete("/admin/tokens/{tokenID}", h.revoke)
}

type issueReq struct {
	Name       string   `json:"name"`
	Scopes     []string `json:"scopes"`
	TTLSeconds int      `json:"ttl_seconds,omitempty"`
}

func (h *Handler) issue(w http.ResponseWriter, r *http.Request) {
	var req issueReq
	if r.Body != http.NoBody {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
	}
	scopes, err := ParseScopes(req.Scopes)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	issued, err := h.svc.Issue(r.Context(), req.Name, scopes, time.Duration(req.TTLSeconds)*time.Second)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, issued)
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	tokens, err := h.svc.List(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, tokens)
}

func (h *Handler) revoke(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "tokenID"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad token id")
		return
	}
	switch err := h.svc.Revoke(r.Context(), id); {
	case errors.Is(err, ErrNotFound):
		writeErr(w, http.StatusNotFound, "token not found")
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err.Error())
	default:
		w.WriteHeader(http.StatusNoContent)
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
