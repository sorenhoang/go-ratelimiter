//go:build integration

// Runs against a real Redis, started by `make up`.
//
// This is the phase whose state is fractional, and miniredis interprets Lua in
// Go rather than running Redis' own. Both were probed and agreed, but agreement
// on a probe is not a guarantee across versions, so the partial token is read
// straight out of the hash here rather than inferred from a verdict.
package tokenbucket_test

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/sorenhoang/go-ratelimiter/internal/limiter/tokenbucket"
)

func realLimiter(t *testing.T, cfg tokenbucket.Config) (*redis.Client, *tokenbucket.Limiter, string) {
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

	l, err := tokenbucket.New(rdb, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	key := fmt.Sprintf("it-%d", time.Now().UnixNano())
	t.Cleanup(func() { _ = l.Reset(context.Background(), key) })

	return rdb, l, key
}

// storedTokens reads the partial token straight out of the hash, which is the
// one thing a verdict cannot tell you.
func storedTokens(t *testing.T, rdb *redis.Client, key string) float64 {
	t.Helper()
	raw, err := rdb.HGet(context.Background(), "rl:tb:"+key, "tokens").Result()
	if err != nil {
		t.Fatalf("HGET tokens: %v", err)
	}
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		t.Fatalf("stored tokens %q does not parse as a number: %v", raw, err)
	}
	return f
}

// The defining case: real Redis has to keep the partial token between calls. If
// it rounds anywhere, a bucket refilling slower than one token per second never
// fills at all and the caller is locked out forever.
func TestIntegration_KeepsTheFractionalToken(t *testing.T) {
	rdb, l, key := realLimiter(t, tokenbucket.Config{Capacity: 1, RefillPerSecond: 2})
	ctx := context.Background()

	if d, err := l.AllowN(ctx, key, 1); err != nil || !d.Allowed {
		t.Fatalf("first: %+v err=%v", d, err)
	}

	// A fifth of a second at two per second is roughly four tenths of a token:
	// a real fraction, and not enough to pay for anything.
	time.Sleep(200 * time.Millisecond)
	if d, err := l.AllowN(ctx, key, 1); err != nil || d.Allowed {
		t.Fatalf("on a partial token: %+v err=%v; want denied", d, err)
	}

	tokens := storedTokens(t, rdb, key)
	if tokens <= 0 || tokens >= 1 {
		t.Fatalf("stored tokens = %v, want a fraction between 0 and 1", tokens)
	}
	if tokens == float64(int64(tokens)) {
		t.Errorf("stored tokens = %v is a whole number: the fraction was rounded away", tokens)
	}
	t.Logf("real Redis held %v of a token between calls", tokens)

	// Enough time for the rest of it.
	time.Sleep(500 * time.Millisecond)
	if d, err := l.AllowN(ctx, key, 1); err != nil || !d.Allowed {
		t.Fatalf("once the token completed: %+v err=%v; want allowed", d, err)
	}
}

func TestIntegration_BurstsThenSettlesToTheRate(t *testing.T) {
	const capacity = 4
	_, l, key := realLimiter(t, tokenbucket.Config{Capacity: capacity, RefillPerSecond: 2})
	ctx := context.Background()

	for i := 1; i <= capacity; i++ {
		d, err := l.AllowN(ctx, key, 1)
		if err != nil {
			t.Fatalf("burst %d: %v", i, err)
		}
		if !d.Allowed {
			t.Fatalf("burst %d denied, want the whole capacity at once", i)
		}
	}

	d, err := l.AllowN(ctx, key, 1)
	if err != nil {
		t.Fatalf("past the capacity: %v", err)
	}
	if d.Allowed {
		t.Fatal("allowed past the capacity, want denied")
	}
	// One token at two per second is half a second, give or take the time the
	// burst itself took.
	if d.RetryAfter <= 0 || d.RetryAfter > 600*time.Millisecond {
		t.Errorf("RetryAfter = %s, want about 500ms", d.RetryAfter)
	}

	// After the wait, one token — not the whole capacity back.
	time.Sleep(600 * time.Millisecond)
	if d, err := l.AllowN(ctx, key, 1); err != nil || !d.Allowed {
		t.Fatalf("after the refill: %+v err=%v; want allowed", d, err)
	}
	if d, err := l.AllowN(ctx, key, 1); err != nil || d.Allowed {
		t.Fatalf("second request after one token refilled: %+v err=%v; want denied", d, err)
	}
}

func TestIntegration_KeyLivesAsLongAsAFullRefill(t *testing.T) {
	const capacity, rate = 4, 2.0
	rdb, l, key := realLimiter(t, tokenbucket.Config{Capacity: capacity, RefillPerSecond: rate})
	ctx := context.Background()

	if d, err := l.AllowN(ctx, key, 1); err != nil || !d.Allowed {
		t.Fatalf("first: %+v err=%v", d, err)
	}

	ttl, err := rdb.PTTL(ctx, "rl:tb:"+key).Result()
	if err != nil {
		t.Fatalf("PTTL: %v", err)
	}

	// capacity/rate is how long an empty bucket takes to fill. Past that the key
	// is worth nothing: a caller arriving later meets a full bucket either way.
	want := time.Duration(float64(capacity)/rate*1000) * time.Millisecond
	if ttl <= 0 || ttl > want {
		t.Errorf("TTL = %s, want up to %s", ttl, want)
	}
	if ttl < want/2 {
		t.Errorf("TTL = %s, want close to %s", ttl, want)
	}
}

func TestIntegration_CostAndReset(t *testing.T) {
	_, l, key := realLimiter(t, tokenbucket.Config{Capacity: 5, RefillPerSecond: 1})
	ctx := context.Background()

	if d, err := l.AllowN(ctx, key, 4); err != nil || !d.Allowed || d.Remaining != 1 {
		t.Fatalf("cost 4 of 5: %+v err=%v; want allowed with 1 left", d, err)
	}
	if d, err := l.AllowN(ctx, key, 4); err != nil || d.Allowed {
		t.Fatalf("cost 4 with 1 token: %+v err=%v; want denied", d, err)
	}

	if err := l.Reset(ctx, key); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if d, err := l.AllowN(ctx, key, 4); err != nil || !d.Allowed || d.Remaining != 1 {
		t.Fatalf("after Reset: %+v err=%v; want a full bucket", d, err)
	}
}
