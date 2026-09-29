package tokenbucket_test

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/sorenhoang/go-ratelimiter/internal/limiter"
	"github.com/sorenhoang/go-ratelimiter/internal/limiter/tokenbucket"
)

var baseTime = time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)

func newTestLimiter(t *testing.T, cfg tokenbucket.Config) (*miniredis.Miniredis, *tokenbucket.Limiter, func(time.Duration)) {
	t.Helper()

	s, err := miniredis.Run()
	if err != nil {
		t.Fatalf("start miniredis: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	rdb := redis.NewClient(&redis.Options{
		Addr: s.Addr(),
		// -1 disables retries. The backend-down tests kill miniredis on purpose,
		// so waiting out go-redis' retry backoff is pure dead time.
		MaxRetries: -1,
	})

	s.SetTime(baseTime)
	l, err := tokenbucket.New(rdb, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	elapsed := time.Duration(0)
	advance := func(d time.Duration) {
		elapsed += d
		s.SetTime(baseTime.Add(elapsed))
		s.FastForward(d)
	}

	return s, l, advance
}

func allow(t *testing.T, l *tokenbucket.Limiter) limiter.Decision {
	t.Helper()
	d, err := l.AllowN(context.Background(), "user", 1)
	if err != nil {
		t.Fatalf("AllowN: %v", err)
	}
	return d
}

// The whole capacity is spendable at once. No other limiter here does that on
// purpose, and it is why a client that has been idle is not punished for
// arriving with a backlog.
func TestAllowN_SpendsTheWholeCapacityAtOnce(t *testing.T) {
	const capacity = 5
	_, l, _ := newTestLimiter(t, tokenbucket.Config{Capacity: capacity, RefillPerSecond: 1})

	for i := 1; i <= capacity; i++ {
		d := allow(t, l)
		if !d.Allowed {
			t.Fatalf("burst %d denied, want allowed", i)
		}
		if want := int64(capacity - i); d.Remaining != want {
			t.Errorf("burst %d: Remaining = %d, want %d", i, d.Remaining, want)
		}
	}

	d := allow(t, l)
	if d.Allowed {
		t.Error("request past the capacity allowed, want denied")
	}
	// One token short at one per second is exactly one second.
	if want := time.Second; d.RetryAfter != want {
		t.Errorf("RetryAfter = %s, want %s", d.RetryAfter, want)
	}
}

// A caller that has never been seen meets a full bucket. Reading the missing
// hash as zero instead would refuse everybody's first ever request.
func TestAllowN_FirstRequestMeetsAFullBucket(t *testing.T) {
	_, l, _ := newTestLimiter(t, tokenbucket.Config{Capacity: 3, RefillPerSecond: 1})

	d := allow(t, l)
	if !d.Allowed {
		t.Fatal("first ever request denied")
	}
	if d.Remaining != 2 {
		t.Errorf("Remaining = %d, want 2", d.Remaining)
	}
	if d.Limit != 3 {
		t.Errorf("Limit = %d, want the capacity 3", d.Limit)
	}
}

func TestAllowN_RefillsAtTheConfiguredRate(t *testing.T) {
	const capacity = 5
	_, l, advance := newTestLimiter(t, tokenbucket.Config{Capacity: capacity, RefillPerSecond: 1})

	for i := 1; i <= capacity; i++ {
		allow(t, l)
	}
	if allow(t, l).Allowed {
		t.Fatal("bucket should be empty")
	}

	// Three seconds at one per second buys exactly three, no more.
	advance(3 * time.Second)
	for i := 1; i <= 3; i++ {
		if d := allow(t, l); !d.Allowed {
			t.Fatalf("after refill, request %d denied", i)
		}
	}
	if allow(t, l).Allowed {
		t.Error("fourth request after a three second refill allowed, want denied")
	}
}

// The refill stops at the capacity.
//
// Reaching that ceiling takes care: the TTL is exactly the time to refill from
// empty, so a bucket left to fill for longer than that has expired and the next
// caller meets a fresh full one through a different branch entirely. The ceiling
// is only observable while the key is still alive, which means starting with
// tokens in hand and waiting less than a full refill.
func TestAllowN_DoesNotFillPastCapacity(t *testing.T) {
	const capacity = 5
	_, l, advance := newTestLimiter(t, tokenbucket.Config{Capacity: capacity, RefillPerSecond: 1})

	// Spend two, leaving three and a five second TTL.
	allow(t, l)
	allow(t, l)

	// Four seconds buys four tokens against three in hand: seven without a
	// ceiling, five with one. The key still has a second of TTL left.
	advance(4 * time.Second)

	for i := 1; i <= capacity; i++ {
		if d := allow(t, l); !d.Allowed {
			t.Fatalf("request %d after the refill denied, want allowed", i)
		}
	}
	if d := allow(t, l); d.Allowed {
		t.Errorf("request %d allowed: the refill went past the capacity of %d",
			capacity+1, capacity)
	}
}

// The defining test of this phase. At half a token per second a whole second
// buys half a token, which cannot pay for a request costing one. Round that
// fraction away anywhere along the path and the bucket never fills at all.
func TestAllowN_KeepsTheFractionalToken(t *testing.T) {
	s, l, advance := newTestLimiter(t, tokenbucket.Config{Capacity: 1, RefillPerSecond: 0.5})

	if !allow(t, l).Allowed {
		t.Fatal("first request denied")
	}

	advance(time.Second)
	if allow(t, l).Allowed {
		t.Error("allowed on half a token, want denied")
	}
	if got := s.HGet("rl:tb:user", "tokens"); got != "0.5" {
		t.Errorf("stored tokens = %q, want %q — the fraction was lost", got, "0.5")
	}

	advance(time.Second)
	if !allow(t, l).Allowed {
		t.Error("denied after the second half arrived, want allowed")
	}
}

func TestAllowN_CostAboveTheBalanceIsRejectedNotClamped(t *testing.T) {
	_, l, _ := newTestLimiter(t, tokenbucket.Config{Capacity: 5, RefillPerSecond: 1})
	ctx := context.Background()

	d, err := l.AllowN(ctx, "user", 3)
	if err != nil {
		t.Fatalf("AllowN: %v", err)
	}
	if !d.Allowed || d.Remaining != 2 {
		t.Fatalf("cost 3 of 5: Allowed = %v, Remaining = %d; want true, 2", d.Allowed, d.Remaining)
	}

	d, err = l.AllowN(ctx, "user", 3)
	if err != nil {
		t.Fatalf("AllowN: %v", err)
	}
	if d.Allowed {
		t.Error("cost 3 with 2 tokens was allowed, want denied")
	}
	if d.Remaining != 2 {
		t.Errorf("Remaining = %d after a denial, want 2 untouched", d.Remaining)
	}
	// One token short of three, at one per second.
	if want := time.Second; d.RetryAfter != want {
		t.Errorf("RetryAfter = %s, want %s", d.RetryAfter, want)
	}

	d, err = l.AllowN(ctx, "user", 2)
	if err != nil {
		t.Fatalf("AllowN: %v", err)
	}
	if !d.Allowed || d.Remaining != 0 {
		t.Errorf("cost 2 with 2 tokens: Allowed = %v, Remaining = %d; want true, 0", d.Allowed, d.Remaining)
	}
}

func TestNew_RejectsUnusableConfig(t *testing.T) {
	tests := []struct {
		name string
		cfg  tokenbucket.Config
	}{
		{"zero capacity", tokenbucket.Config{Capacity: 0, RefillPerSecond: 1}},
		{"negative capacity", tokenbucket.Config{Capacity: -1, RefillPerSecond: 1}},
		{"zero rate", tokenbucket.Config{Capacity: 1, RefillPerSecond: 0}},
		{"negative rate", tokenbucket.Config{Capacity: 1, RefillPerSecond: -1}},
		// NaN fails every comparison, so a plain `<= 0` check waves it through
		// and the script then computes NaN tokens without erroring.
		{"NaN rate", tokenbucket.Config{Capacity: 1, RefillPerSecond: math.NaN()}},
		{"infinite rate", tokenbucket.Config{Capacity: 1, RefillPerSecond: math.Inf(1)}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tokenbucket.New(nil, tc.cfg); err == nil {
				t.Errorf("New(%+v) succeeded, want an error", tc.cfg)
			}
		})
	}
}

