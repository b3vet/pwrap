package pwrap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SchemaVersion is the minimum tenant migration version this SDK expects.
// The Client refuses to proceed if the project hasn't been migrated to at least this version.
const SchemaVersion = "0001_init"

// refreshLead is how far ahead of expiry we proactively fetch a new DSN.
// Keep this comfortably below the 1-hour minimum TTL the control plane mints.
const refreshLead = 2 * time.Minute

type Config struct {
	// ControlURL is the pwrapd base URL. Defaults to "http://localhost:8080".
	ControlURL string
	// APIKey is a project API key (pwk_...). Required.
	APIKey string
	// HTTPClient lets callers inject a client (for retries, instrumentation, etc).
	// If nil, a client with a 15s timeout is used.
	HTTPClient *http.Client
	// Logger is used for refresh failures and other background-goroutine events.
	// Defaults to slog.Default().
	Logger *slog.Logger
}

// Client is the root handle against a pwrap project.
type Client struct {
	cfg    Config
	http   *http.Client
	schema string
	logger *slog.Logger

	// userID, when non-empty (set via WithUser), causes Table operations to run
	// inside a short-lived tx with `request.jwt.claims.user_id` set — so RLS
	// policies referencing the same claim apply identically whether the request
	// came through the SDK or PostgREST.
	userID string

	// conn is a *pointer* to shared state so WithUser clones share the same pool
	// and refresh-driven DSN updates. Never assign to c.conn directly after construction.
	conn *connState

	// Background refresh lifecycle. refreshCtx cancels the loop on Close;
	// refreshDone signals the loop has exited. Only the root Client (not WithUser
	// clones) owns these — clones leave them nil.
	refreshCtx    context.Context
	refreshCancel context.CancelFunc
	refreshDone   chan struct{}
}

// connState holds shared, mutable-by-refresh handles. Wrapped in a pointer so
// WithUser shallow-clones safely share it.
type connState struct {
	pool    atomic.Pointer[pgxpool.Pool]
	mu      sync.RWMutex
	dsn     string
	expires time.Time
}

// querier is the minimal subset of pgxpool.Pool / pgx.Tx used by Table, Vector, Queue.
// Having both satisfy this interface lets `Client.run` transparently swap in a tx
// when an RLS context is attached.
type querier interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

type connectionResp struct {
	DSN           string    `json:"dsn"`
	Schema        string    `json:"schema"`
	SchemaVersion string    `json:"schema_version"`
	ExpiresAt     time.Time `json:"expires_at"`
}

