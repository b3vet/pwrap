// Command conformance-go runs the cross-SDK conformance scenarios against a live
// pwrapd and writes report-go.json. See conformance/scenarios.json for the
// canonical scenario list and conformance/check.py for the parity gate.
//
// Each scenario provisions its own project so a failure can't cascade.
//
//	PWRAP_CONTROL_URL      default http://localhost:8080
//	PWRAP_BOOTSTRAP_TOKEN  default dev-admin
//	PWRAP_REPORT_DIR       where to write report-go.json (default: cwd)
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/b3vet/pwrap/sdk/go/pwrap"
)

var (
	controlURL = envOr("PWRAP_CONTROL_URL", "http://localhost:8080")
	adminToken = envOr("PWRAP_BOOTSTRAP_TOKEN", "dev-admin")
)

func main() {
	// Split out of main so deferred cleanup runs before os.Exit.
	os.Exit(run())
}

func run() int {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	scenarios := []struct {
		id  string
		run func(context.Context) error
	}{
		{"table_crud", scenarioTableCRUD},
		{"table_batch", scenarioTableBatch},
		{"vector_upsert_search", scenarioVectorUpsertSearch},
		{"vector_dim_validation", scenarioVectorDimValidation},
		{"queue_enqueue_stats", scenarioQueueEnqueueStats},
		{"geo_radius_nearest", scenarioGeoRadiusNearest},
		{"rest_token", scenarioRestToken},
		{"schema_version_guard", scenarioSchemaVersionGuard},
		{"rls_with_user", scenarioRLSWithUser},
		{"realtime_subscribe", scenarioRealtimeSubscribe},
		{"matview_register_refresh", scenarioMatviewRegisterRefresh},
	}

	results := map[string]string{}
	failed := 0
	for _, s := range scenarios {
		err := s.run(ctx)
		if err != nil {
			results[s.id] = "fail: " + err.Error()
			fmt.Printf("FAIL %s: %v\n", s.id, err)
			failed++
			continue
		}
		results[s.id] = "pass"
		fmt.Printf("ok   %s\n", s.id)
	}

	if err := writeReport(results); err != nil {
		fmt.Fprintln(os.Stderr, "write report:", err)
		return 1
	}
	fmt.Printf("\ngo: %d/%d scenarios passed\n", len(scenarios)-failed, len(scenarios))
	if failed > 0 {
		return 1
	}
	return 0
}

func writeReport(results map[string]string) error {
	dir := envOr("PWRAP_REPORT_DIR", ".")
	buf, err := json.MarshalIndent(map[string]any{
		"sdk":     "go",
		"results": results,
	}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "report-go.json"), append(buf, '\n'), 0o644)
}

// --- scenarios ----------------------------------------------------------------

func scenarioTableCRUD(ctx context.Context) error {
	c, cleanup, err := newProjectClient(ctx, "conf-table-crud")
	if err != nil {
		return err
	}
	defer cleanup()

	notes := c.Table("notes")
	id, err := notes.Insert(ctx, map[string]any{"title": "hello", "tags": []string{"a"}})
	if err != nil {
		return fmt.Errorf("insert: %w", err)
	}

	got, err := notes.Get(ctx, id)
	if err != nil {
		return fmt.Errorf("get: %w", err)
	}
	if got.Data["title"] != "hello" {
		return fmt.Errorf("title = %v, want hello", got.Data["title"])
	}

	rows, err := notes.Find(ctx, map[string]any{"tags": []string{"a"}}, 10)
	if err != nil {
		return fmt.Errorf("find: %w", err)
	}
	if len(rows) != 1 {
		return fmt.Errorf("find returned %d rows, want 1", len(rows))
	}

	if err := notes.Update(ctx, id, map[string]any{"pinned": true}); err != nil {
		return fmt.Errorf("update: %w", err)
	}
	got, err = notes.Get(ctx, id)
	if err != nil {
		return fmt.Errorf("get after update: %w", err)
	}
	if got.Data["pinned"] != true {
		return fmt.Errorf("pinned = %v, want true", got.Data["pinned"])
	}

	n, err := notes.Count(ctx, map[string]any{"title": "hello"})
	if err != nil {
		return fmt.Errorf("count: %w", err)
	}
	if n != 1 {
		return fmt.Errorf("count = %d, want 1", n)
	}

	if err := notes.Delete(ctx, id); err != nil {
		return fmt.Errorf("delete: %w", err)
	}
	if err := notes.Delete(ctx, id); err == nil {
		return errors.New("second delete succeeded, want an error")
	}
	return nil
}

