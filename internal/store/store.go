package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// NewPool opens a pgx connection pool against the given DSN.
// Used by pwrapd for its own schema; per-tenant pools live in a separate cache (added in M2).
//
// The pool's ConnConfig is wired with a QueryTracer so every Query/Exec gets an
// OpenTelemetry span. Spans are no-ops until telemetry.Init has installed an
// exporter; instrumentation is always-on so tracing toggles without a redeploy.
func NewPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	cfg.ConnConfig.Tracer = NewQueryTracer()
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("new pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return pool, nil
}
