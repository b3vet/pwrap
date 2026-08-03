package migrations

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type Handler struct {
	runner *Runner
}

func NewHandler(runner *Runner) *Handler { return &Handler{runner: runner} }

func (h *Handler) Routes(r chi.Router) {
	r.Post("/projects/{id}/migrations", h.apply)
	r.Get("/projects/{id}/migrations", h.status)
}

func (h *Handler) apply(w http.ResponseWriter, r *http.Request) {
	pid, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad project id")
		return
	}
	entry, err := h.runner.Apply(r.Context(), pid)
	if err != nil {
		// Best-effort: the pending/failed log row exists in the DB regardless.
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, entry)
}

func (h *Handler) status(w http.ResponseWriter, r *http.Request) {
	pid, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad project id")
		return
	}
	s, err := h.runner.Status(r.Context(), pid)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
