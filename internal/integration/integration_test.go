//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/b3vet/pwrap/internal/controlplane/migrations"
	"github.com/b3vet/pwrap/sdk/go/pwrap"
)

// TestMain spins up one Stack for the whole package. Per-test isolation comes
// from each test creating its own project; cleanup deletes the project on test end.
var sharedStack *Stack

func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	s, err := NewStack(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "integration: stack setup failed:", err)
		fmt.Fprintln(os.Stderr, "did you build pwrap-postgres:local first? (`docker-compose build postgres`)")
		os.Exit(1)
	}
	sharedStack = s
	code := m.Run()
	s.Close()
	os.Exit(code)
}

// --- helpers ----------------------------------------------------------------

type adminAPI struct {
	base, token string
	http        *http.Client
}

func newAdmin() *adminAPI {
	return &adminAPI{
		base:  sharedStack.ControlURL,
		token: sharedStack.AdminToken,
		http:  &http.Client{Timeout: 30 * time.Second},
	}
}

func (a *adminAPI) do(ctx context.Context, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		buf, _ := json.Marshal(body)
		rdr = bytes.NewReader(buf)
	}
	req, _ := http.NewRequestWithContext(ctx, method, a.base+path, rdr)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	resp, err := a.http.Do(req)
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

// provision creates a project + applies migrations + issues a key. The project
// is auto-deleted via t.Cleanup.
func provision(ctx context.Context, t *testing.T, name string) (uuid.UUID, string) {
	t.Helper()
	a := newAdmin()
	var p struct {
		ID uuid.UUID `json:"id"`
	}
	if err := a.do(ctx, http.MethodPost, "/v1/projects", map[string]string{"name": name}, &p); err != nil {
		t.Fatalf("create project: %v", err)
	}
	t.Cleanup(func() { _ = a.do(context.Background(), http.MethodDelete, "/v1/projects/"+p.ID.String(), nil, nil) })
	if err := a.do(ctx, http.MethodPost, "/v1/projects/"+p.ID.String()+"/migrations", nil, nil); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	var k struct {
		Key string `json:"key"`
	}
	if err := a.do(ctx, http.MethodPost, "/v1/projects/"+p.ID.String()+"/keys", map[string]string{"name": "test"}, &k); err != nil {
		t.Fatalf("issue key: %v", err)
	}
	return p.ID, k.Key
}

// --- tests --------------------------------------------------------------------

func TestSchemaVersionGuard_FailsForUnmigratedProject(t *testing.T) {
	ctx := context.Background()
	a := newAdmin()
	var p struct {
		ID uuid.UUID `json:"id"`
	}
	if err := a.do(ctx, http.MethodPost, "/v1/projects", map[string]string{"name": "unmigrated"}, &p); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.do(context.Background(), http.MethodDelete, "/v1/projects/"+p.ID.String(), nil, nil) })
	var k struct {
		Key string `json:"key"`
	}
	if err := a.do(ctx, http.MethodPost, "/v1/projects/"+p.ID.String()+"/keys", map[string]string{"name": "test"}, &k); err != nil {
		t.Fatal(err)
	}
	_, err := pwrap.New(ctx, pwrap.Config{ControlURL: sharedStack.ControlURL, APIKey: k.Key})
	if err == nil {
		t.Fatal("SDK should have refused unmigrated project")
	}
	if !strings.Contains(err.Error(), "no migrations applied") {
		t.Fatalf("error = %q, want substring 'no migrations applied'", err.Error())
	}
}

func TestBranching_DataCopyAndIsolation(t *testing.T) {
	ctx := context.Background()
	parentID, parentKey := provision(ctx, t, "parent")

	pc, err := pwrap.New(ctx, pwrap.Config{ControlURL: sharedStack.ControlURL, APIKey: parentKey})
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	for i := 0; i < 3; i++ {
		if _, err := pc.Table("notes").Insert(ctx, map[string]any{"i": i, "src": "parent"}); err != nil {
			t.Fatal(err)
		}
	}

	a := newAdmin()
	var child struct {
		ID uuid.UUID `json:"id"`
	}
	if err := a.do(ctx, http.MethodPost, "/v1/projects/"+parentID.String()+"/branches",
		map[string]any{"name": "branch", "with_data": true}, &child); err != nil {
		t.Fatal(err)
	}

	var childKey struct {
		Key string `json:"key"`
	}
	if err := a.do(ctx, http.MethodPost, "/v1/projects/"+child.ID.String()+"/keys",
		map[string]string{"name": "branch-test"}, &childKey); err != nil {
		t.Fatal(err)
	}
	bc, err := pwrap.New(ctx, pwrap.Config{ControlURL: sharedStack.ControlURL, APIKey: childKey.Key})
	if err != nil {
		t.Fatal(err)
	}
	defer bc.Close()

	branchDocs, _ := bc.Table("notes").Find(ctx, nil, 100)
	if len(branchDocs) != 3 {
		t.Fatalf("branch saw %d rows after with_data clone, want 3", len(branchDocs))
	}

	// Mutate branch — parent must NOT see it.
	if _, err := bc.Table("notes").Insert(ctx, map[string]any{"src": "branch-only"}); err != nil {
		t.Fatal(err)
	}
	parentAfter, _ := pc.Table("notes").Find(ctx, nil, 100)
	if len(parentAfter) != 3 {
		t.Fatalf("parent saw %d rows, want 3 (branch mutation leaked)", len(parentAfter))
	}
}

