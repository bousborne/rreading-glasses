package internal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const hardcoverRateLimitStateKey = "rate-limit-hardcover-v1"

// RateLimitStateStore is the small persistent-cache surface used to retain a
// provider cooldown across process restarts. Implementations must make Set
// durable before it returns.
type RateLimitStateStore interface {
	Get(ctx context.Context, key string) ([]byte, bool)
	PersistRateLimitState(ctx context.Context, key string, value []byte, ttl time.Duration) error
	DeleteRateLimitState(ctx context.Context, key string) error
}

// RateLimitError reports that the upstream provider is unavailable until a
// known deadline. It intentionally unwraps to HTTP 429 so callers can preserve
// the status code while still reading the precise retry delay.
type RateLimitError struct {
	Until time.Time
	Cause error
	now   func() time.Time
}

// BackoffError is a local admission-control response. Unlike RateLimitError it
// does not open the provider circuit, but it still tells clients when retrying
// is reasonable.
type BackoffError struct {
	Code    int
	Delay   time.Duration
	Message string
}

func (e *BackoffError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return http.StatusText(e.Code)
}

func (e *BackoffError) Unwrap() error {
	return statusErr(e.Code)
}

func (e *BackoffError) RetryAfter() time.Duration {
	return max(time.Second, e.Delay)
}

func (e *RateLimitError) Error() string {
	if e == nil {
		return statusErr(http.StatusTooManyRequests).Error()
	}
	if e.Cause != nil {
		return fmt.Sprintf("Hardcover rate limit active for %s: %v", e.RetryAfter().Round(time.Second), e.Cause)
	}
	return fmt.Sprintf("Hardcover rate limit active for %s", e.RetryAfter().Round(time.Second))
}

func (e *RateLimitError) Unwrap() []error {
	if e == nil || e.Cause == nil {
		return []error{statusErr(http.StatusTooManyRequests)}
	}
	return []error{statusErr(http.StatusTooManyRequests), e.Cause}
}

// RetryAfter returns the remaining cooldown, never a negative duration.
func (e *RateLimitError) RetryAfter() time.Duration {
	if e == nil {
		return 0
	}
	now := time.Now
	if e.now != nil {
		now = e.now
	}
	remaining := e.Until.Sub(now())
	if remaining < 0 {
		return 0
	}
	return remaining
}

// RetryAfterSeconds returns a Retry-After delta suitable for an HTTP header.
func (e *RateLimitError) RetryAfterSeconds() int {
	return max(1, int(math.Ceil(e.RetryAfter().Seconds())))
}

// RateLimitGate is a process-wide circuit breaker for a single upstream
// provider. Once opened, all callers fail locally until the provider's retry
// deadline. A later, longer deadline extends the cooldown; it never shortens
// an existing one.
type RateLimitGate struct {
	mu       sync.RWMutex
	until    time.Time
	probing  bool
	fallback time.Duration
	now      func() time.Time
	ctx      context.Context
	store    RateLimitStateStore
}

func NewRateLimitGate(fallback time.Duration) *RateLimitGate {
	if fallback <= 0 {
		fallback = 15 * time.Minute
	}
	return &RateLimitGate{fallback: fallback, now: time.Now}
}

// NewPersistentRateLimitGate restores a previously observed provider
// Retry-After deadline before any new upstream work is admitted. Persisting
// the absolute deadline prevents a container restart from bypassing an active
// Hardcover cooldown.
func NewPersistentRateLimitGate(ctx context.Context, fallback time.Duration, store RateLimitStateStore) *RateLimitGate {
	gate := NewRateLimitGate(fallback)
	if ctx == nil {
		ctx = context.Background()
	}
	gate.ctx = context.WithoutCancel(ctx)
	gate.store = store
	if store == nil {
		return gate
	}

	value, ok := store.Get(gate.ctx, hardcoverRateLimitStateKey)
	if !ok {
		return gate
	}
	until, err := time.Parse(time.RFC3339Nano, string(value))
	if err != nil {
		Log(gate.ctx).Warn("discarding invalid persisted Hardcover rate-limit deadline", "err", err)
		if deleteErr := store.DeleteRateLimitState(gate.ctx, hardcoverRateLimitStateKey); deleteErr != nil {
			Log(gate.ctx).Warn("unable to delete invalid Hardcover rate-limit deadline", "err", deleteErr)
		}
		return gate
	}
	if !gate.now().Before(until) {
		if deleteErr := store.DeleteRateLimitState(gate.ctx, hardcoverRateLimitStateKey); deleteErr != nil {
			Log(gate.ctx).Warn("unable to delete expired Hardcover rate-limit deadline", "err", deleteErr)
		}
		return gate
	}

	gate.until = until
	Log(gate.ctx).Warn("restored active Hardcover rate-limit cooldown", "until", until, "remaining", until.Sub(gate.now()).Round(time.Second))
	return gate
}

