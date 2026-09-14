//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/b3vet/pwrap/internal/controlplane/admintokens"
)

// mintToken issues an admin token with the given scopes, using the bootstrap
// token — the only thing it can still do.
func mintToken(ctx context.Context, t *testing.T, scopes ...admintokens.Scope) string {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"name": t.Name(), "scopes": admintokens.Strings(scopes),
	})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		sharedStack.ControlURL+"/v1/admin/tokens", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+bootstrapToken())
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("mint: %s %s", resp.Status, bytes.TrimSpace(b))
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out.Token
}

func bootstrapToken() string { return adminToken }

// call makes a request with an explicit bearer and returns the status.
func call(ctx context.Context, t *testing.T, method, path, bearer string, body any) int {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		buf, _ := json.Marshal(body)
		rdr = bytes.NewReader(buf)
	}
	req, _ := http.NewRequestWithContext(ctx, method, sharedStack.ControlURL+path, rdr)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)
	return resp.StatusCode
}

// The headline change: a leaked deployment secret no longer grants the whole
// management API.
func TestAdminTokens_BootstrapRejectedOnManagementEndpoints(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, method, path string
		body               any
	}{
		{"create project", http.MethodPost, "/v1/projects", map[string]string{"name": "nope"}},
		{"list projects", http.MethodGet, "/v1/projects", nil},
		{"apply sql", http.MethodPost, "/v1/projects/" + newUUID() + "/sql", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := call(ctx, t, tc.method, tc.path, bootstrapToken(), tc.body); got != http.StatusUnauthorized {
				t.Errorf("bootstrap token got %d, want 401 — it must no longer authenticate management calls", got)
			}
		})
	}

	// It must still mint, or a fresh deployment has no way in.
	if got := call(ctx, t, http.MethodGet, "/v1/admin/tokens", bootstrapToken(), nil); got != http.StatusOK {
		t.Errorf("bootstrap token got %d listing tokens, want 200", got)
	}
}

// A token must be able to do what it was granted and nothing else.
func TestAdminTokens_ScopesAreEnforced(t *testing.T) {
	ctx := context.Background()
	projectsOnly := mintToken(ctx, t, admintokens.ScopeProjects)

	// In scope.
	if got := call(ctx, t, http.MethodGet, "/v1/projects", projectsOnly, nil); got != http.StatusOK {
		t.Errorf("projects scope got %d listing projects, want 200", got)
	}
	// Out of scope: the same token must not reach other capabilities.
	for _, tc := range []struct{ name, path string }{
		{"sql", "/v1/projects/" + newUUID() + "/sql"},
		{"keys", "/v1/projects/" + newUUID() + "/keys"},
		{"migrate", "/v1/projects/" + newUUID() + "/migrations"},
	} {
		if got := call(ctx, t, http.MethodPost, tc.path, projectsOnly, nil); got != http.StatusForbidden {
			t.Errorf("projects-only token got %d on %s, want 403", got, tc.name)
		}
	}
}

// Revocation has to bite immediately, or it is not revocation.
func TestAdminTokens_RevokedTokenIsRefused(t *testing.T) {
	ctx := context.Background()
	tok := mintToken(ctx, t, admintokens.ScopeProjects)
	if got := call(ctx, t, http.MethodGet, "/v1/projects", tok, nil); got != http.StatusOK {
		t.Fatalf("fresh token got %d, want 200", got)
	}

	var list []struct {
		ID     string   `json:"id"`
		Name   string   `json:"name"`
		Scopes []string `json:"scopes"`
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, sharedStack.ControlURL+"/v1/admin/tokens", nil)
	req.Header.Set("Authorization", "Bearer "+bootstrapToken())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	_ = json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()

	var id string
	for _, x := range list {
		if x.Name == t.Name() {
			id = x.ID
		}
	}
	if id == "" {
		t.Fatal("could not find the freshly minted token in the list")
	}
	if got := call(ctx, t, http.MethodDelete, "/v1/admin/tokens/"+id, bootstrapToken(), nil); got != http.StatusNoContent {
		t.Fatalf("revoke got %d, want 204", got)
	}
	if got := call(ctx, t, http.MethodGet, "/v1/projects", tok, nil); got != http.StatusForbidden {
		t.Errorf("revoked token got %d, want 403", got)
	}
}

func TestAdminTokens_UnknownScopeIsRejected(t *testing.T) {
	ctx := context.Background()
	body, _ := json.Marshal(map[string]any{"name": "bad", "scopes": []string{"projects", "roooot"}})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		sharedStack.ControlURL+"/v1/admin/tokens", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+bootstrapToken())
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	defer resp.Body.Close()
	// A typo in a grant must fail loudly rather than quietly issuing a token
	// that cannot do its job.
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("unknown scope got %d, want 400", resp.StatusCode)
	}
}

