package realtime

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/b3vet/pwrap/internal/auth"
	"github.com/b3vet/pwrap/internal/controlplane/keys"
	"github.com/b3vet/pwrap/internal/controlplane/projects"
)

// Handler upgrades GET /v1/subscribe into a WebSocket and pipes matching
// ChangeEvents from the Hub. Auth + filter come from query params (or headers
// for non-browser clients).
//
// Query params:
//
//	api_key  — required if no Authorization header. Browsers can't set headers
//	           on WebSocket constructors, hence this fallback. Logged at debug
//	           with a prefix only.
//	table    — optional. Restrict to one table.
//	user_id  — optional. Restrict to rows whose change carried this jwt user_id
//	           (typical use: pass current end-user id to mirror RLS scoping).
type Handler struct {
	hub      *Hub
	keys     *keys.Service
	projects *projects.Service
	logger   *slog.Logger
}

func NewHandler(hub *Hub, keys *keys.Service, projects *projects.Service, logger *slog.Logger) *Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{hub: hub, keys: keys, projects: projects, logger: logger}
}

func (h *Handler) Subscribe(w http.ResponseWriter, r *http.Request) {
	apiKey := bearerOrQuery(r)
	if apiKey == "" {
		http.Error(w, `{"error":"missing api key"}`, http.StatusUnauthorized)
		return
	}
	projectID, _, err := h.keys.Verify(r.Context(), apiKey)
	if err != nil {
		http.Error(w, `{"error":"invalid api key"}`, http.StatusUnauthorized)
		return
	}
	proj, err := h.projects.Get(r.Context(), projectID)
	if err != nil {
		http.Error(w, `{"error":"project not found"}`, http.StatusNotFound)
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// pwrapd is usually behind a proxy; CORS is the proxy's problem. Allow any
		// Origin so the realtime-todos demo works from file:// during testing.
		InsecureSkipVerify: true,
	})
	if err != nil {
		h.logger.Warn("realtime: ws accept", "err", err)
		return
	}
	// Use a longer-than-default close timeout; some pwrap clients send a final
	// frame on graceful close.
	defer func() { _ = conn.CloseNow() }()

	filter := Filter{
		Schema: proj.PgSchema,
		Table:  r.URL.Query().Get("table"),
		UserID: r.URL.Query().Get("user_id"),
	}
	sub := &Subscriber{
		Filter: filter,
		Events: make(chan ChangeEvent, 64),
	}
	unreg := h.hub.Subscribe(sub)
	defer unreg()

	// Send a one-time hello so clients can confirm they're connected.
	hello := map[string]any{
		"type":   "hello",
		"schema": proj.PgSchema,
		"filter": filter,
	}
	if err := wsjson.Write(r.Context(), conn, hello); err != nil {
		return
	}

	// Read loop runs in a goroutine so the server gets notified when the client
	// disconnects (websocket reads need to be drained to detect close).
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		for {
			if _, _, err := conn.Reader(r.Context()); err != nil {
				return
			}
		}
	}()

	// Periodic ping keeps connections alive through stingy proxies.
	pingTicker := time.NewTicker(30 * time.Second)
	defer pingTicker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-readDone:
			return
		case ev := <-sub.Events:
			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			err := wsjson.Write(ctx, conn, ev)
			cancel()
			if err != nil {
				return
			}
		case <-pingTicker.C:
			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			err := conn.Ping(ctx)
			cancel()
			if err != nil {
				return
			}
		}
	}
}

// bearerOrQuery returns the API key from the Authorization header (preferred,
// used by Go/Python SDKs) or the `api_key` query param (used by browsers, which
// can't set headers on the WebSocket constructor).
func bearerOrQuery(r *http.Request) string {
	if tok, err := auth.ExtractBearer(r); err == nil {
		return tok
	} else if !errors.Is(err, auth.ErrNoBearer) {
		// Header present but malformed — surface as a missing key rather than
		// continuing to query lookup, so the client gets a clear 401.
		return ""
	}
	return r.URL.Query().Get("api_key")
}
