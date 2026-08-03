package neon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeNeon spins up an httptest.Server that mimics the subset of the Neon API the
// client touches. Tests use it to verify URL shapes, headers, payloads, and error
// translation without hitting the real API.
func fakeNeon(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	s := httptest.NewServer(handler)
	t.Cleanup(s.Close)
	return NewClientWithBaseURL("test-key", s.URL)
}

func TestCreateBranch_HappyPath(t *testing.T) {
	c := fakeNeon(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %q, want POST", r.Method)
		}
		if r.URL.Path != "/projects/proj-1/branches" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Fatalf("Authorization = %q", got)
		}
		var body createBranchPayload
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if body.Branch.Name != "feature-x" {
			t.Fatalf("branch.name = %q", body.Branch.Name)
		}
		if len(body.Endpoints) != 1 || body.Endpoints[0].Type != "read_write" {
			t.Fatalf("endpoints = %+v", body.Endpoints)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(createBranchResponse{
			Branch: Branch{
				ID:           "br-9",
				ProjectID:    "proj-1",
				ParentID:     "br-main",
				Name:         "feature-x",
				CurrentState: "ready",
				CreatedAt:    time.Now().UTC(),
			},
			Endpoints: []Endpoint{{
				ID: "ep-1", BranchID: "br-9", ProjectID: "proj-1",
				Host: "ep-1.us-east-1.aws.neon.tech", Type: "read_write",
				CurrentState: "active",
			}},
		})
	})

	br, eps, err := c.CreateBranch(context.Background(), "proj-1", CreateBranchRequest{
		Name: "feature-x", CreateEndpoint: true,
	})
	if err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}
	if br.ID != "br-9" || br.Name != "feature-x" {
		t.Fatalf("branch = %+v", br)
	}
	if len(eps) != 1 || eps[0].Host == "" {
		t.Fatalf("endpoints = %+v", eps)
	}
}

func TestCreateBranch_ValidatesArgs(t *testing.T) {
	c := NewClientWithBaseURL("k", "http://ignored")
	if _, _, err := c.CreateBranch(context.Background(), "", CreateBranchRequest{Name: "x"}); err == nil {
		t.Fatal("expected error for empty project id")
	}
	if _, _, err := c.CreateBranch(context.Background(), "p", CreateBranchRequest{}); err == nil {
		t.Fatal("expected error for empty name")
	}
}

func TestDeleteBranch_Idempotent(t *testing.T) {
	c := fakeNeon(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	if err := c.DeleteBranch(context.Background(), "p", "br-1"); err != nil {
		t.Fatalf("DeleteBranch on missing should be nil, got %v", err)
	}
}

func TestDeleteBranch_ErrorBubblesUp(t *testing.T) {
	c := fakeNeon(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"code":"INTERNAL","message":"boom"}`))
	})
	err := c.DeleteBranch(context.Background(), "p", "br-1")
	if err == nil {
		t.Fatal("expected error")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if apiErr.Status != 500 || apiErr.Message != "boom" {
		t.Fatalf("apiErr = %+v", apiErr)
	}
}

func TestListBranches(t *testing.T) {
	c := fakeNeon(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"branches":[{"id":"b1","name":"main"},{"id":"b2","name":"staging"}]}`))
	})
	got, err := c.ListBranches(context.Background(), "p")
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, b := range got {
		names = append(names, b.Name)
	}
	if want := "main,staging"; strings.Join(names, ",") != want {
		t.Fatalf("names = %v, want %s", names, want)
	}
}

func TestGetConnectionURI(t *testing.T) {
	c := fakeNeon(t, func(w http.ResponseWriter, r *http.Request) {
		// Verify query params are correctly encoded.
		q := r.URL.Query()
		if q.Get("branch_id") != "br-9" || q.Get("role_name") != "owner" ||
			q.Get("database_name") != "appdb" || q.Get("pooled") != "true" {
			t.Fatalf("query = %v", q)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"uri":"postgres://owner:pw@ep-1.us-east-1.aws.neon.tech/appdb?sslmode=require"}`))
	})
	uri, err := c.GetConnectionURI(context.Background(), "proj-1", ConnectionURIRequest{
		BranchID: "br-9", RoleName: "owner", DatabaseName: "appdb", Pooled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(uri, "postgres://") || !strings.Contains(uri, "neon.tech") {
		t.Fatalf("uri = %q", uri)
	}
}