// Check returns a typed local 429 while the cooldown is active.
func (g *RateLimitGate) Check() error {
	if g == nil {
		return nil
	}
	g.mu.RLock()
	until := g.until
	now := g.now
	g.mu.RUnlock()
	if until.IsZero() || !now().Before(until) {
		return nil
	}
	return &RateLimitError{Until: until, now: now}
}

// Acquire is called immediately before a physical request. After an expired
// cooldown it permits exactly one half-open probe while concurrent callers
// continue to fail locally.
func (g *RateLimitGate) Acquire() error {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	if !g.until.IsZero() && now.Before(g.until) {
		return &RateLimitError{Until: g.until, now: g.now}
	}
	if g.until.IsZero() {
		return nil
	}
	if g.probing {
		return &RateLimitError{Until: now.Add(time.Second), now: g.now}
	}
	g.probing = true
	return nil
}

// Success closes a half-open circuit after a physical response proves the
// provider is accepting requests again.
func (g *RateLimitGate) Success() {
	if g == nil {
		return
	}
	g.mu.Lock()
	hadCooldown := !g.until.IsZero() || g.probing
	g.until = time.Time{}
	g.probing = false
	if hadCooldown && g.store != nil {
		if err := g.store.DeleteRateLimitState(g.ctx, hardcoverRateLimitStateKey); err != nil {
			Log(g.ctx).Warn("unable to clear persisted Hardcover rate-limit deadline", "err", err)
		}
	}
	g.mu.Unlock()
}

func (g *RateLimitGate) probeFailed() {
	if g == nil {
		return
	}
	g.mu.Lock()
	g.probing = false
	g.mu.Unlock()
}

// Open starts or extends the cooldown using Retry-After. It accepts both the
// standard delta-seconds form and an HTTP-date. Missing or invalid values use
// the configured conservative fallback.
func (g *RateLimitGate) Open(retryAfter string, cause error) *RateLimitError {
	if g == nil {
		return &RateLimitError{Until: time.Now().Add(15 * time.Minute), Cause: cause}
	}

	g.mu.Lock()
	now := g.now()
	delay := parseRetryAfter(retryAfter, now)
	if delay <= 0 {
		delay = g.fallback
	}
	until := now.Add(delay)
	if g.until.After(until) {
		until = g.until
	} else {
		g.until = until
	}
	g.probing = false
	if g.store != nil {
		// Keep the row just beyond the deadline so a restart immediately before
		// expiry cannot race cache expiration. Restoration deletes stale rows.
		if err := g.store.PersistRateLimitState(g.ctx, hardcoverRateLimitStateKey, []byte(until.UTC().Format(time.RFC3339Nano)), until.Sub(now)+time.Minute); err != nil {
			// The in-memory deadline was already installed and must remain active
			// even when durable persistence is temporarily unavailable.
			Log(g.ctx).Error("unable to persist Hardcover rate-limit deadline; keeping in-memory cooldown active", "err", err, "until", until)
		}
	}
	nowFn := g.now
	g.mu.Unlock()

	return &RateLimitError{Until: until, Cause: cause, now: nowFn}
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		if seconds <= 0 {
			return 0
		}
		return time.Duration(seconds) * time.Second
	}
	when, err := http.ParseTime(value)
	if err != nil || !when.After(now) {
		return 0
	}
	return when.Sub(now)
}

// RateLimitTransport enforces the gate before a physical request and opens it
// when Hardcover responds with 429. The response body is drained and closed so
// the underlying connection remains reusable.
type RateLimitTransport struct {
	Gate *RateLimitGate
	http.RoundTripper
}

func (t RateLimitTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if err := t.Gate.Acquire(); err != nil {
		return nil, err
	}
	transport := t.RoundTripper
	if transport == nil {
		transport = http.DefaultTransport
	}
	resp, err := transport.RoundTrip(r)
	if err != nil {
		t.Gate.probeFailed()
		return nil, err
	}
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Gate.Success()
		return resp, nil
	}

	retryAfter := resp.Header.Get("Retry-After")
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	cause := errors.Join(statusErr(http.StatusTooManyRequests), fmt.Errorf("hardcover returned %s", resp.Status))
	return nil, t.Gate.Open(retryAfter, cause)
}
