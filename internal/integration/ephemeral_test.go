//go:build integration

package integration

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/b3vet/pwrap/internal/controlplane/tenancy"
)

// dsnAs rewrites the stack's admin DSN to use the given credentials, so a test
// can attempt a real login as a minted role over the same TCP path an SDK uses.
// That matters: Postgres only enforces VALID UNTIL on password authentication,
// and the container trusts its own loopback, so a check that avoids scram would
// pass no matter what.
func dsnAs(t *testing.T, user, password string) string {
	t.Helper()
	u, err := url.Parse(sharedStack.DatabaseURL)
	if err != nil {
		t.Fatalf("parse stack dsn: %v", err)
	}
	u.User = url.UserPassword(user, password)
	return u.String()
}

func tenantRoleAndSchema(ctx context.Context, t *testing.T, projectID uuid.UUID) (role, schema string) {
	t.Helper()
	if err := sharedStack.Pool().QueryRow(ctx,
		`SELECT pg_role, pg_schema FROM projects WHERE id = $1`, projectID,
	).Scan(&role, &schema); err != nil {
		t.Fatalf("load project role: %v", err)
	}
	return role, schema
}

// Every exchange must hand out fresh credentials. If two calls returned the same
// role, revoking one client's access would revoke everyone's, which is the whole
// problem rotation exists to solve.
func TestEphemeral_EachExchangeMintsDistinctCredentials(t *testing.T) {
	ctx := context.Background()
	pid, key := provision(ctx, t, "eph-distinct")
	_ = pid

	first := connectionDSN(ctx, t, key)
	second := connectionDSN(ctx, t, key)

	u1, _ := url.Parse(first)
	u2, _ := url.Parse(second)
	if u1.User.Username() == u2.User.Username() {
		t.Errorf("two exchanges returned the same role %q — credentials are not rotating", u1.User.Username())
	}
	p1, _ := u1.User.Password()
	p2, _ := u2.User.Password()
	if p1 == p2 {
		t.Error("two exchanges returned the same password")
	}
	// And neither may be the tenant role's own long-lived credential.
	role, _ := tenantRoleAndSchema(ctx, t, pid)
	if u1.User.Username() == role {
		t.Errorf("exchange handed out the tenant role %q itself", role)
	}
}

// The point of the change: expiry is enforced by Postgres, not by client
// goodwill. A DSN past VALID UNTIL must be refused even with the right password.
func TestEphemeral_ExpiredCredentialIsRefusedByPostgres(t *testing.T) {
	ctx := context.Background()
	pid, _ := provision(ctx, t, "eph-expiry")
	role, schema := tenantRoleAndSchema(ctx, t, pid)

	// A TTL already in the past: VALID UNTIL is set at mint time, so this role
	// is born expired.
	expired, expiredPw, _, err := tenancy.Mint(ctx, sharedStack.Pool(), pid, role, schema, -time.Minute)
	if err != nil {
		t.Fatalf("mint expired: %v", err)
	}
	if _, err := pgx.Connect(ctx, dsnAs(t, expired, expiredPw)); err == nil {
		t.Fatal("expired credential was accepted — VALID UNTIL is not being enforced")
	}

	// Control: the same code path with a live TTL must work, otherwise the test
	// above proves nothing about expiry.
	live, livePw, expiresAt, err := tenancy.Mint(ctx, sharedStack.Pool(), pid, role, schema, time.Hour)
	if err != nil {
		t.Fatalf("mint live: %v", err)
	}
	if !expiresAt.After(time.Now()) {
		t.Errorf("expiresAt %v is not in the future", expiresAt)
	}
	conn, err := pgx.Connect(ctx, dsnAs(t, live, livePw))
	if err != nil {
		t.Fatalf("live credential refused: %v", err)
	}
	defer conn.Close(ctx)

	// A wrong password must still fail, or "accepted" would mean nothing.
	if _, err := pgx.Connect(ctx, dsnAs(t, live, "not-the-password")); err == nil {
		t.Error("wrong password was accepted")
	}
}

