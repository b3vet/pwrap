package pwrap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// ChangeEvent is the wire format pwrapd's realtime endpoint emits. Fields mirror
// the server's realtime.ChangeEvent. Before/After are arbitrary JSON; decode them
// with json.Unmarshal into a concrete struct if you know the table shape.
type ChangeEvent struct {
	LogID     int64           `json:"log_id"`
	Schema    string          `json:"schema"`
	Table     string          `json:"table"`
	Op        string          `json:"op"`
	RowID     string          `json:"row_id,omitempty"`
	UserID    string          `json:"user_id,omitempty"`
	Before    json.RawMessage `json:"before,omitempty"`
	After     json.RawMessage `json:"after,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
}

// SubscribeOpts narrows which events the subscription receives.
// Table optionally filters to one table; UserID filters to changes that carried
// the matching `request.jwt.claims.user_id` (use the same convention as RLS).
type SubscribeOpts struct {
	Table  string
	UserID string
}

// Subscription is an active WebSocket subscription. Range over Changes() until
// the channel closes, then check Err() for the reason. Call Close() to tear it
// down early.
//
// The Subscription auto-reconnects on transient WebSocket failures with
// exponential backoff. Permanent errors (e.g. invalid API key) close Changes.
type Subscription struct {
	client *Client
	opts   SubscribeOpts

	ctx    context.Context
	cancel context.CancelFunc

	events chan ChangeEvent

	mu        sync.Mutex
	err       error
	closed    bool
	helloOnce sync.Once
	helloCh   chan struct{}
}

// Subscribe opens a WebSocket against /v1/subscribe and returns a Subscription
// whose Changes() channel emits ChangeEvents as they happen. Blocks until the
// initial connection is established and the server's "hello" frame is received,
// or until ctx is cancelled.
func (c *Client) Subscribe(ctx context.Context, opts SubscribeOpts) (*Subscription, error) {
	sCtx, cancel := context.WithCancel(context.Background())
	s := &Subscription{
		client:  c,
		opts:    opts,
		ctx:     sCtx,
		cancel:  cancel,
		events:  make(chan ChangeEvent, 128),
		helloCh: make(chan struct{}),
	}
	go s.runLoop()

	// Wait for the first connection to settle (hello received) or ctx cancellation.
	select {
	case <-s.helloCh:
		return s, nil
	case <-ctx.Done():
		s.Close()
		return nil, ctx.Err()
	}
}

// Changes returns the event channel. Closed when the subscription terminates
// for any reason; check Err() afterward.
func (s *Subscription) Changes() <-chan ChangeEvent { return s.events }

// Err returns the terminal error, if any. Nil before the channel closes; non-nil
// after a permanent failure.
func (s *Subscription) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// Close terminates the subscription. Idempotent.
func (s *Subscription) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.mu.Unlock()
	s.cancel()
}

// --- internals -----------------------------------------------------------------

func (s *Subscription) setErr(err error) {
	s.mu.Lock()
	if s.err == nil {
		s.err = err
	}
	s.mu.Unlock()
}

// runLoop manages the WebSocket connection with exponential backoff reconnection.
// On terminal errors (auth failures, ctx cancel) it closes the events channel
// and exits.
func (s *Subscription) runLoop() {
	defer close(s.events)

	backoff := time.Second
	for {
		if s.ctx.Err() != nil {
			return
		}
		err := s.runOnce()
		if err == nil {
			return // graceful close
		}
		if isPermanent(err) {
			s.setErr(err)
			return
		}
		// Transient: backoff and retry.
		select {
		case <-s.ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > 30*time.Second {
			backoff = 30 * time.Second
		}
	}
}

func (s *Subscription) runOnce() error {
	wsURL, err := s.buildURL()
	if err != nil {
		return permanentErr{err}
	}
	conn, _, err := websocket.Dial(s.ctx, wsURL, &websocket.DialOptions{HTTPClient: s.client.http})
	if err != nil {
		// Dial errors are usually transient — let the loop retry.
		return err
	}
	defer conn.CloseNow()

	// First frame is the server's hello. Wait up to 5s.
	helloCtx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
	var hello map[string]any
	err = wsjson.Read(helloCtx, conn, &hello)
	cancel()
	if err != nil {
		return err
	}
	s.helloOnce.Do(func() { close(s.helloCh) })

	// Read events until error or close.
	for {
		var ev ChangeEvent
		if err := wsjson.Read(s.ctx, conn, &ev); err != nil {
			// Status 1008 (policy violation) typically means auth failure — permanent.
			st := websocket.CloseStatus(err)
			if st == websocket.StatusPolicyViolation {
				return permanentErr{err}
			}
			return err
		}
		select {
		case s.events <- ev:
		case <-s.ctx.Done():
			return nil
		}
	}
}

func (s *Subscription) buildURL() (string, error) {
	base := strings.TrimRight(s.client.cfg.ControlURL, "/")
	u, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	default:
		return "", fmt.Errorf("pwrap: unsupported scheme %q", u.Scheme)
	}
	u.Path = "/v1/subscribe"
	q := u.Query()
	q.Set("api_key", s.client.cfg.APIKey)
	if s.opts.Table != "" {
		q.Set("table", s.opts.Table)
	}
	if s.opts.UserID != "" {
		q.Set("user_id", s.opts.UserID)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// permanentErr wraps errors that shouldn't trigger reconnection (auth fail, bad URL).
type permanentErr struct{ err error }

func (p permanentErr) Error() string { return p.err.Error() }
func (p permanentErr) Unwrap() error { return p.err }

func isPermanent(err error) bool {
	var p permanentErr
	if errors.As(err, &p) {
		return true
	}
	return false
}
