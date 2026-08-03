package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/b3vet/pwrap/internal/config"
	"github.com/b3vet/pwrap/internal/controlplane"
	"github.com/b3vet/pwrap/internal/controlplane/authsetup"
	"github.com/b3vet/pwrap/internal/controlplane/migrations"
	"github.com/b3vet/pwrap/internal/store"
	"github.com/b3vet/pwrap/internal/telemetry"
	cpmigrations "github.com/b3vet/pwrap/migrations/controlplane"
)

func main() {
	logger := telemetry.NewLogger()
	slog.SetDefault(logger)
	if err := run(logger); err != nil {
		logger.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Telemetry comes up before anything else so the pool's QueryTracer hits a
	// real provider on the first query. Shutdown (deferred) flushes spans.
	tel, err := telemetry.Init(ctx, telemetry.Options{
		ServiceName:     cfg.OTelServiceName,
		Environment:     cfg.Environment,
		OTLPEndpoint:    cfg.OTLPEndpoint,
		OTLPInsecure:    cfg.OTLPInsecure,
		TraceSampleRate: cfg.TraceSampleRate,
		PrometheusAddr:  cfg.MetricsAddr,
		Logger:          logger,
	})
	if err != nil {
		logger.Warn("telemetry init failed", "err", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := tel.Shutdown(shutdownCtx); err != nil {
			logger.Warn("telemetry shutdown", "err", err)
		}
	}()

	pool, err := store.NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	// Self-bootstrap the control plane schema. Every statement is IF NOT EXISTS so
	// re-running on each startup is a no-op when the schema is already current.
	// This makes `docker-compose down -v && docker-compose up -d && ./bin/pwrapd`
	// a working flow without a manual psql step.
	if _, err := pool.Exec(ctx, cpmigrations.UpSQL); err != nil {
		return fmt.Errorf("bootstrap control plane schema: %w", err)
	}
	logger.Info("control plane schema applied")

	// Sweep orphaned migration_log rows from any prior crash. Idempotent: zero rows
	// touched in the happy path. >5min stale is the threshold (real applies finish
	// well under a minute, even with River + index builds).
	if swept, err := migrations.SweepStale(ctx, pool, 5*time.Minute); err != nil {
		logger.Warn("orphan sweep failed", "err", err)
	} else if swept > 0 {
		logger.Warn("swept orphaned migration_log rows", "count", swept)
	}

	// REST / PostgREST integration: bootstrap authenticator + anon roles and rebuild
	// the schemas list from live projects. Idempotent; safe on every start.
	if cfg.RestEnabled() {
		if err := authsetup.EnsureRoles(ctx, pool, cfg.AuthenticatorRole, cfg.AuthenticatorPassword, cfg.AnonRole, cfg.JWTSecret); err != nil {
			return err
		}
		if err := authsetup.RebuildSchemasList(ctx, pool, cfg.AuthenticatorRole); err != nil {
			logger.Warn("rebuild db_schemas list", "err", err)
		}
		logger.Info("REST integration enabled", "authenticator", cfg.AuthenticatorRole, "rest_url", cfg.RestExternalURL)
	} else {
		logger.Info("REST integration disabled (PWRAP_JWT_SECRET / PWRAP_AUTHENTICATOR_PASSWORD not set)")
	}

	srv := controlplane.NewServer(logger, pool, cfg)

	// One-shot re-encryption pass. Picks up any rows whose pg_password is still
	// plaintext (upgrades from a no-key deployment, or a row inserted while the
	// cipher was misconfigured). Idempotent — zero rows touched when everything
	// is already encrypted.
	if swept, err := srv.ProjectsService().ReencryptAll(ctx); err != nil {
		logger.Warn("re-encrypt sweep failed", "err", err)
	} else if swept > 0 {
		logger.Info("re-encrypted legacy pg_password rows", "count", swept)
	}

	httpSrv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           srv.Router(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("pwrapd listening", "addr", cfg.HTTPAddr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		logger.Info("shutting down")
	case err := <-errCh:
		if err != nil {
			return err
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return httpSrv.Shutdown(shutdownCtx)
}
