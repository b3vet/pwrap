// todo-plus is the pwrap dogfood demo.
//
// It exercises all three MVP pillars in one flow:
//  1. Control plane: create a project, issue an API key, apply migrations.
//  2. Table (JSONB): insert a handful of notes.
//  3. Queue (River): enqueue one "embed" job per note.
//  4. Vector (pgvector): the worker computes a (fake) embedding and upserts it.
//  5. Search: pick a query, return top-k nearest notes.
//
// Env:
//
//	PWRAP_CONTROL_URL      (default http://localhost:8080)
//	PWRAP_BOOTSTRAP_TOKEN  required — admin token for pwrapd
//	TODO_PLUS_PROJECT      optional — reuse a project slug instead of creating one
//	TODO_PLUS_KEEP=1       don't delete the project at the end
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/b3vet/pwrap/sdk/go/pwrap"
)

func main() {
	if err := run(context.Background()); err != nil {
		log.Fatalf("todo-plus: %v", err)
	}
}

func run(ctx context.Context) error {
	controlURL := envOr("PWRAP_CONTROL_URL", "http://localhost:8080")
	adminToken := os.Getenv("PWRAP_BOOTSTRAP_TOKEN")
	if adminToken == "" {
		return errors.New("PWRAP_BOOTSTRAP_TOKEN is required")
	}

	admin := newAdminClient(controlURL, adminToken)

	// Step 1 — provision project, issue key, apply migrations.
	projectName := fmt.Sprintf("todo-plus-%d", time.Now().Unix())
	project, err := admin.createProject(ctx, projectName)
	if err != nil {
		return fmt.Errorf("create project: %w", err)
	}
	log.Printf("[control] project %s (%s)", project.Name, project.ID)

	if !boolEnv("TODO_PLUS_KEEP") {
		defer func() {
			if err := admin.deleteProject(context.Background(), project.ID); err != nil {
				log.Printf("[control] delete project: %v (leaving behind)", err)
			} else {
				log.Printf("[control] deleted project %s", project.ID)
			}
		}()
	}

	issued, err := admin.issueKey(ctx, project.ID, "todo-plus")
	if err != nil {
		return fmt.Errorf("issue key: %w", err)
	}
	log.Printf("[control] key issued prefix=%s", issued.Prefix)

	if _, err := admin.applyMigrations(ctx, project.ID); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	log.Printf("[control] migrations applied")

	// Step 2 — boot the SDK against the new project.
	client, err := pwrap.New(ctx, pwrap.Config{
		ControlURL: controlURL,
		APIKey:     issued.Plaintext,
	})
	if err != nil {
		return fmt.Errorf("sdk connect: %w", err)
	}
	defer client.Close()
	log.Printf("[sdk] connected to schema %s", client.Schema())

	// Step 3 — register a worker for the "embed" kind and start River.
	workers := river.NewWorkers()
	river.AddWorker(workers, &embedWorker{client: client})

	rc, err := river.NewClient(riverpgxv5.New(client.Pool()), &river.Config{
		Queues:  map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: 4}},
		Workers: workers,
	})
	if err != nil {
		return fmt.Errorf("river client: %w", err)
	}
	if err := rc.Start(ctx); err != nil {
		return fmt.Errorf("river start: %w", err)
	}
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = rc.Stop(stopCtx)
	}()
	log.Printf("[river] started")

	// Step 4 — insert notes + enqueue embed jobs.
	notes := client.Table("notes")
	q := client.Queue()

	samples := []string{
		"Buy groceries: milk, eggs, bread",
		"Ship pwrap M6 demo by end of day",
		"Walk the dog and pick up a package",
		"Book flights for the Tokyo trip",
		"Write a blog post about postgres + pgvector",
		"Fix the flaky test in CI for the auth module",
		"Plan dinner for Saturday with friends",
	}

	var ids []uuid.UUID
	for _, s := range samples {
		id, err := notes.Insert(ctx, map[string]any{"title": s, "status": "todo"})
		if err != nil {
			return fmt.Errorf("insert: %w", err)
		}
		ids = append(ids, id)
		if _, err := q.Enqueue(ctx, pwrap.EnqueueRequest{
			Kind: "embed",
			Args: embedArgs{DocID: id.String(), Content: s},
		}); err != nil {
			return fmt.Errorf("enqueue: %w", err)
		}
	}
	log.Printf("[data] inserted %d notes and enqueued %d embed jobs", len(ids), len(ids))

	// Step 5 — drain the queue.
	if err := drainQueue(ctx, q, 10*time.Second); err != nil {
		return fmt.Errorf("drain: %w", err)
	}
	log.Printf("[river] queue drained")

	// Step 6 — search.
	queryText := "postgres vector search"
	matches, err := client.Vector("notes").Search(ctx, fakeEmbed(queryText), 3)
	if err != nil {
		return fmt.Errorf("search: %w", err)
	}
	fmt.Println()
	fmt.Printf("Top 3 matches for query: %q\n", queryText)
	for i, m := range matches {
		fmt.Printf("  %d. dist=%.4f  %q\n", i+1, m.Distance, m.Metadata["content"])
	}
	fmt.Println()

	// Also show a JSONB find to prove Table works.
	found, err := notes.Find(ctx, map[string]any{"status": "todo"}, 3)
	if err != nil {
		return fmt.Errorf("find: %w", err)
	}
	fmt.Printf("First %d notes via JSONB find(status=todo):\n", len(found))
	for _, d := range found {
		fmt.Printf("  - %s\n", d.Data["title"])
	}

	// Step 7 — matview: aggregate "how many notes does each title's first word have?"
	//     as a materialised view, refresh it once, and print.
	mv := client.Matview("notes_word_counts")
	if err := mv.Register(ctx, `
		SELECT split_part(data->>'title', ' ', 1) AS first_word,
		       count(*)                           AS notes
		  FROM pwrap_documents
		 WHERE collection = 'notes'
		 GROUP BY 1
	`); err != nil {
		return fmt.Errorf("matview register: %w", err)
	}
	if err := mv.Refresh(ctx); err != nil {
		return fmt.Errorf("matview refresh: %w", err)
	}
	info, err := mv.Info(ctx)
	if err != nil {
		return fmt.Errorf("matview info: %w", err)
	}
	fmt.Printf("\nmatview notes_word_counts — last refresh %s\n", info.LastRefreshAt.Format(time.RFC3339))
	rows, err := client.Pool().Query(ctx, `SELECT first_word, notes FROM notes_word_counts ORDER BY notes DESC, first_word`)
	if err != nil {
		return fmt.Errorf("matview query: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var word string
		var n int
		if err := rows.Scan(&word, &n); err != nil {
			return err
		}
		fmt.Printf("  %-10s %d\n", word, n)
	}

	return nil
}

