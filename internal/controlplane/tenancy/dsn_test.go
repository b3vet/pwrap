package tenancy

import (
	"net/url"
	"strings"
	"testing"

	"github.com/b3vet/pwrap/internal/config"
)

func testCfg() config.Config {
	return config.Config{
		TenantHost:     "db.internal",
		TenantPort:     "5432",
		TenantDatabase: "pwrap",
		TenantSSLMode:  "require",
	}
}

func TestBuildDSNShape(t *testing.T) {
	got := BuildDSN(testCfg(), "p_myproj", "s3cret")

	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("BuildDSN produced an unparseable URL %q: %v", got, err)
	}
	if u.Scheme != "postgres" {
		t.Errorf("scheme = %q, want %q", u.Scheme, "postgres")
	}
	if u.Host != "db.internal:5432" {
		t.Errorf("host = %q, want %q", u.Host, "db.internal:5432")
	}
	if u.Path != "/pwrap" {
		t.Errorf("path = %q, want %q", u.Path, "/pwrap")
	}
	if u.User.Username() != "p_myproj" {
		t.Errorf("user = %q, want %q", u.User.Username(), "p_myproj")
	}
	if pw, _ := u.User.Password(); pw != "s3cret" {
		t.Errorf("password = %q, want %q", pw, "s3cret")
	}
	if got := u.Query().Get("sslmode"); got != "require" {
		t.Errorf("sslmode = %q, want %q", got, "require")
	}
}

// A password containing URL metacharacters must survive the round trip. Getting
// this wrong yields a DSN that silently connects as the wrong user, points at
// the wrong host, or fails to parse at all.
func TestBuildDSNEscapesCredentials(t *testing.T) {
	tests := []struct {
		name     string
		role     string
		password string
	}{
		{"at sign", "p_proj", "pa@ss"},
		{"colon", "p_proj", "pa:ss"},
		{"slash", "p_proj", "pa/ss"},
		{"question mark", "p_proj", "pa?ss"},
		{"hash", "p_proj", "pa#ss"},
		{"ampersand", "p_proj", "pa&ss=x"},
		{"percent", "p_proj", "pa%ss"},
		{"space", "p_proj", "pa ss"},
		{"host injection", "p_proj", "x@evil.example.com/other"},
		{"role with at sign", "p_pr@oj", "plain"},
		{"all of it", "p_proj", "p@:/?#&% ss"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dsn := BuildDSN(testCfg(), tt.role, tt.password)

			u, err := url.Parse(dsn)
			if err != nil {
				t.Fatalf("unparseable DSN %q: %v", dsn, err)
			}
			if got := u.User.Username(); got != tt.role {
				t.Errorf("role round-tripped as %q, want %q (dsn=%q)", got, tt.role, dsn)
			}
			pw, ok := u.User.Password()
			if !ok {
				t.Fatalf("no password in %q", dsn)
			}
			if pw != tt.password {
				t.Errorf("password round-tripped as %q, want %q (dsn=%q)", pw, tt.password, dsn)
			}
			// Credentials must not be able to move the host or database.
			if u.Host != "db.internal:5432" {
				t.Errorf("host = %q, want %q — credentials escaped their field (dsn=%q)", u.Host, "db.internal:5432", dsn)
			}
			if u.Path != "/pwrap" {
				t.Errorf("path = %q, want %q (dsn=%q)", u.Path, "/pwrap", dsn)
			}
		})
	}
}

func TestBuildDSNHonoursSSLMode(t *testing.T) {
	for _, mode := range []string{"disable", "require", "verify-full"} {
		cfg := testCfg()
		cfg.TenantSSLMode = mode
		u, err := url.Parse(BuildDSN(cfg, "p_x", "pw"))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if got := u.Query().Get("sslmode"); got != mode {
			t.Errorf("sslmode = %q, want %q", got, mode)
		}
	}
}

// search_path is deliberately not in the DSN — the tenant role carries it via
// ALTER ROLE, and libpq rejects it as a connection parameter.
func TestBuildDSNOmitsSearchPath(t *testing.T) {
	dsn := BuildDSN(testCfg(), "p_x", "pw")
	if strings.Contains(dsn, "search_path") {
		t.Errorf("DSN contains search_path, which libpq rejects: %q", dsn)
	}
}

func TestBuildDSNUsesConfiguredHostNotDatabaseURL(t *testing.T) {
	// pwrapd may reach Postgres at a different address than clients do (internal
	// host vs. public pooler). The minted DSN must use the tenant-facing values.
	cfg := config.Config{
		DatabaseURL:    "postgres://admin:admin@127.0.0.1:5432/pwrap?sslmode=disable",
		TenantHost:     "pooler.example.com",
		TenantPort:     "6543",
		TenantDatabase: "appdb",
		TenantSSLMode:  "require",
	}
	u, err := url.Parse(BuildDSN(cfg, "p_x", "pw"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if u.Host != "pooler.example.com:6543" {
		t.Errorf("host = %q, want the tenant-facing pooler address", u.Host)
	}
	if u.Path != "/appdb" {
		t.Errorf("path = %q, want %q", u.Path, "/appdb")
	}
}