// New bootstraps the SDK: exchanges the API key at /v1/connection and opens a pgx pool.
// On success, also starts a background goroutine that refreshes the DSN ~2 minutes
// before it expires. Call Close to stop the goroutine and release pool resources.
func New(ctx context.Context, cfg Config) (*Client, error) {
	if cfg.ControlURL == "" {
		cfg.ControlURL = "http://localhost:8080"
	}
	if cfg.APIKey == "" {
		return nil, errors.New("pwrap: APIKey is required")
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 15 * time.Second}
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	conn, err := exchange(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := checkSchemaVersion(conn.SchemaVersion); err != nil {
		return nil, err
	}

	pool, err := pgxpool.New(ctx, conn.DSN)
	if err != nil {
		return nil, fmt.Errorf("pwrap: open pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pwrap: ping: %w", err)
	}

	refreshCtx, refreshCancel := context.WithCancel(context.Background())
	cs := &connState{dsn: conn.DSN, expires: conn.ExpiresAt}
	cs.pool.Store(pool)
	c := &Client{
		cfg:           cfg,
		http:          cfg.HTTPClient,
		schema:        conn.Schema,
		logger:        cfg.Logger,
		conn:          cs,
		refreshCtx:    refreshCtx,
		refreshCancel: refreshCancel,
		refreshDone:   make(chan struct{}),
	}
	go c.refreshLoop()
	return c, nil
}

// Close stops the refresh goroutine and releases the underlying pool. Safe to call
// once per Client. Derived clients from WithUser share state — do not call Close
// on them.
func (c *Client) Close() {
	if c.userID != "" {
		// WithUser clones share the lifecycle of the parent; do nothing.
		return
	}
	if c.refreshCancel != nil {
		c.refreshCancel()
		<-c.refreshDone
	}
	if p := c.conn.pool.Load(); p != nil {
		p.Close()
	}
}

// Pool exposes the live pgx pool. Callers that hold this pointer across a DSN
// refresh will be using a closed pool — re-load via Client.Pool() per operation
// if you keep a long-lived reference.
func (c *Client) Pool() *pgxpool.Pool { return c.conn.pool.Load() }

// Schema returns the tenant schema name.
func (c *Client) Schema() string { return c.schema }

// ExpiresAt returns when the current DSN will need to be refreshed.
func (c *Client) ExpiresAt() time.Time {
	c.conn.mu.RLock()
	defer c.conn.mu.RUnlock()
	return c.conn.expires
}

// WithUser returns a derived Client whose Table operations execute inside a short-lived
// tx with `request.jwt.claims.user_id` set to the given value. Shares the underlying
// pool and refresh lifecycle — do not call Close on the returned Client.
func (c *Client) WithUser(userID string) *Client {
	clone := *c
	clone.userID = userID
	// Don't share the refresh lifecycle handles; the parent owns them.
	clone.refreshCtx = nil
	clone.refreshCancel = nil
	clone.refreshDone = nil
	return &clone
}

// UserID returns the user id bound via WithUser (empty if none).
func (c *Client) UserID() string { return c.userID }

// run invokes fn against a querier. Without a bound user, that's the pool directly
// (no per-query cost). With a bound user, fn runs inside a short tx that first
// sets request.jwt.claims.user_id.
func (c *Client) run(ctx context.Context, fn func(context.Context, querier) error) error {
	pool := c.conn.pool.Load()
	if c.userID == "" {
		return fn(ctx, pool)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	claims, err := json.Marshal(map[string]string{"user_id": c.userID})
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('request.jwt.claims', $1, true)`, string(claims)); err != nil {
		return fmt.Errorf("pwrap: set_config: %w", err)
	}
	if err := fn(ctx, tx); err != nil {
		return fmt.Errorf("pwrap: body: %w", err)
	}
	return tx.Commit(ctx)
}

// --- background DSN refresh ---------------------------------------------------

func (c *Client) refreshLoop() {
	defer close(c.refreshDone)
	for {
		c.conn.mu.RLock()
		expiresAt := c.conn.expires
		c.conn.mu.RUnlock()

		wake := time.Until(expiresAt.Add(-refreshLead))
		// Floor to 30s so we don't spin in case of clock skew or a freshly-expired
		// DSN at startup.
		if wake < 30*time.Second {
			wake = 30 * time.Second
		}
		timer := time.NewTimer(wake)
		select {
		case <-c.refreshCtx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}

		if err := c.refresh(c.refreshCtx); err != nil {
			c.logger.Warn("pwrap: dsn refresh failed", "err", err)
			// Retry after a minute. The current pool keeps working until the DSN's
			// underlying password actually rotates server-side.
			select {
			case <-c.refreshCtx.Done():
				return
			case <-time.After(time.Minute):
			}
		}
	}
}

// refresh fetches a new DSN, opens a new pool, swaps it atomically, and closes
// the old one. Errors leave the existing pool in place.
func (c *Client) refresh(ctx context.Context) error {
	conn, err := exchange(ctx, c.cfg)
	if err != nil {
		return err
	}
	newPool, err := pgxpool.New(ctx, conn.DSN)
	if err != nil {
		return err
	}
	if err := newPool.Ping(ctx); err != nil {
		newPool.Close()
		return err
	}
	old := c.conn.pool.Swap(newPool)
	c.conn.mu.Lock()
	c.conn.dsn = conn.DSN
	c.conn.expires = conn.ExpiresAt
	c.conn.mu.Unlock()
	c.logger.Info("pwrap: dsn refreshed", "expires_at", conn.ExpiresAt)
	if old != nil {
		// Brief grace period before closing the old pool, in case any in-flight
		// queries are still using it. pgxpool.Close blocks until idle, so even
		// without this delay no query gets cut short — but the delay avoids the
		// log noise from queries holding the old pool past Swap.
		go func(p *pgxpool.Pool) {
			time.Sleep(5 * time.Second)
			p.Close()
		}(old)
	}
	return nil
}

// --- HTTP exchange ------------------------------------------------------------

// checkSchemaVersion refuses to proceed when the tenant's applied migration is
// behind what this SDK expects. Lexicographic compare works because pwrap's tenant
// migrations are named like "0001_init", "0002_foo" — the digits sort correctly.
func checkSchemaVersion(serverVersion string) error {
	if serverVersion == "" {
		return fmt.Errorf("pwrap: project has no migrations applied — run `pwrap migrate apply --project <id>`")
	}
	if serverVersion < SchemaVersion {
		return fmt.Errorf("pwrap: tenant schema is %q but this SDK requires %q — run `pwrap migrate apply --project <id>` to upgrade", serverVersion, SchemaVersion)
	}
	return nil
}

func exchange(ctx context.Context, cfg Config) (connectionResp, error) {
	base := strings.TrimRight(cfg.ControlURL, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/connection", nil)
	if err != nil {
		return connectionResp{}, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.APIKey)

	resp, err := cfg.HTTPClient.Do(req)
	if err != nil {
		return connectionResp{}, fmt.Errorf("pwrap: control plane: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return connectionResp{}, fmt.Errorf("pwrap: /v1/connection: %s %s", resp.Status, bytes.TrimSpace(b))
	}
	var out connectionResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return connectionResp{}, fmt.Errorf("pwrap: decode connection: %w", err)
	}
	return out, nil
}
