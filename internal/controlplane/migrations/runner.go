package migrations

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"

	"github.com/b3vet/pwrap/internal/config"
	"github.com/b3vet/pwrap/internal/controlplane/projects"
	"github.com/b3vet/pwrap/internal/controlplane/tenancy"
	tenantmigs "github.com/b3vet/pwrap/migrations/tenant"
)

// Entry is one row in migration_log, exposed to clients via GET /v1/projects/:id/migrations.
type Entry struct {
	ID          int64      `json:"id"`
	Version     string     `json:"version"`
	Source      string     `json:"source"`
	Checksum    string     `json:"checksum"`
	State       string     `json:"state"`
	Error       string     `json:"error,omitempty"`
	StartedAt   time.Time  `json:"started_at"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
}

// Status reports what's been applied and what's available.
type Status struct {
	Current  string  `json:"current"` // latest applied version, "" if none
	Target   string  `json:"target"`  // latest available version
	UpToDate bool    `json:"up_to_date"`
	Log      []Entry `json:"log"`
}

type Runner struct {
	pool     *pgxpool.Pool
	projects *projects.Service
	cfg      config.Config
}

func NewRunner(pool *pgxpool.Pool, ps *projects.Service, cfg config.Config) *Runner {
	return &Runner{pool: pool, projects: ps, cfg: cfg}
}

const baselineVersion = "0001_init"

// Apply runs the baseline pwrap tenant migration + River migrations for the given project.
// Idempotent: re-running returns success with no-op if already up to date.
func (r *Runner) Apply(ctx context.Context, projectID uuid.UUID) (Entry, error) {
	role, password, _, err := r.projects.GetCredentials(ctx, projectID)
	if err != nil {
		return Entry{}, err
	}

	// Load baseline SQL from embedded FS and compute checksum for the log.
	sqlBytes, err := tenantmigs.FS.ReadFile(baselineVersion + ".up.sql")
	if err != nil {
		return Entry{}, fmt.Errorf("read baseline: %w", err)
	}
	sum := sha256.Sum256(sqlBytes)
	checksum := hex.EncodeToString(sum[:])

	// Short-circuit if already applied with matching checksum.
	if done, prev, err := r.isApplied(ctx, projectID, baselineVersion, checksum); err != nil {
		return Entry{}, err
	} else if done {
		return prev, nil
	}

	// Record pending row.
	var logID int64
	err = r.pool.QueryRow(ctx, `
		INSERT INTO migration_log (project_id, version, source, checksum, state)
		VALUES ($1, $2, 'embedded', $3, 'pending')
		RETURNING id
	`, projectID, baselineVersion, checksum).Scan(&logID)
	if err != nil {
		return Entry{}, fmt.Errorf("log pending: %w", err)
	}

	applyErr := r.applyAll(ctx, role, password, string(sqlBytes))

	// Update final state.
	state := "applied"
	var errStr *string
	if applyErr != nil {
		state = "failed"
		s := applyErr.Error()
		errStr = &s
	}
	var entry Entry
	scanErr := r.pool.QueryRow(ctx, `
		UPDATE migration_log
		   SET state = $1, error = $2, completed_at = now()
		 WHERE id = $3
		RETURNING id, version, source, checksum, state, error, started_at, completed_at
	`, state, errStr, logID).Scan(
		&entry.ID, &entry.Version, &entry.Source, &entry.Checksum, &entry.State,
		&nullStr{&entry.Error}, &entry.StartedAt, &entry.CompletedAt,
	)
	if scanErr != nil {
		return Entry{}, fmt.Errorf("log update: %w", scanErr)
	}
	return entry, applyErr
}

func (r *Runner) applyAll(ctx context.Context, role, password, baselineSQL string) error {
	// 1) Superuser connection: ensure database-wide extensions are installed.
	// vector: pgvector for embeddings (used by pwrap_embeddings).
	// pg_graphql: auto-GraphQL for PostgREST; tenant roles get USAGE via authenticator.
	// Both extensions install into `public` and become visible through the tenant
	// role's search_path, which includes public (see projects.Create).
	for _, ext := range []string{"vector", "pg_graphql", "postgis"} {
		if _, err := r.pool.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS `+ext); err != nil {
			return fmt.Errorf("create extension %s: %w", ext, err)
		}
	}
	// pg_graphql.resolve() lives in the `graphql` schema. Tenant roles need USAGE +
	// EXECUTE to call it through the tenant-local `graphql()` wrapper. Granting to
	// PUBLIC is the canonical pg_graphql setup — the function checks caller's table
	// privileges itself, so no extra data leak.
	for _, q := range []string{
		`GRANT USAGE ON SCHEMA graphql TO PUBLIC`,
		`GRANT ALL ON ALL FUNCTIONS IN SCHEMA graphql TO PUBLIC`,
	} {
		if _, err := r.pool.Exec(ctx, q); err != nil {
			return fmt.Errorf("grant graphql: %w", err)
		}
	}

	// 2) Tenant connection: apply pwrap baseline + River migrations into the tenant schema.
	tenantDSN := tenancy.BuildDSN(r.cfg, role, password)
	tenantPool, err := pgxpool.New(ctx, tenantDSN)
	if err != nil {
		return fmt.Errorf("tenant pool: %w", err)
	}
	defer tenantPool.Close()

	if _, err := tenantPool.Exec(ctx, baselineSQL); err != nil {
		return fmt.Errorf("apply baseline: %w", err)
	}

	migrator, err := rivermigrate.New(riverpgxv5.New(tenantPool), nil)
	if err != nil {
		return fmt.Errorf("river migrator: %w", err)
	}
	if _, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
		return fmt.Errorf("river migrate: %w", err)
	}
	return nil
}

