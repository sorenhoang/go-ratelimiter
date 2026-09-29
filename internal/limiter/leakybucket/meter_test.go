package leakybucket_test

import (
	"context"
	"errors"
	"math"
	"strconv"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/sorenhoang/go-ratelimiter/internal/limiter"
	"github.com/sorenhoang/go-ratelimiter/internal/limiter/leakybucket"
)

var baseTime = time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)

func newTestMeter(t *testing.T, cfg leakybucket.MeterConfig) (*miniredis.Miniredis, *leakybucket.Meter, func(time.Duration)) {
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
	m, err := leakybucket.NewMeter(rdb, cfg)
	if err != nil {
		t.Fatalf("NewMeter: %v", err)
	}

	elapsed := time.Duration(0)
	advance := func(d time.Duration) {
		elapsed += d
		s.SetTime(baseTime.Add(elapsed))
		s.FastForward(d)
	}

	return s, m, advance
}

func pour(t *testing.T, m *leakybucket.Meter) limiter.Decision {
	t.Helper()
	d, err := m.AllowN(context.Background(), "user", 1)
	if err != nil {
		t.Fatalf("AllowN: %v", err)
	}
	return d
}

func level(t *testing.T, s *miniredis.Miniredis) float64 {
	t.Helper()
	raw := s.HGet("rl:lb:user", "level")
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		t.Fatalf("stored level %q does not parse: %v", raw, err)
	}
	return f
}

// A bucket never seen is empty, which is the mirror of phase 04 where a missing
// key meant a full bucket of tokens.
func TestAllowN_FirstRequestMeetsAnEmptyBucket(t *testing.T) {
	_, m, _ := newTestMeter(t, leakybucket.MeterConfig{Capacity: 3, LeakPerSecond: 1})

	d := pour(t, m)
	if !d.Allowed {
		t.Fatal("first ever request denied")
	}
	if d.Remaining != 2 {
		t.Errorf("Remaining = %d, want 2 of headroom", d.Remaining)
	}
	if d.Limit != 3 {
		t.Errorf("Limit = %d, want the capacity 3", d.Limit)
	}
}

func TestAllowN_RefusesOverflow(t *testing.T) {
	const capacity = 5
	_, m, _ := newTestMeter(t, leakybucket.MeterConfig{Capacity: capacity, LeakPerSecond: 1})

	for i := 1; i <= capacity; i++ {
		d := pour(t, m)
		if !d.Allowed {
			t.Fatalf("pour %d refused, want allowed", i)
		}
		if want := int64(capacity - i); d.Remaining != want {
			t.Errorf("pour %d: Remaining = %d, want %d", i, d.Remaining, want)
		}
	}

	d := pour(t, m)
	if d.Allowed {
		t.Error("overflow allowed, want refused")
	}
	// One unit over at one per second is exactly one second of draining.
	if want := time.Second; d.RetryAfter != want {
		t.Errorf("RetryAfter = %s, want %s", d.RetryAfter, want)
	}
}

func TestAllowN_DrainsAtTheConfiguredRate(t *testing.T) {
	const capacity = 5
	_, m, advance := newTestMeter(t, leakybucket.MeterConfig{Capacity: capacity, LeakPerSecond: 1})

	for i := 1; i <= capacity; i++ {
		pour(t, m)
	}
	if pour(t, m).Allowed {
		t.Fatal("bucket should be full")
	}

	// Three seconds of draining makes exactly three units of room.
	advance(3 * time.Second)
	for i := 1; i <= 3; i++ {
		if d := pour(t, m); !d.Allowed {
			t.Fatalf("after draining, pour %d refused", i)
		}
	}
	if pour(t, m).Allowed {
		t.Error("fourth pour after three seconds of draining allowed, want refused")
	}
}

// The level floors at empty. Reaching that floor needs the key to still be
// alive, and the TTL is exactly a full drain, so it has to start part full and
// wait less than that.
func TestAllowN_LevelDoesNotDrainBelowEmpty(t *testing.T) {
	const capacity = 5
	s, m, advance := newTestMeter(t, leakybucket.MeterConfig{Capacity: capacity, LeakPerSecond: 1})

	pour(t, m)
	pour(t, m)

	// Four seconds of draining against two units in the bucket: empty, not
	// minus two. The key still has a second of TTL left.
	advance(4 * time.Second)
	pour(t, m)

	if got := level(t, s); got != 1 {
		t.Errorf("stored level = %v after draining past empty and pouring one, want 1", got)
	}
	for i := 1; i < capacity; i++ {
		if d := pour(t, m); !d.Allowed {
			t.Fatalf("pour %d into a drained bucket refused", i)
		}
	}
	if pour(t, m).Allowed {
		t.Error("the bucket took more than its capacity, so the level went negative")
	}
}