func scenarioTableBatch(ctx context.Context) error {
	c, cleanup, err := newProjectClient(ctx, "conf-table-batch")
	if err != nil {
		return err
	}
	defer cleanup()

	notes := c.Table("notes")
	ids, err := notes.InsertMany(ctx, []any{
		map[string]any{"title": "a"},
		map[string]any{"title": "b"},
		map[string]any{"title": "c"},
	})
	if err != nil {
		return fmt.Errorf("insert_many: %w", err)
	}
	if len(ids) != 3 {
		return fmt.Errorf("got %d ids, want 3", len(ids))
	}
	// Order must match the input, not insertion race order.
	for i, want := range []string{"a", "b", "c"} {
		got, err := notes.Get(ctx, ids[i])
		if err != nil {
			return fmt.Errorf("get %d: %w", i, err)
		}
		if got.Data["title"] != want {
			return fmt.Errorf("ids[%d] has title %v, want %q — order not preserved", i, got.Data["title"], want)
		}
	}
	return nil
}

func scenarioVectorUpsertSearch(ctx context.Context) error {
	c, cleanup, err := newProjectClient(ctx, "conf-vector")
	if err != nil {
		return err
	}
	defer cleanup()

	v := c.Vector("notes")
	for _, e := range []struct {
		id   string
		seed float64
	}{{"a", 0}, {"b", 10}, {"c", 20}} {
		if err := v.Upsert(ctx, e.id, fakeEmbedding(e.seed), map[string]any{"name": e.id}); err != nil {
			return fmt.Errorf("upsert %s: %w", e.id, err)
		}
	}

	matches, err := v.Search(ctx, fakeEmbedding(0.01), 3)
	if err != nil {
		return fmt.Errorf("search: %w", err)
	}
	if len(matches) != 3 {
		return fmt.Errorf("got %d matches, want 3", len(matches))
	}
	if matches[0].DocID != "a" {
		return fmt.Errorf("nearest = %s, want a", matches[0].DocID)
	}
	for i := 1; i < len(matches); i++ {
		if matches[i].Distance < matches[i-1].Distance {
			return fmt.Errorf("distances not ascending: %v then %v", matches[i-1].Distance, matches[i].Distance)
		}
	}
	return nil
}

func scenarioVectorDimValidation(ctx context.Context) error {
	c, cleanup, err := newProjectClient(ctx, "conf-vector-dim")
	if err != nil {
		return err
	}
	defer cleanup()

	if err := c.Vector("notes").Upsert(ctx, "short", []float32{0.1, 0.2}, nil); err == nil {
		return errors.New("upsert with 2 dimensions succeeded, want a validation error")
	}
	return nil
}

func scenarioQueueEnqueueStats(ctx context.Context) error {
	c, cleanup, err := newProjectClient(ctx, "conf-queue")
	if err != nil {
		return err
	}
	defer cleanup()

	q := c.Queue()
	id, err := q.Enqueue(ctx, pwrap.EnqueueRequest{Kind: "embed", Args: map[string]any{"doc_id": "x"}})
	if err != nil {
		return fmt.Errorf("enqueue: %w", err)
	}
	if id <= 0 {
		return fmt.Errorf("job id = %d, want positive", id)
	}
	stats, err := q.Stats(ctx)
	if err != nil {
		return fmt.Errorf("stats: %w", err)
	}
	if stats.Available < 1 {
		return fmt.Errorf("available = %d, want >= 1", stats.Available)
	}
	return nil
}

