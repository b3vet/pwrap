// branching demonstrates Track C's project branching:
//
//   - Provision a parent project with some pwrap_documents and pwrap_geo rows.
//   - Create a child branch project with with_data=true. Verify the child sees
//     a snapshot of the parent's data.
//   - Mutate the child (insert a new doc). Verify the parent is unaffected —
//     branches are independent post-fork.
//   - Add fresh rows to the parent, then call `sync` on the child — the new
//     rows show up on the branch.
//   - List branches under the parent.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/b3vet/pwrap/sdk/go/pwrap"
)

func main() {
	if err := run(context.Background()); err != nil {
		log.Fatalf("branching: %v", err)
	}
}

func run(ctx context.Context) error {
	controlURL := envOr("PWRAP_CONTROL_URL", "http://localhost:8080")
	adminToken := os.Getenv("PWRAP_BOOTSTRAP_TOKEN")
	if adminToken == "" {
		return errors.New("PWRAP_BOOTSTRAP_TOKEN required")
	}
	admin := newAdmin(controlURL, adminToken)

	parentName := fmt.Sprintf("branching-parent-%d", time.Now().Unix())
	parent, err := admin.createProject(ctx, parentName)
	if err != nil {
		return err
	}
	log.Printf("parent %s (%s)", parent.Name, parent.ID)
	defer func() {
		_ = admin.deleteProject(context.Background(), parent.ID)
	}()
	if _, err := admin.applyMigrations(ctx, parent.ID); err != nil {
		return err
	}

	pKey, err := admin.issueKey(ctx, parent.ID, "parent-app")
	if err != nil {
		return err
	}
	pc, err := pwrap.New(ctx, pwrap.Config{ControlURL: controlURL, APIKey: pKey.Key})
	if err != nil {
		return err
	}
	defer pc.Close()

	// Seed the parent.
	for i, title := range []string{"original-1", "original-2", "original-3"} {
		if _, err := pc.Table("notes").Insert(ctx, map[string]any{"title": title, "seq": i}); err != nil {
			return fmt.Errorf("parent insert: %w", err)
		}
	}
	if _, err := pc.Geo("places").InsertPoint(ctx, 2.2945, 48.8584, map[string]any{"name": "Eiffel Tower"}); err != nil {
		return err
	}
	log.Printf("parent seeded: 3 notes + 1 geo point")

	// Branch with data.
	branch, err := admin.createBranch(ctx, parent.ID, "staging", true)
	if err != nil {
		return fmt.Errorf("create branch: %w", err)
	}
	log.Printf("branch %s (%s, parent=%s)", branch.Name, branch.ID, *branch.ParentProjectID)

	// Open SDK against the branch.
	bKey, err := admin.issueKey(ctx, branch.ID, "branch-app")
	if err != nil {
		return err
	}
	bc, err := pwrap.New(ctx, pwrap.Config{ControlURL: controlURL, APIKey: bKey.Key})
	if err != nil {
		return err
	}
	defer bc.Close()

	// Verify the branch sees the parent's data.
	branchDocs, _ := bc.Table("notes").Find(ctx, nil, 100)
	branchGeo, _ := bc.Geo("places").Count(ctx)
	fmt.Println()
	fmt.Printf("=== After branch create (with_data=true) ===\n")
	fmt.Printf("  branch.notes:  %d (expected 3)\n", len(branchDocs))
	fmt.Printf("  branch.geo:    %d (expected 1)\n", branchGeo)
	if len(branchDocs) != 3 || branchGeo != 1 {
		return fmt.Errorf("data copy mismatch — got notes=%d geo=%d, want 3/1", len(branchDocs), branchGeo)
	}

	// Mutate branch — parent must NOT see this.
	if _, err := bc.Table("notes").Insert(ctx, map[string]any{"title": "branch-only", "seq": 99}); err != nil {
		return err
	}
	parentDocsAfter, _ := pc.Table("notes").Find(ctx, nil, 100)
	branchDocsAfter, _ := bc.Table("notes").Find(ctx, nil, 100)
	fmt.Printf("\n=== After branch-only mutation ===\n")
	fmt.Printf("  parent.notes:  %d (expected 3, unchanged)\n", len(parentDocsAfter))
	fmt.Printf("  branch.notes:  %d (expected 4)\n", len(branchDocsAfter))
	if len(parentDocsAfter) != 3 || len(branchDocsAfter) != 4 {
		return fmt.Errorf("isolation broke")
	}

	// Add to parent — branch must NOT see this until sync.
	if _, err := pc.Table("notes").Insert(ctx, map[string]any{"title": "after-fork", "seq": 100}); err != nil {
		return err
	}
	branchPreSync, _ := bc.Table("notes").Find(ctx, nil, 100)
	fmt.Printf("\n=== After parent-only mutation ===\n")
	fmt.Printf("  branch.notes (pre-sync): %d (expected 4)\n", len(branchPreSync))
	if len(branchPreSync) != 4 {
		return fmt.Errorf("branch saw parent's row before sync")
	}

	// Sync: pull parent rows into branch (ON CONFLICT DO NOTHING — branch's row stays).
	if err := admin.syncBranch(ctx, branch.ID, false, nil); err != nil {
		return fmt.Errorf("sync: %w", err)
	}
	branchPostSync, _ := bc.Table("notes").Find(ctx, nil, 100)
	fmt.Printf("\n=== After sync (idempotent merge) ===\n")
	fmt.Printf("  branch.notes (post-sync): %d (expected 5: 3 orig + 1 branch-only + 1 after-fork)\n", len(branchPostSync))
	if len(branchPostSync) != 5 {
		return fmt.Errorf("sync result wrong: got %d", len(branchPostSync))
	}

	// List branches under parent.
	branches, err := admin.listBranches(ctx, parent.ID)
	if err != nil {
		return err
	}
	fmt.Printf("\n=== Branches under parent ===\n")
	for _, b := range branches {
		fmt.Printf("  %s  parent=%v\n", b.Name, b.ParentProjectID)
	}

	return nil
}

