// Package realtime delivers tenant change events to subscribed clients.
//
// Architecture:
//
//   triggers in each tenant schema → pwrap_change_log + pg_notify('pwrap_changes', minimal payload)
//                              │
//                              ▼
//        Hub (one per pwrapd process) LISTENs on pwrap_changes,
//        fetches the full change row from the right tenant's pwrap_change_log
//        (using the schema name in the payload), then fans out to filtered Subscribers.
//
//   Subscribers (one per active WebSocket / SDK Subscribe call) declare
//   {schema, table, user_id} filters; non-matching events get skipped.
//
// One Hub serves all tenants. Single LISTEN connection on the admin pool.
package realtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ChangeEvent is what subscribers receive. Field names match the JSON wire format
// the SDK sees, so this struct doubles as the WebSocket payload after a json.Marshal.
type ChangeEvent struct {
	LogID     int64           `json:"log_id"`
	Schema    string          `json:"schema"`
	Table     string          `json:"table"`
	Op        string          `json:"op"`
	RowID     string          `json:"row_id,omitempty"`
	UserID    string          `json:"user_id,omitempty"`
	Before    json.RawMessage `json:"before,omitempty"`
	After     json.RawMessage `json:"after,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
}

// Filter restricts which events a Subscriber receives. Empty Table or UserID
// means "all". Schema is required (== tenant schema name == project's pg_schema).
type Filter struct {
	Schema string
	Table  string
	UserID string
}

func (f Filter) matches(ev ChangeEvent) bool {
	if f.Schema != ev.Schema {
		return false
	}
	if f.Table != "" && f.Table != ev.Table {
		return false
	}
	if f.UserID != "" && f.UserID != ev.UserID {
		return false
	}
	return true
}

// Subscriber is a sink for matching events. Events is a send-only channel that
// the caller must drain — slow consumers risk dropped events (see fanout policy).
type Subscriber struct {
	Filter Filter
	Events chan ChangeEvent
}

// Hub fans out events to subscribers. Construct with NewHub then call Start to
// kick off the LISTEN loop. Start is idempotent. Call Stop on shutdown so the
// goroutine releases its pool connection (otherwise pool.Close blocks).
type Hub struct {
	pool   *pgxpool.Pool
	logger *slog.Logger

	mu      sync.RWMutex
	subs    map[*Subscriber]struct{}
	started atomic.Bool

	stopOnce sync.Once
	cancel   context.CancelFunc
	done     chan struct{}

	// dropCount tracks how many events were dropped due to slow subscribers — for
	// telemetry/visibility, not correctness. Exported via Stats().
	dropCount atomic.Int64
}

func NewHub(pool *pgxpool.Pool, logger *slog.Logger) *Hub {
	if logger == nil {
		logger = slog.Default()
	}
	return &Hub{pool: pool, logger: logger, subs: map[*Subscriber]struct{}{}}
}

// Start runs the LISTEN loop until ctx is cancelled or Stop is called. Idempotent.
// Errors during LISTEN trigger a brief backoff + reconnect.
func (h *Hub) Start(ctx context.Context) {
	if !h.started.CompareAndSwap(false, true) {
		return
	}
	loopCtx, cancel := context.WithCancel(ctx)
	h.cancel = cancel
	h.done = make(chan struct{})
	go func() {
		defer close(h.done)
		for {
			if err := h.runOnce(loopCtx); err != nil {
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return
				}
				h.logger.Warn("realtime: listen loop exited", "err", err)
				select {
				case <-loopCtx.Done():
					return
				case <-time.After(time.Second):
				}
			} else {
				return
			}
		}
	}()
}

// Stop cancels the LISTEN loop and waits for the goroutine to release its
// pool connection. Idempotent. Safe to call before Start or after the loop
// already exited.
func (h *Hub) Stop() {
	h.stopOnce.Do(func() {
		if h.cancel != nil {
			h.cancel()
		}
		if h.done != nil {
			<-h.done
		}
	})
}

// Subscribe registers a Subscriber and returns its deregister func. Call the
// returned func when the subscriber goes away (e.g. on WebSocket close).
func (h *Hub) Subscribe(s *Subscriber) func() {
	h.mu.Lock()
	h.subs[s] = struct{}{}
	h.mu.Unlock()
	return func() {
		h.mu.Lock()
		delete(h.subs, s)
		h.mu.Unlock()
	}
}

// Stats reports current subscriber count and lifetime dropped-event count.
func (h *Hub) Stats() (subscribers int, dropped int64) {
	h.mu.RLock()
	subscribers = len(h.subs)
	h.mu.RUnlock()
	return subscribers, h.dropCount.Load()
}

// --- internal ----------------------------------------------------------------

func (h *Hub) runOnce(ctx context.Context) error {
	conn, err := h.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "LISTEN pwrap_changes"); err != nil {
		return fmt.Errorf("LISTEN: %w", err)
	}
	h.logger.Info("realtime: hub listening")

	for {
		n, err := conn.Conn().WaitForNotification(ctx)
		if err != nil {
			return err
		}
		ev, err := h.hydrate(ctx, n.Payload)
		if err != nil {
			h.logger.Warn("realtime: hydrate failed", "err", err, "payload", n.Payload)
			continue
		}
		h.fanout(ev)
	}
}

// hydrate turns a NOTIFY payload + a SELECT on the tenant's pwrap_change_log into
// a full ChangeEvent. The NOTIFY carries only metadata + log_id; we look up the
// row in the correct schema to get before/after blobs.
type notifyPayload struct {
	LogID  int64  `json:"log_id"`
	Schema string `json:"schema"`
	Table  string `json:"table"`
	Op     string `json:"op"`
	RowID  string `json:"row_id"`
	UserID string `json:"user_id"`
}

func (h *Hub) hydrate(ctx context.Context, raw string) (ChangeEvent, error) {
	var p notifyPayload
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return ChangeEvent{}, fmt.Errorf("decode payload: %w", err)
	}
	if p.Schema == "" {
		return ChangeEvent{}, errors.New("payload missing schema")
	}
	// Schema name came from TG_TABLE_SCHEMA inside Postgres — already validated as
	// a real identifier. pgx.Identifier{}.Sanitize handles quoting.
	q := fmt.Sprintf(
		`SELECT before::text, after::text, created_at
		   FROM %s.pwrap_change_log
		  WHERE id = $1`,
		pgx.Identifier{p.Schema}.Sanitize(),
	)
	var before, after *string
	var createdAt time.Time
	if err := h.pool.QueryRow(ctx, q, p.LogID).Scan(&before, &after, &createdAt); err != nil {
		return ChangeEvent{}, fmt.Errorf("fetch log id=%d: %w", p.LogID, err)
	}
	ev := ChangeEvent{
		LogID:     p.LogID,
		Schema:    p.Schema,
		Table:     p.Table,
		Op:        p.Op,
		RowID:     p.RowID,
		UserID:    p.UserID,
		CreatedAt: createdAt,
	}
	if before != nil {
		ev.Before = json.RawMessage(*before)
	}
	if after != nil {
		ev.After = json.RawMessage(*after)
	}
	return ev, nil
}

func (h *Hub) fanout(ev ChangeEvent) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for sub := range h.subs {
		if !sub.Filter.matches(ev) {
			continue
		}
		// Non-blocking send. Slow consumers drop events — by design; the alternative
		// is unbounded buffering, which trades memory for latency in pathological cases.
		select {
		case sub.Events <- ev:
		default:
			h.dropCount.Add(1)
		}
	}
}
