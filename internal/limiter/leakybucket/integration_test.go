//go:build integration

// Runs against a real Redis, started by `make up`.
//
// Only the meter is covered here. The queue is pure Go with no Redis in it at
// all, so its unit tests under -race and goleak are the whole story.
package leakybucket_test

import (
	"context"
	"fmt"
	"math"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/sorenhoang/go-ratelimiter/internal/limiter/leakybucket"
	"github.com/sorenhoang/go-ratelimiter/internal/limiter/tokenbucket"
)

func realRedis(t *testing.T) *redis.Client {
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

	return rdb
}

func realMeter(t *testing.T, cfg leakybucket.MeterConfig) (*redis.Client, *leakybucket.Meter, string) {
	t.Helper()

	rdb := realRedis(t)
	m, err := leakybucket.NewMeter(rdb, cfg)
	if err != nil {
		t.Fatalf("NewMeter: %v", err)
	}

	key := fmt.Sprintf("it-%d", time.Now().UnixNano())
	t.Cleanup(func() { _ = m.Reset(context.Background(), key) })

	return rdb, m, key
}

func storedLevel(t *testing.T, rdb *redis.Client, key string) float64 {
	t.Helper()
	raw, err := rdb.HGet(context.Background(), "rl:lb:"+key, "level").Result()
	if err != nil {
		t.Fatalf("HGET level: %v", err)
	}
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		t.Fatalf("stored level %q does not parse: %v", raw, err)
	}
	return f
}

// The partial unit has to persist on the real server, read out of the hash
// rather than inferred from a verdict. If it rounds, a bucket draining slower
// than one unit per second never empties and the caller is shut out for good.
func TestIntegration_KeepsTheFractionalLevel(t *testing.T) {
	rdb, m, key := realMeter(t, leakybucket.MeterConfig{Capacity: 1, LeakPerSecond: 2})
	ctx := context.Background()

	if d, err := m.AllowN(ctx, key, 1); err != nil || !d.Allowed {
		t.Fatalf("first: %+v err=%v", d, err)
	}

	// A fifth of a second at two per second drains about four tenths: a real
	// fraction, and not enough room for another unit.
	time.Sleep(200 * time.Millisecond)
	if d, err := m.AllowN(ctx, key, 1); err != nil || d.Allowed {
		t.Fatalf("with a partial unit still in the bucket: %+v err=%v; want refused", d, err)
	}

	level := storedLevel(t, rdb, key)
	if level <= 0 || level >= 1 {
		t.Fatalf("stored level = %v, want a fraction between 0 and 1", level)
	}
	t.Logf("real Redis held %v of a unit between calls", level)

	time.Sleep(500 * time.Millisecond)
	if d, err := m.AllowN(ctx, key, 1); err != nil || !d.Allowed {
		t.Fatalf("once it drained: %+v err=%v; want allowed", d, err)
	}
}

// The duality again, on the server that actually runs the scripts. miniredis
// agreed, but the claim is about Redis.
func TestIntegration_MeterMatchesTokenBucket(t *testing.T) {
	const (
		capacity = 4
		rate     = 4.0
	)
	rdb := realRedis(t)
	ctx := context.Background()

	meter, err := leakybucket.NewMeter(rdb, leakybucket.MeterConfig{
		Capacity: capacity, LeakPerSecond: rate,
	})
	if err != nil {
		t.Fatalf("meter: %v", err)
	}
	bucket, err := tokenbucket.New(rdb, tokenbucket.Config{
		Capacity: capacity, RefillPerSecond: rate,
	})
	if err != nil {
		t.Fatalf("bucket: %v", err)
	}

	key := fmt.Sprintf("it-dual-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_ = meter.Reset(ctx, key)
		_ = bucket.Reset(ctx, key)
	})

	steps := []struct {
		wait time.Duration
		cost int64
	}{
		{0, 1}, {0, 1}, {0, 1}, {0, 1},
		{0, 1},
		{300 * time.Millisecond, 1},
		{0, 2},
		{600 * time.Millisecond, 2},
		{0, 4},
	}

	for i, step := range steps {
		if step.wait > 0 {
			time.Sleep(step.wait)
		}

		// Ordered so the pair sees as nearly the same instant as a real clock
		// allows; a few hundred microseconds of drift is why the state check
		// below carries a tolerance.
		lb, err := meter.AllowN(ctx, key, step.cost)
		if err != nil {
			t.Fatalf("step %d, meter: %v", i, err)
		}
		tb, err := bucket.AllowN(ctx, key, step.cost)
		if err != nil {
			t.Fatalf("step %d, bucket: %v", i, err)
		}

		if lb.Allowed != tb.Allowed {
			t.Errorf("step %d (wait %s, cost %d): meter allowed=%v, bucket allowed=%v",
				i, step.wait, step.cost, lb.Allowed, tb.Allowed)
		}

		water := storedLevel(t, rdb, key)
		raw, err := rdb.HGet(ctx, "rl:tb:"+key, "tokens").Result()
		if err != nil {
			t.Fatalf("step %d, HGET tokens: %v", i, err)
		}
		tokens, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			t.Fatalf("step %d, tokens %q: %v", i, raw, err)
		}

		// Tolerance rather than exactness: unlike the unit test's frozen clock,
		// here the two scripts run a moment apart and each leaks or refills over
		// its own elapsed time.
		if sum := water + tokens; math.Abs(sum-capacity) > 0.05 {
			t.Errorf("step %d: level %v + tokens %v = %v, want the capacity %d",
				i, water, tokens, sum, capacity)
		}
	}
}

func TestIntegration_KeyLivesAsLongAsAFullDrain(t *testing.T) {
	const capacity, rate = 4, 2.0
	rdb, m, key := realMeter(t, leakybucket.MeterConfig{Capacity: capacity, LeakPerSecond: rate})
	ctx := context.Background()

	if d, err := m.AllowN(ctx, key, 1); err != nil || !d.Allowed {
		t.Fatalf("first: %+v err=%v", d, err)
	}

	ttl, err := rdb.PTTL(ctx, "rl:lb:"+key).Result()
	if err != nil {
		t.Fatalf("PTTL: %v", err)
	}

	want := time.Duration(float64(capacity)/rate*1000) * time.Millisecond
	if ttl <= 0 || ttl > want {
		t.Errorf("TTL = %s, want up to %s", ttl, want)
	}
	if ttl < want/2 {
		t.Errorf("TTL = %s, want close to %s", ttl, want)
	}
}

func TestIntegration_CostAndReset(t *testing.T) {
	_, m, key := realMeter(t, leakybucket.MeterConfig{Capacity: 5, LeakPerSecond: 1})
	ctx := context.Background()

	if d, err := m.AllowN(ctx, key, 4); err != nil || !d.Allowed || d.Remaining != 1 {
		t.Fatalf("cost 4 of 5: %+v err=%v; want allowed with 1 of headroom", d, err)
	}
	if d, err := m.AllowN(ctx, key, 4); err != nil || d.Allowed {
		t.Fatalf("cost 4 with 1 of headroom: %+v err=%v; want refused", d, err)
	}

	if err := m.Reset(ctx, key); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if d, err := m.AllowN(ctx, key, 4); err != nil || !d.Allowed || d.Remaining != 1 {
		t.Fatalf("after Reset: %+v err=%v; want an empty bucket", d, err)
	}
}