func TestAllowN_ReturnsBackendUnavailableWhenRedisIsDown(t *testing.T) {
	s, l, _ := newTestLimiter(t, tokenbucket.Config{Capacity: 5, RefillPerSecond: 1})

	if !allow(t, l).Allowed {
		t.Fatal("denied before the outage")
	}

	s.Close()

	_, err := l.AllowN(context.Background(), "user", 1)
	if err == nil {
		t.Fatal("no error with Redis down")
	}
	if !errors.Is(err, limiter.ErrBackendUnavailable) {
		t.Errorf("err = %v, want it to wrap ErrBackendUnavailable", err)
	}
}

func TestReset_ReturnsBackendUnavailableWhenRedisIsDown(t *testing.T) {
	s, l, _ := newTestLimiter(t, tokenbucket.Config{Capacity: 5, RefillPerSecond: 1})
	ctx := context.Background()

	if err := l.Reset(ctx, "user"); err != nil {
		t.Fatalf("Reset while up: %v", err)
	}

	s.Close()

	err := l.Reset(ctx, "user")
	if err == nil {
		t.Fatal("no error with Redis down")
	}
	if !errors.Is(err, limiter.ErrBackendUnavailable) {
		t.Errorf("err = %v, want it to wrap ErrBackendUnavailable", err)
	}
}

func TestReset_RefillsTheBucket(t *testing.T) {
	_, l, _ := newTestLimiter(t, tokenbucket.Config{Capacity: 2, RefillPerSecond: 1})
	ctx := context.Background()

	allow(t, l)
	allow(t, l)
	if allow(t, l).Allowed {
		t.Fatal("bucket should be empty")
	}

	if err := l.Reset(ctx, "user"); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	if d := allow(t, l); !d.Allowed || d.Remaining != 1 {
		t.Errorf("after reset: Allowed = %v, Remaining = %d; want true, 1", d.Allowed, d.Remaining)
	}
}
