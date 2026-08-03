// Package branches implements "fork-style" branching: a new project is created as a
// fresh tenant (own role + schema + keys + baseline migrations applied) and linked to
// its parent via projects.parent_project_id. Optionally, rows from the parent's
// built-in tables (pwrap_documents / pwrap_embeddings / pwrap_geo) are copied over so
// the branch starts with a snapshot of the parent's data.
//
// User-defined tables (created via `pwrap sql apply`) are NOT replicated automatically
// in M13 — re-run your migrations against the branch project, then copy data with the
// /v1/projects/:id/branches/:branch_id/sync endpoint listing the tables you want.
package branches

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/berkeucvet/pwrap/internal/controlplane/migrations"
	"github.com/berkeucvet/pwrap/internal/controlplane/projects"
)

// DefaultCopyTables lists the pwrap built-ins that get their rows copied when a branch
// is created with `with_data: true`. River queue state is deliberately excluded.
var DefaultCopyTables = []string{"pwrap_documents", "pwrap_embeddings", "pwrap_geo", "pwrap_matviews"}

type Service struct {
	pool       *pgxpool.Pool
	projects   *projects.Service
	migrations *migrations.Runner
}

func NewService(pool *pgxpool.Pool, ps *projects.Service, mr *migrations.Runner) *Service {
	return &Service{pool: pool, projects: ps, migrations: mr}
}

// CreateOpts controls branching behaviour.
type CreateOpts struct {
	// Name of the new branch project. If empty, derived from parent slug + suffix.
	Name string
	// WithData, when true, copies rows from each table in CopyTables (or DefaultCopyTables
	// when CopyTables is empty) from parent to child after migrations apply.
	WithData bool
	// CopyTables overrides the default set. Each name must be a valid Postgres identifier
	// (validated). Unknown tables in the parent schema are silently skipped.
	CopyTables []string
}

// Create branches the given project. Returns the new child Project, fully provisioned:
//   - role + schema + baseline migrations applied
//   - parent_project_id set to parentID
//   - optionally, rows copied from parent's built-in tables
func (s *Service) Create(ctx context.Context, parentID uuid.UUID, opts CreateOpts) (projects.Project, error) {
	parent, err := s.projects.Get(ctx, parentID)
	if err != nil {
		return projects.Project{}, fmt.Errorf("parent: %w", err)
	}

	if opts.Name == "" {
		opts.Name = parent.Slug + "_branch"
	}

	child, err := s.projects.Create(ctx, opts.Name)
	if err != nil {
		return projects.Project{}, fmt.Errorf("create child: %w", err)
	}

	if _, err := s.pool.Exec(ctx,
		`UPDATE projects SET parent_project_id = $1, updated_at = now() WHERE id = $2`,
		parent.ID, child.ID,
	); err != nil {
		return projects.Project{}, fmt.Errorf("set parent: %w", err)
	}
	// Re-fetch so the returned object reflects the parent linkage.
	child, err = s.projects.Get(ctx, child.ID)
	if err != nil {
		return projects.Project{}, err
	}

	// Apply baseline migrations so the child has pwrap_documents / embeddings / geo /
	// matviews / River. This is idempotent — same behaviour as `pwrap migrate apply`.
	if _, err := s.migrations.Apply(ctx, child.ID); err != nil {
		return projects.Project{}, fmt.Errorf("apply migrations: %w", err)
	}

	if opts.WithData {
		tables := opts.CopyTables
		if len(tables) == 0 {
			tables = DefaultCopyTables
		}
		if err := s.copyData(ctx, parent.PgSchema, child.PgSchema, tables); err != nil {
			return projects.Project{}, fmt.Errorf("copy data: %w", err)
		}
	}

	return child, nil
}

// SyncTables copies rows from parent to child for the given tables. Existing rows in
// the child are NOT cleared first — use TruncateBeforeCopy to wipe-and-replace. Skips
// tables that don't exist in BOTH schemas.
type SyncOpts struct {
	Tables              []string
	TruncateBeforeCopy  bool
}

