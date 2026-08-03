package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/b3vet/pwrap/internal/neon"
)

var version = "0.0.0-dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]

	var err error
	switch cmd {
	case "version":
		fmt.Println("pwrap", version)
	case "health":
		err = runHealth()
	case "project":
		err = runProject(args)
	case "key":
		err = runKey(args)
	case "migrate":
		err = runMigrate(args)
	case "sql":
		err = runSQL(args)
	case "branch":
		err = runBranch(args)
	case "neon":
		err = runNeon(args)
	case "help", "-h", "--help":
		usage()
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `pwrap — control-plane CLI

usage:
  pwrap health
  pwrap version
  pwrap project create --name NAME
  pwrap project list
  pwrap project get ID
  pwrap project delete ID
  pwrap key issue  --project ID [--name NAME]
  pwrap key list   --project ID
  pwrap key revoke --project ID --key KEYID
  pwrap migrate apply  --project ID
  pwrap migrate status --project ID
  pwrap sql apply      --project ID --file PATH
  pwrap branch create  --project ID --name NAME [--with-data]
  pwrap branch list    --project ID
  pwrap branch sync    --project ID [--truncate] [--tables a,b,c]
  pwrap neon branch create --neon-project NEON_PROJ --name NAME [--parent BRANCH_ID]
  pwrap neon branch delete --neon-project NEON_PROJ --branch BRANCH_ID
  pwrap neon branch list   --neon-project NEON_PROJ
  pwrap neon connection-uri --neon-project NEON_PROJ --branch BRANCH_ID --role R --database D [--pooled]

env:
  PWRAP_NEON_API_KEY      required for "pwrap neon ..." subcommands
  PWRAP_CONTROL_URL       default http://localhost:8080
  PWRAP_BOOTSTRAP_TOKEN   required for project/key subcommands`)
}

// --- transport -----------------------------------------------------------------

type client struct {
	base  string
	token string
	http  *http.Client
}

func newClient(requireToken bool) (*client, error) {
	base := os.Getenv("PWRAP_CONTROL_URL")
	if base == "" {
		base = "http://localhost:8080"
	}
	if _, err := url.Parse(base); err != nil {
		return nil, fmt.Errorf("bad PWRAP_CONTROL_URL: %w", err)
	}
	tok := os.Getenv("PWRAP_BOOTSTRAP_TOKEN")
	if requireToken && tok == "" {
		return nil, errors.New("PWRAP_BOOTSTRAP_TOKEN is required")
	}
	return &client{
		base:  strings.TrimRight(base, "/"),
		token: tok,
		http:  &http.Client{Timeout: 15 * time.Second},
	}, nil
}

func (c *client) do(method, path string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(buf)
	}
	req, err := http.NewRequest(method, c.base+path, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s %s: %s %s", method, path, resp.Status, strings.TrimSpace(string(b)))
	}
	if out != nil && resp.StatusCode != http.StatusNoContent {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// --- commands ------------------------------------------------------------------

func runHealth() error {
	c, err := newClient(false)
	if err != nil {
		return err
	}
	var body map[string]string
	if err := c.do(http.MethodGet, "/v1/readyz", nil, &body); err != nil {
		return err
	}
	printJSON(body)
	return nil
}

func runProject(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: pwrap project <create|list|get|delete>")
	}
	sub, args := args[0], args[1:]
	c, err := newClient(true)
	if err != nil {
		return err
	}
	switch sub {
	case "create":
		fs := flag.NewFlagSet("project create", flag.ExitOnError)
		name := fs.String("name", "", "project name")
		_ = fs.Parse(args)
		if *name == "" {
			return errors.New("--name required")
		}
		var out map[string]any
		if err := c.do(http.MethodPost, "/v1/projects", map[string]string{"name": *name}, &out); err != nil {
			return err
		}
		printJSON(out)
	case "list":
		var out []map[string]any
		if err := c.do(http.MethodGet, "/v1/projects", nil, &out); err != nil {
			return err
		}
		printJSON(out)
	case "get":
		if len(args) < 1 {
			return errors.New("usage: pwrap project get ID")
		}
		var out map[string]any
		if err := c.do(http.MethodGet, "/v1/projects/"+args[0], nil, &out); err != nil {
			return err
		}
		printJSON(out)
	case "delete":
		if len(args) < 1 {
			return errors.New("usage: pwrap project delete ID")
		}
		return c.do(http.MethodDelete, "/v1/projects/"+args[0], nil, nil)
	default:
		return fmt.Errorf("unknown project subcommand %q", sub)
	}
	return nil
}

