package tenancy

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DefaultTTL is how long a minted DSN stays usable. Short enough that a leaked
// connection string is a bounded problem, long enough that refresh traffic stays
// negligible: one extra control-plane call per client per hour.
const DefaultTTL = time.Hour

// sweepGrace delays dropping a role past its expiry. Postgres already refuses
// new logins the moment VALID UNTIL passes, so the grace costs nothing in
// exposure; it just avoids dropping a role out from under a client whose clock
// disagrees with the server's by a few seconds.
const sweepGrace = 5 * time.Minute

// roleNameMax is Postgres's identifier limit. Names longer than this are
// silently truncated, which would let two ephemeral roles collide.
const roleNameMax = 63

// Mint creates a short-lived login role for the project and records it for
// sweeping. The role inherits the tenant role, so it sees exactly the tenant's
// privileges and nothing more, and carries VALID UNTIL so Postgres itself stops
// accepting the credential — no client cooperation required.
//
// Role-level settings are not inherited, so search_path is set explicitly;
// without it the client would land on the default path and miss the tenant
// schema entirely.
//
// ttl is used exactly as given, including values in the past — defaulting
// belongs to the caller (see NewService), and a primitive that silently
// rewrites its argument cannot be tested for the expiry behaviour that is the
// whole point of it.
func Mint(ctx context.Context, pool *pgxpool.Pool, projectID uuid.UUID, tenantRole, schema string, ttl time.Duration) (role, password string, expiresAt time.Time, err error) {
	role, err = ephemeralRoleName(tenantRole)
	if err != nil {
		return "", "", time.Time{}, err
	}
	password, err = randomSecret(24)
	if err != nil {
		return "", "", time.Time{}, fmt.Errorf("generate password: %w", err)
	}
	expiresAt = time.Now().UTC().Add(ttl)

	tx, err := pool.Begin(ctx)
	if err != nil {
		return "", "", time.Time{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Record first: if the CREATE succeeds but the insert fails, the sweeper
	// would never learn about the role and it would outlive its expiry as an
	// orphan. This ordering can only leave a row with no role, which the
	// sweeper handles.
	if _, err = tx.Exec(ctx, `
		INSERT INTO ephemeral_roles (role_name, project_id, expires_at)
		VALUES ($1, $2, $3)
	`, role, projectID, expiresAt); err != nil {
		return "", "", time.Time{}, fmt.Errorf("record ephemeral role: %w", err)
	}

	// VALID UNTIL is what makes the expiry real: Postgres refuses password
	// authentication past it, whatever the client believes.
	stmts := []string{
		fmt.Sprintf(`CREATE ROLE %s WITH LOGIN INHERIT PASSWORD %s VALID UNTIL %s IN ROLE %s`,
			quoteIdent(role), quoteLiteral(password),
			quoteLiteral(expiresAt.Format(time.RFC3339)), quoteIdent(tenantRole)),
		fmt.Sprintf(`ALTER ROLE %s SET search_path TO %s, %s`,
			quoteIdent(role), quoteIdent(schema), quoteIdent("public")),
	}
	for _, q := range stmts {
		if _, err = tx.Exec(ctx, q); err != nil {
			return "", "", time.Time{}, fmt.Errorf("create ephemeral role: %w", err)
		}
	}

	if err = tx.Commit(ctx); err != nil {
		return "", "", time.Time{}, err
	}
	return role, password, expiresAt, nil
}

// Sweep drops every ephemeral role past its expiry plus the grace window.
// Returns how many were dropped. A role that has already vanished still has its
// row removed, so the table cannot accumulate tombstones.
//
// DROP ROLE is issued outside a transaction per role: one failure (a role that
// still owns an object, say) must not roll back the rest of the sweep.
func Sweep(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	// The tenant role comes along because anything the expired credential
	// created has to be handed to it before the role can go. See retireRole.
	rows, err := pool.Query(ctx, `
		SELECT e.role_name, p.pg_role
		  FROM ephemeral_roles e
		  JOIN projects p ON p.id = e.project_id
		 WHERE e.expires_at < now() - $1::interval
	`, sweepGrace.String())
	if err != nil {
		return 0, err
	}
	type expired struct{ role, tenantRole string }
	var names []expired
	for rows.Next() {
		var e expired
		if err := rows.Scan(&e.role, &e.tenantRole); err != nil {
			rows.Close()
			return 0, err
		}
		names = append(names, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	dropped := 0
	var failures []string
	for _, e := range names {
		if err := retireRole(ctx, pool, e.role, e.tenantRole); err != nil {
			// Leave the row so the next sweep retries it, but say so — a role
			// that can never be dropped would otherwise be retried forever in
			// silence.
			failures = append(failures, fmt.Sprintf("%s: %v", e.role, err))
			continue
		}
		if _, err := pool.Exec(ctx, `DELETE FROM ephemeral_roles WHERE role_name = $1`, e.role); err != nil {
			failures = append(failures, fmt.Sprintf("%s (row): %v", e.role, err))
			continue
		}
		dropped++
	}
	if len(failures) > 0 {
		return dropped, fmt.Errorf("sweep: %d of %d could not be dropped: %s",
			len(failures), len(names), strings.Join(failures, "; "))
	}
	return dropped, nil
}

// DropForProject removes every ephemeral role belonging to a project. Called
// before the tenant role itself goes away, since the ephemeral roles are its
// members and would otherwise be left pointing at nothing.
func DropForProject(ctx context.Context, pool *pgxpool.Pool, projectID uuid.UUID) error {
	rows, err := pool.Query(ctx, `
		SELECT e.role_name, p.pg_role
		  FROM ephemeral_roles e
		  JOIN projects p ON p.id = e.project_id
		 WHERE e.project_id = $1
	`, projectID)
	if err != nil {
		return err
	}
	type owned struct{ role, tenantRole string }
	var names []owned
	for rows.Next() {
		var o owned
		if err := rows.Scan(&o.role, &o.tenantRole); err != nil {
			rows.Close()
			return err
		}
		names = append(names, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, o := range names {
		if err := retireRole(ctx, pool, o.role, o.tenantRole); err != nil {
			return fmt.Errorf("drop ephemeral role %s: %w", o.role, err)
		}
	}
	_, err = pool.Exec(ctx, `DELETE FROM ephemeral_roles WHERE project_id = $1`, projectID)
	return err
}

// retireRole hands everything an ephemeral role owns to the project's tenant
// role, then drops it.
//
// Clients connect *as* the ephemeral role, so anything they create — a
// materialized view through the SDK, a table through the SQL escape hatch — is
// owned by a credential built to vanish within the hour, and Postgres refuses to
// drop a role that still owns objects. Without this the sweeper fails on that
// role forever and the object outlives its owner. Ownership belongs to the
// tenant role, which outlives every credential minted for it.
//
// The GRANT is needed because REASSIGN OWNED and DROP OWNED check
// has_privs_of_role, which follows INHERIT and not SET: a non-superuser
// CREATEROLE admin holds ADMIN OPTION on roles it created but not their
// privileges. It is a harmless no-op for a superuser, and re-granting an
// existing membership is idempotent, so this also repairs roles minted before
// this function existed.
func retireRole(ctx context.Context, pool *pgxpool.Pool, role, tenantRole string) error {
	// A previous sweep may have dropped the role and failed to delete its row.
	// Nothing below tolerates a missing role, and there is nothing left to do.
	var exists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`, role).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return nil
	}
	for _, q := range []string{
		fmt.Sprintf(`GRANT %s TO CURRENT_USER`, quoteIdent(role)),
		fmt.Sprintf(`GRANT %s TO CURRENT_USER`, quoteIdent(tenantRole)),
		fmt.Sprintf(`REASSIGN OWNED BY %s TO %s`, quoteIdent(role), quoteIdent(tenantRole)),
		// Clears the privilege grants REASSIGN leaves behind; those block DROP
		// ROLE just as ownership does.
		fmt.Sprintf(`DROP OWNED BY %s`, quoteIdent(role)),
		fmt.Sprintf(`DROP ROLE IF EXISTS %s`, quoteIdent(role)),
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			return fmt.Errorf("%s: %w", q, err)
		}
	}
	return nil
}

// ephemeralRoleName derives a unique login name from the tenant role, trimming
// the stem as needed so the result cannot exceed Postgres's identifier limit —
// a truncated name would collide with another mint and hand two clients the
// same role.
func ephemeralRoleName(tenantRole string) (string, error) {
	suffix, err := randomSecret(9) // 12 base64url chars
	if err != nil {
		return "", fmt.Errorf("generate role suffix: %w", err)
	}
	const sep = "_e_"
	stem := tenantRole
	if max := roleNameMax - len(sep) - len(suffix); len(stem) > max {
		stem = stem[:max]
	}
	return stem + sep + suffix, nil
}

func randomSecret(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func quoteIdent(s string) string   { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
func quoteLiteral(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
