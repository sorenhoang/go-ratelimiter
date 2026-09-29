//go:build integration

// Runs against a real Redis, started by `make up`.
//
// miniredis interprets Lua in Go and keeps its own idea of expiry, so the two
// places this algorithm is most likely to drift are exactly the two it is
// checked on here: the arithmetic that weights the previous window, and the TTL
// that keeps that window alive long enough to be weighted.
package slidingwindowcounter_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/sorenhoang/go-ratelimiter/internal/limiter/slidingwindowcounter"
)

func realLimiter(t *testing.T, cfg slidingwindowcounter.Config) (*redis.Client, *slidingwindowcounter.Limiter, string) {
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

	l, err := slidingwindowcounter.New(rdb, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	key := fmt.Sprintf("it-%d", time.Now().UnixNano())
	t.Cleanup(func() { _ = l.Reset(context.Background(), key) })

	return rdb, l, key
}

// waitForWindowStart blocks until just after the next window boundary, so the
// offsets the test relies on are measured from a known zero.
func waitForWindowStart(window time.Duration) {
	now := time.Now().UnixMilli()
	next := now - now%window.Milliseconds() + window.Milliseconds()
	time.Sleep(time.Duration(next-now)*time.Millisecond + 20*time.Millisecond)
}

func TestIntegration_CounterKeyLivesTwoWindows(t *testing.T) {
	const window = 2 * time.Second
	rdb, l, key := realLimiter(t, slidingwindowcounter.Config{Limit: 5, Window: window})
	ctx := context.Background()

	waitForWindowStart(window)
	if d, err := l.AllowN(ctx, key, 1); err != nil || !d.Allowed {
		t.Fatalf("first: %+v err=%v", d, err)
	}

	now := time.Now().UnixMilli()
	start := now - now%window.Milliseconds()
	ttl, err := rdb.PTTL(ctx, fmt.Sprintf("rl:swc:%s:%d", key, start)).Result()
	if err != nil {
		t.Fatalf("PTTL: %v", err)
	}

	// Anything at or below one window means the counter dies before the next
	// window can weight it, and the algorithm silently becomes a fixed window.
	if ttl <= window {
		t.Errorf("TTL = %s, want more than one window (%s)", ttl, window)
	}
	if ttl > 2*window {
		t.Errorf("TTL = %s, want no more than two windows (%s)", ttl, 2*window)
	}
}

func TestIntegration_PreviousWindowStillCounts(t *testing.T) {
	const window = 2 * time.Second
	_, l, key := realLimiter(t, slidingwindowcounter.Config{Limit: 4, Window: window})
	ctx := context.Background()

	waitForWindowStart(window)

	// Spend the whole allowance inside this window.
	for i := 1; i <= 4; i++ {
		d, err := l.AllowN(ctx, key, 1)
		if err != nil {
			t.Fatalf("fill %d: %v", i, err)
		}
		if !d.Allowed {
			t.Fatalf("fill %d denied, want allowed", i)
		}
	}
	if d, err := l.AllowN(ctx, key, 1); err != nil || d.Allowed {
		t.Fatalf("fifth in the same window: %+v err=%v; want denied", d, err)
	}

	// Just into the next window the previous count is still almost fully in
	// view, so the quota has not come back. A fixed window would be wide open
	// here -- this is the boundary burst, on a real clock.
	waitForWindowStart(window)
	if d, err := l.AllowN(ctx, key, 1); err != nil || d.Allowed {
		t.Fatalf("start of the next window: %+v err=%v; want denied", d, err)
	}

	// Three quarters through, the weight is about 0.25 and the estimate about 1,
	// so there is room again.
	time.Sleep(window * 3 / 4)
	if d, err := l.AllowN(ctx, key, 1); err != nil || !d.Allowed {
		t.Fatalf("three quarters through the next window: %+v err=%v; want allowed", d, err)
	}
}

func TestIntegration_CostAndReset(t *testing.T) {
	const window = time.Minute
	_, l, key := realLimiter(t, slidingwindowcounter.Config{Limit: 5, Window: window})
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
