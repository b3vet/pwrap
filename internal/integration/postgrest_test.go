//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/b3vet/pwrap/sdk/go/pwrap"
)

// restToken exchanges a project API key for a PostgREST JWT via /v1/rest/token.
func restToken(ctx context.Context, t *testing.T, apiKey, userID string) (token, url, role string) {
	t.Helper()
	body := map[string]any{}
	if userID != "" {
		body["user_id"] = userID
	}
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		sharedStack.ControlURL+"/v1/rest/token", bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("issue rest token: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST /v1/rest/token: %s %s", resp.Status, bytes.TrimSpace(b))
	}
	var out struct {
		Token string `json:"token"`
		URL   string `json:"url"`
		Role  string `json:"role"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out.Token, out.URL, out.Role
}

// pgrst issues a request against PostgREST with the given JWT and profile.
// PostgREST only picks up a newly created tenant schema after the NOTIFY-driven
// reload lands, so a 404/406 on the first attempt is retried briefly.
func pgrst(ctx context.Context, t *testing.T, method, path, token, profile string, body any) (int, []byte) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		var rdr io.Reader
		if body != nil {
			buf, err := json.Marshal(body)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			rdr = bytes.NewReader(buf)
		}
		req, err := http.NewRequestWithContext(ctx, method, sharedStack.RestURL+path, rdr)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Content-Profile", profile)
			req.Header.Set("Prefer", "return=representation")
		} else {
			req.Header.Set("Accept-Profile", profile)
		}

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			if time.Now().Before(deadline) {
				time.Sleep(500 * time.Millisecond)
				continue
			}
			t.Fatalf("%s %s: %v", method, path, err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		// 404 (unknown relation) and 406 (schema not exposed) both mean the
		// schema cache hasn't caught up yet.
		if (resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusNotAcceptable) &&
			time.Now().Before(deadline) {
			time.Sleep(500 * time.Millisecond)
			continue
		}
		return resp.StatusCode, raw
	}
}

// applySQL runs DDL against a project's schema through the admin escape hatch.
func applySQL(ctx context.Context, t *testing.T, projectID, sql string) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		sharedStack.ControlURL+"/v1/projects/"+projectID+"/sql", strings.NewReader(sql))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+sharedStack.AdminToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("apply sql: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("apply sql: %s %s", resp.Status, bytes.TrimSpace(b))
	}
}

func TestRest_TokenIssuedForProjectRole(t *testing.T) {
	ctx := context.Background()
	pid, key := provision(ctx, t, "rest-token")

	token, url, role := restToken(ctx, t, key, "")
	if token == "" {
		t.Fatal("empty token")
	}
	// The URL handed to clients must be the real sidecar, not a placeholder.
	if url != sharedStack.RestURL {
		t.Errorf("url = %q, want %q", url, sharedStack.RestURL)
	}
	if !strings.HasPrefix(role, "p_") {
		t.Errorf("role = %q, want a p_ tenant role", role)
	}
	_ = pid
}

// The headline Track A claim: a table created via `sql apply` is immediately
// readable and writable through PostgREST using a pwrap-issued JWT.
func TestRest_CRUDThroughPostgREST(t *testing.T) {
	ctx := context.Background()
	pid, key := provision(ctx, t, "rest-crud")

	token, _, role := restToken(ctx, t, key, "")
	applySQL(ctx, t, pid.String(), fmt.Sprintf(`
		CREATE TABLE widgets (
			id    bigserial PRIMARY KEY,
			label text NOT NULL
		);
		GRANT SELECT, INSERT, UPDATE, DELETE ON widgets TO %s;
		GRANT USAGE, SELECT ON SEQUENCE widgets_id_seq TO %s;
	`, role, role))

	status, body := pgrst(ctx, t, http.MethodPost, "/widgets", token, role,
		[]map[string]any{{"label": "first"}})
	if status != http.StatusCreated {
		t.Fatalf("POST /widgets = %d %s, want 201", status, body)
	}

	status, body = pgrst(ctx, t, http.MethodGet, "/widgets?select=label", token, role, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /widgets = %d %s, want 200", status, body)
	}
	var rows []struct {
		Label string `json:"label"`
	}
	if err := json.Unmarshal(body, &rows); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	if len(rows) != 1 || rows[0].Label != "first" {
		t.Fatalf("rows = %+v, want one row labelled %q", rows, "first")
	}
}

// A JWT minted for project A must not reach project B's schema. This is the
// SET ROLE boundary that makes multi-tenancy safe over the REST surface.
func TestRest_TokenCannotReachAnotherProject(t *testing.T) {
	ctx := context.Background()

	pidA, keyA := provision(ctx, t, "rest-tenant-a")
	_, keyB := provision(ctx, t, "rest-tenant-b")

	tokenA, _, roleA := restToken(ctx, t, keyA, "")
	_, _, roleB := restToken(ctx, t, keyB, "")

	applySQL(ctx, t, pidA.String(), fmt.Sprintf(`
		CREATE TABLE secrets (id bigserial PRIMARY KEY, v text NOT NULL);
		INSERT INTO secrets (v) VALUES ('tenant-a-only');
		GRANT SELECT ON secrets TO %s;
	`, roleA))

	// Sanity: A can read its own table.
	if status, body := pgrst(ctx, t, http.MethodGet, "/secrets", tokenA, roleA, nil); status != http.StatusOK {
		t.Fatalf("tenant A reading own table = %d %s, want 200", status, body)
	}

	// A's token, aimed at B's schema, must be refused. PostgREST answers 406 for
	// a schema the request's role may not use.
	status, body := pgrst(ctx, t, http.MethodGet, "/secrets", tokenA, roleB, nil)
	if status == http.StatusOK {
		t.Fatalf("tenant A read tenant B's schema %q: %d %s — cross-tenant leak", roleB, status, body)
	}
	if status < 400 {
		t.Errorf("cross-schema read = %d %s, want a 4xx", status, body)
	}
}

func TestRest_GraphQLEndpoint(t *testing.T) {
	ctx := context.Background()
	pid, key := provision(ctx, t, "rest-graphql")

	token, _, role := restToken(ctx, t, key, "")
	applySQL(ctx, t, pid.String(), fmt.Sprintf(`
		CREATE TABLE todos (
			id    bigserial PRIMARY KEY,
			title text NOT NULL
		);
		INSERT INTO todos (title) VALUES ('write a test');
		GRANT SELECT ON todos TO %s;
		GRANT USAGE ON SCHEMA graphql TO %s;
		GRANT ALL ON FUNCTION graphql.resolve TO %s;
	`, role, role, role))

	status, body := pgrst(ctx, t, http.MethodPost, "/rpc/graphql", token, role,
		map[string]any{"query": "{ todosCollection { edges { node { id title } } } }"})
	if status != http.StatusOK {
		t.Fatalf("POST /rpc/graphql = %d %s, want 200", status, body)
	}
	if bytes.Contains(body, []byte(`"errors"`)) {
		t.Fatalf("graphql returned errors: %s", body)
	}
	if !bytes.Contains(body, []byte("write a test")) {
		t.Fatalf("graphql response missing the seeded row: %s", body)
	}
}

// RLS on a user-defined table, enforced over PostgREST. The SDK half of this
// contract is covered by TestRLS_WithUserScopesQueries against pwrap_documents;
// this is the PostgREST half against a table created via `sql apply`.
//
// FORCE is what makes it work. The tenant role owns any table it creates through
// the escape hatch, and an owner bypasses its own policies without FORCE — while
// PostgREST does SET ROLE into exactly that role. Omit it and the policy is
// silently inert on the REST path. See examples/rls-notes/rls.sql.
func TestRest_RLSOnUserTableOverPostgREST(t *testing.T) {
	ctx := context.Background()
	pid, key := provision(ctx, t, "rest-rls")

	_, _, role := restToken(ctx, t, key, "")
	// Seed before FORCE: once forced, the policy applies to the owner too, so
	// `sql apply` itself is subject to WITH CHECK and — carrying no JWT claims —
	// would be denied. Worth knowing before enabling FORCE on a populated table.
	applySQL(ctx, t, pid.String(), fmt.Sprintf(`
		CREATE TABLE notes (
			id      bigserial PRIMARY KEY,
			user_id text NOT NULL,
			body    text NOT NULL
		);
		INSERT INTO notes (user_id, body) VALUES ('alice','alice note'), ('bob','bob note');

		ALTER TABLE notes ENABLE ROW LEVEL SECURITY;
		ALTER TABLE notes FORCE  ROW LEVEL SECURITY;
		CREATE POLICY notes_owner ON notes
			USING      (user_id = NULLIF(current_setting('request.jwt.claims', true), '')::jsonb->>'user_id')
			WITH CHECK (user_id = NULLIF(current_setting('request.jwt.claims', true), '')::jsonb->>'user_id');
		GRANT SELECT ON notes TO %s;
	`, role))

	read := func(t *testing.T, userID string) []string {
		t.Helper()
		token, _, _ := restToken(ctx, t, key, userID)
		status, body := pgrst(ctx, t, http.MethodGet, "/notes?select=user_id,body", token, role, nil)
		if status != http.StatusOK {
			t.Fatalf("GET /notes as %q = %d %s, want 200", userID, status, body)
		}
		var rows []struct {
			UserID string `json:"user_id"`
			Body   string `json:"body"`
		}
		if err := json.Unmarshal(body, &rows); err != nil {
			t.Fatalf("decode %s: %v", body, err)
		}
		out := make([]string, 0, len(rows))
		for _, r := range rows {
			out = append(out, r.UserID)
		}
		return out
	}

	// Each user sees exactly their own row — asserting both directions, so a
	// policy that happens to return one arbitrary row can't pass.
	if got := read(t, "alice"); len(got) != 1 || got[0] != "alice" {
		t.Errorf("alice saw %v, want [alice]", got)
	}
	if got := read(t, "bob"); len(got) != 1 || got[0] != "bob" {
		t.Errorf("bob saw %v, want [bob]", got)
	}

	// A token carrying no user_id must see nothing. The NULLIF in the policy is
	// what makes this deny cleanly instead of erroring on a cast of ''.
	if got := read(t, ""); len(got) != 0 {
		t.Errorf("a token with no user_id saw %v, want no rows", got)
	}
}

// Verify the SDK still reaches the same database the REST path does, so the two
// halves of the RLS contract are demonstrably talking about one system.
func TestRest_SDKAndPostgRESTShareASchema(t *testing.T) {
	ctx := context.Background()
	_, key := provision(ctx, t, "rest-shared-schema")

	c, err := pwrap.New(ctx, pwrap.Config{ControlURL: sharedStack.ControlURL, APIKey: key})
	if err != nil {
		t.Fatalf("sdk connect: %v", err)
	}
	defer c.Close()

	id, err := c.Table("shared").Insert(ctx, map[string]any{"marker": "via-sdk"})
	if err != nil {
		t.Fatalf("insert via sdk: %v", err)
	}

	token, _, role := restToken(ctx, t, key, "")
	if role != c.Schema() {
		t.Errorf("rest role %q != sdk schema %q", role, c.Schema())
	}

	status, body := pgrst(ctx, t, http.MethodGet,
		"/pwrap_documents?select=id&id=eq."+id.String(), token, role, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /pwrap_documents = %d %s, want 200", status, body)
	}
	if !bytes.Contains(body, []byte(id.String())) {
		t.Errorf("row inserted via the SDK not visible over PostgREST: %s", body)
	}
}