func scenarioGeoRadiusNearest(ctx context.Context) error {
	c, cleanup, err := newProjectClient(ctx, "conf-geo")
	if err != nil {
		return err
	}
	defer cleanup()

	geo := c.Geo("places")
	for _, p := range []struct {
		lng, lat float64
		name     string
	}{
		{2.2945, 48.8584, "Eiffel"},
		{2.3499, 48.8530, "Notre-Dame"},
		{-73.9857, 40.7484, "Empire State"},
	} {
		if _, err := geo.InsertPoint(ctx, p.lng, p.lat, map[string]any{"name": p.name}); err != nil {
			return fmt.Errorf("insert %s: %w", p.name, err)
		}
	}

	near, err := geo.WithinRadius(ctx, 2.3522, 48.8566, 5000, 100)
	if err != nil {
		return fmt.Errorf("within_radius: %w", err)
	}
	names := make([]string, 0, len(near))
	for _, f := range near {
		names = append(names, fmt.Sprint(f.Metadata["name"]))
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "Eiffel,Notre-Dame" {
		return fmt.Errorf("within 5km of Paris = %v, want [Eiffel Notre-Dame]", names)
	}

	nearest, err := geo.Nearest(ctx, 2.3522, 48.8566, 1)
	if err != nil {
		return fmt.Errorf("nearest: %w", err)
	}
	if len(nearest) != 1 {
		return fmt.Errorf("nearest returned %d, want 1", len(nearest))
	}
	if n := fmt.Sprint(nearest[0].Metadata["name"]); n != "Eiffel" && n != "Notre-Dame" {
		return fmt.Errorf("nearest to Paris = %q, want a Paris landmark", n)
	}
	return nil
}

func scenarioRestToken(ctx context.Context) error {
	c, cleanup, err := newProjectClient(ctx, "conf-rest-token")
	if err != nil {
		return err
	}
	defer cleanup()

	tok, err := c.IssueRestToken(ctx, pwrap.RestTokenOpts{UserID: "someone", TTLSeconds: 60})
	if err != nil {
		return fmt.Errorf("issue: %w", err)
	}
	if !strings.HasPrefix(tok.Token, "ey") {
		return fmt.Errorf("token %q does not look like a JWT", tok.Token)
	}
	if tok.URL == "" {
		return errors.New("empty url")
	}
	if !strings.HasPrefix(tok.Role, "p_") {
		return fmt.Errorf("role = %q, want a p_ tenant role", tok.Role)
	}
	return nil
}

func scenarioSchemaVersionGuard(ctx context.Context) error {
	// Deliberately skip `migrate apply` — connecting must fail loudly.
	pid, err := createProject(ctx, "conf-unmigrated")
	if err != nil {
		return err
	}
	defer func() { _ = deleteProject(context.Background(), pid) }()

	key, err := issueKey(ctx, pid)
	if err != nil {
		return err
	}
	c, err := pwrap.New(ctx, pwrap.Config{ControlURL: controlURL, APIKey: key})
	if err == nil {
		c.Close()
		return errors.New("connected to an unmigrated project, want an error")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "migration") {
		return fmt.Errorf("error %q does not mention migrations", err)
	}
	return nil
}

func scenarioRLSWithUser(ctx context.Context) error {
	pid, err := createProject(ctx, "conf-rls")
	if err != nil {
		return err
	}
	defer func() { _ = deleteProject(context.Background(), pid) }()
	if err := applyMigrations(ctx, pid); err != nil {
		return err
	}
	if err := applySQL(ctx, pid, rlsPolicySQL); err != nil {
		return fmt.Errorf("apply rls: %w", err)
	}
	key, err := issueKey(ctx, pid)
	if err != nil {
		return err
	}
	c, err := pwrap.New(ctx, pwrap.Config{ControlURL: controlURL, APIKey: key})
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer c.Close()

	alice, bob := c.WithUser("alice"), c.WithUser("bob")
	if _, err := alice.Table("notes").Insert(ctx, map[string]any{"user_id": "alice", "n": 1}); err != nil {
		return fmt.Errorf("alice insert: %w", err)
	}
	if _, err := alice.Table("notes").Insert(ctx, map[string]any{"user_id": "alice", "n": 2}); err != nil {
		return fmt.Errorf("alice insert 2: %w", err)
	}
	if _, err := bob.Table("notes").Insert(ctx, map[string]any{"user_id": "bob", "n": 99}); err != nil {
		return fmt.Errorf("bob insert: %w", err)
	}

	for _, tc := range []struct {
		name string
		c    *pwrap.Client
		want int
	}{
		{"alice", alice, 2},
		{"bob", bob, 1},
		{"unscoped", c, 0},
	} {
		rows, err := tc.c.Table("notes").Find(ctx, nil, 100)
		if err != nil {
			return fmt.Errorf("%s find: %w", tc.name, err)
		}
		if len(rows) != tc.want {
			return fmt.Errorf("%s saw %d rows, want %d", tc.name, len(rows), tc.want)
		}
	}
	return nil
}

func scenarioRealtimeSubscribe(ctx context.Context) error {
	c, cleanup, err := newProjectClient(ctx, "conf-realtime")
	if err != nil {
		return err
	}
	defer cleanup()

	subCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	sub, err := c.Subscribe(subCtx, pwrap.SubscribeOpts{Table: "pwrap_documents"})
	if err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}
	defer sub.Close()

	// Subscribe blocks until the server's hello frame lands, so the insert
	// cannot race ahead of the subscription.
	if _, err := c.Table("live").Insert(ctx, map[string]any{"marker": "realtime"}); err != nil {
		return fmt.Errorf("insert: %w", err)
	}

	select {
	case ev, ok := <-sub.Changes():
		if !ok {
			return fmt.Errorf("event channel closed before an event arrived: %w", sub.Err())
		}
		if !strings.EqualFold(ev.Op, "INSERT") {
			return fmt.Errorf("op = %q, want INSERT", ev.Op)
		}
	case <-subCtx.Done():
		return errors.New("timed out waiting for the change event")
	}
	return nil
}