func runKey(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: pwrap key <issue|list|revoke>")
	}
	sub, args := args[0], args[1:]
	c, err := newClient(true)
	if err != nil {
		return err
	}
	switch sub {
	case "issue":
		fs := flag.NewFlagSet("key issue", flag.ExitOnError)
		project := fs.String("project", "", "project id")
		name := fs.String("name", "", "human label")
		_ = fs.Parse(args)
		if *project == "" {
			return errors.New("--project required")
		}
		var out map[string]any
		if err := c.do(http.MethodPost, "/v1/projects/"+*project+"/keys", map[string]string{"name": *name}, &out); err != nil {
			return err
		}
		printJSON(out)
	case "list":
		fs := flag.NewFlagSet("key list", flag.ExitOnError)
		project := fs.String("project", "", "project id")
		_ = fs.Parse(args)
		if *project == "" {
			return errors.New("--project required")
		}
		var out []map[string]any
		if err := c.do(http.MethodGet, "/v1/projects/"+*project+"/keys", nil, &out); err != nil {
			return err
		}
		printJSON(out)
	case "revoke":
		fs := flag.NewFlagSet("key revoke", flag.ExitOnError)
		project := fs.String("project", "", "project id")
		key := fs.String("key", "", "key id")
		_ = fs.Parse(args)
		if *project == "" || *key == "" {
			return errors.New("--project and --key required")
		}
		return c.do(http.MethodDelete, "/v1/projects/"+*project+"/keys/"+*key, nil, nil)
	default:
		return fmt.Errorf("unknown key subcommand %q", sub)
	}
	return nil
}

func runMigrate(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: pwrap migrate <apply|status> --project ID")
	}
	sub, args := args[0], args[1:]
	c, err := newClient(true)
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("migrate "+sub, flag.ExitOnError)
	project := fs.String("project", "", "project id")
	_ = fs.Parse(args)
	if *project == "" {
		return errors.New("--project required")
	}
	switch sub {
	case "apply":
		var out map[string]any
		if err := c.do(http.MethodPost, "/v1/projects/"+*project+"/migrations", nil, &out); err != nil {
			return err
		}
		printJSON(out)
	case "status":
		var out map[string]any
		if err := c.do(http.MethodGet, "/v1/projects/"+*project+"/migrations", nil, &out); err != nil {
			return err
		}
		printJSON(out)
	default:
		return fmt.Errorf("unknown migrate subcommand %q", sub)
	}
	return nil
}

func runSQL(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: pwrap sql apply --project ID --file PATH")
	}
	sub, args := args[0], args[1:]
	if sub != "apply" {
		return fmt.Errorf("unknown sql subcommand %q", sub)
	}
	fs := flag.NewFlagSet("sql apply", flag.ExitOnError)
	project := fs.String("project", "", "project id")
	file := fs.String("file", "", "path to .sql file")
	_ = fs.Parse(args)
	if *project == "" || *file == "" {
		return errors.New("--project and --file required")
	}
	body, err := os.ReadFile(*file)
	if err != nil {
		return fmt.Errorf("read %s: %w", *file, err)
	}
	c, err := newClient(true)
	if err != nil {
		return err
	}
	// Send the SQL as the raw body (not JSON) — server reads it verbatim.
	req, err := http.NewRequest(http.MethodPost, c.base+"/v1/projects/"+*project+"/sql", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/sql")
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("POST /sql: %s %s", resp.Status, strings.TrimSpace(string(b)))
	}
	fmt.Printf("applied %d bytes to project %s\n", len(body), *project)
	return nil
}

func runBranch(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: pwrap branch <create|list|sync>")
	}
	sub, args := args[0], args[1:]
	c, err := newClient(true)
	if err != nil {
		return err
	}
	switch sub {
	case "create":
		fs := flag.NewFlagSet("branch create", flag.ExitOnError)
		project := fs.String("project", "", "parent project id")
		name := fs.String("name", "", "branch project name (defaults to <slug>_branch)")
		withData := fs.Bool("with-data", false, "copy parent's pwrap_documents/embeddings/geo/matviews data")
		_ = fs.Parse(args)
		if *project == "" {
			return errors.New("--project required")
		}
		body := map[string]any{"with_data": *withData}
		if *name != "" {
			body["name"] = *name
		}
		var out map[string]any
		if err := c.do(http.MethodPost, "/v1/projects/"+*project+"/branches", body, &out); err != nil {
			return err
		}
		printJSON(out)
	case "list":
		fs := flag.NewFlagSet("branch list", flag.ExitOnError)
		project := fs.String("project", "", "parent project id")
		_ = fs.Parse(args)
		if *project == "" {
			return errors.New("--project required")
		}
		var out []map[string]any
		if err := c.do(http.MethodGet, "/v1/projects/"+*project+"/branches", nil, &out); err != nil {
			return err
		}
		printJSON(out)
	case "sync":
		fs := flag.NewFlagSet("branch sync", flag.ExitOnError)
		project := fs.String("project", "", "branch (child) project id")
		truncate := fs.Bool("truncate", false, "truncate target tables before copy")
		tablesCSV := fs.String("tables", "", "comma-separated table names (default: built-ins)")
		_ = fs.Parse(args)
		if *project == "" {
			return errors.New("--project required (the branch project, not the parent)")
		}
		body := map[string]any{"truncate": *truncate}
		if *tablesCSV != "" {
			var ts []string
			for _, t := range strings.Split(*tablesCSV, ",") {
				ts = append(ts, strings.TrimSpace(t))
			}
			body["tables"] = ts
		}
		return c.do(http.MethodPost, "/v1/projects/"+*project+"/sync", body, nil)
	default:
		return fmt.Errorf("unknown branch subcommand %q", sub)
	}
	return nil
}

