package projects

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/b3vet/pwrap/internal/controlplane/authsetup"
	"github.com/b3vet/pwrap/internal/crypto"
)

type Project struct {
	ID              uuid.UUID  `json:"id"`
	Name            string     `json:"name"`
	Slug            string     `json:"slug"`
	PgRole          string     `json:"pg_role"`
	PgSchema        string     `json:"pg_schema"`
	ParentProjectID *uuid.UUID `json:"parent_project_id,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

var (
	ErrNotFound      = errors.New("project not found")
	ErrInvalidName   = errors.New("invalid project name")
	ErrAlreadyExists = errors.New("project already exists")
)

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

type Service struct {
	pool *pgxpool.Pool
	// authRole, if non-empty, enables PostgREST integration: project create/delete
	// grants/revokes tenant-role membership to it and triggers a schemas-list rebuild.
	authRole string
	// cipher protects pg_password at rest. Always non-nil — defaults to a noop
	// cipher when the operator hasn't configured PWRAP_ENCRYPTION_KEY.
	cipher crypto.Cipher
}

func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool, cipher: crypto.NewNoop()}
}

// NewServiceWithAuth wires the authenticator role so project lifecycle ops keep
// PostgREST's schemas list in sync.
func NewServiceWithAuth(pool *pgxpool.Pool, authRole string) *Service {
	return &Service{pool: pool, authRole: authRole, cipher: crypto.NewNoop()}
}

// WithCipher returns the same service configured to encrypt/decrypt at-rest
// credentials with the given cipher. Passing nil falls back to no-op (cleartext).
func (s *Service) WithCipher(c crypto.Cipher) *Service {
	if c == nil {
		c = crypto.NewNoop()
	}
	s.cipher = c
	return s
}

// Cipher exposes the underlying cipher so callers (e.g. startup re-encryption)
// can detect whether encryption is active.
func (s *Service) Cipher() crypto.Cipher { return s.cipher }

// Create provisions a Postgres role + schema for the project and inserts the project row.
// Role naming: "p_<slug>". Schema naming: "p_<slug>". The role owns the schema and has its
// search_path scoped to it.
func (s *Service) Create(ctx context.Context, name string) (Project, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 64 {
		return Project{}, ErrInvalidName
	}
	slug := slugify(name)
	if slug == "" {
		return Project{}, ErrInvalidName
	}

	role := "p_" + slug
	schema := "p_" + slug
	password, err := randomPassword()
	if err != nil {
		return Project{}, err
	}
	storedPassword, err := s.cipher.Encrypt(password)
	if err != nil {
		return Project{}, fmt.Errorf("encrypt password: %w", err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Project{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var p Project
	err = tx.QueryRow(ctx, `
		INSERT INTO projects (name, slug, pg_role, pg_schema, pg_password, parent_project_id)
		VALUES ($1, $2, $3, $4, $5, NULL)
		RETURNING id, name, slug, pg_role, pg_schema, parent_project_id, created_at, updated_at
	`, name, slug, role, schema, storedPassword).Scan(
		&p.ID, &p.Name, &p.Slug, &p.PgRole, &p.PgSchema, &p.ParentProjectID, &p.CreatedAt, &p.UpdatedAt,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return Project{}, ErrAlreadyExists
		}
		return Project{}, fmt.Errorf("insert project: %w", err)
	}

	// Role + schema provisioning can't use parameters (DDL).
	// Identifiers are server-generated from the slug regex, so injection surface is nil.
	// Tenant search_path includes "public" so extension-provided types (e.g. `vector`
	// from pgvector, installed database-wide into public) resolve. Tenant-created objects
	// still land in the tenant schema because it's first on the path.
	ddls := []string{
		fmt.Sprintf(`CREATE ROLE %s WITH LOGIN PASSWORD %s`, quoteIdent(role), quoteLiteral(password)),
		fmt.Sprintf(`CREATE SCHEMA %s AUTHORIZATION %s`, quoteIdent(schema), quoteIdent(role)),
		fmt.Sprintf(`ALTER ROLE %s SET search_path TO %s, %s`, quoteIdent(role), quoteIdent(schema), quoteIdent("public")),
	}
	for _, q := range ddls {
		if _, err := tx.Exec(ctx, q); err != nil {
			return Project{}, fmt.Errorf("provision role/schema: %w", err)
		}
	}

	// If PostgREST integration is enabled, make the tenant role a member of the
	// authenticator in the same tx so the new schema is usable the moment we commit.
	if s.authRole != "" {
		if err := authsetup.GrantTenant(ctx, tx, s.authRole, role); err != nil {
			return Project{}, fmt.Errorf("grant authenticator: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return Project{}, err
	}

	// Post-commit, outside the tx: refresh PostgREST's schemas list so the new
	// schema is exposed without a restart. Best-effort — a failure here doesn't
	// roll back the project.
	if s.authRole != "" {
		_ = authsetup.RebuildSchemasList(ctx, s.pool, s.authRole)
	}

	return p, nil
}

func (s *Service) Get(ctx context.Context, id uuid.UUID) (Project, error) {
	var p Project
	err := s.pool.QueryRow(ctx, `
		SELECT id, name, slug, pg_role, pg_schema, parent_project_id, created_at, updated_at
		FROM projects
		WHERE id = $1 AND deleted_at IS NULL
	`, id).Scan(&p.ID, &p.Name, &p.Slug, &p.PgRole, &p.PgSchema, &p.ParentProjectID, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Project{}, ErrNotFound
	}
	return p, err
}

func (s *Service) List(ctx context.Context) ([]Project, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, name, slug, pg_role, pg_schema, parent_project_id, created_at, updated_at
		FROM projects
		WHERE deleted_at IS NULL
		ORDER BY created_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Project
	for rows.Next() {
		var p Project
		if err := rows.Scan(&p.ID, &p.Name, &p.Slug, &p.PgRole, &p.PgSchema, &p.ParentProjectID, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Delete soft-deletes the project row, drops its schema (CASCADE), and drops the role.
// Idempotent after first success.
func (s *Service) Delete(ctx context.Context, id uuid.UUID) error {
	p, err := s.Get(ctx, id)
	if err != nil {
		return err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if _, err := tx.Exec(ctx, `UPDATE projects SET deleted_at = now() WHERE id = $1`, id); err != nil {
		return err
	}
	// Revoke the zombie api_keys (known gap #2): when a project is soft-deleted, its
	// keys still verify — they just 404 at /connection. Mark them revoked so Verify
	// returns ErrRevoked cleanly.
	if _, err := tx.Exec(ctx,
		`UPDATE api_keys SET revoked_at = now() WHERE project_id = $1 AND revoked_at IS NULL`,
		id,
	); err != nil {
		return fmt.Errorf("revoke keys: %w", err)
	}
	// If PostgREST is enabled, revoke authenticator membership before dropping the role.
	if s.authRole != "" {
		if err := authsetup.RevokeTenant(ctx, tx, s.authRole, p.PgRole); err != nil {
			return fmt.Errorf("revoke authenticator: %w", err)
		}
	}
	drops := []string{
		fmt.Sprintf(`DROP SCHEMA IF EXISTS %s CASCADE`, quoteIdent(p.PgSchema)),
		fmt.Sprintf(`DROP ROLE IF EXISTS %s`, quoteIdent(p.PgRole)),
	}
	for _, q := range drops {
		if _, err := tx.Exec(ctx, q); err != nil {
			return fmt.Errorf("drop: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	if s.authRole != "" {
		_ = authsetup.RebuildSchemasList(ctx, s.pool, s.authRole)
	}
	return nil
}

// LatestAppliedVersion returns the most recent migration version applied to the
// project's tenant schema, or "" if none has been applied yet. Used by the SDK's
// schema-version guard at bootstrap so clients fail loudly when their tenant DB
// is behind what their SDK expects.
func (s *Service) LatestAppliedVersion(ctx context.Context, projectID uuid.UUID) (string, error) {
	var v string
	err := s.pool.QueryRow(ctx, `
		SELECT version FROM migration_log
		 WHERE project_id = $1 AND state = 'applied'
		 ORDER BY started_at DESC LIMIT 1
	`, projectID).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// GetCredentials returns the role + password used to build a tenant DSN.
// Kept package-local helpers out of the public Project type to avoid leaking secrets.
//
// The stored pg_password is decrypted via the configured cipher. If the cipher
// is a noop and the stored value happens to be an envelope (operator removed
// PWRAP_ENCRYPTION_KEY after enabling it), Decrypt returns an explicit error.
func (s *Service) GetCredentials(ctx context.Context, id uuid.UUID) (role, password, schema string, err error) {
	var stored string
	err = s.pool.QueryRow(ctx, `
		SELECT pg_role, pg_password, pg_schema FROM projects WHERE id = $1 AND deleted_at IS NULL
	`, id).Scan(&role, &stored, &schema)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", "", ErrNotFound
	}
	if err != nil {
		return "", "", "", err
	}
	password, err = s.cipher.Decrypt(stored)
	if err != nil {
		return "", "", "", fmt.Errorf("decrypt pg_password: %w", err)
	}
	return role, password, schema, nil
}

// ReencryptAll rewrites any pg_password rows whose stored value is not already
// in the current cipher's envelope form. Idempotent. Returns the count of rows
// updated. Use at pwrapd startup so upgrading from a no-key deployment to a
// keyed one rotates legacy plaintext rows without operator intervention.
func (s *Service) ReencryptAll(ctx context.Context) (int, error) {
	if !s.cipher.Enabled() {
		return 0, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, pg_password FROM projects WHERE deleted_at IS NULL
	`)
	if err != nil {
		return 0, fmt.Errorf("list projects: %w", err)
	}
	type row struct {
		id     uuid.UUID
		stored string
	}
	var pending []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.stored); err != nil {
			rows.Close()
			return 0, err
		}
		if !crypto.IsEnvelope(r.stored) {
			pending = append(pending, r)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	var n int
	for _, r := range pending {
		ct, err := s.cipher.Encrypt(r.stored)
		if err != nil {
			return n, fmt.Errorf("encrypt id=%s: %w", r.id, err)
		}
		if _, err := s.pool.Exec(ctx,
			`UPDATE projects SET pg_password = $1 WHERE id = $2 AND pg_password = $3`,
			ct, r.id, r.stored,
		); err != nil {
			return n, fmt.Errorf("update id=%s: %w", r.id, err)
		}
		n++
	}
	return n, nil
}

func slugify(s string) string {
	lower := strings.ToLower(s)
	return strings.Trim(slugRe.ReplaceAllString(lower, "_"), "_")
}

func randomPassword() (string, error) {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// quoteIdent mirrors pgx.Identifier{}.Sanitize() but is inlined to avoid the dep path.
func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// quoteLiteral escapes a string for inclusion in DDL. Inputs here are server-generated
// random tokens; this is defence-in-depth.
func quoteLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "SQLSTATE 23505")
}
