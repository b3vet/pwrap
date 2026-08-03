package auth

import (
	"context"
	"net/http"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// Limiter is a per-key token-bucket rate limiter with TTL-based eviction.
// Construct one per endpoint (or share across endpoints with the same key shape)
// and apply via Middleware. StartCleanup is best-effort; without it the map grows
// unboundedly when many distinct keys are seen.
type Limiter struct {
	mu       sync.Mutex
	limiters map[string]*entry
	rps      rate.Limit
	burst    int
	ttl      time.Duration
}

type entry struct {
	l          *rate.Limiter
	lastAccess time.Time
}

// NewLimiter creates a limiter with rps tokens/sec and the given burst capacity.
// Entries unused for ~10 minutes are eligible for eviction by StartCleanup.
func NewLimiter(rps float64, burst int) *Limiter {
	return &Limiter{
		limiters: map[string]*entry{},
		rps:      rate.Limit(rps),
		burst:    burst,
		ttl:      10 * time.Minute,
	}
}

// Allow returns true if the bucket for `key` has capacity.
// Empty key is always allowed — middleware translates that to "skip the check".
func (l *Limiter) Allow(key string) bool {
	if key == "" {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.limiters[key]
	if !ok {
		e = &entry{l: rate.NewLimiter(l.rps, l.burst)}
		l.limiters[key] = e
	}
	e.lastAccess = time.Now()
	return e.l.Allow()
}

// StartCleanup periodically evicts entries idle for longer than the TTL. Blocks
// until ctx is cancelled — call in a goroutine. Safe to skip if you don't care
// about long-lived process memory growth.
func (l *Limiter) StartCleanup(ctx context.Context) {
	t := time.NewTicker(l.ttl)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			l.mu.Lock()
			for k, e := range l.limiters {
				if now.Sub(e.lastAccess) > l.ttl {
					delete(l.limiters, k)
				}
			}
			l.mu.Unlock()
		}
	}
}

// RateLimit returns an http middleware that gates requests through the limiter,
// keyed by whatever keyFn extracts. Returns 429 when the bucket is empty.
func RateLimit(l *Limiter, keyFn func(*http.Request) string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !l.Allow(keyFn(r)) {
				w.Header().Set("Retry-After", "1")
				http.Error(w, `{"error":"rate limit exceeded"}`, http.StatusTooManyRequests)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// BearerKey extracts the bearer-token prefix (first 16 chars of the token after
// "Bearer ") as the rate-limit key. Falls back to empty (= skip) if missing.
// Using a prefix instead of the full token keeps the in-memory map cheap and means
// rotating a single API key doesn't fragment the limiter.
func BearerKey(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) <= len(prefix) || h[:len(prefix)] != prefix {
		return ""
	}
	tok := h[len(prefix):]
	if len(tok) > 16 {
		tok = tok[:16]
	}
	return tok
}