// Sync copies data from parent to an existing branch.
func (s *Service) Sync(ctx context.Context, childID uuid.UUID, opts SyncOpts) error {
	child, err := s.projects.Get(ctx, childID)
	if err != nil {
		return fmt.Errorf("child: %w", err)
	}
	if child.ParentProjectID == nil {
		return errors.New("project has no parent — not a branch")
	}
	parent, err := s.projects.Get(ctx, *child.ParentProjectID)
	if err != nil {
		return fmt.Errorf("parent: %w", err)
	}
	tables := opts.Tables
	if len(tables) == 0 {
		tables = DefaultCopyTables
	}
	if opts.TruncateBeforeCopy {
		if err := s.truncate(ctx, child.PgSchema, tables); err != nil {
			return err
		}
	}
	return s.copyData(ctx, parent.PgSchema, child.PgSchema, tables)
}

// List returns direct children of the given project (one level deep).
func (s *Service) List(ctx context.Context, parentID uuid.UUID) ([]projects.Project, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, name, slug, pg_role, pg_schema, parent_project_id, created_at, updated_at
		FROM projects
		WHERE parent_project_id = $1 AND deleted_at IS NULL
		ORDER BY created_at DESC
	`, parentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []projects.Project
	for rows.Next() {
		var p projects.Project
		if err := rows.Scan(&p.ID, &p.Name, &p.Slug, &p.PgRole, &p.PgSchema, &p.ParentProjectID, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// --- internal --------------------------------------------------------------

func (s *Service) copyData(ctx context.Context, fromSchema, toSchema string, tables []string) error {
	for _, t := range tables {
		if err := validateIdent(t); err != nil {
			return fmt.Errorf("table %q: %w", t, err)
		}
		// Skip tables that don't exist in either schema.
		existsBoth, err := s.tableExistsInBoth(ctx, fromSchema, toSchema, t)
		if err != nil {
			return err
		}
		if !existsBoth {
			continue
		}
		// ON CONFLICT (id) DO NOTHING so re-syncing is idempotent for UUID-PK tables
		// like pwrap_documents/embeddings/geo. For pwrap_matviews the PK is `name`.
		conflictCol := "id"
		if t == "pwrap_matviews" {
			conflictCol = "name"
		}
		sql := fmt.Sprintf(
			`INSERT INTO %s.%s SELECT * FROM %s.%s ON CONFLICT (%s) DO NOTHING`,
			quoteIdent(toSchema), quoteIdent(t),
			quoteIdent(fromSchema), quoteIdent(t),
			conflictCol,
		)
		if _, err := s.pool.Exec(ctx, sql); err != nil {
			return fmt.Errorf("copy %s.%s -> %s.%s: %w", fromSchema, t, toSchema, t, err)
		}
	}
	return nil
}

func (s *Service) truncate(ctx context.Context, schema string, tables []string) error {
	for _, t := range tables {
		if err := validateIdent(t); err != nil {
			return err
		}
		// IF EXISTS would be ideal here but TRUNCATE doesn't support it — query first.
		exists, err := s.tableExists(ctx, schema, t)
		if err != nil {
			return err
		}
		if !exists {
			continue
		}
		if _, err := s.pool.Exec(ctx, fmt.Sprintf(
			`TRUNCATE %s.%s RESTART IDENTITY CASCADE`, quoteIdent(schema), quoteIdent(t),
		)); err != nil {
			return fmt.Errorf("truncate %s.%s: %w", schema, t, err)
		}
	}
	return nil
}

func (s *Service) tableExists(ctx context.Context, schema, table string) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.tables
			WHERE table_schema = $1 AND table_name = $2
		)
	`, schema, table).Scan(&ok)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return ok, err
}

func (s *Service) tableExistsInBoth(ctx context.Context, a, b, table string) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx, `
		SELECT (
			SELECT count(*) FROM information_schema.tables
			WHERE table_schema IN ($1, $2) AND table_name = $3
		) = 2
	`, a, b, table).Scan(&ok)
	return ok, err
}

func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// validateIdent is a belt-and-suspenders check on table names destined for SQL string
// concatenation. Accepts standard Postgres identifier characters; rejects anything else.
func validateIdent(s string) error {
	if s == "" || len(s) > 63 {
		return errors.New("identifier must be 1..63 chars")
	}
	for _, r := range s {
		ok := (r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') ||
			r == '_'
		if !ok {
			return fmt.Errorf("invalid char %q", r)
		}
	}
	return nil
}