// --- helpers / admin client ---

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

type admin struct {
	base, token string
	http        *http.Client
}
type project struct {
	ID              uuid.UUID  `json:"id"`
	Name            string     `json:"name"`
	ParentProjectID *uuid.UUID `json:"parent_project_id,omitempty"`
}
type issuedKey struct {
	Prefix string `json:"prefix"`
	Key    string `json:"key"`
}

func newAdmin(base, token string) *admin {
	return &admin{base: strings.TrimRight(base, "/"), token: token, http: &http.Client{Timeout: 60 * time.Second}}
}
func (a *admin) createProject(ctx context.Context, name string) (project, error) {
	var out project
	return out, a.do(ctx, http.MethodPost, "/v1/projects", map[string]string{"name": name}, &out)
}
func (a *admin) deleteProject(ctx context.Context, id uuid.UUID) error {
	return a.do(ctx, http.MethodDelete, "/v1/projects/"+id.String(), nil, nil)
}
func (a *admin) issueKey(ctx context.Context, id uuid.UUID, name string) (issuedKey, error) {
	var out issuedKey
	return out, a.do(ctx, http.MethodPost, "/v1/projects/"+id.String()+"/keys", map[string]string{"name": name}, &out)
}
func (a *admin) applyMigrations(ctx context.Context, id uuid.UUID) (map[string]any, error) {
	var out map[string]any
	return out, a.do(ctx, http.MethodPost, "/v1/projects/"+id.String()+"/migrations", nil, &out)
}
func (a *admin) createBranch(ctx context.Context, parentID uuid.UUID, name string, withData bool) (project, error) {
	var out project
	return out, a.do(ctx, http.MethodPost, "/v1/projects/"+parentID.String()+"/branches",
		map[string]any{"name": name, "with_data": withData}, &out)
}
func (a *admin) listBranches(ctx context.Context, parentID uuid.UUID) ([]project, error) {
	var out []project
	return out, a.do(ctx, http.MethodGet, "/v1/projects/"+parentID.String()+"/branches", nil, &out)
}
func (a *admin) syncBranch(ctx context.Context, childID uuid.UUID, truncate bool, tables []string) error {
	body := map[string]any{"truncate": truncate}
	if tables != nil {
		body["tables"] = tables
	}
	return a.do(ctx, http.MethodPost, "/v1/projects/"+childID.String()+"/sync", body, nil)
}

func (a *admin) do(ctx context.Context, method, path string, body, out any) error {
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
