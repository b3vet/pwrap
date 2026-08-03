package authjwt

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

const testSecret = "a-test-secret-at-least-16"

func newTestSigner(t *testing.T) *Signer {
	t.Helper()
	s, err := NewSigner(testSecret)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	return s
}

func TestNewSignerRejectsShortSecret(t *testing.T) {
	for _, secret := range []string{"", "short", strings.Repeat("x", 15)} {
		if _, err := NewSigner(secret); err == nil {
			t.Errorf("NewSigner(%q) = nil error, want error", secret)
		}
	}
	if _, err := NewSigner(strings.Repeat("x", 16)); err != nil {
		t.Errorf("NewSigner(16 chars) = %v, want nil", err)
	}
}

func TestIssueRoundTrip(t *testing.T) {
	s := newTestSigner(t)
	pid := uuid.New()

	tok, exp, err := s.Issue(IssueOpts{
		Role:      "p_myproj",
		ProjectID: pid,
		UserID:    "alice",
		TTL:       30 * time.Minute,
	})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	got, err := s.Verify(tok)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.Role != "p_myproj" {
		t.Errorf("Role = %q, want %q", got.Role, "p_myproj")
	}
	if got.ProjectID != pid {
		t.Errorf("ProjectID = %v, want %v", got.ProjectID, pid)
	}
	if got.UserID != "alice" {
		t.Errorf("UserID = %q, want %q", got.UserID, "alice")
	}
	if got.Issuer != "pwrap" {
		t.Errorf("Issuer = %q, want %q", got.Issuer, "pwrap")
	}
	// The returned exp must match the claim, or callers cache the token past
	// the point PostgREST will accept it.
	if delta := got.ExpiresAt.Sub(exp); delta > time.Second || delta < -time.Second {
		t.Errorf("returned expiresAt %v disagrees with exp claim %v", exp, got.ExpiresAt.Time)
	}
}

func TestIssueRequiresRole(t *testing.T) {
	s := newTestSigner(t)
	if _, _, err := s.Issue(IssueOpts{ProjectID: uuid.New()}); err == nil {
		t.Fatal("Issue with empty role = nil error, want error")
	}
}

func TestIssueDefaultsTTLToOneHour(t *testing.T) {
	s := newTestSigner(t)
	for _, ttl := range []time.Duration{0, -time.Minute} {
		_, exp, err := s.Issue(IssueOpts{Role: "p_x", TTL: ttl})
		if err != nil {
			t.Fatalf("Issue(ttl=%v): %v", ttl, err)
		}
		if d := time.Until(exp); d < 59*time.Minute || d > 61*time.Minute {
			t.Errorf("Issue(ttl=%v) expires in %v, want ~1h", ttl, d)
		}
	}
}

func TestUserIDOmittedWhenEmpty(t *testing.T) {
	// PostgREST copies the whole claim set into request.jwt.claims. An empty
	// user_id claim would make `current_setting(...)->>'user_id'` return ""
	// rather than NULL, which quietly changes how an RLS policy evaluates.
	s := newTestSigner(t)
	tok, _, err := s.Issue(IssueOpts{Role: "p_x", ProjectID: uuid.New()})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if _, ok := decodePayload(t, tok)["user_id"]; ok {
		t.Error("user_id present in claims when UserID was empty, want omitted")
	}
}

func TestVerifyRejectsWrongSecret(t *testing.T) {
	s := newTestSigner(t)
	tok, _, err := s.Issue(IssueOpts{Role: "p_x", ProjectID: uuid.New()})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	other, err := NewSigner("a-different-secret-16+")
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	if _, err := other.Verify(tok); err == nil {
		t.Fatal("Verify with wrong secret = nil error, want error")
	}
}

func TestVerifyRejectsExpired(t *testing.T) {
	s := newTestSigner(t)
	// Sign an already-expired token directly; Issue clamps non-positive TTLs.
	claims := Claims{
		Role:      "p_x",
		ProjectID: uuid.New(),
		RegisteredClaims: jwt.RegisteredClaims{
			IssuedAt:  jwt.NewNumericDate(time.Now().Add(-2 * time.Hour)),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(-time.Hour)),
			Issuer:    "pwrap",
		},
	}
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(testSecret))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if _, err := s.Verify(tok); err == nil {
		t.Fatal("Verify of expired token = nil error, want error")
	}
}

func TestVerifyRejectsAlgNone(t *testing.T) {
	// The classic JWT forgery: swap alg to "none" and drop the signature. The
	// keyfunc must reject any non-HMAC method before the secret is handed over.
	s := newTestSigner(t)
	claims := Claims{
		Role:      "p_admin",
		ProjectID: uuid.New(),
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
			Issuer:    "pwrap",
		},
	}
	tok, err := jwt.NewWithClaims(jwt.SigningMethodNone, claims).
		SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if _, err := s.Verify(tok); err == nil {
		t.Fatal("Verify of alg=none token = nil error, want error")
	}
}

func TestVerifyRejectsGarbage(t *testing.T) {
	s := newTestSigner(t)
	for _, tok := range []string{"", "not.a.jwt", "a.b", strings.Repeat("x", 64)} {
		if _, err := s.Verify(tok); err == nil {
			t.Errorf("Verify(%q) = nil error, want error", tok)
		}
	}
}

// decodePayload returns the JWT's claim set without verifying it, so tests can
// assert on the exact wire representation rather than the decoded struct.
func decodePayload(t *testing.T, token string) map[string]any {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d parts, want 3", len(parts))
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	return out
}