// The audit trail is the point of the table; a mutating call that leaves no
// trace would defeat it. Denials are recorded too.
func TestAdminTokens_AuditRecordsMutationsAndDenials(t *testing.T) {
	ctx := context.Background()
	before := auditCount(ctx, t)

	tok := mintToken(ctx, t, admintokens.ScopeProjects)
	name := "audit-" + newUUID()[:8]
	if got := call(ctx, t, http.MethodPost, "/v1/projects", tok, map[string]string{"name": name}); got != http.StatusCreated {
		t.Fatalf("create got %d, want 201", got)
	}
	// A refusal, which must also be recorded.
	if got := call(ctx, t, http.MethodPost, "/v1/projects/"+newUUID()+"/sql", tok, nil); got != http.StatusForbidden {
		t.Fatalf("expected 403 for out-of-scope sql, got %d", got)
	}

	if after := auditCount(ctx, t); after <= before {
		t.Errorf("audit rows did not grow (%d -> %d)", before, after)
	}
	var denials int
	if err := sharedStack.Pool().QueryRow(ctx,
		`SELECT count(*) FROM admin_audit_log WHERE status = 403`).Scan(&denials); err != nil {
		t.Fatalf("count denials: %v", err)
	}
	if denials == 0 {
		t.Error("no denial was recorded — a run of refusals is exactly what an audit trail is for")
	}
	// GETs are skipped on purpose; the trail should not be drowned in reads.
	var reads int
	if err := sharedStack.Pool().QueryRow(ctx,
		`SELECT count(*) FROM admin_audit_log WHERE method = 'GET'`).Scan(&reads); err != nil {
		t.Fatalf("count reads: %v", err)
	}
	if reads != 0 {
		t.Errorf("%d GET rows in the audit log, want 0", reads)
	}
}

func auditCount(ctx context.Context, t *testing.T) int {
	t.Helper()
	var n int
	if err := sharedStack.Pool().QueryRow(ctx, `SELECT count(*) FROM admin_audit_log`).Scan(&n); err != nil {
		t.Fatalf("count audit: %v", err)
	}
	return n
}

func newUUID() string { return uuid.New().String() }

// The audit log has to be bounded, or it becomes the thing that fills the disk.
func TestAdminTokens_AuditPruneRespectsRetention(t *testing.T) {
	ctx := context.Background()

	// Two rows either side of the window, inserted directly so the test does
	// not depend on wall-clock waiting.
	if _, err := sharedStack.Pool().Exec(ctx, `
		INSERT INTO admin_audit_log (token_prefix, action, method, path, status, created_at)
		VALUES ('oldpref', 'POST /v1/projects', 'POST', '/v1/projects', 201, now() - interval '100 days'),
		       ('newpref', 'POST /v1/projects', 'POST', '/v1/projects', 201, now() - interval '1 day')
	`); err != nil {
		t.Fatalf("seed audit rows: %v", err)
	}

	svc := admintokens.NewService(sharedStack.Pool())
	if _, err := svc.PruneAudit(ctx, 90*24*time.Hour); err != nil {
		t.Fatalf("prune: %v", err)
	}

	if n := auditRowsWithPrefix(ctx, t, "oldpref"); n != 0 {
		t.Errorf("row older than the retention window survived (%d rows)", n)
	}
	if n := auditRowsWithPrefix(ctx, t, "newpref"); n != 1 {
		t.Errorf("row inside the window was deleted (%d rows, want 1)", n)
	}

	// Zero retention means keep everything — an operator has to opt into that,
	// and it must not be mistaken for "delete everything".
	if _, err := sharedStack.Pool().Exec(ctx, `
		INSERT INTO admin_audit_log (token_prefix, action, method, path, status, created_at)
		VALUES ('ancient', 'POST /v1/projects', 'POST', '/v1/projects', 201, now() - interval '5 years')
	`); err != nil {
		t.Fatalf("seed ancient row: %v", err)
	}
	if n, err := svc.PruneAudit(ctx, 0); err != nil {
		t.Fatalf("prune with zero retention: %v", err)
	} else if n != 0 {
		t.Errorf("zero retention deleted %d rows, want 0", n)
	}
	if n := auditRowsWithPrefix(ctx, t, "ancient"); n != 1 {
		t.Errorf("zero retention removed a row it should have kept (%d rows, want 1)", n)
	}
}

func auditRowsWithPrefix(ctx context.Context, t *testing.T, prefix string) int {
	t.Helper()
	var n int
	if err := sharedStack.Pool().QueryRow(ctx,
		`SELECT count(*) FROM admin_audit_log WHERE token_prefix = $1`, prefix).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}
