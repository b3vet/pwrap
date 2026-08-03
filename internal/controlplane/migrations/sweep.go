package migrations

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// SweepStale marks any migration_log rows that are still in state='pending' but
// haven't been updated in `olderThan` as failed. This is the recovery path for the
// orphan-pending-row gap (known gap #1): if pwrapd dies between the INSERT pending
// and the completion UPDATE, the row sticks around forever otherwise. Running this
// on every pwrapd startup keeps the log clean and lets Apply re-run cleanly.
//
// Returns the number of rows touched.
func SweepStale(ctx context.Context, pool *pgxpool.Pool, olderThan time.Duration) (int64, error) {
	tag, err := pool.Exec(ctx, `
		UPDATE migration_log
		   SET state        = 'failed',
		       error        = 'orphaned: pwrapd died mid-apply; swept on startup',
		       completed_at = now()
		 WHERE state = 'pending'
		   AND started_at < now() - ($1::int || ' seconds')::interval
	`, int(olderThan.Seconds()))
	if err != nil {
		return 0, fmt.Errorf("migrations: sweep: %w", err)
	}
	return tag.RowsAffected(), nil
}
