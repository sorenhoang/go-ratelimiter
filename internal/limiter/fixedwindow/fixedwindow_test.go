package fixedwindow_test

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
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

	rdb := redis.NewClient(&redis.Options{Addr: s.Addr()})

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
		descision, err := l.AllowN(ctx, "user", 1)
		if err != nil {
			t.Fatalf("failed to check rate limit: %v", err)
		}
		if !descision.Allowed {
			t.Errorf("expected request to be allowed, but it was denied")
		}

		if descision.Remaining != int64(5-i) {
			t.Errorf("call %d: Remaining = %d, want %d", i, descision.Remaining, 5-i)
		}
	}

	descision, err := l.AllowN(ctx, "user", 1)
	if err != nil {
		t.Fatalf("failed to check rate limit: %v", err)
	}
	if descision.Allowed {
		t.Errorf("expected request to be denied, but it was allowed")
	}
	if descision.Remaining != 0 {
		t.Errorf("Remaining = %d, want 0", descision.Remaining)
	}
	if descision.RetryAfter <= 0 {
		t.Errorf("RetryAfter = %s, want positive duration", descision.RetryAfter)
	}
}
