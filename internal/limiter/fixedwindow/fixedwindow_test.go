package fixedwindow_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/sorenhoang/go-ratelimiter/internal/limiter"
	"github.com/sorenhoang/go-ratelimiter/internal/limiter/fixedwindow"
)

var baseTime = time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)

func newTestLimiter(t *testing.T, cfg fixedwindow.Config) (*miniredis.Miniredis, *fixedwindow.Limiter, func(time.Duration)) {
	t.Helper()

	s, err := miniredis.Run()
	if err != nil {
		t.Fatalf("failed to start miniredis: %v", err)
	}

	t.Cleanup(func() { s.Close() })

	rdb := redis.NewClient(&redis.Options{
		Addr: s.Addr(),
		// -1 disables retries. The backend-down tests kill miniredis on purpose, so
		// waiting out go-redis' retry backoff is pure dead time.
		MaxRetries: -1,
	})

	s.SetTime(baseTime)
	l, err := fixedwindow.New(rdb, cfg)

	if err != nil {
		t.Fatalf("failed to create limiter: %v", err)
	}

	elapsed := time.Duration(0)
	advance := func(d time.Duration) {
		elapsed += d
		s.SetTime(baseTime.Add(elapsed))
		s.FastForward(d)
	}
	return s, l, advance
}

func TestAllowN_ExhaustsLimit(t *testing.T) {
	_, l, _ := newTestLimiter(t, fixedwindow.Config{
		Limit:  5,
		Window: time.Minute,
	})

	ctx := context.Background()
	for i := 1; i <= 5; i++ {
		decision, err := l.AllowN(ctx, "user", 1)
		if err != nil {
			t.Fatalf("failed to check rate limit: %v %v", err, limiter.ErrBackendUnavailable)
		}
		if !decision.Allowed {
			t.Errorf("expected request to be allowed, but it was denied")
		}

		if decision.Remaining != int64(5-i) {
			t.Errorf("call %d: Remaining = %d, want %d", i, decision.Remaining, 5-i)
		}
	}

	decision, err := l.AllowN(ctx, "user", 1)
	if err != nil {
		t.Fatalf("failed to check rate limit: %v %v", err, limiter.ErrBackendUnavailable)
	}
	if decision.Allowed {
		t.Errorf("expected request to be denied, but it was allowed")
	}
	if decision.Remaining != 0 {
		t.Errorf("Remaining = %d, want 0", decision.Remaining)
	}
	if decision.RetryAfter <= 0 {
		t.Errorf("RetryAfter = %s, want positive duration", decision.RetryAfter)
	}
}

func TestAllowN_AllowsDoubleLimitAcrossWindowBoundary(t *testing.T) {

	_, l, advance := newTestLimiter(t, fixedwindow.Config{
		Limit:  5,
		Window: time.Minute,
	})

	ctx := context.Background()
	advance(59 * time.Second)
	for i := 1; i <= 5; i++ {
		decision, err := l.AllowN(ctx, "user", 1)
		if err != nil {
			t.Fatalf("failed to check rate limit: %v %v", err, limiter.ErrBackendUnavailable)
		}
		if !decision.Allowed {
			t.Errorf("expected request to be allowed, but it was denied")
		}
	}

	decision, err := l.AllowN(ctx, "user", 1)
	if err != nil {
		t.Fatalf("failed to check rate limit: %v", err)
	}
	if decision.Allowed {
		t.Errorf("expected request to be denied, but it was allowed")
	}

	advance(2 * time.Second)

	for i := 1; i <= 5; i++ {
		decision, err := l.AllowN(ctx, "user", 1)
		if err != nil {
			t.Fatalf("failed to check rate limit: %v", err)
		}
		if !decision.Allowed {
			t.Errorf("expected request to be allowed, but it was denied")
		}
	}
}

func TestAllowN_ReturnsBackendUnavailableWhenRedisIsDown(t *testing.T) {
	s, l, _ := newTestLimiter(t, fixedwindow.Config{
		Limit:  5,
		Window: time.Minute,
	})

	ctx := context.Background()

	decision, err := l.AllowN(ctx, "user", 1)
	if err != nil {
		t.Fatalf("failed to check rate limit: %v", err)
	}
	if !decision.Allowed {
		t.Errorf("expected request to be allowed, but it was denied")
	}

	s.Close() // Simulate Redis going down

	_, err = l.AllowN(ctx, "user", 1)
	if err == nil {
		t.Fatalf("expected error when Redis is down, but got nil")
	}
	if !errors.Is(err, limiter.ErrBackendUnavailable) {
		t.Errorf("expected ErrBackendUnavailable, got %v", err)
	}
}

