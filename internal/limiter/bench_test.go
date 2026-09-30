//go:build integration

// Benchmarks for every limiter, in one file so the numbers are comparable.
//
// They run against real Redis, started by `make up`. A figure from miniredis
// would describe the test double -- it interprets Lua in Go -- rather than the
// thing anyone is choosing between.
//
// Read the differences, not the absolutes. A local round trip is most of each
// number, so the absolute figure says more about Redis and the loopback than
// about any algorithm; BenchmarkBaseline measures that floor so the rest can be
// read against it.
//
// The queue is absent on purpose. Benchmarking Wait would measure the rate it
// was configured with, which is a setting rather than a cost.
package limiter_test

import (
	"context"
	"fmt"
	"os"
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

// Large enough that nothing is refused for the length of a run. A limiter that
// starts refusing takes a cheaper path -- no write -- and would flatter itself.
const benchLimit = 1_000_000

func benchRedis(b *testing.B) *redis.Client {
	b.Helper()
	return dialRedis(b)
}

func realRedisForMemory(t *testing.T) *redis.Client {
	t.Helper()
	return dialRedis(t)
}

// dialRedis serves both, since testing.TB is all either needs.
func dialRedis(tb testing.TB) *redis.Client {
	tb.Helper()

	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}

	rdb := redis.NewClient(&redis.Options{Addr: addr})
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		tb.Fatalf("no Redis at %s (run `make up`): %v", addr, err)
	}
	tb.Cleanup(func() { _ = rdb.Close() })

	return rdb
}

// BenchmarkBaseline is a round trip that decides nothing: the floor every
// figure below sits on.
func BenchmarkBaseline(b *testing.B) {
	rdb := benchRedis(b)
	ctx := context.Background()

	b.ReportAllocs()
	for b.Loop() {
		if err := rdb.Ping(ctx).Err(); err != nil {
			b.Fatal(err)
		}
	}
}

func benchLimiter(b *testing.B, l limiter.Limiter) {
	b.Helper()
	ctx := context.Background()

	// One key for the whole run. A hot key is the case worth measuring, and for
	// the sliding window log it is the only way to reach the cost that defines
	// it: its set has to actually grow.
	key := fmt.Sprintf("bench-%d", time.Now().UnixNano())
	b.Cleanup(func() { _ = l.Reset(context.Background(), key) })

	b.ReportAllocs()
	for b.Loop() {
		d, err := l.AllowN(ctx, key, 1)
		if err != nil {
			b.Fatal(err)
		}
		if !d.Allowed {
			b.Fatal("refused during a benchmark: the limit is too low to measure the write path")
		}
	}
}

func BenchmarkFixedWindow(b *testing.B) {
	l, err := fixedwindow.New(benchRedis(b), fixedwindow.Config{
		Limit: benchLimit, Window: time.Hour,
	})
	if err != nil {
		b.Fatal(err)
	}
	benchLimiter(b, l)
}

func BenchmarkSlidingWindowLog(b *testing.B) {
	l, err := slidingwindowlog.New(benchRedis(b), slidingwindowlog.Config{
		Limit: benchLimit, Window: time.Hour,
	})
	if err != nil {
		b.Fatal(err)
	}
	benchLimiter(b, l)
}

func BenchmarkSlidingWindowCounter(b *testing.B) {
	l, err := slidingwindowcounter.New(benchRedis(b), slidingwindowcounter.Config{
		Limit: benchLimit, Window: time.Hour,
	})
	if err != nil {
		b.Fatal(err)
	}
	benchLimiter(b, l)
}

func BenchmarkTokenBucket(b *testing.B) {
	l, err := tokenbucket.New(benchRedis(b), tokenbucket.Config{
		Capacity: benchLimit, RefillPerSecond: 1,
	})
	if err != nil {
		b.Fatal(err)
	}
	benchLimiter(b, l)
}

func BenchmarkLeakyBucketMeter(b *testing.B) {
	l, err := leakybucket.NewMeter(benchRedis(b), leakybucket.MeterConfig{
		Capacity: benchLimit, LeakPerSecond: 1,
	})
	if err != nil {
		b.Fatal(err)
	}
	benchLimiter(b, l)
}
