package tenancy

import (
	"context"
	"net"
	"net/url"
	"time"

	"github.com/google/uuid"

	"github.com/b3vet/pwrap/internal/config"
	"github.com/b3vet/pwrap/internal/controlplane/projects"
)

// Connection is what /v1/connection returns to an authenticated SDK.
// DSN is the short-lived connection string; ExpiresAt is when the SDK should refresh.
// (In M2 the underlying role password is stable; the short-lived contract exists so we
// can swap in rotating creds in later milestones without breaking the SDK.)
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

const defaultTTL = 24 * time.Hour

type Service struct {
	projects *projects.Service
	cfg      config.Config
}

func NewService(ps *projects.Service, cfg config.Config) *Service {
	return &Service{projects: ps, cfg: cfg}
}

func (s *Service) Exchange(ctx context.Context, projectID uuid.UUID) (Connection, error) {
	role, password, schema, err := s.projects.GetCredentials(ctx, projectID)
	if err != nil {
		return Connection{}, err
	}
	// Best-effort: an unmigrated project has no version yet; the SDK turns "" into a
	// helpful error pointing the user at `pwrap migrate apply`.
	version, _ := s.projects.LatestAppliedVersion(ctx, projectID)
	return Connection{
		DSN:           BuildDSN(s.cfg, role, password),
		Schema:        schema,
		SchemaVersion: version,
		ExpiresAt:     time.Now().Add(defaultTTL),
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
