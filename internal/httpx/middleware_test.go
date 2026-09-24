package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sorenhoang/go-ratelimiter/internal/limiter"
)

// stubLimiter answers with whatever the test sets, so the HTTP layer can be
// exercised without Redis and without any real algorithm in the way.
type stubLimiter struct {
	decision limiter.Decision
	err      error

	gotKey string
	gotN   int64
	calls  int
}

func (s *stubLimiter) AllowN(_ context.Context, key string, n int64) (limiter.Decision, error) {
	s.calls++
	s.gotKey = key
	s.gotN = n
	return s.decision, s.err
}

func (s *stubLimiter) Name() string                        { return "stub" }
func (s *stubLimiter) Reset(context.Context, string) error { return nil }

var _ limiter.Limiter = (*stubLimiter)(nil)

// serve runs one request through the middleware and reports whether the wrapped
// handler was reached.
func serve(t *testing.T, cfg Config) (*httptest.ResponseRecorder, bool) {
	t.Helper()

	reached := false
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusTeapot) // distinctive: only next can produce it
	})

	rec := httptest.NewRecorder()
	RateLimit(cfg)(next).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", nil))
	return rec, reached
}

func TestRateLimit_AllowedSetsHeadersAndCallsNext(t *testing.T) {
	rec, reached := serve(t, Config{
		Limiter: &stubLimiter{decision: limiter.Decision{
			Allowed: true, Limit: 10, Remaining: 7, ResetAfter: 30 * time.Second,
		}},
	})

	if !reached {
		t.Fatal("next was not called for an allowed request")
	}
	if rec.Code != http.StatusTeapot {
		t.Errorf("status = %d, want %d from next", rec.Code, http.StatusTeapot)
	}
	for header, want := range map[string]string{
		"RateLimit-Limit":     "10",
		"RateLimit-Remaining": "7",
		"RateLimit-Reset":     "30",
	} {
		if got := rec.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	if got := rec.Header().Get("Retry-After"); got != "" {
		t.Errorf("Retry-After = %q on an allowed request, want it absent", got)
	}
}

func TestRateLimit_DeniedWrites429AndDoesNotCallNext(t *testing.T) {
	rec, reached := serve(t, Config{
		Limiter: &stubLimiter{decision: limiter.Decision{
			Allowed: false, Limit: 5, Remaining: 0,
			ResetAfter: 2500 * time.Millisecond, RetryAfter: 2500 * time.Millisecond,
		}},
	})

	if reached {
		t.Fatal("next was called for a denied request")
	}
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	// 2.5s must round up to 3, never down to 2: a short reply makes the client
	// come back before the window has actually rolled.
	if got := rec.Header().Get("Retry-After"); got != "3" {
		t.Errorf("Retry-After = %q, want 3", got)
	}
	if got := rec.Header().Get("RateLimit-Reset"); got != "3" {
		t.Errorf("RateLimit-Reset = %q, want 3", got)
	}

	var body ErrorBody
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if body.Error == "" || body.RetryAfter != 3 {
		t.Errorf("body = %+v, want a message and retry_after 3", body)
	}
}

func TestRateLimit_FailOpenOnlyForBackendOutage(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		failOpen   bool
		wantNext   bool
		wantStatus int
	}{
		{
			name:     "backend down, fail open",
			err:      errors.Join(limiter.ErrBackendUnavailable, errors.New("dial tcp: refused")),
			failOpen: true, wantNext: true, wantStatus: http.StatusTeapot,
		},
		{
			name:     "backend down, fail closed",
			err:      errors.Join(limiter.ErrBackendUnavailable, errors.New("dial tcp: refused")),
			failOpen: false, wantNext: false, wantStatus: http.StatusServiceUnavailable,
		},
		{
			// A bug on our side must never open the gate, or it hides behind the
			// traffic it let through until someone abuses it.
			name:     "other error, fail open still closes",
			err:      errors.New("invalid n: 0"),
			failOpen: true, wantNext: false, wantStatus: http.StatusServiceUnavailable,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec, reached := serve(t, Config{
				Limiter:  &stubLimiter{err: tc.err},
				FailOpen: tc.failOpen,
			})

			if reached != tc.wantNext {
				t.Errorf("next called = %v, want %v", reached, tc.wantNext)
			}
			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if tc.wantNext {
				// Nothing is known about the quota here, so claiming a number
				// would be a lie.
				if got := rec.Header().Get("RateLimit-Limit"); got != "" {
					t.Errorf("RateLimit-Limit = %q on the fail-open path, want it absent", got)
				}
			}
		})
	}
}

func TestRateLimit_DefaultsKeyFuncAndCost(t *testing.T) {
	stub := &stubLimiter{decision: limiter.Decision{Allowed: true}}

	// Config leaves KeyFunc and Cost unset: a nil KeyFunc would panic here, and
	// a zero Cost would make every real limiter reject the call as invalid n.
	if _, reached := serve(t, Config{Limiter: stub}); !reached {
		t.Fatal("next was not called")
	}
	if stub.gotN != 1 {
		t.Errorf("cost passed to AllowN = %d, want the default 1", stub.gotN)
	}
	// httptest gives every request RemoteAddr 192.0.2.1:1234; KeyByIP drops the port.
	if stub.gotKey != "192.0.2.1" {
		t.Errorf("key = %q, want the default KeyByIP result %q", stub.gotKey, "192.0.2.1")
	}
}

func TestCeilSeconds(t *testing.T) {
	tests := []struct {
		in   time.Duration
		want int64
	}{
		{0, 0},
		{-time.Second, 0},
		{time.Nanosecond, 1},
		{400 * time.Millisecond, 1},
		{time.Second, 1},
		{1001 * time.Millisecond, 2},
		{59500 * time.Millisecond, 60},
	}
	for _, tc := range tests {
		if got := ceilSeconds(tc.in); got != tc.want {
			t.Errorf("ceilSeconds(%s) = %d, want %d", tc.in, got, tc.want)
		}
	}
}