func scenarioMatviewRegisterRefresh(ctx context.Context) error {
	c, cleanup, err := newProjectClient(ctx, "conf-matview")
	if err != nil {
		return err
	}
	defer cleanup()

	if _, err := c.Table("posts").Insert(ctx, map[string]any{"topic": "go"}); err != nil {
		return fmt.Errorf("insert: %w", err)
	}
	mv := c.Matview("conf_counts")
	if err := mv.Register(ctx, `SELECT count(*) AS n FROM pwrap_documents`); err != nil {
		return fmt.Errorf("register: %w", err)
	}
	if err := mv.Refresh(ctx); err != nil {
		return fmt.Errorf("refresh: %w", err)
	}
	info, err := mv.Info(ctx)
	if err != nil {
		return fmt.Errorf("info: %w", err)
	}
	if info.LastRefreshAt == nil {
		return errors.New("last_refresh_at is nil after refresh")
	}
	return nil
}

const rlsPolicySQL = `
ALTER TABLE pwrap_documents ENABLE ROW LEVEL SECURITY;
ALTER TABLE pwrap_documents FORCE  ROW LEVEL SECURITY;
DROP POLICY IF EXISTS p ON pwrap_documents;
CREATE POLICY p ON pwrap_documents
    USING      (data->>'user_id' = NULLIF(current_setting('request.jwt.claims', true), '')::jsonb->>'user_id')
    WITH CHECK (data->>'user_id' = NULLIF(current_setting('request.jwt.claims', true), '')::jsonb->>'user_id');
`

// --- control-plane helpers ------------------------------------------------------

func newProjectClient(ctx context.Context, name string) (*pwrap.Client, func(), error) {
	pid, err := createProject(ctx, name)
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() { _ = deleteProject(context.Background(), pid) }
	if err := applyMigrations(ctx, pid); err != nil {
		cleanup()
		return nil, nil, err
	}
	key, err := issueKey(ctx, pid)
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	c, err := pwrap.New(ctx, pwrap.Config{ControlURL: controlURL, APIKey: key})
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("connect: %w", err)
	}
	return c, func() { c.Close(); cleanup() }, nil
}

func createProject(ctx context.Context, name string) (uuid.UUID, error) {
	var out struct {
		ID uuid.UUID `json:"id"`
	}
	// Unique suffix so reruns against a live stack don't 409.
	unique := fmt.Sprintf("%s-%s", name, uuid.New().String()[:8])
	if err := adminDo(ctx, http.MethodPost, "/v1/projects", map[string]string{"name": unique}, &out); err != nil {
		return uuid.Nil, fmt.Errorf("create project: %w", err)
	}
	return out.ID, nil
}

func deleteProject(ctx context.Context, id uuid.UUID) error {
	return adminDo(ctx, http.MethodDelete, "/v1/projects/"+id.String(), nil, nil)
}

func applyMigrations(ctx context.Context, id uuid.UUID) error {
	if err := adminDo(ctx, http.MethodPost, "/v1/projects/"+id.String()+"/migrations", nil, nil); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}

func issueKey(ctx context.Context, id uuid.UUID) (string, error) {
	var out struct {
		Key string `json:"key"`
	}
	if err := adminDo(ctx, http.MethodPost, "/v1/projects/"+id.String()+"/keys",
		map[string]string{"name": "conformance"}, &out); err != nil {
		return "", fmt.Errorf("issue key: %w", err)
	}
	return out.Key, nil
}

func applySQL(ctx context.Context, id uuid.UUID, sql string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		controlURL+"/v1/projects/"+id.String()+"/sql", strings.NewReader(sql))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+adminToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s: %s", resp.Status, bytes.TrimSpace(b))
	}
	return nil
}

func adminDo(ctx context.Context, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, controlURL+path, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+adminToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s %s: %s %s", method, path, resp.Status, bytes.TrimSpace(b))
	}
	if out != nil && resp.StatusCode != http.StatusNoContent {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// fakeEmbedding mirrors the Python and TypeScript runners exactly, so the three
// SDKs are compared on identical vectors rather than merely similar ones.
func fakeEmbedding(seed float64) []float32 {
	v := make([]float32, pwrap.VectorDim)
	for i := range v {
		v[i] = float32(math.Sin(seed + float64(i)*0.001))
	}
	return v
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
