//go:build integration

// Runs against a real Redis, started by `make up`.
//
// miniredis executes Lua through a Go interpreter rather than Redis' own, so
// float formatting, TIME resolution and sorted set semantics can drift at the
// edges. The unit tests give a fast loop; this file proves the same script
// behaves on the server it will actually run against.
package slidingwindowlog_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/sorenhoang/go-ratelimiter/internal/limiter/slidingwindowlog"
)

func realLimiter(t *testing.T, cfg slidingwindowlog.Config) (*redis.Client, *slidingwindowlog.Limiter, string) {
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

	// Unique per run, so repeated runs never share a log.
	key := fmt.Sprintf("it-%d", time.Now().UnixNano())
	l, err := slidingwindowlog.New(rdb, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = l.Reset(context.Background(), key) })

	return rdb, l, key
}

func TestIntegration_ExhaustsThenSlides(t *testing.T) {
	const window = 2 * time.Second
	_, l, key := realLimiter(t, slidingwindowlog.Config{Limit: 2, Window: window})
	ctx := context.Background()

	first, err := l.AllowN(ctx, key, 1)
	if err != nil || !first.Allowed {
		t.Fatalf("first: %+v err=%v", first, err)
	}

	// Half a window later, so the two entries expire at clearly different times.
	time.Sleep(window / 2)

	second, err := l.AllowN(ctx, key, 1)
	if err != nil || !second.Allowed {
		t.Fatalf("second: %+v err=%v", second, err)
	}

	denied, err := l.AllowN(ctx, key, 1)
	if err != nil {
		t.Fatalf("third: %v", err)
	}
	if denied.Allowed {
		t.Fatal("third request allowed, want denied")
	}
	// The wait is until the first entry ages out, which is about half a window
	// from here -- not a whole one, which is what a fixed window would report.
	if denied.RetryAfter <= 0 || denied.RetryAfter > window/2+300*time.Millisecond {
		t.Errorf("RetryAfter = %s, want roughly half of %s", denied.RetryAfter, window)
	}

	// Once the first entry leaves, exactly one slot opens.
	time.Sleep(window/2 + 300*time.Millisecond)

	if d, err := l.AllowN(ctx, key, 1); err != nil || !d.Allowed {
		t.Fatalf("after the first entry expired: %+v err=%v", d, err)
	}
	if d, err := l.AllowN(ctx, key, 1); err != nil || d.Allowed {
		t.Fatalf("second slot should still be taken: %+v err=%v", d, err)
	}
}

// Real Redis resolves TIME and sorted set writes itself, and a tight loop lands
// several requests on one millisecond. If the member were built from the
// timestamp alone they would overwrite each other and the log would come up
// short, so this asserts the log length directly rather than trusting Remaining,
// which is arithmetic on the count read before the write.
func TestIntegration_SameMillisecondRequestsAllRecorded(t *testing.T) {
	const limit = 5
	rdb, l, key := realLimiter(t, slidingwindowlog.Config{Limit: limit, Window: time.Minute})
	ctx := context.Background()

	for i := 1; i <= limit; i++ {
		d, err := l.AllowN(ctx, key, 1)
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if !d.Allowed {
			t.Fatalf("call %d denied, want allowed", i)
		}
	}

	entries, err := rdb.ZRangeWithScores(ctx, "rl:swl:"+key, 0, -1).Result()
	if err != nil {
		t.Fatalf("read the log: %v", err)
	}
	if len(entries) != limit {
		t.Fatalf("log holds %d entries, want %d — members collided", len(entries), limit)
	}

	distinct := map[float64]int{}
	for _, e := range entries {
		distinct[e.Score]++
	}
	t.Logf("%d entries across %d distinct millisecond(s)", len(entries), len(distinct))
	if len(distinct) == limit {
		t.Log("note: every request landed on its own millisecond, so this run did " +
			"not actually exercise the collision case")
	}

	if d, err := l.AllowN(ctx, key, 1); err != nil || d.Allowed {
		t.Fatalf("call %d: %+v err=%v; want denied", limit+1, d, err)
	}
}

func TestIntegration_CostAndReset(t *testing.T) {
	_, l, key := realLimiter(t, slidingwindowlog.Config{Limit: 5, Window: time.Minute})
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