func runNeon(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: pwrap neon <branch|connection-uri> …")
	}
	apiKey := os.Getenv("PWRAP_NEON_API_KEY")
	if apiKey == "" {
		return errors.New("PWRAP_NEON_API_KEY is required for `pwrap neon ...`")
	}
	// PWRAP_NEON_BASE_URL overrides DefaultBaseURL; used by tests and for self-hosted
	// Neon-compatible APIs.
	var nc *neon.Client
	if base := os.Getenv("PWRAP_NEON_BASE_URL"); base != "" {
		nc = neon.NewClientWithBaseURL(apiKey, base)
	} else {
		nc = neon.NewClient(apiKey)
	}
	sub := args[0]
	args = args[1:]
	switch sub {
	case "branch":
		return runNeonBranch(nc, args)
	case "connection-uri":
		return runNeonConnURI(nc, args)
	default:
		return fmt.Errorf("unknown neon subcommand %q", sub)
	}
}

func runNeonBranch(nc *neon.Client, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: pwrap neon branch <create|delete|list> …")
	}
	op := args[0]
	args = args[1:]
	ctx := context.Background()
	switch op {
	case "create":
		fs := flag.NewFlagSet("neon branch create", flag.ExitOnError)
		project := fs.String("neon-project", "", "Neon project id")
		name := fs.String("name", "", "new branch name")
		parent := fs.String("parent", "", "parent branch id (default: primary)")
		noEndpoint := fs.Bool("no-endpoint", false, "don't create a read-write endpoint")
		_ = fs.Parse(args)
		if *project == "" || *name == "" {
			return errors.New("--neon-project and --name required")
		}
		br, eps, err := nc.CreateBranch(ctx, *project, neon.CreateBranchRequest{
			Name: *name, ParentID: *parent, CreateEndpoint: !*noEndpoint,
		})
		if err != nil {
			return err
		}
		printJSON(map[string]any{"branch": br, "endpoints": eps})
	case "delete":
		fs := flag.NewFlagSet("neon branch delete", flag.ExitOnError)
		project := fs.String("neon-project", "", "Neon project id")
		branch := fs.String("branch", "", "branch id")
		_ = fs.Parse(args)
		if *project == "" || *branch == "" {
			return errors.New("--neon-project and --branch required")
		}
		return nc.DeleteBranch(ctx, *project, *branch)
	case "list":
		fs := flag.NewFlagSet("neon branch list", flag.ExitOnError)
		project := fs.String("neon-project", "", "Neon project id")
		_ = fs.Parse(args)
		if *project == "" {
			return errors.New("--neon-project required")
		}
		branches, err := nc.ListBranches(ctx, *project)
		if err != nil {
			return err
		}
		printJSON(branches)
	default:
		return fmt.Errorf("unknown neon branch op %q", op)
	}
	return nil
}

func runNeonConnURI(nc *neon.Client, args []string) error {
	fs := flag.NewFlagSet("neon connection-uri", flag.ExitOnError)
	project := fs.String("neon-project", "", "Neon project id")
	branch := fs.String("branch", "", "branch id")
	role := fs.String("role", "", "Postgres role name")
	database := fs.String("database", "", "Postgres database name")
	pooled := fs.Bool("pooled", false, "use PgBouncer-pooled URI")
	_ = fs.Parse(args)
	if *project == "" || *branch == "" || *role == "" || *database == "" {
		return errors.New("--neon-project, --branch, --role, --database required")
	}
	uri, err := nc.GetConnectionURI(context.Background(), *project, neon.ConnectionURIRequest{
		BranchID: *branch, RoleName: *role, DatabaseName: *database, Pooled: *pooled,
	})
	if err != nil {
		return err
	}
	fmt.Println(uri)
	return nil
}

func printJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}
