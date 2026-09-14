package tenancy

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/b3vet/pwrap/internal/config"
	"github.com/b3vet/pwrap/internal/controlplane/projects"
)

// Connection is what /v1/connection returns to an authenticated SDK.
//
// DSN carries a freshly minted, short-lived role. ExpiresAt is the moment
// Postgres stops accepting it — a real deadline enforced by VALID UNTIL, not a
// hint the client may ignore. SDKs must re-exchange before it passes.
type Connection struct {
	DSN    string `json:"dsn"`
	Schema string `json:"schema"`
	// SchemaVersion is the latest migration version applied to the tenant schema.
	// Empty if `pwrap migrate apply` has never been run for this project. SDKs
	// compare against their compile-time SchemaVersion constant to refuse a
	// tenant that's behind what the SDK was built against.
	SchemaVersion string    `json:"schema_version"`
	ExpiresAt     time.Time `json:"expires_at"`
}

type Service struct {
	projects *projects.Service
	cfg      config.Config
	pool     *pgxpool.Pool
	ttl      time.Duration
}

func NewService(ps *projects.Service, cfg config.Config, pool *pgxpool.Pool) *Service {
	ttl := time.Duration(cfg.DSNTTLSeconds) * time.Second
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	return &Service{projects: ps, cfg: cfg, pool: pool, ttl: ttl}
}

// Exchange mints a fresh short-lived role for the project and returns a DSN for
// it. Each call produces new credentials; nothing here hands out the tenant
// role's own password, so a DSN that leaks expires on its own and can be revoked
// independently of every other client.
func (s *Service) Exchange(ctx context.Context, projectID uuid.UUID) (Connection, error) {
	tenantRole, _, schema, err := s.projects.GetCredentials(ctx, projectID)
	if err != nil {
		return Connection{}, err
	}
	// Best-effort: an unmigrated project has no version yet; the SDK turns "" into a
	// helpful error pointing the user at `pwrap migrate apply`.
	version, _ := s.projects.LatestAppliedVersion(ctx, projectID)

	role, password, expiresAt, err := Mint(ctx, s.pool, projectID, tenantRole, schema, s.ttl)
	if err != nil {
		return Connection{}, fmt.Errorf("mint credentials: %w", err)
	}
	return Connection{
		DSN:           BuildDSN(s.cfg, role, password),
		Schema:        schema,
		SchemaVersion: version,
		ExpiresAt:     expiresAt,
	}, nil
}

// BuildDSN returns a Postgres URI for the given tenant role against the pwrapd-configured
// tenant host/port/database. The role's default search_path (set at project-create time
// via ALTER ROLE) scopes the connection to its schema — no need to pass search_path in
// the DSN (libpq rejects it there anyway).
func BuildDSN(cfg config.Config, role, password string) string {
	u := &url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(role, password),
		Host:   net.JoinHostPort(cfg.TenantHost, cfg.TenantPort),
		Path:   "/" + cfg.TenantDatabase,
	}
	q := u.Query()
	q.Set("sslmode", cfg.TenantSSLMode)
	u.RawQuery = q.Encode()
	return u.String()
}
