//go:build integration

package integration

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"

	tenantmigrations "github.com/b3vet/pwrap/migrations/tenant"
	"github.com/b3vet/pwrap/sdk/go/pwrap"
)

// sdkFor provisions a project and returns a connected SDK client.
func sdkFor(ctx context.Context, t *testing.T, name string) *pwrap.Client {
	t.Helper()
	_, key := provision(ctx, t, name)
	c, err := pwrap.New(ctx, pwrap.Config{ControlURL: sharedStack.ControlURL, APIKey: key})
	if err != nil {
		t.Fatalf("sdk connect: %v", err)
	}
	t.Cleanup(c.Close)
	return c
}

// --- queue -------------------------------------------------------------------

func TestQueue_EnqueueAndStats(t *testing.T) {
	ctx := context.Background()
	c := sdkFor(ctx, t, "queue-suite")
	q := c.Queue()

	before, err := q.Stats(ctx)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}

	id, err := q.Enqueue(ctx, pwrap.EnqueueRequest{
		Kind: "embed",
		Args: map[string]any{"doc": "abc"},
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if id <= 0 {
		t.Errorf("enqueue returned id %d, want a positive river_job.id", id)
	}

	after, err := q.Stats(ctx)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if after.Available != before.Available+1 {
		t.Errorf("available = %d, want %d", after.Available, before.Available+1)
	}

	// Jobs land in river_job in the shape River's own workers expect. If this
	// drifts, jobs enqueued by any SDK become unrunnable by a Go worker.
	var kind, queue string
	var priority, maxAttempts int
	var args []byte
	if err := c.Pool().QueryRow(ctx,
		`SELECT kind, queue, priority, max_attempts, args FROM river_job WHERE id = $1`, id,
	).Scan(&kind, &queue, &priority, &maxAttempts, &args); err != nil {
		t.Fatalf("read back job: %v", err)
	}
	if kind != "embed" {
		t.Errorf("kind = %q, want %q", kind, "embed")
	}
	if queue != "default" {
		t.Errorf("queue = %q, want the default queue", queue)
	}
	if priority != 1 {
		t.Errorf("priority = %d, want 1", priority)
	}
	if maxAttempts != 25 {
		t.Errorf("max_attempts = %d, want River's default of 25", maxAttempts)
	}
	// River decodes args into a struct, so it must be a JSON object.
	if !strings.HasPrefix(strings.TrimSpace(string(args)), "{") {
		t.Errorf("args = %s, want a JSON object", args)
	}
}

func TestQueue_RejectsEmptyKind(t *testing.T) {
	ctx := context.Background()
	c := sdkFor(ctx, t, "queue-validation")
	if _, err := c.Queue().Enqueue(ctx, pwrap.EnqueueRequest{Args: map[string]any{}}); err == nil {
		t.Fatal("enqueue with empty Kind = nil error, want error")
	}
}

// --- vector ------------------------------------------------------------------

// embedding builds a deterministic unit-ish vector so distances are stable.
func embedding(seed int64) []float32 {
	r := rand.New(rand.NewSource(seed))
	v := make([]float32, pwrap.VectorDim)
	for i := range v {
		v[i] = float32(r.NormFloat64())
	}
	return v
}

func TestVector_UpsertAndSearch(t *testing.T) {
	ctx := context.Background()
	c := sdkFor(ctx, t, "vector-suite")
	tbl := c.Table("docs")
	v := c.Vector("docs")

	// Three documents with distinct embeddings; querying with one of them back
	// must rank that document first.
	ids := make([]string, 3)
	for i := range ids {
		id, err := tbl.Insert(ctx, map[string]any{"n": i})
		if err != nil {
			t.Fatalf("insert doc %d: %v", i, err)
		}
		ids[i] = id.String()
		if err := v.Upsert(ctx, ids[i], embedding(int64(i+1)), map[string]any{"n": i}); err != nil {
			t.Fatalf("upsert vector %d: %v", i, err)
		}
	}

	matches, err := v.Search(ctx, embedding(2), 3)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(matches) == 0 {
		t.Fatal("search returned no matches")
	}
	if matches[0].DocID != ids[1] {
		t.Errorf("nearest = %s, want %s (the document whose embedding was queried)", matches[0].DocID, ids[1])
	}
	if matches[0].Distance > 1e-3 {
		t.Errorf("distance to itself = %v, want ~0", matches[0].Distance)
	}
	// Results must come back nearest-first.
	for i := 1; i < len(matches); i++ {
		if matches[i].Distance < matches[i-1].Distance {
			t.Errorf("results not ordered by distance: %v then %v", matches[i-1].Distance, matches[i].Distance)
		}
	}
}

func TestVector_RejectsWrongDimension(t *testing.T) {
	ctx := context.Background()
	c := sdkFor(ctx, t, "vector-dim")
	id, err := c.Table("docs").Insert(ctx, map[string]any{"x": 1})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := c.Vector("docs").Upsert(ctx, id.String(), []float32{0.1, 0.2}, nil); err == nil {
		t.Fatalf("upsert with %d dims = nil error, want a dimension error", 2)
	}
}

func TestVector_UpsertIsIdempotent(t *testing.T) {
	ctx := context.Background()
	c := sdkFor(ctx, t, "vector-upsert")
	id, err := c.Table("docs").Insert(ctx, map[string]any{"x": 1})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	v := c.Vector("docs")
	for i := 0; i < 2; i++ {
		if err := v.Upsert(ctx, id.String(), embedding(7), map[string]any{"pass": i}); err != nil {
			t.Fatalf("upsert pass %d: %v", i, err)
		}
	}
	var n int
	if err := c.Pool().QueryRow(ctx,
		`SELECT count(*) FROM pwrap_embeddings WHERE doc_id = $1`, id).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("got %d embedding rows after two upserts, want 1", n)
	}
}

