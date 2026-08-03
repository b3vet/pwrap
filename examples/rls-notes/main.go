// rls-notes proves that a single RLS policy on pwrap_documents enforces per-user
// isolation across both access paths pwrap exposes:
//
//   1. Via the Go SDK: Client.WithUser(userID) wraps Table ops in a tx that sets
//      request.jwt.claims.user_id, so the policy USING/WITH CHECK clause matches.
//   2. Via PostgREST: the JWT from /v1/rest/token carries user_id as a claim, which
//      PostgREST forwards into request.jwt.claims automatically.
//
// The test flow:
//   - Create project + issue key + apply pwrap migrations.
//   - Apply rls.sql (this directory) as the tenant role: enables + forces RLS + defines policy.
//   - Alice and Bob each insert 3 notes via SDK.WithUser.
//   - SDK.WithUser(alice).Find returns exactly alice's 3 notes (and same for bob).
//   - A direct (non-WithUser) SDK call returns 0 rows — the policy denies by default.
//   - A PostgREST GET with alice's JWT returns alice's 3 notes.
package main

import (
	"bytes"
	"context"
	_ "embed"
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

//go:embed rls.sql
var rlsSQL string

func main() {
	if err := run(context.Background()); err != nil {
		log.Fatalf("rls-notes: %v", err)
	}
}

func run(ctx context.Context) error {
	controlURL := envOr("PWRAP_CONTROL_URL", "http://localhost:8080")
	adminToken := os.Getenv("PWRAP_BOOTSTRAP_TOKEN")
	if adminToken == "" {
		return errors.New("PWRAP_BOOTSTRAP_TOKEN required")
	}
	admin := newAdmin(controlURL, adminToken)

	proj, err := admin.createProject(ctx, fmt.Sprintf("rls-notes-%d", time.Now().Unix()))
	if err != nil {
		return err
	}
	log.Printf("project %s", proj.ID)
	defer func() {
		if err := admin.deleteProject(context.Background(), proj.ID); err != nil {
			log.Printf("cleanup: %v", err)
		}
	}()

	if _, err := admin.applyMigrations(ctx, proj.ID); err != nil {
		return err
	}
	log.Printf("migrations applied")

	if err := admin.applySQL(ctx, proj.ID, rlsSQL); err != nil {
		return fmt.Errorf("apply rls.sql: %w", err)
	}
	log.Printf("RLS policy applied")

	issued, err := admin.issueKey(ctx, proj.ID, "rls-notes")
	if err != nil {
		return fmt.Errorf("issueKey: %w", err)
	}
	log.Printf("key issued, prefix=%s", issued.Prefix)

	c, err := pwrap.New(ctx, pwrap.Config{ControlURL: controlURL, APIKey: issued.Key})
	if err != nil {
		return fmt.Errorf("pwrap.New: %w", err)
	}
	log.Printf("SDK connected")
	defer c.Close()

	aliceID := uuid.New().String()
	bobID := uuid.New().String()

	// Insert through the RLS-aware tx wrapper.
	alice := c.WithUser(aliceID)
	bob := c.WithUser(bobID)

	log.Printf("inserting alice's notes")
	for i, title := range []string{"a-note-1", "a-note-2", "a-note-3"} {
		if _, err := alice.Table("notes").Insert(ctx, map[string]any{
			"user_id": aliceID,
			"title":   title,
			"seq":     i,
		}); err != nil {
			return fmt.Errorf("alice insert[%d]: %w", i, err)
		}
	}
	log.Printf("alice inserts done")
	log.Printf("inserting bob's notes")
	for i, title := range []string{"b-note-1", "b-note-2", "b-note-3"} {
		if _, err := bob.Table("notes").Insert(ctx, map[string]any{
			"user_id": bobID,
			"title":   title,
			"seq":     i,
		}); err != nil {
			return fmt.Errorf("bob insert[%d]: %w", i, err)
		}
	}
	log.Printf("bob inserts done")

	// Prove SDK isolation.
	log.Printf("alice.Find()")
	aliceRows, err := alice.Table("notes").Find(ctx, nil, 100)
	if err != nil {
		return fmt.Errorf("alice.Find: %w", err)
	}
	log.Printf("bob.Find()")
	bobRows, err := bob.Table("notes").Find(ctx, nil, 100)
	if err != nil {
		return fmt.Errorf("bob.Find: %w", err)
	}
	log.Printf("unscoped.Find()")
	noUserRows, err := c.Table("notes").Find(ctx, nil, 100)
	if err != nil {
		return fmt.Errorf("unscoped.Find: %w", err)
	}

	fmt.Println()
	fmt.Println("=== SDK RLS isolation ===")
	fmt.Printf("alice (WithUser %s…) sees %d rows: %s\n", aliceID[:8], len(aliceRows), titlesOf(aliceRows))
	fmt.Printf("bob   (WithUser %s…) sees %d rows: %s\n", bobID[:8], len(bobRows), titlesOf(bobRows))
	fmt.Printf("unscoped client      sees %d rows (expected 0 — policy denies by default)\n", len(noUserRows))

	if len(aliceRows) != 3 || len(bobRows) != 3 || len(noUserRows) != 0 {
		return fmt.Errorf("SDK isolation broke: alice=%d bob=%d noUser=%d (want 3/3/0)",
			len(aliceRows), len(bobRows), len(noUserRows))
	}

	// Prove PostgREST isolation: mint a JWT with user_id=alice, hit PostgREST.
	aliceToken, err := c.IssueRestToken(ctx, pwrap.RestTokenOpts{UserID: aliceID, TTLSeconds: 120})
	if errors.Is(err, pwrap.ErrRestDisabled) {
		fmt.Println("\n(REST integration disabled, skipping PostgREST leg of the demo)")
		return nil
	}
	if err != nil {
		return err
	}
	fmt.Println()
	fmt.Println("=== PostgREST RLS isolation ===")
	rows, err := restGet(aliceToken.URL+"/pwrap_documents", aliceToken.Token, c.Schema())
	if err != nil {
		return err
	}
	fmt.Printf("PostgREST GET /pwrap_documents with alice's JWT returned %d rows\n", len(rows))
	if len(rows) != 3 {
		return fmt.Errorf("PostgREST returned %d rows for alice, want 3", len(rows))
	}
	for _, r := range rows {
		fmt.Printf("  - %s\n", r["data"].(map[string]any)["title"])
	}
	return nil
}

// --- helpers -----------------------------------------------------------------

func titlesOf(docs []pwrap.Document) string {
	var out []string
	for _, d := range docs {
		if t, ok := d.Data["title"].(string); ok {
			out = append(out, t)
		}
	}
	return strings.Join(out, ", ")
}

func restGet(url, jwt, schema string) ([]map[string]any, error) {
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept-Profile", schema)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("%s %s: %s", http.MethodGet, url, b)
	}
	var out []map[string]any
	return out, json.NewDecoder(resp.Body).Decode(&out)
}

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
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
}
type issuedKey struct {
	Prefix string `json:"prefix"`
	Key    string `json:"key"`
}

