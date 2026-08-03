package pwrap

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

// RestToken is what /v1/rest/token returns. The client hits URL with `Authorization: Bearer Token`
// against PostgREST. Refresh before ExpiresAt.
type RestToken struct {
	Token     string    `json:"token"`
	URL       string    `json:"url"`
	Role      string    `json:"role"`
	ExpiresAt time.Time `json:"expires_at"`
}

// RestTokenOpts lets callers attach an app-level user id (for RLS policies referencing
// `request.jwt.claims->>'user_id'`) and override the default TTL.
type RestTokenOpts struct {
	UserID     string
	TTLSeconds int
}

// IssueRestToken exchanges the cached API key for a PostgREST-compatible JWT.
// Returns ErrRestDisabled if pwrapd was configured without REST support.
func (c *Client) IssueRestToken(ctx context.Context, opts RestTokenOpts) (RestToken, error) {
	base := strings.TrimRight(c.cfg.ControlURL, "/")
	body, err := json.Marshal(map[string]any{
		"user_id":     opts.UserID,
		"ttl_seconds": opts.TTLSeconds,
	})
	if err != nil {
		return RestToken{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/rest/token", bytes.NewReader(body))
	if err != nil {
		return RestToken{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return RestToken{}, fmt.Errorf("pwrap: rest token: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusServiceUnavailable {
		return RestToken{}, ErrRestDisabled
	}
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return RestToken{}, fmt.Errorf("pwrap: /v1/rest/token: %s %s", resp.Status, bytes.TrimSpace(b))
	}
	var out RestToken
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return RestToken{}, err
	}
	return out, nil
}

// ErrRestDisabled is returned when pwrapd reports REST support is off.
var ErrRestDisabled = errors.New("pwrap: REST integration not enabled on this pwrapd")