// A minted role is only useful if it lands in the tenant's schema with the
// tenant's privileges. Role-level settings are not inherited, so search_path has
// to be set on the ephemeral role explicitly — easy to forget, silent when wrong.
func TestEphemeral_RoleInheritsTenantAccess(t *testing.T) {
	ctx := context.Background()
	pid, key := provision(ctx, t, "eph-access")

	conn, err := pgx.Connect(ctx, connectionDSN(ctx, t, key))
	if err != nil {
		t.Fatalf("connect with minted dsn: %v", err)
	}
	defer conn.Close(ctx)

	_, schema := tenantRoleAndSchema(ctx, t, pid)
	var current string
	if err := conn.QueryRow(ctx, `SELECT current_schema()`).Scan(&current); err != nil {
		t.Fatalf("current_schema: %v", err)
	}
	if current != schema {
		t.Errorf("current_schema() = %q, want the tenant schema %q", current, schema)
	}

	// Unqualified DML must reach the tenant's own tables.
	var id string
	if err := conn.QueryRow(ctx,
		`INSERT INTO pwrap_documents (collection, data) VALUES ('eph', '{"v":1}'::jsonb) RETURNING id`,
	).Scan(&id); err != nil {
		t.Fatalf("insert via minted role: %v", err)
	}
}

// Expired roles must not pile up in pg_authid forever.
func TestEphemeral_SweepDropsExpiredRolesOnly(t *testing.T) {
	ctx := context.Background()
	pid, _ := provision(ctx, t, "eph-sweep")
	role, schema := tenantRoleAndSchema(ctx, t, pid)

	// Past the grace window the sweeper honours, so it is eligible now.
	stale, _, _, err := tenancy.Mint(ctx, sharedStack.Pool(), pid, role, schema, -time.Hour)
	if err != nil {
		t.Fatalf("mint stale: %v", err)
	}
	live, _, _, err := tenancy.Mint(ctx, sharedStack.Pool(), pid, role, schema, time.Hour)
	if err != nil {
		t.Fatalf("mint live: %v", err)
	}

	if _, err := tenancy.Sweep(ctx, sharedStack.Pool()); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if roleExists(ctx, t, stale) {
		t.Errorf("expired role %q survived the sweep", stale)
	}
	if !roleExists(ctx, t, live) {
		t.Errorf("live role %q was dropped by the sweep", live)
	}
	// The bookkeeping row must go with it, or the sweeper retries forever.
	var n int
	if err := sharedStack.Pool().QueryRow(ctx,
		`SELECT count(*) FROM ephemeral_roles WHERE role_name = $1`, stale).Scan(&n); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if n != 0 {
		t.Errorf("ephemeral_roles still has a row for the dropped role %q", stale)
	}
}

// Deleting a project must not leave its minted roles behind — and must not fail
// because the tenant role still has members.
func TestEphemeral_ProjectDeleteRemovesMintedRoles(t *testing.T) {
	ctx := context.Background()
	a := newAdmin()
	var p struct {
		ID uuid.UUID `json:"id"`
	}
	if err := a.do(ctx, "POST", "/v1/projects", map[string]string{"name": "eph-delete"}, &p); err != nil {
		t.Fatalf("create project: %v", err)
	}
	if err := a.do(ctx, "POST", fmt.Sprintf("/v1/projects/%s/migrations", p.ID), nil, nil); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	role, schema := tenantRoleAndSchema(ctx, t, p.ID)
	minted, _, _, err := tenancy.Mint(ctx, sharedStack.Pool(), p.ID, role, schema, time.Hour)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if !roleExists(ctx, t, minted) {
		t.Fatalf("minted role %q missing before delete", minted)
	}

	if err := a.do(ctx, "DELETE", "/v1/projects/"+p.ID.String(), nil, nil); err != nil {
		t.Fatalf("delete project: %v", err)
	}
	if roleExists(ctx, t, minted) {
		t.Errorf("minted role %q outlived its project", minted)
	}
	if roleExists(ctx, t, role) {
		t.Errorf("tenant role %q outlived its project", role)
	}
}

