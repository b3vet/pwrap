// Package authsetup manages the PostgREST authenticator + anon roles and the
// in-DB PostgREST config that tells PostgREST which tenant schemas to expose.
//
// Flow:
//  1. On pwrapd startup, EnsureRoles creates pwrap_anon + pwrap_authenticator
//     (idempotent) and stores the JWT secret as a role-level setting that
//     PostgREST reads via `db-config = true`.
//  2. On project create, GrantTenant + RebuildSchemasList keeps the schema list
//     in sync and NOTIFYs PostgREST to reload.
//  3. On project delete, RevokeTenant + RebuildSchemasList does the reverse.
package authsetup

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// EnsureRoles creates the anon + authenticator roles if missing, syncs the
// authenticator password, and seeds PostgREST config on the authenticator role.
// Idempotent. Safe to call on every pwrapd startup.
func EnsureRoles(ctx context.Context, pool *pgxpool.Pool, authRole, authPassword, anonRole, jwtSecret string) error {
	if err := ensureRole(ctx, pool, anonRole, "NOLOGIN NOINHERIT", ""); err != nil {
		return fmt.Errorf("anon: %w", err)
	}
	if err := ensureRole(ctx, pool, authRole, "LOGIN NOINHERIT", authPassword); err != nil {
		return fmt.Errorf("authenticator: %w", err)
	}

	// Authenticator needs to SET ROLE into anon (and every tenant). GRANT anon first;
	// tenant grants happen at project-create time.
	if _, err := pool.Exec(ctx, fmt.Sprintf(
		`GRANT %s TO %s`, quoteIdent(anonRole), quoteIdent(authRole),
	)); err != nil {
		return fmt.Errorf("grant anon: %w", err)
	}

	// PostgREST reads pgrst.* settings from the authenticator role when db-config=true.
	// jwt_secret is required; db_schemas gets rebuilt separately via RebuildSchemasList.
	if jwtSecret != "" {
		if _, err := pool.Exec(ctx, fmt.Sprintf(
			`ALTER ROLE %s SET pgrst.jwt_secret = %s`,
			quoteIdent(authRole), quoteLiteral(jwtSecret),
		)); err != nil {
			return fmt.Errorf("set jwt_secret: %w", err)
		}
		if _, err := pool.Exec(ctx, fmt.Sprintf(
			`ALTER ROLE %s SET pgrst.jwt_secret_is_base64 = 'false'`, quoteIdent(authRole),
		)); err != nil {
			return fmt.Errorf("set jwt_secret_is_base64: %w", err)
		}
	}

	return nil
}

// GrantTenant allows the authenticator role to SET ROLE into the tenant role.
func GrantTenant(ctx context.Context, tx pgx.Tx, authRole, tenantRole string) error {
	_, err := tx.Exec(ctx, fmt.Sprintf(
		`GRANT %s TO %s`, quoteIdent(tenantRole), quoteIdent(authRole),
	))
	return err
}

// RevokeTenant drops the authenticator's membership in the tenant role.
func RevokeTenant(ctx context.Context, tx pgx.Tx, authRole, tenantRole string) error {
	_, err := tx.Exec(ctx, fmt.Sprintf(
		`REVOKE %s FROM %s`, quoteIdent(tenantRole), quoteIdent(authRole),
	))
	return err
}

// RebuildSchemasList queries the live projects table, writes the CSV list onto the
// authenticator role, and NOTIFYs PostgREST. Called after project create/delete.
// Uses the passed pool so the update lands in the main control-plane DB.
func RebuildSchemasList(ctx context.Context, pool *pgxpool.Pool, authRole string) error {
	rows, err := pool.Query(ctx, `
		SELECT pg_schema FROM projects WHERE deleted_at IS NULL ORDER BY pg_schema
	`)
	if err != nil {
		return err
	}
	defer rows.Close()

	var schemas []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return err
		}
		schemas = append(schemas, s)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	// PostgREST refuses an empty db-schemas; keep "public" as a harmless anchor so the
	// role's setting always has a usable value between project create/delete cycles.
	list := append([]string{"public"}, schemas...)
	csv := strings.Join(list, ",")

	if _, err := pool.Exec(ctx, fmt.Sprintf(
		`ALTER ROLE %s SET pgrst.db_schemas = %s`,
		quoteIdent(authRole), quoteLiteral(csv),
	)); err != nil {
		return fmt.Errorf("set db_schemas: %w", err)
	}
	// Best-effort — PostgREST may be offline, that's fine; it'll pick up the config
	// on its next connection or explicit reload.
	_, _ = pool.Exec(ctx, `NOTIFY pgrst, 'reload config'`)
	_, _ = pool.Exec(ctx, `NOTIFY pgrst, 'reload schema'`)
	return nil
}

func ensureRole(ctx context.Context, pool *pgxpool.Pool, name, attrs, password string) error {
	// Existence check first; CREATE ROLE errors on duplicate and we want idempotency.
	var exists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`, name,
	).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		pwClause := ""
		if password != "" {
			pwClause = " PASSWORD " + quoteLiteral(password)
		}
		if _, err := pool.Exec(ctx, fmt.Sprintf(
			`CREATE ROLE %s WITH %s%s`, quoteIdent(name), attrs, pwClause,
		)); err != nil {
			return err
		}
		return nil
	}
	// Role exists — keep the password synced with env. Without this, rotating
	// PWRAP_AUTHENTICATOR_PASSWORD without dropping the role would leave PostgREST
	// unable to connect.
	if password != "" {
		if _, err := pool.Exec(ctx, fmt.Sprintf(
			`ALTER ROLE %s WITH PASSWORD %s`, quoteIdent(name), quoteLiteral(password),
		)); err != nil {
			return err
		}
	}
	return nil
}

func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

func quoteLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
