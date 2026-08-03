//go:build ignore

// mock-neon is a tiny stand-in for the Neon control plane, used in local CLI smoke
// tests of `pwrap neon …`. Boot it and point the CLI at it:
//
//	$ go run scripts/mock-neon.go &
//	$ PWRAP_NEON_API_KEY=anything PWRAP_NEON_BASE_URL=http://localhost:9911 \
//	    ./bin/pwrap neon branch create --neon-project proj-1 --name demo
//
// The mock honours create/list/delete branches and connection_uri for one canned
// project ("proj-1"). Don't rely on it for anything beyond verifying CLI plumbing.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
)

func main() {
	h := http.NewServeMux()
	h.HandleFunc("/projects/proj-1/branches", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodPost:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"branch": map[string]any{
					"id": "br-9", "project_id": "proj-1", "name": "demo",
					"current_state": "ready",
				},
				"endpoints": []map[string]any{{
					"id": "ep-1", "branch_id": "br-9", "project_id": "proj-1",
					"host": "ep-1.mock", "type": "read_write", "current_state": "active",
				}},
			})
		case http.MethodGet:
			_, _ = w.Write([]byte(
				`{"branches":[{"id":"br-main","name":"main"},{"id":"br-9","name":"demo"}]}`))
		}
	})
	h.HandleFunc("/projects/proj-1/branches/br-9", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h.HandleFunc("/projects/proj-1/connection_uri", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(
			`{"uri":"postgres://owner:pw@ep-1.mock.neon.tech/appdb?sslmode=require"}`))
	})
	fmt.Fprintln(os.Stderr, "mock-neon listening on :9911")
	if err := http.ListenAndServe(":9911", h); err != nil {
		fmt.Fprintln(os.Stderr, "mock-neon:", err)
		os.Exit(1)
	}
}
