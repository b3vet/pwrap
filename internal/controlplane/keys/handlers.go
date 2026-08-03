package keys

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

func (h *Handler) Routes(r chi.Router) {
	r.Post("/projects/{id}/keys", h.issue)
	r.Get("/projects/{id}/keys", h.list)
	r.Delete("/projects/{id}/keys/{keyID}", h.revoke)
}

type issueReq struct {
	Name string `json:"name"`
}

func (h *Handler) issue(w http.ResponseWriter, r *http.Request) {
	pid, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad project id")
		return
	}
	var req issueReq
	_ = json.NewDecoder(r.Body).Decode(&req) // name optional
	issued, err := h.svc.Issue(r.Context(), pid, req.Name)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, issued)
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	pid, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad project id")
		return
	}
	ks, err := h.svc.List(r.Context(), pid)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if ks == nil {
		ks = []APIKey{}
	}
	writeJSON(w, http.StatusOK, ks)
}

func (h *Handler) revoke(w http.ResponseWriter, r *http.Request) {
	pid, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad project id")
		return
	}
	kid, err := uuid.Parse(chi.URLParam(r, "keyID"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad key id")
		return
	}
	switch err := h.svc.Revoke(r.Context(), pid, kid); {
	case errors.Is(err, ErrNotFound):
		writeErr(w, http.StatusNotFound, "not found")
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
