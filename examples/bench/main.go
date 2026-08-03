// bench inserts N fake notes + embeddings and then times M vector searches.
// Used to sanity-check HNSW latency and GIN scan behaviour under modest load.
//
// Env:
//   PWRAP_CONTROL_URL, PWRAP_BOOTSTRAP_TOKEN  (same as todo-plus)
//   BENCH_N         number of rows (default 5000)
//   BENCH_QUERIES   number of search queries (default 100)
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
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/b3vet/pwrap/sdk/go/pwrap"
)

func main() {
	if err := run(context.Background()); err != nil {
		log.Fatalf("bench: %v", err)
	}
}

func run(ctx context.Context) error {
	n := intEnv("BENCH_N", 5000)
	qn := intEnv("BENCH_QUERIES", 100)

	adminToken := os.Getenv("PWRAP_BOOTSTRAP_TOKEN")
	if adminToken == "" {
		return errors.New("PWRAP_BOOTSTRAP_TOKEN required")
	}
	controlURL := envOr("PWRAP_CONTROL_URL", "http://localhost:8080")
	admin := newAdmin(controlURL, adminToken)

	name := fmt.Sprintf("bench-%d", time.Now().UnixNano())
	proj, err := admin.createProject(ctx, name)
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

	issued, err := admin.issueKey(ctx, proj.ID, "bench")
	if err != nil {
		return err
	}

	c, err := pwrap.New(ctx, pwrap.Config{ControlURL: controlURL, APIKey: issued.Key})
	if err != nil {
		return err
	}
	defer c.Close()

	// --- insert: per-row (legacy) ----------------------------------------------
	notes := c.Table("bench")
	vec := c.Vector("bench")

	insertStart := time.Now()
	for i := 0; i < n; i++ {
		text := fmt.Sprintf("bench note %d about topic %d", i, i%50)
		id, err := notes.Insert(ctx, map[string]any{"title": text, "idx": i, "topic": i % 50})
		if err != nil {
			return fmt.Errorf("insert %d: %w", i, err)
		}
		if err := vec.Upsert(ctx, id.String(), fakeEmbed(text), map[string]any{"idx": i}); err != nil {
			return fmt.Errorf("upsert %d: %w", i, err)
		}
		if (i+1)%1000 == 0 {
			log.Printf("  inserted %d/%d (per-row)", i+1, n)
		}
	}
	insertDur := time.Since(insertStart)
	perRowRate := float64(n) / insertDur.Seconds()
	log.Printf("per-row insert: %d rows in %s (%.1f rows/sec)", n, insertDur, perRowRate)

	// --- insert: batch APIs ----------------------------------------------------
	notesBatch := c.Table("bench_batch")
	vecBatch := c.Vector("bench_batch")

	const batchSize = 1000
	batchStart := time.Now()
	var batchedTotal int
	for start := 0; start < n; start += batchSize {
		end := start + batchSize
		if end > n {
			end = n
		}
		docs := make([]any, 0, end-start)
		vecs := make([]pwrap.VectorRecord, 0, end-start)
		for i := start; i < end; i++ {
			text := fmt.Sprintf("bench note %d about topic %d", i, i%50)
			docs = append(docs, map[string]any{"title": text, "idx": i, "topic": i % 50})
		}
		ids, err := notesBatch.InsertMany(ctx, docs)
		if err != nil {
			return fmt.Errorf("batch insert: %w", err)
		}
		for i, id := range ids {
			text := fmt.Sprintf("bench note %d about topic %d", start+i, (start+i)%50)
			vecs = append(vecs, pwrap.VectorRecord{
				DocID:     id.String(),
				Embedding: fakeEmbed(text),
				Metadata:  map[string]any{"idx": start + i},
			})
		}
		if err := vecBatch.UpsertMany(ctx, vecs); err != nil {
			return fmt.Errorf("batch upsert: %w", err)
		}
		batchedTotal = end
		log.Printf("  inserted %d/%d (batch=%d)", batchedTotal, n, batchSize)
	}
	batchDur := time.Since(batchStart)
	batchRate := float64(n) / batchDur.Seconds()
	log.Printf("batch insert:   %d rows in %s (%.1f rows/sec)", n, batchDur, batchRate)
	log.Printf("speedup: %.1fx (batch vs per-row)", batchRate/perRowRate)

	// ANALYZE so the planner has fresh stats.
	if _, err := c.Pool().Exec(ctx, "ANALYZE pwrap_documents; ANALYZE pwrap_embeddings"); err != nil {
		return err
	}

	// --- search ----------------------------------------------------------------
	var latencies []time.Duration
	for q := 0; q < qn; q++ {
		query := fakeEmbed(fmt.Sprintf("query probe %d about topic %d", q, q%50))
		t0 := time.Now()
		matches, err := vec.Search(ctx, query, 10)
		if err != nil {
			return fmt.Errorf("search %d: %w", q, err)
		}
		if len(matches) == 0 {
			return fmt.Errorf("search %d returned 0", q)
		}
		latencies = append(latencies, time.Since(t0))
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	p := func(q float64) time.Duration { return latencies[int(float64(len(latencies))*q)] }
	fmt.Printf("\nvector search over %d rows, %d queries (k=10):\n", n, qn)
	fmt.Printf("  p50=%-10s p95=%-10s p99=%-10s max=%s\n", p(0.50), p(0.95), p(0.99), latencies[len(latencies)-1])

	// --- JSONB find --------------------------------------------------------------
	var findLatencies []time.Duration
	for q := 0; q < qn; q++ {
		topic := q % 50
		t0 := time.Now()
		ds, err := notes.Find(ctx, map[string]any{"topic": topic}, 5)
		if err != nil {
			return fmt.Errorf("find %d: %w", q, err)
		}
		if len(ds) == 0 {
			return fmt.Errorf("find %d returned 0 for topic %d", q, topic)
		}
		findLatencies = append(findLatencies, time.Since(t0))
	}
	sort.Slice(findLatencies, func(i, j int) bool { return findLatencies[i] < findLatencies[j] })
	pf := func(q float64) time.Duration { return findLatencies[int(float64(len(findLatencies))*q)] }
	fmt.Printf("JSONB find(topic=?) over %d rows, %d queries:\n", n, qn)
	fmt.Printf("  p50=%-10s p95=%-10s p99=%-10s max=%s\n", pf(0.50), pf(0.95), pf(0.99), findLatencies[len(findLatencies)-1])

	return nil
}

// --- helpers (trimmed duplicate of todo-plus) -------------------------------

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

func intEnv(k string, def int) int {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
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
