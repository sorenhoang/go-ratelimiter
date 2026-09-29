package slidingwindowcounter_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/sorenhoang/go-ratelimiter/internal/limiter"
	"github.com/sorenhoang/go-ratelimiter/internal/limiter/slidingwindowcounter"
)

// baseTime sits on a whole minute, so a one minute window starts exactly on a
// boundary and the weights below are exact rather than "about".
var baseTime = time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)

func newTestLimiter(t *testing.T, cfg slidingwindowcounter.Config) (*miniredis.Miniredis, *slidingwindowcounter.Limiter, func(time.Duration)) {
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
	l, err := slidingwindowcounter.New(rdb, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// SetTime takes an absolute moment and FastForward a relative step, and
	// neither does the other's job: FastForward expires keys without moving the
	// clock that TIME reports.
	elapsed := time.Duration(0)
	advance := func(d time.Duration) {
		elapsed += d
		s.SetTime(baseTime.Add(elapsed))
		s.FastForward(d)
	}

	return s, l, advance
}

// seedPreviousWindow writes a count straight into the window before baseTime,
// which is the only way to set up a previous count larger than the limit itself.
func seedPreviousWindow(t *testing.T, s *miniredis.Miniredis, key string, window time.Duration, count int) {
	t.Helper()
	start := baseTime.UnixMilli() - window.Milliseconds()
	if err := s.Set(fmt.Sprintf("rl:swc:%s:%d", key, start), fmt.Sprint(count)); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func allow(t *testing.T, l *slidingwindowcounter.Limiter) limiter.Decision {
	t.Helper()
	d, err := l.AllowN(context.Background(), "user", 1)
	if err != nil {
		t.Fatalf("AllowN: %v", err)
	}
	return d
}

func TestAllowN_ExhaustsLimit(t *testing.T) {
	_, l, _ := newTestLimiter(t, slidingwindowcounter.Config{Limit: 5, Window: time.Minute})

	for i := 1; i <= 5; i++ {
		d := allow(t, l)
		if !d.Allowed {
			t.Fatalf("call %d denied, want allowed", i)
		}
		if want := int64(5 - i); d.Remaining != want {
			t.Errorf("call %d: Remaining = %d, want %d", i, d.Remaining, want)
		}
	}

	d := allow(t, l)
	if d.Allowed {
		t.Error("sixth call allowed, want denied")
	}
	if d.RetryAfter <= 0 {
		t.Errorf("RetryAfter = %s, want positive", d.RetryAfter)
	}
}

// The scenario the fixed window counter admits ten requests for. The trailing
// window still sees the first burst, so the second one is refused -- the same
// verdict slidingwindowlog reaches, by a cheaper route.
func TestAllowN_DeniesTheBurstFixedWindowWouldAllow(t *testing.T) {
	_, l, advance := newTestLimiter(t, slidingwindowcounter.Config{Limit: 5, Window: time.Minute})

	advance(59 * time.Second)
	for i := 1; i <= 5; i++ {
		if d := allow(t, l); !d.Allowed {
			t.Fatalf("end of the first minute, call %d: denied", i)
		}
	}

	advance(2 * time.Second)
	for i := 1; i <= 5; i++ {
		if d := allow(t, l); d.Allowed {
			t.Fatalf("start of the second minute, call %d: allowed, want denied", i)
		}
	}
}

// Half a window in, half the previous window's count still counts.
func TestAllowN_WeightsThePreviousWindow(t *testing.T) {
	s, l, advance := newTestLimiter(t, slidingwindowcounter.Config{Limit: 10, Window: time.Minute})

	for i := 1; i <= 10; i++ {
		if d := allow(t, l); !d.Allowed {
			t.Fatalf("filling the first window, call %d: denied", i)
		}
	}
	_ = s

	// A whole window on, the previous count is still fully in view.
	advance(time.Minute)
	if d := allow(t, l); d.Allowed {
		t.Error("allowed at weight 1.0, want denied: the previous window is fully in view")
	}

	// Half a window further, the weight is 0.5 and so is the estimate.
	advance(30 * time.Second)
	d := allow(t, l)
	if !d.Allowed {
		t.Fatal("denied at weight 0.5, want allowed")
	}
	// estimate = 10*0.5 = 5, so 10 - 5 - 1 of the quota is left.
	if d.Remaining != 4 {
		t.Errorf("Remaining = %d, want 4 (limit 10 - estimate 5 - cost 1)", d.Remaining)
	}
}

// The estimate has to keep its fraction through the comparison. Both cases sit
// at exactly half a window, and differ only in the previous count.
func TestAllowN_FractionalEstimateDecidesTheVerdict(t *testing.T) {
	tests := []struct {
		name     string
		previous int
		want     bool
	}{
		// 9*0.5 = 4.5; 4.5 + 1 = 5.5 > 5. A floored 4 would make it 5, which fits.
		{name: "4.5 does not fit", previous: 9, want: false},
		// 8*0.5 = 4.0; 4.0 + 1 = 5.0, which fits exactly.
		{name: "4.0 fits exactly", previous: 8, want: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, l, advance := newTestLimiter(t, slidingwindowcounter.Config{Limit: 5, Window: time.Minute})
			seedPreviousWindow(t, s, "user", time.Minute, tc.previous)

			advance(30 * time.Second)

			if got := allow(t, l).Allowed; got != tc.want {
				t.Errorf("previous=%d at weight 0.5: Allowed = %v, want %v",
					tc.previous, got, tc.want)
			}
		})
	}
}

func TestAllowN_CountersExpireAfterTwoWindows(t *testing.T) {
	s, l, advance := newTestLimiter(t, slidingwindowcounter.Config{Limit: 5, Window: time.Minute})

	allow(t, l)
	if got := len(s.Keys()); got != 1 {
		t.Fatalf("keys after one call = %d, want 1", got)
	}

	// Still readable a window later: that is the whole point of the 2*window TTL.
	advance(time.Minute)
	if d := allow(t, l); d.Remaining != 3 {
		t.Errorf("Remaining = %d, want 3 — the previous window's count was not read", d.Remaining)
	}

	// Two windows past the first write, it is gone.
	advance(2 * time.Minute)
	for _, k := range s.Keys() {
		if k == fmt.Sprintf("rl:swc:user:%d", baseTime.UnixMilli()) {
			t.Errorf("the first window's counter is still present as %s", k)
		}
	}
}

func TestAllowN_CostAboveRemainingIsRejectedNotClamped(t *testing.T) {
	_, l, _ := newTestLimiter(t, slidingwindowcounter.Config{Limit: 5, Window: time.Minute})
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
		t.Error("cost 3 with 2 left was allowed, want denied")
	}
	if d.Remaining != 2 {
		t.Errorf("Remaining = %d after a denial, want 2 untouched", d.Remaining)
	}

	d, err = l.AllowN(ctx, "user", 2)
	if err != nil {
		t.Fatalf("AllowN: %v", err)
	}
	if !d.Allowed || d.Remaining != 0 {
		t.Errorf("cost 2 with 2 left: Allowed = %v, Remaining = %d; want true, 0", d.Allowed, d.Remaining)
	}
}

func TestAllowN_ReturnsBackendUnavailableWhenRedisIsDown(t *testing.T) {
	s, l, _ := newTestLimiter(t, slidingwindowcounter.Config{Limit: 5, Window: time.Minute})

	// Prove the happy path first, or a helper that always failed would make this
	// pass for the wrong reason.
	if d := allow(t, l); !d.Allowed {
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
	s, l, _ := newTestLimiter(t, slidingwindowcounter.Config{Limit: 5, Window: time.Minute})
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

// Reset has to clear the previous window too. Clearing only the current one
// leaves the old count still weighting the estimate upward.
func TestReset_ClearsBothCounters(t *testing.T) {
	s, l, advance := newTestLimiter(t, slidingwindowcounter.Config{Limit: 5, Window: time.Minute})
	seedPreviousWindow(t, s, "user", time.Minute, 5)

	advance(30 * time.Second)
	allow(t, l)

	if err := l.Reset(context.Background(), "user"); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	d := allow(t, l)
	if !d.Allowed || d.Remaining != 4 {
		t.Errorf("after reset: Allowed = %v, Remaining = %d; want true, 4", d.Allowed, d.Remaining)
	}
}
