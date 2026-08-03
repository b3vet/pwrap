// Package neon is a thin wrapper over the Neon Cloud API (console.neon.tech).
//
// It exists so that operators using pwrap on top of Neon can script per-PR preview
// environments without writing the Neon API by hand. The wrapper intentionally
// covers only the calls needed for the "create a branch → run pwrap migrations
// against it → tear it down" workflow:
//
//   - CreateBranch:     POST /projects/{id}/branches
//   - GetConnectionURI: GET  /projects/{id}/connection_uri
//   - DeleteBranch:     DELETE /projects/{id}/branches/{branch_id}
//   - ListBranches:     GET  /projects/{id}/branches
//
// pwrapd itself doesn't speak Neon. The intended pattern is: external orchestration
// (CI script, app deploy hook) creates a Neon branch with this wrapper, grabs the
// connection URI, then points a fresh pwrapd at it via PWRAP_DATABASE_URL.
// See docs/neon-integration.md for the full pattern.
package neon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const DefaultBaseURL = "https://console.neon.tech/api/v2"

// Client is the Neon API client. Construct with NewClient — keep one instance per process.
type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

func NewClient(apiKey string) *Client {
	return NewClientWithBaseURL(apiKey, DefaultBaseURL)
}

// NewClientWithBaseURL is the same as NewClient but accepts an override URL —
// used by tests against httptest.Server.
func NewClientWithBaseURL(apiKey, baseURL string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

// --- types ----------------------------------------------------------------

type Branch struct {
	ID             string    `json:"id"`
	ProjectID      string    `json:"project_id"`
	ParentID       string    `json:"parent_id,omitempty"`
	ParentLSN      string    `json:"parent_lsn,omitempty"`
	Name           string    `json:"name"`
	Default        bool      `json:"default"`
	Protected      bool      `json:"protected"`
	CurrentState   string    `json:"current_state"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	LastResetAt    time.Time `json:"last_reset_at,omitempty"`
	LogicalSize    int64     `json:"logical_size,omitempty"`
}

type Endpoint struct {
	ID          string `json:"id"`
	Host        string `json:"host"`
	BranchID    string `json:"branch_id"`
	ProjectID   string `json:"project_id"`
	Type        string `json:"type"`
	CurrentState string `json:"current_state"`
}

type CreateBranchRequest struct {
	// Optional parent branch ID; defaults to the project's primary branch.
	ParentID string
	// Required: human-friendly branch name.
	Name string
	// If true, also create a read-write endpoint (you typically want this).
	CreateEndpoint bool
}

type createBranchPayload struct {
	Branch struct {
		ParentID string `json:"parent_id,omitempty"`
		Name     string `json:"name,omitempty"`
	} `json:"branch"`
	Endpoints []struct {
		Type string `json:"type"`
	} `json:"endpoints,omitempty"`
}

type createBranchResponse struct {
	Branch    Branch     `json:"branch"`
	Endpoints []Endpoint `json:"endpoints"`
}

// --- public methods -------------------------------------------------------

// CreateBranch creates a new branch in the given Neon project, optionally with a
// read-write endpoint attached. Returns the branch + any endpoints created.
func (c *Client) CreateBranch(ctx context.Context, projectID string, req CreateBranchRequest) (Branch, []Endpoint, error) {
	if projectID == "" {
		return Branch{}, nil, errors.New("projectID is required")
	}
	if req.Name == "" {
		return Branch{}, nil, errors.New("Name is required")
	}
	body := createBranchPayload{}
	body.Branch.Name = req.Name
	if req.ParentID != "" {
		body.Branch.ParentID = req.ParentID
	}
	if req.CreateEndpoint {
		body.Endpoints = []struct {
			Type string `json:"type"`
		}{{Type: "read_write"}}
	}

	var resp createBranchResponse
	if err := c.do(ctx, http.MethodPost,
		fmt.Sprintf("/projects/%s/branches", projectID), body, &resp,
	); err != nil {
		return Branch{}, nil, err
	}
	return resp.Branch, resp.Endpoints, nil
}

// DeleteBranch removes a branch. Idempotent on the caller's side: Neon returns 404
// when the branch is already gone; we translate that to a nil error.
func (c *Client) DeleteBranch(ctx context.Context, projectID, branchID string) error {
	if projectID == "" || branchID == "" {
		return errors.New("projectID and branchID are required")
	}
	err := c.do(ctx, http.MethodDelete,
		fmt.Sprintf("/projects/%s/branches/%s", projectID, branchID), nil, nil,
	)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// ListBranches enumerates branches in the project.
func (c *Client) ListBranches(ctx context.Context, projectID string) ([]Branch, error) {
	var resp struct {
		Branches []Branch `json:"branches"`
	}
	if err := c.do(ctx, http.MethodGet,
		fmt.Sprintf("/projects/%s/branches", projectID), nil, &resp,
	); err != nil {
		return nil, err
	}
	return resp.Branches, nil
}

// ConnectionURIRequest names the role + database to encode into the returned URI.
type ConnectionURIRequest struct {
	BranchID     string
	RoleName     string
	DatabaseName string
	// Pooled, when true, asks for the pooled (PgBouncer) URI variant.
	Pooled bool
}

// GetConnectionURI returns a postgres:// URI for the given branch/role/db.
// Convenient because Neon's "real" host has the branch+endpoint baked in.
func (c *Client) GetConnectionURI(ctx context.Context, projectID string, req ConnectionURIRequest) (string, error) {
	if projectID == "" || req.BranchID == "" || req.RoleName == "" || req.DatabaseName == "" {
		return "", errors.New("projectID, BranchID, RoleName, and DatabaseName are required")
	}
	q := fmt.Sprintf(
		"/projects/%s/connection_uri?branch_id=%s&role_name=%s&database_name=%s&pooled=%t",
		projectID, req.BranchID, req.RoleName, req.DatabaseName, req.Pooled,
	)
	var resp struct {
		URI string `json:"uri"`
	}
	if err := c.do(ctx, http.MethodGet, q, nil, &resp); err != nil {
		return "", err
	}
	return resp.URI, nil
}

// --- errors ---------------------------------------------------------------

// ErrNotFound is returned (wrapped) when the API responds 404.
var ErrNotFound = errors.New("neon: not found")

// APIError carries the HTTP status and Neon's response body for debugging.
type APIError struct {
	Status  int
	Body    string
	Message string
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("neon: %d %s", e.Status, e.Message)
	}
	return fmt.Sprintf("neon: %d %s", e.Status, e.Body)
}

// --- internal -------------------------------------------------------------

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("neon: do: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		// Neon error bodies typically look like {"code":"...","message":"..."}.
		var ne struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(b, &ne)
		return &APIError{Status: resp.StatusCode, Body: string(b), Message: ne.Message}
	}

	if out != nil && resp.StatusCode != http.StatusNoContent {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}