func roleExists(ctx context.Context, t *testing.T, name string) bool {
	t.Helper()
	var ok bool
	if err := sharedStack.Pool().QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`, name).Scan(&ok); err != nil {
		t.Fatalf("check role: %v", err)
	}
	return ok
}

// connectionDSN performs the real /v1/connection exchange an SDK would.
func connectionDSN(ctx context.Context, t *testing.T, apiKey string) string {
	t.Helper()
	var out struct {
		DSN string `json:"dsn"`
	}
	req := newAdmin()
	req.token = apiKey // the endpoint authenticates with the project key, not admin
	if err := req.do(ctx, "POST", "/v1/connection", nil, &out); err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if !strings.HasPrefix(out.DSN, "postgres://") {
		t.Fatalf("unexpected dsn %q", out.DSN)
	}
	return out.DSN
}

// createAs opens a real login session as the given credentials and runs stmt, so
// whatever it creates is genuinely owned by that role. Going through the admin
// pool would make the admin the owner and prove nothing.
func createAs(ctx context.Context, t *testing.T, user, password, stmt string) {
	t.Helper()
	conn, err := pgx.Connect(ctx, dsnAs(t, user, password))
	if err != nil {
		t.Fatalf("connect as %s: %v", user, err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, stmt); err != nil {
		t.Fatalf("exec as %s: %v", user, err)
	}
}

func objectOwner(ctx context.Context, t *testing.T, schema, name string) string {
	t.Helper()
	var owner string
	err := sharedStack.Pool().QueryRow(ctx, `
		SELECT pg_get_userbyid(c.relowner)
		  FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = $1 AND c.relname = $2
	`, schema, name).Scan(&owner)
	if err != nil {
		t.Fatalf("owner of %s.%s: %v", schema, name, err)
	}
	return owner
}

// A client creates objects while connected as its ephemeral role, which makes
// that role the owner — and Postgres refuses to drop a role that owns anything.
// The sweeper has to hand ownership to the tenant role, which outlives every
// credential minted for it, rather than fail on that role forever.
func TestEphemeral_SweepReassignsObjectsItOwns(t *testing.T) {
	ctx := context.Background()
	pid, _ := provision(ctx, t, "eph-owns")
	role, schema := tenantRoleAndSchema(ctx, t, pid)

	stale, password, _, err := tenancy.Mint(ctx, sharedStack.Pool(), pid, role, schema, -time.Hour)
	if err != nil {
		t.Fatalf("mint stale: %v", err)
	}
	// VALID UNTIL is already past, so make the role usable just long enough to
	// create something as itself. The expiry is not what this test is about.
	if _, err := sharedStack.Pool().Exec(ctx,
		fmt.Sprintf(`ALTER ROLE %q VALID UNTIL 'infinity'`, stale)); err != nil {
		t.Fatalf("extend stale role: %v", err)
	}
	createAs(ctx, t, stale, password,
		`CREATE MATERIALIZED VIEW owned_by_client AS SELECT 1 AS n`)
	if got := objectOwner(ctx, t, schema, "owned_by_client"); got != stale {
		t.Fatalf("matview owner is %q, want the ephemeral role %q — test proves nothing", got, stale)
	}
	if _, err := sharedStack.Pool().Exec(ctx,
		fmt.Sprintf(`ALTER ROLE %q VALID UNTIL '2000-01-01'`, stale)); err != nil {
		t.Fatalf("re-expire stale role: %v", err)
	}

	if _, err := tenancy.Sweep(ctx, sharedStack.Pool()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if roleExists(ctx, t, stale) {
		t.Errorf("role %q owning a matview survived the sweep", stale)
	}
	// The object must survive — it is the tenant's data, not the credential's.
	if got := objectOwner(ctx, t, schema, "owned_by_client"); got != role {
		t.Errorf("matview owner is %q after the sweep, want the tenant role %q", got, role)
	}
}

// Same hazard on the delete path: a project whose client created a matview must
// still delete cleanly.
func TestEphemeral_ProjectDeleteWithClientOwnedObjects(t *testing.T) {
	ctx := context.Background()
	a := newAdmin()
	var p struct {
		ID uuid.UUID `json:"id"`
	}
	if err := a.do(ctx, "POST", "/v1/projects", map[string]string{"name": "eph-delete-owned"}, &p); err != nil {
		t.Fatalf("create project: %v", err)
	}
	if err := a.do(ctx, "POST", fmt.Sprintf("/v1/projects/%s/migrations", p.ID), nil, nil); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	role, schema := tenantRoleAndSchema(ctx, t, p.ID)
	minted, password, _, err := tenancy.Mint(ctx, sharedStack.Pool(), p.ID, role, schema, time.Hour)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	createAs(ctx, t, minted, password,
		`CREATE MATERIALIZED VIEW owned_by_client AS SELECT 1 AS n`)
	if got := objectOwner(ctx, t, schema, "owned_by_client"); got != minted {
		t.Fatalf("matview owner is %q, want the ephemeral role %q — test proves nothing", got, minted)
	}

	if err := a.do(ctx, "DELETE", "/v1/projects/"+p.ID.String(), nil, nil); err != nil {
		t.Fatalf("delete project holding a client-owned matview: %v", err)
	}
	if roleExists(ctx, t, minted) {
		t.Errorf("minted role %q outlived its project", minted)
	}
	if roleExists(ctx, t, role) {
		t.Errorf("tenant role %q outlived its project", role)
	}
}
