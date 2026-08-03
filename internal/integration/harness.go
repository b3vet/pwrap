// Package integration spins up pwrap-postgres:local in a testcontainer, boots
// pwrapd in-process against it, and exposes a Stack handle for tests.
//
// Behind a build tag (`//go:build integration`) so unit-test runs aren't slowed
// down by Docker image pulls. Run via `make test-integration`.
//
//go:build integration

package integration

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	tc "github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/b3vet/pwrap/internal/config"
	"github.com/b3vet/pwrap/internal/controlplane"
	"github.com/b3vet/pwrap/internal/controlplane/authsetup"
	"github.com/b3vet/pwrap/internal/controlplane/migrations"
	cpmigrations "github.com/b3vet/pwrap/migrations/controlplane"
)

const (
	// imageName must be pre-built — `docker-compose build postgres` or the Makefile
	// `test-integration` target. It carries pgvector + pg_graphql + postgis.
	imageName = "pwrap-postgres:local"

	authPassword = "test-authpw"
	jwtSecret    = "test-jwt-secret-32-bytes-padding-xxxxx"
	adminToken   = "test-admin"
	// Deterministic 32-byte AES-256 key (base64) so the encryption-at-rest path
	// is exercised by every integration test. Keep in sync with the real prod
	// guidance: rotate keys via PWRAP_ENCRYPTION_KEY, never commit live keys.
	encryptionKey = "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="
)

// Stack is the live test environment: a Postgres container, pwrapd bound to a
// random localhost port via httptest, and an admin token. Tests share one Stack
// per package via TestMain (see integration_test.go).
type Stack struct {
	pgContainer *postgres.PostgresContainer
	pool        *pgxpool.Pool
	httpServer  *httptest.Server
	server      *controlplane.Server
	ControlURL  string
	AdminToken  string
	DatabaseURL string
}

// NewStack spins up a Postgres container, applies the control-plane schema, and
// boots pwrapd's HTTP surface in-process. Returns the Stack or an error.
func NewStack(ctx context.Context) (*Stack, error) {
	pgCtr, err := postgres.Run(ctx, imageName,
		postgres.WithDatabase("pwrap"),
		postgres.WithUsername("pwrap"),
		postgres.WithPassword("pwrap"),
		tc.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("start postgres: %w", err)
	}

	dsn, err := pgCtr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		_ = pgCtr.Terminate(ctx)
		return nil, fmt.Errorf("conn string: %w", err)
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		_ = pgCtr.Terminate(ctx)
		return nil, fmt.Errorf("open pool: %w", err)
	}

	// Mirror pwrapd's startup: bootstrap schema → sweep → ensure roles.
	if _, err := pool.Exec(ctx, cpmigrations.UpSQL); err != nil {
		pool.Close()
		_ = pgCtr.Terminate(ctx)
		return nil, fmt.Errorf("bootstrap schema: %w", err)
	}
	if _, err := migrations.SweepStale(ctx, pool, 5*time.Minute); err != nil {
		pool.Close()
		_ = pgCtr.Terminate(ctx)
		return nil, fmt.Errorf("sweep: %w", err)
	}
	if err := authsetup.EnsureRoles(ctx, pool, "pwrap_authenticator", authPassword, "pwrap_anon", jwtSecret); err != nil {
		pool.Close()
		_ = pgCtr.Terminate(ctx)
		return nil, fmt.Errorf("ensure roles: %w", err)
	}
	if err := authsetup.RebuildSchemasList(ctx, pool, "pwrap_authenticator"); err != nil {
		pool.Close()
		_ = pgCtr.Terminate(ctx)
		return nil, fmt.Errorf("rebuild schemas: %w", err)
	}

	cfg, err := loadTestConfig(dsn)
	if err != nil {
		pool.Close()
		_ = pgCtr.Terminate(ctx)
		return nil, fmt.Errorf("test config: %w", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := controlplane.NewServer(logger, pool, cfg)
	httpSrv := httptest.NewServer(srv.Router())

	return &Stack{
		pgContainer: pgCtr,
		pool:        pool,
		httpServer:  httpSrv,
		server:      srv,
		ControlURL:  httpSrv.URL,
		AdminToken:  adminToken,
		DatabaseURL: dsn,
	}, nil
}

// Close tears down pwrapd, the realtime hub, the pool, and the Postgres container.
// Order matters: the hub holds a long-lived LISTEN connection that would
// otherwise block pool.Close() indefinitely.
func (s *Stack) Close() {
	if s.httpServer != nil {
		s.httpServer.Close()
	}
	if s.server != nil {
		s.server.StopHub()
	}
	if s.pool != nil {
		s.pool.Close()
	}
	if s.pgContainer != nil {
		_ = s.pgContainer.Terminate(context.Background())
	}
}

// Pool returns the pwrap-admin pgxpool so tests can inspect/seed control-plane state.
func (s *Stack) Pool() *pgxpool.Pool { return s.pool }

// loadTestConfig stuffs the test DSN + auth into env, then calls config.Load.
// Avoids hand-rolling a Config struct that has to keep pace with the real schema.
func loadTestConfig(dsn string) (config.Config, error) {
	for k, v := range map[string]string{
		"PWRAP_DATABASE_URL":           dsn,
		"PWRAP_BOOTSTRAP_TOKEN":        adminToken,
		"PWRAP_AUTHENTICATOR_PASSWORD": authPassword,
		"PWRAP_JWT_SECRET":             jwtSecret,
		"PWRAP_REST_URL":               "http://localhost:3000", // unused; no PostgREST in this suite
		"PWRAP_ENV":                    "test",
		"PWRAP_ENCRYPTION_KEY":         encryptionKey,
	} {
		if err := os.Setenv(k, v); err != nil {
			return config.Config{}, fmt.Errorf("setenv %s: %w", k, err)
		}
	}
	return config.Load()
}
