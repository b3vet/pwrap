// realtime_smoke is a single-process verification of the Path R wire path:
//   - subscribe via SDK on goroutine A
//   - insert via SDK on goroutine B
//   - observe events arrive on A
//
// Cleans up its own project at the end.
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

	"github.com/b3vet/pwrap/sdk/go/pwrap"
)

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
	admin := newAdmin(controlURL, adminToken)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	name := fmt.Sprintf("realtime-smoke-%d", time.Now().Unix())
	var proj struct {
		ID uuid.UUID `json:"id"`
	}
	if err := admin.do(ctx, http.MethodPost, "/v1/projects", map[string]string{"name": name}, &proj); err != nil {
		return err
	}
	defer func() {
		// Best-effort teardown; the smoke test's exit status shouldn't hinge on it.
		_ = admin.do(context.Background(), http.MethodDelete, "/v1/projects/"+proj.ID.String(), nil, nil)
	}()
	if err := admin.do(ctx, http.MethodPost, "/v1/projects/"+proj.ID.String()+"/migrations", nil, nil); err != nil {
		return err
	}
	var key struct {
		Key string `json:"key"`
	}
	if err := admin.do(ctx, http.MethodPost, "/v1/projects/"+proj.ID.String()+"/keys",
		map[string]string{"name": "smoke"}, &key); err != nil {
		return err
	}

	c, err := pwrap.New(ctx, pwrap.Config{ControlURL: controlURL, APIKey: key.Key})
	if err != nil {
		return err
	}
	defer c.Close()
	log.Println("[sdk] connected")

	sub, err := c.Subscribe(ctx, pwrap.SubscribeOpts{Table: "pwrap_documents"})
	if err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}
	defer sub.Close()
	log.Println("[sub] subscribed to pwrap_documents")

	// Fire inserts on a goroutine while the main routine drains events.
	go func() {
		for i := 0; i < 3; i++ {
			time.Sleep(300 * time.Millisecond)
			if _, err := c.Table("notes").Insert(ctx, map[string]any{"i": i, "title": fmt.Sprintf("note-%d", i)}); err != nil {
				log.Printf("insert: %v", err)
				return
			}
		}
	}()

	got := 0
	timeout := time.After(5 * time.Second)
	for got < 3 {
		select {
		case ev, ok := <-sub.Changes():
			if !ok {
				return fmt.Errorf("subscription ended early: %v", sub.Err())
			}
			got++
			var after map[string]any
			_ = json.Unmarshal(ev.After, &after)
			data, _ := after["data"].(map[string]any)
			log.Printf("[event] op=%s id=%s title=%v", ev.Op, ev.RowID, data["title"])
		case <-timeout:
			return fmt.Errorf("timeout: only got %d/3 events", got)
		}
	}
	log.Printf("[ok] received %d events", got)
	return nil
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