func TestRLS_WithUserScopesQueries(t *testing.T) {
	ctx := context.Background()
	id, key := provision(ctx, t, "rls-suite")

	a := newAdmin()
	sql := `
		ALTER TABLE pwrap_documents ENABLE ROW LEVEL SECURITY;
		ALTER TABLE pwrap_documents FORCE  ROW LEVEL SECURITY;
		DROP POLICY IF EXISTS p ON pwrap_documents;
		CREATE POLICY p ON pwrap_documents
		  USING      (data->>'user_id' = NULLIF(current_setting('request.jwt.claims', true), '')::jsonb->>'user_id')
		  WITH CHECK (data->>'user_id' = NULLIF(current_setting('request.jwt.claims', true), '')::jsonb->>'user_id');
	`
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		sharedStack.ControlURL+"/v1/projects/"+id.String()+"/sql", bytes.NewReader([]byte(sql)))
	req.Header.Set("Content-Type", "application/sql")
	req.Header.Set("Authorization", "Bearer "+sharedStack.AdminToken)
	resp, err := a.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("apply RLS: %s %s", resp.Status, b)
	}
	resp.Body.Close()

	c, err := pwrap.New(ctx, pwrap.Config{ControlURL: sharedStack.ControlURL, APIKey: key})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	alice := c.WithUser("alice")
	bob := c.WithUser("bob")
	for i := 0; i < 3; i++ {
		if _, err := alice.Table("notes").Insert(ctx, map[string]any{"user_id": "alice", "i": i}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := bob.Table("notes").Insert(ctx, map[string]any{"user_id": "bob", "i": 0}); err != nil {
		t.Fatal(err)
	}

	aliceRows, _ := alice.Table("notes").Find(ctx, nil, 100)
	bobRows, _ := bob.Table("notes").Find(ctx, nil, 100)
	noUserRows, _ := c.Table("notes").Find(ctx, nil, 100)
	if len(aliceRows) != 3 {
		t.Fatalf("alice saw %d, want 3", len(aliceRows))
	}
	if len(bobRows) != 1 {
		t.Fatalf("bob saw %d, want 1", len(bobRows))
	}
	if len(noUserRows) != 0 {
		t.Fatalf("unscoped saw %d, want 0 (policy denies by default)", len(noUserRows))
	}
}

func TestGeo_RoundTrip(t *testing.T) {
	ctx := context.Background()
	_, key := provision(ctx, t, "geo-suite")

	c, err := pwrap.New(ctx, pwrap.Config{ControlURL: sharedStack.ControlURL, APIKey: key})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	geo := c.Geo("places")
	if _, err := geo.InsertPoint(ctx, 2.2945, 48.8584, map[string]any{"name": "Eiffel"}); err != nil {
		t.Fatal(err)
	}
	if _, err := geo.InsertPoint(ctx, -73.9857, 40.7484, map[string]any{"name": "Empire State"}); err != nil {
		t.Fatal(err)
	}
	near, err := geo.WithinRadius(ctx, 2.3522, 48.8566, 5000, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(near) != 1 || near[0].Metadata["name"] != "Eiffel" {
		t.Fatalf("near = %+v", near)
	}
}

func TestRealtime_InsertEmitsEvent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, key := provision(ctx, t, "realtime-suite")

	c, err := pwrap.New(ctx, pwrap.Config{ControlURL: sharedStack.ControlURL, APIKey: key})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	sub, err := c.Subscribe(ctx, pwrap.SubscribeOpts{Table: "pwrap_documents"})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer sub.Close()

	// Insert in a goroutine so the main routine can drain the channel without
	// racing against the SDK Insert call.
	insertDone := make(chan uuid.UUID, 1)
	go func() {
		id, err := c.Table("notes").Insert(ctx, map[string]any{"title": "hello realtime"})
		if err != nil {
			t.Logf("insert: %v", err)
			return
		}
		insertDone <- id
	}()

	deadline := time.After(5 * time.Second)
	var insertedID uuid.UUID
	for {
		select {
		case id := <-insertDone:
			insertedID = id
		case ev, ok := <-sub.Changes():
			if !ok {
				t.Fatalf("subscription ended early: %v", sub.Err())
			}
			if ev.Op != "INSERT" || ev.Table != "pwrap_documents" {
				t.Fatalf("unexpected event: %+v", ev)
			}
			if insertedID != uuid.Nil && ev.RowID != insertedID.String() {
				t.Fatalf("row_id mismatch: got %s, inserted %s", ev.RowID, insertedID)
			}
			if !strings.Contains(string(ev.After), "hello realtime") {
				t.Fatalf("after missing title: %s", ev.After)
			}
			return
		case <-deadline:
			t.Fatalf("timed out waiting for realtime event")
		}
	}
}

// TestEncryptionAtRest_PgPasswordStored verifies that a newly-created project's
// pg_password is stored as a v1: envelope (encrypted) — proving the cipher is
// wired through projects.Service. The harness sets PWRAP_ENCRYPTION_KEY so this
// path is on for the whole suite.
func TestEncryptionAtRest_PgPasswordStored(t *testing.T) {
	ctx := context.Background()
	id, _ := provision(ctx, t, "enc-suite")

	var stored string
	if err := sharedStack.pool.QueryRow(ctx,
		`SELECT pg_password FROM projects WHERE id = $1`, id,
	).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(stored, "v1:") {
		t.Fatalf("expected v1: envelope, got %q", stored)
	}
	// Confirm the DSN handoff path still works (i.e. decrypt succeeds).
	c, err := pwrap.New(ctx, pwrap.Config{ControlURL: sharedStack.ControlURL, APIKey: mustIssueKey(ctx, t, id)})
	if err != nil {
		t.Fatalf("dsn handoff with encrypted password failed: %v", err)
	}
	c.Close()
}

// TestReencryptAll_BackfillsLegacyPlaintext flips an existing project's
// pg_password back to plaintext, runs ReencryptAll, and verifies the column is
// re-enveloped — the upgrade path from a no-key deployment.
func TestReencryptAll_BackfillsLegacyPlaintext(t *testing.T) {
	ctx := context.Background()
	id, _ := provision(ctx, t, "reenc-suite")

	if _, err := sharedStack.pool.Exec(ctx,
		`UPDATE projects SET pg_password = 'legacy-cleartext-secret' WHERE id = $1`, id,
	); err != nil {
		t.Fatal(err)
	}
	n, err := sharedStack.server.ProjectsService().ReencryptAll(ctx)
	if err != nil {
		t.Fatalf("reencrypt: %v", err)
	}
	if n < 1 {
		t.Fatalf("expected >= 1 row re-encrypted, got %d", n)
	}
	var stored string
	if err := sharedStack.pool.QueryRow(ctx,
		`SELECT pg_password FROM projects WHERE id = $1`, id,
	).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(stored, "v1:") {
		t.Fatalf("expected v1: envelope after reencrypt, got %q", stored)
	}
}

// mustIssueKey is a tiny helper for tests that need a second key on a project
// without going through the full provision() boilerplate.
func mustIssueKey(ctx context.Context, t *testing.T, projectID uuid.UUID) string {
	t.Helper()
	a := newAdmin()
	var k struct {
		Key string `json:"key"`
	}
	if err := a.do(ctx, http.MethodPost, "/v1/projects/"+projectID.String()+"/keys", map[string]string{"name": "extra"}, &k); err != nil {
		t.Fatalf("issue key: %v", err)
	}
	return k.Key
}

func TestOrphanSweep_MarksStalePending(t *testing.T) {
	ctx := context.Background()
	id, _ := provision(ctx, t, "sweep-suite")

	if _, err := sharedStack.pool.Exec(ctx, `
		INSERT INTO migration_log (project_id, version, source, checksum, state, started_at)
		VALUES ($1, '0001_init', 'embedded', 'fake', 'pending', now() - interval '10 minutes')
	`, id); err != nil {
		t.Fatal(err)
	}
	swept, err := migrations.SweepStale(ctx, sharedStack.pool, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if swept == 0 {
		t.Fatal("expected at least one swept row")
	}
	var state string
	if err := sharedStack.pool.QueryRow(ctx,
		`SELECT state FROM migration_log WHERE project_id=$1 AND checksum='fake'`, id,
	).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "failed" {
		t.Fatalf("state=%q, want failed", state)
	}
}
