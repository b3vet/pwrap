package pwrap

import (
	"context"
	"fmt"
)

// EnableChangeCapture attaches pwrap's INSERT/UPDATE/DELETE trigger to the given
// table so its mutations land in pwrap_change_log + emit pg_notify('pwrap_changes').
// pwrap_documents already has the trigger attached at migration time, so this is
// only needed for user tables (created via `pwrap sql apply`) you want to subscribe to.
//
// The target table must have an `id` column (any type, cast to text in the log).
// If your table uses a different primary key column, capture it under `data->>'id'`
// or refactor to add an id column.
//
// Idempotent: re-running silently no-ops via DROP TRIGGER IF EXISTS + CREATE.
func (c *Client) EnableChangeCapture(ctx context.Context, table string) error {
	if err := validateIdent(table); err != nil {
		return fmt.Errorf("pwrap: change capture: %w", err)
	}
	pool := c.Pool()
	q := quoteIdent(table)
	trig := "pwrap_capture_" + table
	if err := validateIdent(trig); err != nil {
		return fmt.Errorf("pwrap: change capture: trigger name: %w", err)
	}
	tq := quoteIdent(trig)
	if _, err := pool.Exec(ctx, `DROP TRIGGER IF EXISTS `+tq+` ON `+q); err != nil {
		return fmt.Errorf("pwrap: change capture: drop existing: %w", err)
	}
	_, err := pool.Exec(ctx, `
		CREATE TRIGGER `+tq+`
		AFTER INSERT OR UPDATE OR DELETE
		ON `+q+`
		FOR EACH ROW
		EXECUTE FUNCTION pwrap_capture_change()
	`)
	if err != nil {
		return fmt.Errorf("pwrap: change capture: %w", err)
	}
	return nil
}

// DisableChangeCapture removes the trigger from the given table. The pwrap_change_log
// rows for prior events remain — drop them yourself if you want a clean slate.
func (c *Client) DisableChangeCapture(ctx context.Context, table string) error {
	if err := validateIdent(table); err != nil {
		return fmt.Errorf("pwrap: change capture: %w", err)
	}
	q := quoteIdent(table)
	trig := quoteIdent("pwrap_capture_" + table)
	_, err := c.Pool().Exec(ctx, `DROP TRIGGER IF EXISTS `+trig+` ON `+q)
	return err
}
