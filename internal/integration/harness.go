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
	"github.com/testcontainers/testcontainers-go/network"
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

	// postgrestImage must match the sidecar in docker-compose.yml, so the suite
	// tests the version operators actually run.
	postgrestImage = "postgrest/postgrest:v12.2.0"

	// pgNetworkAlias is how PostgREST addresses Postgres over the shared docker
	// network. Container-to-container traffic can't use the host-mapped port.
	pgNetworkAlias = "postgres"

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
	pgContainer    *postgres.PostgresContainer
	pgrstContainer *tc.DockerContainer
	network        *tc.DockerNetwork
	pool           *pgxpool.Pool
	httpServer     *httptest.Server
	server         *controlplane.Server
	ControlURL     string
	AdminToken     string
	DatabaseURL    string

	// RestURL is the host-reachable PostgREST base URL.
	RestURL string
}

// NewStack spins up Postgres and the PostgREST sidecar on a shared docker
// network, applies the control-plane schema, and boots pwrapd's HTTP surface
// in-process.
//
// Ordering matters: PostgREST authenticates as pwrap_authenticator, so it can
// only start once EnsureRoles has created that role. docker-compose papers over
// the same race with `restart: unless-stopped`; here we simply start it last.
func NewStack(ctx context.Context) (_ *Stack, err error) {
	s := &Stack{AdminToken: adminToken}
	// Tear down whatever came up before the failure, so a mid-setup error
	// doesn't strand containers.
	defer func() {
		if err != nil {
			s.Close()
		}
	}()

	nw, err := network.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("create network: %w", err)
	}
	s.network = nw

	s.pgContainer, err = postgres.Run(ctx, imageName,
		postgres.WithDatabase("pwrap"),
		postgres.WithUsername("pwrap"),
		postgres.WithPassword("pwrap"),
		network.WithNetwork([]string{pgNetworkAlias}, nw),
		tc.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("start postgres: %w", err)
	}

	dsn, err := s.pgContainer.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return nil, fmt.Errorf("conn string: %w", err)
	}
	s.DatabaseURL = dsn

	s.pool, err = pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("open pool: %w", err)
	}

	// Mirror pwrapd's startup: bootstrap schema → sweep → ensure roles.
	if _, err = s.pool.Exec(ctx, cpmigrations.UpSQL); err != nil {
		return nil, fmt.Errorf("bootstrap schema: %w", err)
	}
	if _, err = migrations.SweepStale(ctx, s.pool, 5*time.Minute); err != nil {
		return nil, fmt.Errorf("sweep: %w", err)
	}
	if err = authsetup.EnsureRoles(ctx, s.pool, "pwrap_authenticator", authPassword, "pwrap_anon", jwtSecret); err != nil {
		return nil, fmt.Errorf("ensure roles: %w", err)
	}
	if err = authsetup.RebuildSchemasList(ctx, s.pool, "pwrap_authenticator"); err != nil {
		return nil, fmt.Errorf("rebuild schemas: %w", err)
	}

	if err = s.startPostgREST(ctx, nw); err != nil {
		return nil, err
	}

	cfg, err := loadTestConfig(dsn, s.RestURL)
	if err != nil {
		return nil, fmt.Errorf("test config: %w", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	s.server = controlplane.NewServer(logger, s.pool, cfg)
	s.httpServer = httptest.NewServer(s.server.Router())
	s.ControlURL = s.httpServer.URL

	return s, nil
}

// startPostgREST brings up the sidecar on the shared network and records its
// host-reachable URL. db-config=true means it reads jwt_secret and db_schemas
// from the authenticator role, so pwrapd can add tenant schemas via NOTIFY
// without a restart.
func (s *Stack) startPostgREST(ctx context.Context, nw *tc.DockerNetwork) error {
	ctr, err := tc.Run(ctx, postgrestImage,
		network.WithNetwork([]string{"postgrest"}, nw),
		tc.WithExposedPorts("3000/tcp"),
		tc.WithEnv(map[string]string{
			// Container-to-container, so the network alias and internal port —
			// not the host-mapped one.
			"PGRST_DB_URI": fmt.Sprintf("postgres://pwrap_authenticator:%s@%s:5432/pwrap",
				authPassword, pgNetworkAlias),
			"PGRST_DB_CONFIG":    "true",
			"PGRST_DB_SCHEMAS":   "public",
			"PGRST_DB_ANON_ROLE": "pwrap_anon",
			"PGRST_JWT_SECRET":   jwtSecret,
			"PGRST_SERVER_PORT":  "3000",
		}),
		tc.WithWaitStrategy(
			wait.ForListeningPort("3000/tcp").WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		return fmt.Errorf("start postgrest: %w", err)
	}
	s.pgrstContainer = ctr

	host, err := ctr.Host(ctx)
	if err != nil {
		return fmt.Errorf("postgrest host: %w", err)
	}
	port, err := ctr.MappedPort(ctx, "3000/tcp")
	if err != nil {
		return fmt.Errorf("postgrest port: %w", err)
	}
	s.RestURL = fmt.Sprintf("http://%s:%s", host, port.Port())
	return nil
}

// Close tears down pwrapd, the realtime hub, the pool, and the Postgres container.
// Order matters: the hub holds a long-lived LISTEN connection that would
// otherwise block pool.Close() indefinitely.
func (s *Stack) Close() {
	ctx := context.Background()
	if s.httpServer != nil {
		s.httpServer.Close()
	}
	if s.server != nil {
		s.server.StopHub()
	}
	if s.pool != nil {
		s.pool.Close()
	}
	if s.pgrstContainer != nil {
		_ = s.pgrstContainer.Terminate(ctx)
	}
	if s.pgContainer != nil {
		_ = s.pgContainer.Terminate(ctx)
	}
	// Network last — removing it while a container is still attached fails.
	if s.network != nil {
		_ = s.network.Remove(ctx)
	}
}

// Pool returns the pwrap-admin pgxpool so tests can inspect/seed control-plane state.
func (s *Stack) Pool() *pgxpool.Pool { return s.pool }

// loadTestConfig stuffs the test DSN + auth into env, then calls config.Load.
// Avoids hand-rolling a Config struct that has to keep pace with the real schema.
func loadTestConfig(dsn, restURL string) (config.Config, error) {
	for k, v := range map[string]string{
		"PWRAP_DATABASE_URL":           dsn,
		"PWRAP_BOOTSTRAP_TOKEN":        adminToken,
		"PWRAP_AUTHENTICATOR_PASSWORD": authPassword,
		"PWRAP_JWT_SECRET":             jwtSecret,
		// The real sidecar URL, so /v1/rest/token hands clients something they
		// can actually reach.
		"PWRAP_REST_URL":       restURL,
		"PWRAP_ENV":            "test",
		"PWRAP_ENCRYPTION_KEY": encryptionKey,
	} {
		if err := os.Setenv(k, v); err != nil {
			return config.Config{}, fmt.Errorf("setenv %s: %w", k, err)
		}
	}
	return config.Load()
}