// --- materialized views -------------------------------------------------------

func TestMatview_RegisterRefreshAndInfo(t *testing.T) {
	ctx := context.Background()
	c := sdkFor(ctx, t, "matview-suite")

	tbl := c.Table("posts")
	for _, topic := range []string{"go", "go", "sql"} {
		if _, err := tbl.Insert(ctx, map[string]any{"topic": topic}); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	mv := c.Matview("topic_counts")
	if err := mv.Register(ctx,
		`SELECT data->>'topic' AS topic, count(*) AS n FROM pwrap_documents GROUP BY 1`); err != nil {
		t.Fatalf("register: %v", err)
	}

	// Created WITH NO DATA, so it isn't queryable until the first refresh.
	info, err := mv.Info(ctx)
	if err != nil {
		t.Fatalf("info: %v", err)
	}
	if info.LastRefreshAt != nil {
		t.Errorf("last_refresh_at = %v before any refresh, want nil", info.LastRefreshAt)
	}
	if !info.Enabled {
		t.Error("matview registered as disabled")
	}

	if err := mv.Refresh(ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	info, err = mv.Info(ctx)
	if err != nil {
		t.Fatalf("info after refresh: %v", err)
	}
	if info.LastRefreshAt == nil {
		t.Error("last_refresh_at still nil after refresh")
	}
	if info.LastError != "" {
		t.Errorf("last_error = %q, want empty", info.LastError)
	}

	var n int
	if err := c.Pool().QueryRow(ctx,
		`SELECT n FROM topic_counts WHERE topic = 'go'`).Scan(&n); err != nil {
		t.Fatalf("query matview: %v", err)
	}
	if n != 2 {
		t.Errorf("topic_counts['go'] = %d, want 2", n)
	}
}

func TestMatview_RefreshPicksUpNewRows(t *testing.T) {
	ctx := context.Background()
	c := sdkFor(ctx, t, "matview-refresh")

	tbl := c.Table("posts")
	if _, err := tbl.Insert(ctx, map[string]any{"topic": "go"}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	mv := c.Matview("counts")
	if err := mv.Register(ctx, `SELECT count(*) AS n FROM pwrap_documents`); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := mv.Refresh(ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	if _, err := tbl.Insert(ctx, map[string]any{"topic": "sql"}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Stale until refreshed — that's the contract, and worth pinning.
	var n int
	if err := c.Pool().QueryRow(ctx, `SELECT n FROM counts`).Scan(&n); err != nil {
		t.Fatalf("query: %v", err)
	}
	if n != 1 {
		t.Errorf("matview returned %d before refresh, want the stale value 1", n)
	}

	if err := mv.Refresh(ctx); err != nil {
		t.Fatalf("second refresh: %v", err)
	}
	if err := c.Pool().QueryRow(ctx, `SELECT n FROM counts`).Scan(&n); err != nil {
		t.Fatalf("query: %v", err)
	}
	if n != 2 {
		t.Errorf("matview returned %d after refresh, want 2", n)
	}
}

func TestMatview_RejectsBadIdentifier(t *testing.T) {
	ctx := context.Background()
	c := sdkFor(ctx, t, "matview-ident")
	if err := c.Matview(`bad"; DROP TABLE pwrap_documents; --`).
		Register(ctx, `SELECT 1 AS x`); err == nil {
		t.Fatal("Register with an injected identifier = nil error, want rejection")
	}
}

// --- migrations ---------------------------------------------------------------

// The .down.sql files ship in the binary and have never been executed. A
// rollback that doesn't parse is only discovered when someone needs it.
func TestMigrations_DownThenUpRoundTrip(t *testing.T) {
	ctx := context.Background()
	c := sdkFor(ctx, t, "migrate-roundtrip")

	if _, err := c.Table("things").Insert(ctx, map[string]any{"a": 1}); err != nil {
		t.Fatalf("insert before rollback: %v", err)
	}

	down, err := tenantmigrations.FS.ReadFile("0001_init.down.sql")
	if err != nil {
		t.Fatalf("read down migration: %v", err)
	}
	up, err := tenantmigrations.FS.ReadFile("0001_init.up.sql")
	if err != nil {
		t.Fatalf("read up migration: %v", err)
	}

	if _, err := c.Pool().Exec(ctx, string(down)); err != nil {
		t.Fatalf("down migration failed to apply: %v", err)
	}

	// The baseline tables should be gone.
	var exists bool
	if err := c.Pool().QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.tables
			WHERE table_schema = $1 AND table_name = 'pwrap_documents'
		)`, c.Schema()).Scan(&exists); err != nil {
		t.Fatalf("check table: %v", err)
	}
	if exists {
		t.Error("pwrap_documents still present after the down migration")
	}

	// And re-applying up must restore a working schema.
	if _, err := c.Pool().Exec(ctx, string(up)); err != nil {
		t.Fatalf("up migration failed to re-apply after rollback: %v", err)
	}
	if _, err := c.Table("things").Insert(ctx, map[string]any{"a": 2}); err != nil {
		t.Fatalf("insert after re-applying up: %v", err)
	}
}

// Applying migrations twice must be a no-op rather than an error — pwrapd calls
// this on branch create as well as on explicit `migrate apply`.
func TestMigrations_ApplyIsIdempotent(t *testing.T) {
	ctx := context.Background()
	pid, _ := provision(ctx, t, "migrate-idempotent")
	a := newAdmin()
	for i := 0; i < 2; i++ {
		if err := a.do(ctx, "POST", fmt.Sprintf("/v1/projects/%s/migrations", pid), nil, nil); err != nil {
			t.Fatalf("re-apply migrations (pass %d): %v", i, err)
		}
	}
}

// --- shutdown -----------------------------------------------------------------

// The realtime hub holds a dedicated LISTEN connection, and pgxpool.Close blocks
// until every connection is released. pwrapd shipped without calling StopHub, so
// SIGTERM never completed and each restart needed a SIGKILL — invisible in tests
// because nothing exercised the shutdown ordering.
func TestShutdown_StopHubReleasesThePoolConnection(t *testing.T) {
	ctx := context.Background()

	stack, err := NewStack(ctx)
	if err != nil {
		t.Fatalf("stack: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		stack.Close() // StopHub then pool.Close, the order pwrapd must use
	}()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("shutdown blocked for 30s — the hub is still holding a pool connection")
	}
}