func newAdmin(base, token string) *admin {
	return &admin{base: strings.TrimRight(base, "/"), token: token, http: &http.Client{Timeout: 30 * time.Second}}
}
func (a *admin) createProject(ctx context.Context, name string) (project, error) {
	var out project
	return out, a.do(ctx, http.MethodPost, "/v1/projects", map[string]string{"name": name}, &out, "")
}
func (a *admin) deleteProject(ctx context.Context, id uuid.UUID) error {
	return a.do(ctx, http.MethodDelete, "/v1/projects/"+id.String(), nil, nil, "")
}
func (a *admin) issueKey(ctx context.Context, id uuid.UUID, name string) (issuedKey, error) {
	var out issuedKey
	return out, a.do(ctx, http.MethodPost, "/v1/projects/"+id.String()+"/keys", map[string]string{"name": name}, &out, "")
}
func (a *admin) applyMigrations(ctx context.Context, id uuid.UUID) (map[string]any, error) {
	var out map[string]any
	return out, a.do(ctx, http.MethodPost, "/v1/projects/"+id.String()+"/migrations", nil, &out, "")
}
func (a *admin) applySQL(ctx context.Context, id uuid.UUID, sql string) error {
	return a.do(ctx, http.MethodPost, "/v1/projects/"+id.String()+"/sql", nil, nil, sql)
}
func (a *admin) do(ctx context.Context, method, path string, body, out any, rawBody string) error {
	var rdr io.Reader
	var contentType string
	if rawBody != "" {
		rdr = strings.NewReader(rawBody)
		contentType = "application/sql"
	} else if body != nil {
		buf, _ := json.Marshal(body)
		rdr = bytes.NewReader(buf)
		contentType = "application/json"
	}
	req, _ := http.NewRequestWithContext(ctx, method, a.base+path, rdr)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
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
