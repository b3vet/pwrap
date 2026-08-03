package pwrap

import (
	"context"
	"encoding/json"
	"fmt"
)

// EnqueueRequest is the parameters for enqueuing a single job.
// Args must marshal to a JSON object (River's worker decoder expects an object, not a scalar).
type EnqueueRequest struct {
	Kind        string // required; 1..127 chars
	Args        any    // required; json-encoded into river_job.args
	Queue       string // optional; default "default"
	Priority    int    // optional; 1 (highest) .. 4 (lowest). Default 1.
	MaxAttempts int    // optional; default 25 (River's default)
}

// Queue is a handle over the tenant's River-backed job queue.
// Enqueue inserts directly into river_job (language-neutral wire-compatible path).
// To register workers, use `github.com/riverqueue/river` against Client.Pool().
type Queue struct {
	client *Client
}

func (c *Client) Queue() *Queue { return &Queue{client: c} }

// Enqueue inserts one job and returns the river_job.id.
// The SDK uses direct SQL (not river.Client.Insert) so the exact same path works from the
// TypeScript SDK, which can't import River.
func (q *Queue) Enqueue(ctx context.Context, req EnqueueRequest) (int64, error) {
	if req.Kind == "" {
		return 0, fmt.Errorf("pwrap: queue: Kind is required")
	}
	args := req.Args
	if args == nil {
		args = map[string]any{}
	}
	argsJSON, err := json.Marshal(args)
	if err != nil {
		return 0, fmt.Errorf("pwrap: queue: marshal args: %w", err)
	}
	queue := req.Queue
	if queue == "" {
		queue = "default"
	}
	priority := req.Priority
	if priority == 0 {
		priority = 1
	}
	maxAttempts := req.MaxAttempts
	if maxAttempts == 0 {
		maxAttempts = 25
	}

	var id int64
	err = q.client.Pool().QueryRow(ctx, `
		INSERT INTO river_job (kind, args, queue, priority, max_attempts)
		VALUES ($1, $2::jsonb, $3, $4, $5)
		RETURNING id
	`, req.Kind, argsJSON, queue, priority, maxAttempts).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("pwrap: queue: enqueue: %w", err)
	}
	return id, nil
}

// Stats is a minimal snapshot of queue health for dashboards / CLI.
type Stats struct {
	Available int64 `json:"available"`
	Running   int64 `json:"running"`
	Scheduled int64 `json:"scheduled"`
	Completed int64 `json:"completed"`
	Discarded int64 `json:"discarded"`
	Retryable int64 `json:"retryable"`
}

// Stats counts river_job rows by state.
func (q *Queue) Stats(ctx context.Context) (Stats, error) {
	var s Stats
	rows, err := q.client.Pool().Query(ctx, `
		SELECT state::text, COUNT(*) FROM river_job GROUP BY state
	`)
	if err != nil {
		return s, err
	}
	defer rows.Close()
	for rows.Next() {
		var state string
		var n int64
		if err := rows.Scan(&state, &n); err != nil {
			return s, err
		}
		switch state {
		case "available":
			s.Available = n
		case "running":
			s.Running = n
		case "scheduled":
			s.Scheduled = n
		case "completed":
			s.Completed = n
		case "discarded":
			s.Discarded = n
		case "retryable":
			s.Retryable = n
		}
	}
	return s, rows.Err()
}
