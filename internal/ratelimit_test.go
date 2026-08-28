package internal

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, time.August, 27, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name  string
		value string
		want  time.Duration
	}{
		{name: "delta seconds", value: "120", want: 2 * time.Minute},
		{name: "http date", value: now.Add(3 * time.Minute).Format(http.TimeFormat), want: 3 * time.Minute},
		{name: "missing", value: "", want: 0},
		{name: "invalid", value: "later", want: 0},
		{name: "expired date", value: now.Add(-time.Minute).Format(http.TimeFormat), want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, parseRetryAfter(tt.value, now))
		})
	}
}

func TestRateLimitGateUsesFallbackAndNeverShortensCooldown(t *testing.T) {
	now := time.Date(2026, time.August, 27, 12, 0, 0, 0, time.UTC)
	gate := NewRateLimitGate(15 * time.Minute)
	gate.now = func() time.Time { return now }

	first := gate.Open("invalid", errors.New("quota"))
	assert.Equal(t, 15*time.Minute, first.RetryAfter())
	shorter := gate.Open("30", errors.New("quota again"))
	assert.Equal(t, 15*time.Minute, shorter.RetryAfter())

	err := gate.Check()
	require.Error(t, err)
	assert.ErrorIs(t, err, statusErr(http.StatusTooManyRequests))

	now = now.Add(15 * time.Minute)
	assert.NoError(t, gate.Check())
	assert.NoError(t, gate.Acquire())
	assert.Error(t, gate.Acquire(), "only one half-open probe may run")
	gate.Success()
	assert.NoError(t, gate.Acquire())
}

func TestRateLimitTransportMakesNoCallsDuringCooldown(t *testing.T) {
	var calls atomic.Int32
	gate := NewRateLimitGate(time.Hour)
	transport := RateLimitTransport{
		Gate: gate,
		RoundTripper: roundTripperFunc(func(*http.Request) (*http.Response, error) {
			calls.Add(1)
			return &http.Response{
				StatusCode: http.StatusTooManyRequests,
				Status:     "429 Too Many Requests",
				Header:     http.Header{"Retry-After": []string{"3600"}},
				Body:       io.NopCloser(strings.NewReader("limited")),
			}, nil
		}),
	}

	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://example.test", nil)
	require.NoError(t, err)
	_, err = transport.RoundTrip(request)
	require.Error(t, err)
	assert.ErrorIs(t, err, statusErr(http.StatusTooManyRequests))
	assert.Equal(t, int32(1), calls.Load())

	_, err = transport.RoundTrip(request)
	require.Error(t, err)
	assert.ErrorIs(t, err, statusErr(http.StatusTooManyRequests))
	assert.Equal(t, int32(1), calls.Load())
}

func TestRateLimitGateRestoresCooldownAcrossRestart(t *testing.T) {
	store := newMemoryCache().(*memoryCache)
	first := NewPersistentRateLimitGate(t.Context(), time.Hour, store)
	opened := first.Open("3600", errors.New("quota"))
	require.Greater(t, opened.RetryAfter(), 59*time.Minute)

	// Constructing a new gate simulates restarting the process while the
	// provider's Retry-After window is still active.
	restarted := NewPersistentRateLimitGate(context.Background(), time.Hour, store)
	var calls atomic.Int32
	transport := RateLimitTransport{
		Gate: restarted,
		RoundTripper: roundTripperFunc(func(*http.Request) (*http.Response, error) {
			calls.Add(1)
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       http.NoBody,
			}, nil
		}),
	}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://example.test", nil)
	require.NoError(t, err)
	_, err = transport.RoundTrip(request)
	require.Error(t, err)
	assert.ErrorIs(t, err, statusErr(http.StatusTooManyRequests))
	assert.Zero(t, calls.Load(), "a restart must not bypass the persisted provider cooldown")

	restarted.Success()
	afterSuccess := NewPersistentRateLimitGate(t.Context(), time.Hour, store)
	assert.NoError(t, afterSuccess.Check(), "a successful probe must clear persisted cooldown state")
}

type failingRateLimitStateStore struct {
	*memoryCache
	persistErr error
}

func (s *failingRateLimitStateStore) PersistRateLimitState(context.Context, string, []byte, time.Duration) error {
	return s.persistErr
}

func TestRateLimitGateStaysClosedWhenPersistenceFails(t *testing.T) {
	store := &failingRateLimitStateStore{
		memoryCache: newMemoryCache().(*memoryCache),
		persistErr:  errors.New("database unavailable"),
	}
	gate := NewPersistentRateLimitGate(t.Context(), time.Hour, store)
	opened := gate.Open("3600", errors.New("quota"))
	require.Greater(t, opened.RetryAfter(), 59*time.Minute)

	err := gate.Check()
	require.Error(t, err)
	assert.ErrorIs(t, err, statusErr(http.StatusTooManyRequests))
	_, persisted := store.Get(t.Context(), hardcoverRateLimitStateKey)
	assert.False(t, persisted, "a failed durable write must not be mistaken for persisted state")
}
