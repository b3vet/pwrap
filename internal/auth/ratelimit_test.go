package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLimiter_AllowAndDeny(t *testing.T) {
	l := NewLimiter(1, 2) // 1 rps, burst 2
	// First two calls succeed (burst), third fails until refill.
	for i := 0; i < 2; i++ {
		if !l.Allow("k") {
			t.Fatalf("burst call %d should pass", i+1)
		}
	}
	if l.Allow("k") {
		t.Fatal("third should be denied (burst exhausted)")
	}
	// Different key shares no state.
	if !l.Allow("other") {
		t.Fatal("other key bucket should be independent")
	}
}

func TestLimiter_EmptyKeyAlwaysAllows(t *testing.T) {
	l := NewLimiter(0, 0)
	for i := 0; i < 100; i++ {
		if !l.Allow("") {
			t.Fatal("empty key should be unconstrained")
		}
	}
}

func TestRateLimit_Middleware429(t *testing.T) {
	l := NewLimiter(0, 1) // exactly one token, no refill
	called := 0
	h := RateLimit(l, BearerKey)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called++
		w.WriteHeader(http.StatusOK)
	}))

	hit := func(token string) int {
		req := httptest.NewRequest(http.MethodPost, "/x", nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr.Code
	}

	if c := hit("aaaaaaaaaaaaaaaa"); c != http.StatusOK {
		t.Fatalf("first call code = %d, want 200", c)
	}
	if c := hit("aaaaaaaaaaaaaaaa"); c != http.StatusTooManyRequests {
		t.Fatalf("second call code = %d, want 429", c)
	}
	// Different bearer → different bucket → still allowed once.
	if c := hit("bbbbbbbbbbbbbbbb"); c != http.StatusOK {
		t.Fatalf("different bearer = %d, want 200", c)
	}
	// No Authorization header → key empty → never rate-limited.
	if c := hit(""); c != http.StatusOK {
		t.Fatalf("no auth code = %d, want 200", c)
	}
	if called != 3 {
		t.Fatalf("called = %d, want 3", called)
	}
}

func TestBearerKey_Prefix(t *testing.T) {
	cases := map[string]string{
		"":                              "",
		"Bearer ":                       "",
		"NotBearer xyz":                 "",
		"Bearer short":                  "short",
		"Bearer 0123456789abcdefXYZ":    "0123456789abcdef",
	}
	for h, want := range cases {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		if h != "" {
			r.Header.Set("Authorization", h)
		}
		if got := BearerKey(r); got != want {
			t.Errorf("BearerKey(%q) = %q, want %q", h, got, want)
		}
	}
}
