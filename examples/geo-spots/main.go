// geo-spots demonstrates Track B's spatial pillar:
//
//   - Inserts a handful of well-known coordinates as Points into pwrap_geo.
//   - Runs WithinRadius (geography-accurate, meters) around a city center.
//   - Runs WithinBBox (GIST-fast bounding box) over a country-sized envelope.
//   - Runs Nearest (KNN via GIST `<->`) for top-k closest landmarks.
//   - As a bonus, exercises pwrap_partition_ensure to materialise a monthly partition
//     of a user-created `events` table — the declarative-partitioning escape hatch.
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

	"github.com/berkeucvet/pwrap/sdk/go/pwrap"
)

type landmark struct {
	name string
	lng  float64
	lat  float64
}

var landmarks = []landmark{
	{"Eiffel Tower", 2.2945, 48.8584},
	{"Louvre", 2.3376, 48.8606},
	{"Notre-Dame", 2.3499, 48.8530},
	{"Arc de Triomphe", 2.2950, 48.8738},
	{"Sacré-Cœur", 2.3431, 48.8867},
	{"Buckingham Palace", -0.1419, 51.5014},
	{"Tower of London", -0.0762, 51.5081},
	{"Big Ben", -0.1246, 51.5007},
	{"London Eye", -0.1196, 51.5033},
	{"British Museum", -0.1270, 51.5194},
	{"Brandenburg Gate", 13.3777, 52.5163},
	{"Reichstag", 13.3761, 52.5186},
	{"Berlin TV Tower", 13.4094, 52.5208},
	{"Statue of Liberty", -74.0445, 40.6892},
	{"Empire State Building", -73.9857, 40.7484},
	{"Central Park", -73.9683, 40.7851},
	{"Tokyo Skytree", 139.8107, 35.7101},
	{"Sensō-ji", 139.7967, 35.7148},
	{"Tokyo Tower", 139.7454, 35.6586},
}

func main() {
	if err := run(context.Background()); err != nil {
		log.Fatalf("geo-spots: %v", err)
	}
}

func run(ctx context.Context) error {
	controlURL := envOr("PWRAP_CONTROL_URL", "http://localhost:8080")
	adminToken := os.Getenv("PWRAP_BOOTSTRAP_TOKEN")
	if adminToken == "" {
		return errors.New("PWRAP_BOOTSTRAP_TOKEN required")
	}
	admin := newAdmin(controlURL, adminToken)

	proj, err := admin.createProject(ctx, fmt.Sprintf("geo-spots-%d", time.Now().Unix()))
	if err != nil {
		return err
	}
	log.Printf("project %s", proj.ID)
	defer func() {
		_ = admin.deleteProject(context.Background(), proj.ID)
	}()

	if _, err := admin.applyMigrations(ctx, proj.ID); err != nil {
		return err
	}
	log.Printf("migrations applied")

	issued, err := admin.issueKey(ctx, proj.ID, "geo-spots")
	if err != nil {
		return err
	}
	c, err := pwrap.New(ctx, pwrap.Config{ControlURL: controlURL, APIKey: issued.Key})
	if err != nil {
		return err
	}
	defer c.Close()

	geo := c.Geo("landmarks")
	for _, l := range landmarks {
		if _, err := geo.InsertPoint(ctx, l.lng, l.lat, map[string]any{"name": l.name}); err != nil {
			return fmt.Errorf("insert %s: %w", l.name, err)
		}
	}
	count, _ := geo.Count(ctx)
	log.Printf("inserted %d landmarks", count)

	// Around Paris city center, within 2 km
	fmt.Println()
	fmt.Println("=== WithinRadius (Paris, 2km) ===")
	near, err := geo.WithinRadius(ctx, 2.3522, 48.8566, 2000, 10)
	if err != nil {
		return err
	}
	for _, f := range near {
		fmt.Printf("  %6.0f m  %s\n", f.DistanceMeters, f.Metadata["name"])
	}

	// Bounding box over Greater London (rough envelope)
	fmt.Println()
	fmt.Println("=== WithinBBox (Greater London envelope) ===")
	london, err := geo.WithinBBox(ctx, -0.510, 51.286, 0.334, 51.692, 100)
	if err != nil {
		return err
	}
	for _, f := range london {
		fmt.Printf("  %s\n", f.Metadata["name"])
	}

	// 5 nearest to a query point in Berlin
	fmt.Println()
	fmt.Println("=== Nearest 5 to Berlin (52.520, 13.405) ===")
	nearestBerlin, err := geo.Nearest(ctx, 13.405, 52.520, 5)
	if err != nil {
		return err
	}
	for _, f := range nearestBerlin {
		fmt.Printf("  %7.1f km  %s\n", f.DistanceMeters/1000, f.Metadata["name"])
	}

	// Bonus: partitioning. Create a partitioned table and materialise monthly
	// partitions via the pwrap_partition_ensure helper.
	if err := demoPartitioning(ctx, c); err != nil {
		return fmt.Errorf("partitioning demo: %w", err)
	}

	return nil
}

// demoPartitioning shows the declarative-partitioning escape hatch. A real app would
// schedule pwrap_partition_ensure via River (or pg_cron) ahead of the month change.
func demoPartitioning(ctx context.Context, c *pwrap.Client) error {
	pool := c.Pool()
	// Fresh parent every run.
	if _, err := pool.Exec(ctx, `DROP TABLE IF EXISTS events CASCADE`); err != nil {
		return err
	}
	// Declarative RANGE-partitioned by created_at. The partition key must be part of
	// every unique constraint, so the PK is (id, created_at) rather than just id.
	if _, err := pool.Exec(ctx, `
		CREATE TABLE events (
		    id          BIGSERIAL,
		    payload     JSONB NOT NULL,
		    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
		    PRIMARY KEY (id, created_at)
		) PARTITION BY RANGE (created_at)
	`); err != nil {
		return err
	}

	now := time.Now()
	var thisMonth, nextMonth string
	if err := pool.QueryRow(ctx, `SELECT pwrap_partition_ensure('events', $1::timestamptz)`, now).Scan(&thisMonth); err != nil {
		return err
	}
	if err := pool.QueryRow(ctx, `SELECT pwrap_partition_ensure('events', $1::timestamptz)`, now.AddDate(0, 1, 0)).Scan(&nextMonth); err != nil {
		return err
	}
	// Insert some events to prove partitioning actually accepts writes.
	for i := 0; i < 3; i++ {
		if _, err := pool.Exec(ctx, `INSERT INTO events (payload) VALUES ($1::jsonb)`, fmt.Sprintf(`{"i":%d}`, i)); err != nil {
			return err
		}
	}
	var n int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM events`).Scan(&n); err != nil {
		return err
	}

	fmt.Println()
	fmt.Println("=== Declarative partitioning ===")
	fmt.Printf("  created partition for this month:  %s\n", thisMonth)
	fmt.Printf("  created partition for next month:  %s\n", nextMonth)
	fmt.Printf("  inserted 3 events, total rows:     %d\n", n)
	return nil
}

// --- admin client (copy of the pattern used in todo-plus / rls-notes / bench) -----

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