// Half a unit per second means a whole second drains half a unit, and the
// partial unit has to persist between calls.
func TestAllowN_KeepsTheFractionalLevel(t *testing.T) {
	s, m, advance := newTestMeter(t, leakybucket.MeterConfig{Capacity: 1, LeakPerSecond: 0.5})

	if !pour(t, m).Allowed {
		t.Fatal("first pour refused")
	}

	advance(time.Second)
	if pour(t, m).Allowed {
		t.Error("allowed with half a unit still in the bucket, want refused")
	}
	if got := level(t, s); got != 0.5 {
		t.Errorf("stored level = %v, want 0.5 — the fraction was lost", got)
	}

	advance(time.Second)
	if !pour(t, m).Allowed {
		t.Error("refused after the bucket finished draining, want allowed")
	}
}

func TestAllowN_CostAboveHeadroomIsRefusedNotClamped(t *testing.T) {
	_, m, _ := newTestMeter(t, leakybucket.MeterConfig{Capacity: 5, LeakPerSecond: 1})
	ctx := context.Background()

	d, err := m.AllowN(ctx, "user", 3)
	if err != nil {
		t.Fatalf("AllowN: %v", err)
	}
	if !d.Allowed || d.Remaining != 2 {
		t.Fatalf("cost 3 of 5: Allowed = %v, Remaining = %d; want true, 2", d.Allowed, d.Remaining)
	}

	d, err = m.AllowN(ctx, "user", 3)
	if err != nil {
		t.Fatalf("AllowN: %v", err)
	}
	if d.Allowed {
		t.Error("cost 3 with 2 of headroom allowed, want refused")
	}
	if d.Remaining != 2 {
		t.Errorf("Remaining = %d after a refusal, want 2 untouched", d.Remaining)
	}

	d, err = m.AllowN(ctx, "user", 2)
	if err != nil {
		t.Fatalf("AllowN: %v", err)
	}
	if !d.Allowed || d.Remaining != 0 {
		t.Errorf("cost 2 with 2 of headroom: Allowed = %v, Remaining = %d; want true, 0", d.Allowed, d.Remaining)
	}
}

func TestNewMeter_RejectsUnusableConfig(t *testing.T) {
	tests := []struct {
		name string
		cfg  leakybucket.MeterConfig
	}{
		{"zero capacity", leakybucket.MeterConfig{Capacity: 0, LeakPerSecond: 1}},
		{"negative capacity", leakybucket.MeterConfig{Capacity: -1, LeakPerSecond: 1}},
		{"zero leak", leakybucket.MeterConfig{Capacity: 1, LeakPerSecond: 0}},
		{"negative leak", leakybucket.MeterConfig{Capacity: 1, LeakPerSecond: -1}},
		// NaN fails every comparison, so a plain bounds check waves it through.
		{"NaN leak", leakybucket.MeterConfig{Capacity: 1, LeakPerSecond: math.NaN()}},
		{"infinite leak", leakybucket.MeterConfig{Capacity: 1, LeakPerSecond: math.Inf(1)}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := leakybucket.NewMeter(nil, tc.cfg); err == nil {
				t.Errorf("NewMeter(%+v) succeeded, want an error", tc.cfg)
			}
		})
	}
}

func TestAllowN_ReturnsBackendUnavailableWhenRedisIsDown(t *testing.T) {
	s, m, _ := newTestMeter(t, leakybucket.MeterConfig{Capacity: 5, LeakPerSecond: 1})

	if !pour(t, m).Allowed {
		t.Fatal("refused before the outage")
	}

	s.Close()

	_, err := m.AllowN(context.Background(), "user", 1)
	if err == nil {
		t.Fatal("no error with Redis down")
	}
	if !errors.Is(err, limiter.ErrBackendUnavailable) {
		t.Errorf("err = %v, want it to wrap ErrBackendUnavailable", err)
	}
}

func TestReset_ReturnsBackendUnavailableWhenRedisIsDown(t *testing.T) {
	s, m, _ := newTestMeter(t, leakybucket.MeterConfig{Capacity: 5, LeakPerSecond: 1})
	ctx := context.Background()

	if err := m.Reset(ctx, "user"); err != nil {
		t.Fatalf("Reset while up: %v", err)
	}

	s.Close()

	err := m.Reset(ctx, "user")
	if err == nil {
		t.Fatal("no error with Redis down")
	}
	if !errors.Is(err, limiter.ErrBackendUnavailable) {
		t.Errorf("err = %v, want it to wrap ErrBackendUnavailable", err)
	}
}

func TestReset_EmptiesTheBucket(t *testing.T) {
	_, m, _ := newTestMeter(t, leakybucket.MeterConfig{Capacity: 2, LeakPerSecond: 1})
	ctx := context.Background()

	pour(t, m)
	pour(t, m)
	if pour(t, m).Allowed {
		t.Fatal("bucket should be full")
	}

	if err := m.Reset(ctx, "user"); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	if d := pour(t, m); !d.Allowed || d.Remaining != 1 {
		t.Errorf("after reset: Allowed = %v, Remaining = %d; want true, 1", d.Allowed, d.Remaining)
	}
}
