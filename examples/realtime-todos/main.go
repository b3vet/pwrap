// realtime-todos is a tiny web app that demonstrates pwrap's realtime pillar.
//
// On startup it provisions a fresh project against the local pwrapd, applies
// migrations, and issues an API key. The browser then:
//   • fetches the initial todo list from /api/todos (REST)
//   • opens a WebSocket to pwrapd's /v1/subscribe filtered to pwrap_documents
//   • re-renders on every INSERT/UPDATE/DELETE event the server pushes
//
// Open http://localhost:7799 in two tabs. Add a todo in one tab; watch it appear
// in the other tab instantly, with no polling. (Avoiding port 7000 because macOS
// Control Center binds it for AirPlay Receiver / afs3-fileserver — apt to surprise.)
//
// Run:  PWRAP_BOOTSTRAP_TOKEN=dev-admin ./bin/realtime-todos
package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/berkeucvet/pwrap/sdk/go/pwrap"
)

//go:embed web/*
var web embed.FS

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	controlURL := envOr("PWRAP_CONTROL_URL", "http://localhost:8080")
	adminToken := os.Getenv("PWRAP_BOOTSTRAP_TOKEN")
	if adminToken == "" {
		return errors.New("PWRAP_BOOTSTRAP_TOKEN required")
	}
	listenAddr := envOr("REALTIME_DEMO_ADDR", ":7799")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	admin := newAdmin(controlURL, adminToken)
	name := fmt.Sprintf("realtime-todos-%d", time.Now().Unix())

	var proj struct {
		ID uuid.UUID `json:"id"`
	}
	if err := admin.do(ctx, http.MethodPost, "/v1/projects", map[string]string{"name": name}, &proj); err != nil {
		return fmt.Errorf("create project: %w", err)
	}
	log.Printf("[control] project %s", proj.ID)
	// In a real app you'd keep this around. For a demo, delete on Ctrl-C.
	defer func() {
		_ = admin.do(context.Background(), http.MethodDelete, "/v1/projects/"+proj.ID.String(), nil, nil)
		log.Printf("[control] project deleted")
	}()

	if err := admin.do(ctx, http.MethodPost, "/v1/projects/"+proj.ID.String()+"/migrations", nil, nil); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}

	var issued struct {
		Key string `json:"key"`
	}
	if err := admin.do(ctx, http.MethodPost, "/v1/projects/"+proj.ID.String()+"/keys",
		map[string]string{"name": "browser"}, &issued); err != nil {
		return fmt.Errorf("issue key: %w", err)
	}

	client, err := pwrap.New(ctx, pwrap.Config{ControlURL: controlURL, APIKey: issued.Key})
	if err != nil {
		return fmt.Errorf("sdk: %w", err)
	}
	defer client.Close()
	log.Printf("[sdk] connected to schema %s", client.Schema())

	r := chi.NewRouter()

	// The browser connects directly to pwrapd's /v1/subscribe. We inject the
	// API key + WebSocket URL into the page so the JS can use them. This is
	// a demo — in production you'd issue scoped per-user tokens, not the project key.
	indexTmpl := template.Must(template.New("index").Parse(mustRead(web, "web/index.html")))
	r.Get("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = indexTmpl.Execute(w, map[string]string{
			"ControlURL": controlURL,
			"APIKey":     issued.Key,
		})
	})

	r.Get("/api/todos", func(w http.ResponseWriter, r *http.Request) {
		docs, err := client.Table("todos").Find(r.Context(), nil, 100)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		writeJSON(w, docs)
	})

	r.Post("/api/todos", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Title string `json:"title"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad body", 400)
			return
		}
		title := strings.TrimSpace(body.Title)
		if title == "" {
			http.Error(w, "title required", 400)
			return
		}
		id, err := client.Table("todos").Insert(r.Context(), map[string]any{
			"title": title,
			"done":  false,
		})
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		writeJSON(w, map[string]string{"id": id.String()})
	})

	r.Patch("/api/todos/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, err := uuid.Parse(chi.URLParam(r, "id"))
		if err != nil {
			http.Error(w, "bad id", 400)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad body", 400)
			return
		}
		if err := client.Table("todos").Update(r.Context(), id, body); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	r.Delete("/api/todos/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, err := uuid.Parse(chi.URLParam(r, "id"))
		if err != nil {
			http.Error(w, "bad id", 400)
			return
		}
		if err := client.Table("todos").Delete(r.Context(), id); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	log.Printf("[http] open http://localhost%s in two tabs to see realtime sync", listenAddr)
	return http.ListenAndServe(listenAddr, r)
}

// --- helpers -----------------------------------------------------------------

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func mustRead(fs embed.FS, path string) string {
	b, err := fs.ReadFile(path)
	if err != nil {
		log.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

type admin struct {
	base, token string
	http        *http.Client
}

func newAdmin(base, token string) *admin {
	return &admin{base: strings.TrimRight(base, "/"), token: token, http: &http.Client{Timeout: 30 * time.Second}}
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
