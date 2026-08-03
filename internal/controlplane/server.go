package controlplane

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/b3vet/pwrap/internal/auth"
	"github.com/b3vet/pwrap/internal/config"
	"github.com/b3vet/pwrap/internal/controlplane/authjwt"
	"github.com/b3vet/pwrap/internal/controlplane/branches"
	"github.com/b3vet/pwrap/internal/controlplane/keys"
	"github.com/b3vet/pwrap/internal/controlplane/migrations"
	"github.com/b3vet/pwrap/internal/controlplane/projects"
	"github.com/b3vet/pwrap/internal/controlplane/realtime"
	"github.com/b3vet/pwrap/internal/controlplane/rest"
	"github.com/b3vet/pwrap/internal/controlplane/sqlapply"
	"github.com/b3vet/pwrap/internal/controlplane/tenancy"
	"github.com/b3vet/pwrap/internal/crypto"
)

type Server struct {
	logger     *slog.Logger
	pool       *pgxpool.Pool
	cfg        config.Config
	projects   *projects.Service
	keys       *keys.Service
	tenancy    *tenancy.Service
	migrations *migrations.Runner
	signer     *authjwt.Signer // nil when REST disabled
	hub        *realtime.Hub   // shared across all tenants for the realtime path

	// connectionLimiter rate-limits /v1/connection and /v1/rest/token per API-key
	// prefix. /connection is the only true hot endpoint pwrapd exposes (called on
	// every SDK startup + DSN refresh); cheap to abuse without limits.
	connectionLimiter *auth.Limiter
}

func NewServer(logger *slog.Logger, pool *pgxpool.Pool, cfg config.Config) *Server {
	var ps *projects.Service
	if cfg.RestEnabled() {
		ps = projects.NewServiceWithAuth(pool, cfg.AuthenticatorRole)
	} else {
		ps = projects.NewService(pool)
	}
	// Wire encryption-at-rest. Construction errors are non-fatal here (we have
	// already logged at startup); the noop cipher keeps the service functional.
	if cipher, err := buildCipher(cfg, logger); err == nil {
		ps = ps.WithCipher(cipher)
	} else {
		logger.Warn("encryption-at-rest disabled — pg_password stored in cleartext", "err", err)
	}
	ks := keys.NewService(pool)
	ts := tenancy.NewService(ps, cfg)
	mr := migrations.NewRunner(pool, ps, cfg)

	var signer *authjwt.Signer
	if cfg.RestEnabled() {
		s, err := authjwt.NewSigner(cfg.JWTSecret)
		if err != nil {
			logger.Warn("jwt signer disabled", "err", err)
		} else {
			signer = s
		}
	}

	// 10 rps, burst 20 per API-key prefix. Comfortable for a normal SDK (one
	// /connection on boot, one /rest/token per few minutes) while still cutting
	// pathological loops at the knee.
	limiter := auth.NewLimiter(10, 20)
	go limiter.StartCleanup(context.Background())

	// Hub needs a live pool; the /healthz unit test passes nil. Guard so the test
	// path keeps working without starting a goroutine that would panic on nil.
	var hub *realtime.Hub
	if pool != nil {
		hub = realtime.NewHub(pool, logger)
		hub.Start(context.Background())
	}

	return &Server{
		logger:            logger,
		pool:              pool,
		cfg:               cfg,
		projects:          ps,
		keys:              ks,
		tenancy:           ts,
		migrations:        mr,
		signer:            signer,
		hub:               hub,
		connectionLimiter: limiter,
	}
}

// StopHub cancels the realtime Hub's LISTEN loop and waits for it to release
// the pool connection. Call before pool.Close() in shutdown / test teardown,
// otherwise pool.Close blocks waiting for the held conn.
func (s *Server) StopHub() {
	if s.hub != nil {
		s.hub.Stop()
	}
}

func (s *Server) Router() http.Handler {
	r := chi.NewRouter()
	// otelhttp emits a span per request and propagates W3C tracecontext headers.
	// Placed first so every downstream middleware/handler sees the active span.
	r.Use(func(next http.Handler) http.Handler {
		return otelhttp.NewHandler(next, "pwrapd",
			otelhttp.WithSpanNameFormatter(func(_ string, req *http.Request) string {
				return req.Method + " " + req.URL.Path
			}),
		)
	})
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(30 * time.Second))

	r.Route("/v1", func(r chi.Router) {
		r.Get("/healthz", s.handleHealthz)
		r.Get("/readyz", s.handleReadyz)

		// Management endpoints — admin bootstrap token required.
		r.Group(func(r chi.Router) {
			r.Use(auth.AdminOnly(s.cfg.BootstrapToken))
			projects.NewHandler(s.projects).Routes(r)
			keys.NewHandler(s.keys).Routes(r)
			migrations.NewHandler(s.migrations).Routes(r)
			sqlapply.NewHandler(s.projects, s.cfg).Routes(r)
			branches.NewHandler(branches.NewService(s.pool, s.projects, s.migrations)).Routes(r)
		})

		// SDK bootstrap + REST token issuance — project API key required.
		// Per-bearer rate limit applies BEFORE auth check so a flood of bad-key
		// requests doesn't run argon2id verification on every shot.
		r.Group(func(r chi.Router) {
			r.Use(auth.RateLimit(s.connectionLimiter, auth.BearerKey))
			r.Use(auth.APIKeyAuth(s.keys))
			r.Post("/connection", tenancy.NewHandler(s.tenancy).Connection)
			if s.signer != nil {
				restH := rest.NewHandler(s.pool, s.projects, s.signer, s.cfg)
				r.Post("/rest/token", restH.IssueToken)
			}
		})

		// Realtime: WebSocket upgrade. Auth lives inside the handler because the
		// browser can't set Authorization on a WebSocket constructor; the api_key
		// query param is the fallback. Rate-limited by the same connection limiter.
		if s.hub != nil {
			r.Group(func(r chi.Router) {
				r.Use(auth.RateLimit(s.connectionLimiter, auth.BearerKey))
				rt := realtime.NewHandler(s.hub, s.keys, s.projects, s.logger)
				r.Get("/subscribe", rt.Subscribe)
			})
		}
	})

	return r
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.pool.Ping(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unready", "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// buildCipher resolves the at-rest cipher from configuration. Returns the noop
// cipher (no error) when PWRAP_ENCRYPTION_KEY is unset; returns an error only
// when the key is set but malformed — that's a hard misconfiguration we want
// the operator to see in logs.
func buildCipher(cfg config.Config, logger *slog.Logger) (crypto.Cipher, error) {
	if cfg.EncryptionKey == "" {
		logger.Warn("PWRAP_ENCRYPTION_KEY not set — pg_password stored in cleartext (set a 32-byte base64 key in production)")
		return crypto.NewNoop(), nil
	}
	c, err := crypto.NewFromBase64Key(cfg.EncryptionKey)
	if err != nil {
		return nil, err
	}
	logger.Info("encryption-at-rest enabled (AES-256-GCM)")
	return c, nil
}

// ProjectsService exposes the underlying projects service. Used by pwrapd to
// run the one-shot ReencryptAll sweep at startup without re-deriving the cipher.
func (s *Server) ProjectsService() *projects.Service { return s.projects }