// --- worker ------------------------------------------------------------------

type embedArgs struct {
	DocID   string `json:"doc_id"`
	Content string `json:"content"`
}

func (embedArgs) Kind() string { return "embed" }

type embedWorker struct {
	river.WorkerDefaults[embedArgs]
	client *pwrap.Client
}

func (w *embedWorker) Work(ctx context.Context, job *river.Job[embedArgs]) error {
	emb := fakeEmbed(job.Args.Content)
	return w.client.Vector("notes").Upsert(ctx, job.Args.DocID, emb, map[string]any{
		"content": job.Args.Content,
	})
}

// --- helpers -----------------------------------------------------------------

func drainQueue(ctx context.Context, q *pwrap.Queue, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		s, err := q.Stats(ctx)
		if err != nil {
			return err
		}
		if s.Available+s.Running+s.Scheduled+s.Retryable == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout: queue still has %+v", s)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// fakeEmbed is a deterministic 1536-dim embedding stand-in so the demo runs offline.
// It hashes the input, expands the 256 bits across the vector, then adds a
// content-specific bias + L2-normalises. Not a real embedding — just enough that
// texts sharing words cluster in the cosine-distance sense.
func fakeEmbed(text string) []float32 {
	sum := sha256.Sum256([]byte(strings.ToLower(text)))
	v := make([]float32, pwrap.VectorDim)
	for i := range v {
		bit := (int(sum[i%32]) >> (i % 8)) & 1
		v[i] = float32(bit)*0.5 + float32(i%13)/26.0
	}
	for i, r := range strings.ToLower(text) {
		if i >= pwrap.VectorDim {
			break
		}
		v[i] += float32(r) / 1000.0
	}
	var norm float32
	for _, x := range v {
		norm += x * x
	}
	norm = float32(math.Sqrt(float64(norm)))
	if norm == 0 {
		return v
	}
	for i := range v {
		v[i] /= norm
	}
	return v
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func boolEnv(k string) bool {
	v := os.Getenv(k)
	return v != "" && v != "0" && strings.ToLower(v) != "false"
}

// --- admin client ------------------------------------------------------------

type adminClient struct {
	base  string
	token string
	http  *http.Client
}

func newAdminClient(base, token string) *adminClient {
	return &adminClient{
		base:  strings.TrimRight(base, "/"),
		token: token,
		http:  &http.Client{Timeout: 30 * time.Second},
	}
}

type project struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
}

type issuedKey struct {
	Prefix    string `json:"prefix"`
	Plaintext string `json:"key"`
}

func (a *adminClient) createProject(ctx context.Context, name string) (project, error) {
	var out project
	if err := a.do(ctx, http.MethodPost, "/v1/projects", map[string]string{"name": name}, &out); err != nil {
		return project{}, err
	}
	return out, nil
}

func (a *adminClient) deleteProject(ctx context.Context, id uuid.UUID) error {
	return a.do(ctx, http.MethodDelete, "/v1/projects/"+id.String(), nil, nil)
}

func (a *adminClient) issueKey(ctx context.Context, id uuid.UUID, name string) (issuedKey, error) {
	var out issuedKey
	if err := a.do(ctx, http.MethodPost, "/v1/projects/"+id.String()+"/keys", map[string]string{"name": name}, &out); err != nil {
		return issuedKey{}, err
	}
	return out, nil
}

func (a *adminClient) applyMigrations(ctx context.Context, id uuid.UUID) (map[string]any, error) {
	var out map[string]any
	if err := a.do(ctx, http.MethodPost, "/v1/projects/"+id.String()+"/migrations", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (a *adminClient) do(ctx context.Context, method, path string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.base+path, rdr)
	if err != nil {
		return err
	}
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
