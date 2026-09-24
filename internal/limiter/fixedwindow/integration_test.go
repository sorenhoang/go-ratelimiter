//go:build integration

// Package-level note: these run against a real Redis, started by `make up`.
//
// miniredis executes Lua through a Go interpreter rather than Redis' own, so
// float formatting, TIME resolution and reply types can drift at the edges.
// The unit tests give a fast loop; this file is the proof that the script the
// unit tests exercise behaves the same on the server it will actually run on.
package fixedwindow_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/sorenhoang/go-ratelimiter/internal/limiter/fixedwindow"
)

func realLimiter(t *testing.T, cfg fixedwindow.Config) (*fixedwindow.Limiter, string) {
	t.Helper()

	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}

	rdb := redis.NewClient(&redis.Options{Addr: addr, MaxRetries: -1})
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("no Redis at %s (run `make up`): %v", addr, err)
	}
	t.Cleanup(func() { _ = rdb.Close() })

	l, err := fixedwindow.New(rdb, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Unique per run, so repeated runs and parallel tests never share a counter.
	key := fmt.Sprintf("it-%d", time.Now().UnixNano())
	t.Cleanup(func() { _ = l.Reset(context.Background(), key) })
	return l, key
}

func TestIntegration_ExhaustsThenRecoversInTheNextWindow(t *testing.T) {
	const window = 2 * time.Second
	l, key := realLimiter(t, fixedwindow.Config{Limit: 3, Window: window})
	ctx := context.Background()

	for i := 1; i <= 3; i++ {
		d, err := l.AllowN(ctx, key, 1)
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if !d.Allowed {
			t.Fatalf("call %d denied, want allowed", i)
		}
		if want := int64(3 - i); d.Remaining != want {
			t.Errorf("call %d: Remaining = %d, want %d", i, d.Remaining, want)
		}
		if d.ResetAfter <= 0 || d.ResetAfter > window {
			t.Errorf("call %d: ResetAfter = %s, want within (0, %s]", i, d.ResetAfter, window)
		}
	}

	d, err := l.AllowN(ctx, key, 1)
	if err != nil {
		t.Fatalf("fourth call: %v", err)
	}
	if d.Allowed {
		t.Fatal("fourth call allowed, want denied")
	}
	if d.RetryAfter <= 0 {
		t.Errorf("RetryAfter = %s, want positive", d.RetryAfter)
	}

	// Real clock, real server: wait out the window rather than faking time.
	time.Sleep(window + 200*time.Millisecond)

	d, err = l.AllowN(ctx, key, 1)
	if err != nil {
		t.Fatalf("after the window: %v", err)
	}
	if !d.Allowed {
		t.Error("denied in the next window, want allowed")
	}
}

func TestIntegration_CostAndReset(t *testing.T) {
	l, key := realLimiter(t, fixedwindow.Config{Limit: 5, Window: time.Minute})
	ctx := context.Background()

	if d, err := l.AllowN(ctx, key, 4); err != nil || !d.Allowed || d.Remaining != 1 {
		t.Fatalf("cost 4 of 5: %+v err=%v; want allowed with 1 left", d, err)
	}
	if d, err := l.AllowN(ctx, key, 4); err != nil || d.Allowed {
		t.Fatalf("cost 4 with 1 left: %+v err=%v; want denied", d, err)
	}

	if err := l.Reset(ctx, key); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if d, err := l.AllowN(ctx, key, 4); err != nil || !d.Allowed || d.Remaining != 1 {
		t.Fatalf("after Reset: %+v err=%v; want the quota back", d, err)
	}
}
