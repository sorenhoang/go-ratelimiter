package leakybucket_test

import (
	"context"
	"math"
	"strconv"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/sorenhoang/go-ratelimiter/internal/limiter/leakybucket"
	"github.com/sorenhoang/go-ratelimiter/internal/limiter/tokenbucket"
)

// TestDuality_MeterMatchesTokenBucket states the relationship outright instead
// of leaving it to be believed.
//
// A metered leaky bucket and a token bucket at the same capacity and rate are
// the same algorithm: level = capacity - tokens. Every comparison maps across,
// so for any request stream the two must reach the same verdict at every step,
// and their stored state must always sum to the capacity.
//
// It is worth a test because the claim is easy to accept and easy to break. One
// boundary written `<` instead of `<=` on either side separates them by a single
// request, exactly at the full mark, where nothing else would notice.
func TestDuality_MeterMatchesTokenBucket(t *testing.T) {
	const (
		capacity = 4
		rate     = 2.0
	)

	s, err := miniredis.Run()
	if err != nil {
		t.Fatalf("start miniredis: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	s.SetTime(baseTime)

	rdb := redis.NewClient(&redis.Options{Addr: s.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
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

	// Fixed steps rather than random ones: this asserts an identity, so the
	// sequence only has to cover the interesting shapes — filling, overflowing,
	// draining part way, a cost above the headroom, and a long idle stretch.
	steps := []struct {
		wait time.Duration
		cost int64
	}{
		{0, 1}, {0, 1}, {0, 1}, {0, 1}, // fill it exactly
		{0, 1},                      // overflow
		{500 * time.Millisecond, 1}, // one unit of room
		{0, 1},                      // and no more
		{time.Second, 2},            // two units of room, spend both
		{0, 1},                      // full again
		{250 * time.Millisecond, 1}, // half a unit: not enough
		{250 * time.Millisecond, 1}, // now it is
		{time.Second, 4},            // a cost that needs the whole bucket
		{2 * time.Second, 3},        // drained, a cost under capacity
		{0, 4},                      // more than the headroom
	}

	elapsed := time.Duration(0)
	for i, step := range steps {
		if step.wait > 0 {
			elapsed += step.wait
			s.SetTime(baseTime.Add(elapsed))
			s.FastForward(step.wait)
		}

		lb, err := meter.AllowN(ctx, "user", step.cost)
		if err != nil {
			t.Fatalf("step %d, meter: %v", i, err)
		}
		tb, err := bucket.AllowN(ctx, "user", step.cost)
		if err != nil {
			t.Fatalf("step %d, bucket: %v", i, err)
		}

		if lb.Allowed != tb.Allowed {
			t.Errorf("step %d (wait %s, cost %d): meter allowed=%v, bucket allowed=%v",
				i, step.wait, step.cost, lb.Allowed, tb.Allowed)
		}
		if lb.Remaining != tb.Remaining {
			t.Errorf("step %d: meter Remaining=%d, bucket Remaining=%d",
				i, lb.Remaining, tb.Remaining)
		}
		if lb.RetryAfter != tb.RetryAfter {
			t.Errorf("step %d: meter RetryAfter=%s, bucket RetryAfter=%s",
				i, lb.RetryAfter, tb.RetryAfter)
		}

		// The identity itself, checked on the stored state rather than inferred
		// from the verdicts agreeing.
		water := parse(t, s.HGet("rl:lb:user", "level"))
		tokens := parse(t, s.HGet("rl:tb:user", "tokens"))
		if sum := water + tokens; math.Abs(sum-capacity) > 1e-9 {
			t.Errorf("step %d: level %v + tokens %v = %v, want the capacity %d",
				i, water, tokens, sum, capacity)
		}
	}
}

func parse(t *testing.T, raw string) float64 {
	t.Helper()
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		t.Fatalf("stored value %q does not parse: %v", raw, err)
	}
	return f
}