// isApplied returns true (and the last matching entry) if the baseline has already been applied
// with a matching checksum.
func (r *Runner) isApplied(ctx context.Context, projectID uuid.UUID, version, checksum string) (bool, Entry, error) {
	var e Entry
	var errPtr *string
	err := r.pool.QueryRow(ctx, `
		SELECT id, version, source, checksum, state, error, started_at, completed_at
		FROM migration_log
		WHERE project_id = $1 AND version = $2 AND checksum = $3 AND state = 'applied'
		ORDER BY started_at DESC
		LIMIT 1
	`, projectID, version, checksum).Scan(
		&e.ID, &e.Version, &e.Source, &e.Checksum, &e.State, &errPtr, &e.StartedAt, &e.CompletedAt,
	)
	if err == nil {
		if errPtr != nil {
			e.Error = *errPtr
		}
		return true, e, nil
	}
	// Any error including pgx.ErrNoRows just means "not applied yet".
	return false, Entry{}, nil
}

// Status returns a summary + log entries.
func (r *Runner) Status(ctx context.Context, projectID uuid.UUID) (Status, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, version, source, checksum, state, error, started_at, completed_at
		FROM migration_log
		WHERE project_id = $1
		ORDER BY started_at DESC
		LIMIT 50
	`, projectID)
	if err != nil {
		return Status{}, err
	}
	defer rows.Close()

	var log []Entry
	var current string
	for rows.Next() {
		var e Entry
		var errPtr *string
		if err := rows.Scan(&e.ID, &e.Version, &e.Source, &e.Checksum, &e.State, &errPtr, &e.StartedAt, &e.CompletedAt); err != nil {
			return Status{}, err
		}
		if errPtr != nil {
			e.Error = *errPtr
		}
		if e.State == "applied" && current == "" {
			current = e.Version
		}
		log = append(log, e)
	}
	if err := rows.Err(); err != nil {
		return Status{}, err
	}
	s := Status{Current: current, Target: baselineVersion, Log: log}
	s.UpToDate = s.Current == s.Target
	return s, nil
}

// nullStr is a tiny sql.Scanner that writes NULLs as "" into a *string destination.
type nullStr struct{ dst *string }

func (n *nullStr) Scan(src any) error {
	if src == nil {
		*n.dst = ""
		return nil
	}
	switch v := src.(type) {
	case string:
		*n.dst = v
	case []byte:
		*n.dst = string(v)
	default:
		return errors.New("nullStr: bad type")
	}
	return nil
}
