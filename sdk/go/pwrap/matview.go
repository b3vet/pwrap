package pwrap

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Matview is a handle over a named materialized view backed by the pwrap_matviews
// registry. Scheduling is left to the caller (River periodic jobs work well);
// Refresh() is the primitive to call on a schedule or on demand.
type Matview struct {
	client *Client
	name   string
}

// Matview returns a handle for the given matview name (must be a valid Postgres identifier).
func (c *Client) Matview(name string) *Matview {
	return &Matview{client: c, name: name}
}

// MatviewInfo is the registry row for a matview.
type MatviewInfo struct {
	Name          string     `json:"name"`
	Definition    string     `json:"definition"`
	Checksum      string     `json:"checksum"`
	LastRefreshAt *time.Time `json:"last_refresh_at,omitempty"`
	LastError     string     `json:"last_error,omitempty"`
	Enabled       bool       `json:"enabled"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

// Register ensures the matview exists with the given definition and records it.
// definition is the SQL *after* the `AS` — e.g. `SELECT data->>'topic' AS topic, count(*) FROM pwrap_documents GROUP BY 1`.
// If the matview exists with a different definition, Register drops + recreates it.
// The new matview is created WITH NO DATA — the first Refresh() populates it.
func (mv *Matview) Register(ctx context.Context, definition string) error {
	if err := validateIdent(mv.name); err != nil {
		return err
	}
	definition = strings.TrimSpace(definition)
	if definition == "" {
		return errors.New("pwrap: matview: definition is empty")
	}
	sum := sha256.Sum256([]byte(definition))
	checksum := hex.EncodeToString(sum[:])

	// Fetch current registry row, if any.
	var existingChecksum string
	err := mv.client.Pool().QueryRow(ctx, `
		SELECT checksum FROM pwrap_matviews WHERE name = $1
	`, mv.name).Scan(&existingChecksum)
	needsCreate := err != nil // pgx.ErrNoRows → create fresh

	if !needsCreate && existingChecksum == checksum {
		return nil // already registered with matching definition
	}

	quoted := quoteIdent(mv.name)
	if !needsCreate {
		if _, err := mv.client.Pool().Exec(ctx, `DROP MATERIALIZED VIEW IF EXISTS `+quoted); err != nil {
			return fmt.Errorf("pwrap: matview: drop stale: %w", err)
		}
	}
	if _, err := mv.client.Pool().Exec(ctx,
		`CREATE MATERIALIZED VIEW IF NOT EXISTS `+quoted+` AS `+definition+` WITH NO DATA`,
	); err != nil {
		return fmt.Errorf("pwrap: matview: create: %w", err)
	}
	// Unique index is required for REFRESH CONCURRENTLY; we don't synthesize one
	// (can't know the right columns). Users pick that up in a follow-up SQL if needed.
	_, err = mv.client.Pool().Exec(ctx, `
		INSERT INTO pwrap_matviews (name, definition, checksum)
		VALUES ($1, $2, $3)
		ON CONFLICT (name) DO UPDATE
		    SET definition = EXCLUDED.definition,
		        checksum   = EXCLUDED.checksum,
		        updated_at = now()
	`, mv.name, definition, checksum)
	if err != nil {
		return fmt.Errorf("pwrap: matview: registry: %w", err)
	}
	return nil
}

// Refresh runs `REFRESH MATERIALIZED VIEW [CONCURRENTLY] <name>` and updates the registry.
// concurrent == true requires a unique index on the matview (Postgres requirement).
func (mv *Matview) Refresh(ctx context.Context) error {
	return mv.refresh(ctx, false)
}

// RefreshConcurrent is like Refresh but uses CONCURRENTLY — no ACCESS EXCLUSIVE lock.
// Only works once the matview has been populated at least once AND has a unique index.
func (mv *Matview) RefreshConcurrent(ctx context.Context) error {
	return mv.refresh(ctx, true)
}

func (mv *Matview) refresh(ctx context.Context, concurrent bool) error {
	if err := validateIdent(mv.name); err != nil {
		return err
	}
	sql := `REFRESH MATERIALIZED VIEW `
	if concurrent {
		sql += `CONCURRENTLY `
	}
	sql += quoteIdent(mv.name)

	_, execErr := mv.client.Pool().Exec(ctx, sql)
	// Always update the registry — success writes last_refresh_at + clears error;
	// failure records the error message.
	var errMsg *string
	if execErr != nil {
		s := execErr.Error()
		errMsg = &s
	}
	_, _ = mv.client.Pool().Exec(ctx, `
		UPDATE pwrap_matviews
		   SET last_refresh_at = CASE WHEN $2::text IS NULL THEN now() ELSE last_refresh_at END,
		       last_error      = $2,
		       updated_at      = now()
		 WHERE name = $1
	`, mv.name, errMsg)
	if execErr != nil {
		return fmt.Errorf("pwrap: matview refresh: %w", execErr)
	}
	return nil
}

// Drop removes the matview and its registry row.
func (mv *Matview) Drop(ctx context.Context) error {
	if err := validateIdent(mv.name); err != nil {
		return err
	}
	if _, err := mv.client.Pool().Exec(ctx, `DROP MATERIALIZED VIEW IF EXISTS `+quoteIdent(mv.name)); err != nil {
		return err
	}
	_, err := mv.client.Pool().Exec(ctx, `DELETE FROM pwrap_matviews WHERE name = $1`, mv.name)
	return err
}

// Info returns the current registry row for this matview.
func (mv *Matview) Info(ctx context.Context) (MatviewInfo, error) {
	var info MatviewInfo
	var lastErr *string
	err := mv.client.Pool().QueryRow(ctx, `
		SELECT name, definition, checksum, last_refresh_at, last_error, enabled, created_at, updated_at
		FROM pwrap_matviews WHERE name = $1
	`, mv.name).Scan(
		&info.Name, &info.Definition, &info.Checksum,
		&info.LastRefreshAt, &lastErr, &info.Enabled, &info.CreatedAt, &info.UpdatedAt,
	)
	if err != nil {
		return MatviewInfo{}, err
	}
	if lastErr != nil {
		info.LastError = *lastErr
	}
	return info, nil
}

// --- helpers -----------------------------------------------------------------

// validateIdent is a belt-and-suspenders check. Matview names are embedded into SQL
// (not parameterisable for DDL), so we reject anything that isn't [A-Za-z0-9_]{1,63}.
// Any valid Postgres identifier is accepted; exotic quoted names aren't.
func validateIdent(s string) error {
	if s == "" || len(s) > 63 {
		return errors.New("pwrap: matview: name must be 1..63 chars")
	}
	for i, r := range s {
		ok := (r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') ||
			r == '_'
		if !ok {
			return fmt.Errorf("pwrap: matview: invalid char %q at pos %d", r, i)
		}
	}
	return nil
}

func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}