func TestReset_ReturnsBackendUnavailableWhenRedisIsDown(t *testing.T) {
	s, l, _ := newTestLimiter(t, fixedwindow.Config{
		Limit:  5,
		Window: time.Minute,
	})

	ctx := context.Background()

	// Reset should succeed when Redis is up
	if err := l.Reset(ctx, "user"); err != nil {
		t.Fatalf("failed to reset rate limit: %v", err)
	}
	s.Close() // Simulate Redis going down

	err := l.Reset(ctx, "user")
	if err == nil {
		t.Fatalf("expected error when Redis is down, but got nil")
	}
	if !errors.Is(err, limiter.ErrBackendUnavailable) {
		t.Errorf("err = %v, want it to wrap ErrBackendUnavailable", err)
	}
}

func TestAllowN_ReportsExactResetAndRetry(t *testing.T) {
	// baseTime sits on a whole minute, so with a one minute window the clock
	// starts exactly at a window boundary and every duration below is exact
	// rather than "about".
	_, l, advance := newTestLimiter(t, fixedwindow.Config{
		Limit:  2,
		Window: time.Minute,
	})
	ctx := context.Background()

	d, err := l.AllowN(ctx, "user", 1)
	if err != nil {
		t.Fatalf("AllowN: %v", err)
	}
	if d.Limit != 2 {
		t.Errorf("Limit = %d, want 2", d.Limit)
	}
	if d.ResetAfter != time.Minute {
		t.Errorf("ResetAfter = %s, want %s", d.ResetAfter, time.Minute)
	}
	if d.RetryAfter != 0 {
		t.Errorf("RetryAfter = %s, want 0 while allowed", d.RetryAfter)
	}

	advance(20 * time.Second)

	d, err = l.AllowN(ctx, "user", 1)
	if err != nil {
		t.Fatalf("AllowN: %v", err)
	}
	if want := 40 * time.Second; d.ResetAfter != want {
		t.Errorf("ResetAfter = %s, want %s", d.ResetAfter, want)
	}

	// Quota is spent; the wait to retry is the rest of this window.
	d, err = l.AllowN(ctx, "user", 1)
	if err != nil {
		t.Fatalf("AllowN: %v", err)
	}
	if d.Allowed {
		t.Fatal("third request allowed, want denied")
	}
	if want := 40 * time.Second; d.RetryAfter != want {
		t.Errorf("RetryAfter = %s, want %s", d.RetryAfter, want)
	}
}

func TestAllowN_CostAboveRemainingIsRejectedNotClamped(t *testing.T) {
	_, l, _ := newTestLimiter(t, fixedwindow.Config{
		Limit:  5,
		Window: time.Minute,
	})
	ctx := context.Background()

	d, err := l.AllowN(ctx, "user", 3)
	if err != nil {
		t.Fatalf("AllowN: %v", err)
	}
	if !d.Allowed || d.Remaining != 2 {
		t.Fatalf("cost 3 of 5: Allowed = %v, Remaining = %d; want true, 2", d.Allowed, d.Remaining)
	}

	// 3 more would reach 6 against a limit of 5. It must be refused outright,
	// not partially served down to the limit.
	d, err = l.AllowN(ctx, "user", 3)
	if err != nil {
		t.Fatalf("AllowN: %v", err)
	}
	if d.Allowed {
		t.Error("cost 3 with 2 left was allowed, want denied")
	}
	if d.Remaining != 2 {
		t.Errorf("Remaining = %d after a denial, want 2 untouched", d.Remaining)
	}

	// A cost that fits exactly still goes through.
	d, err = l.AllowN(ctx, "user", 2)
	if err != nil {
		t.Fatalf("AllowN: %v", err)
	}
	if !d.Allowed || d.Remaining != 0 {
		t.Errorf("cost 2 with 2 left: Allowed = %v, Remaining = %d; want true, 0", d.Allowed, d.Remaining)
	}
}

func TestAllowN_KeyExpiresWithItsWindow(t *testing.T) {
	s, l, advance := newTestLimiter(t, fixedwindow.Config{
		Limit:  5,
		Window: time.Minute,
	})
	ctx := context.Background()

	if _, err := l.AllowN(ctx, "user", 1); err != nil {
		t.Fatalf("AllowN: %v", err)
	}
	if got := len(s.Keys()); got != 1 {
		t.Fatalf("keys after one call = %d, want 1", got)
	}

	// The counter carries a TTL of what is left of its own window, so once that
	// window is over Redis drops it instead of holding one key per window for
	// every caller that ever appeared.
	advance(time.Minute)
	if got := s.Keys(); len(got) != 0 {
		t.Errorf("keys after the window elapsed = %v, want none", got)
	}
}
