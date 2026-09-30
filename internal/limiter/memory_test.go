//go:build integration

package limiter_test

import (
	"context"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/sorenhoang/go-ratelimiter/internal/limiter"
	"github.com/sorenhoang/go-ratelimiter/internal/limiter/fixedwindow"
	"github.com/sorenhoang/go-ratelimiter/internal/limiter/leakybucket"
	"github.com/sorenhoang/go-ratelimiter/internal/limiter/slidingwindowcounter"
	"github.com/sorenhoang/go-ratelimiter/internal/limiter/slidingwindowlog"
	"github.com/sorenhoang/go-ratelimiter/internal/limiter/tokenbucket"
)

// TestMemoryPerKey measures what one busy caller costs each algorithm in Redis.
//
// This is the number the latency benchmark cannot show. Every limiter answers
// in about the time of one round trip, so the sliding window log is no slower
// than the rest — the entire price of its exactness is here, in memory, and
// asking Redis is the only way to see it.
//
// It asserts an order of magnitude rather than a figure. The exact byte count
// depends on the Redis version's encoding of a sorted set, and pinning it would
// make this a test of Redis rather than of the algorithms.
func TestMemoryPerKey(t *testing.T) {
	const requests = 2000

	rdb := realRedisForMemory(t)

	fw, err := fixedwindow.New(rdb, fixedwindow.Config{Limit: 1_000_000, Window: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	swl, err := slidingwindowlog.New(rdb, slidingwindowlog.Config{Limit: 1_000_000, Window: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	swc, err := slidingwindowcounter.New(rdb, slidingwindowcounter.Config{Limit: 1_000_000, Window: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	tb, err := tokenbucket.New(rdb, tokenbucket.Config{Capacity: 1_000_000, RefillPerSecond: 1})
	if err != nil {
		t.Fatal(err)
	}
	lb, err := leakybucket.NewMeter(rdb, leakybucket.MeterConfig{Capacity: 1_000_000, LeakPerSecond: 1})
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		l      limiter.Limiter
		prefix string
	}{
		{fw, "rl:fw:"},
		{swl, "rl:swl:"},
		{swc, "rl:swc:"},
		{tb, "rl:tb:"},
		{lb, "rl:lb:"},
	}

	used := map[string]int64{}
	for _, c := range cases {
		used[c.l.Name()] = measure(t, rdb, c.l, c.prefix, requests)
		t.Logf("%-22s %7d bytes", c.l.Name(), used[c.l.Name()])
	}
	t.Logf("(one key, %d requests, none refused)", requests)

	// The log keeps an entry per request; everything else keeps a counter or a
	// two-field hash whose size does not move.
	constant := []string{"fixedwindow", "slidingwindowcounter", "tokenbucket", "leakybucket"}
	for _, name := range constant {
		if used[name] > 1024 {
			t.Errorf("%s used %d bytes for %d requests, want a size that does not grow with traffic",
				name, used[name], requests)
		}
	}
	if smallest := used[constant[0]]; used["slidingwindowlog"] < smallest*100 {
		t.Errorf("slidingwindowlog used %d bytes against %s's %d: the O(limit) cost is missing, "+
			"so the run probably refused most of its requests instead of recording them",
			used["slidingwindowlog"], constant[0], smallest)
	}
}

func measure(t *testing.T, rdb *redis.Client, l limiter.Limiter, prefix string, requests int) int64 {
	t.Helper()
	ctx := context.Background()
	const key = "memprobe"

	if err := l.Reset(ctx, key); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Reset(context.Background(), key) })

	for i := 0; i < requests; i++ {
		d, err := l.AllowN(ctx, key, 1)
		if err != nil {
			t.Fatal(err)
		}
		if !d.Allowed {
			t.Fatalf("%s refused request %d: the limit is too low to measure what a busy key costs",
				l.Name(), i)
		}
	}

	// Across every key the limiter owns: the window counters use one per window,
	// so summing is the only fair comparison against a single sorted set.
	var total int64
	iter := rdb.Scan(ctx, 0, prefix+key+"*", 200).Iterator()
	for iter.Next(ctx) {
		n, err := rdb.MemoryUsage(ctx, iter.Val()).Result()
		if err != nil {
			t.Fatal(err)
		}
		total += n
	}
	if err := iter.Err(); err != nil {
		t.Fatal(err)
	}

	return total
}
