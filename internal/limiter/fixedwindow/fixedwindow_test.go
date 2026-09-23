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
