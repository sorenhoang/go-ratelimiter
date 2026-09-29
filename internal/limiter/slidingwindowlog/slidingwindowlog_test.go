package slidingwindowlog_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/sorenhoang/go-ratelimiter/internal/limiter"
	"github.com/sorenhoang/go-ratelimiter/internal/limiter/slidingwindowlog"
)

// baseTime sits on a whole minute, so a one minute window starts exactly on a
// boundary and every duration asserted below is exact rather than "about".
var baseTime = time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)

func newTestLimiter(t *testing.T, cfg slidingwindowlog.Config) (*miniredis.Miniredis, *slidingwindowlog.Limiter, func(time.Duration)) {
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
	l, err := slidingwindowlog.New(rdb, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// SetTime takes an absolute moment and FastForward a relative step, and
	// neither does the other's job: FastForward expires keys without moving the
	// clock that TIME reports, which is the clock this algorithm scores by.
	elapsed := time.Duration(0)
	advance := func(d time.Duration) {
		elapsed += d
		s.SetTime(baseTime.Add(elapsed))
		s.FastForward(d)
	}

	return s, l, advance
}

// mustAllow fires one request and fails unless it is admitted.
func mustAllow(t *testing.T, l *slidingwindowlog.Limiter, label string) limiter.Decision {
	t.Helper()
	d, err := l.AllowN(context.Background(), "user", 1)
	if err != nil {
		t.Fatalf("%s: %v", label, err)
	}
	if !d.Allowed {
		t.Fatalf("%s: denied, want allowed", label)
	}
	return d
}

// mustDeny fires one request and fails unless it is refused.
func mustDeny(t *testing.T, l *slidingwindowlog.Limiter, label string) limiter.Decision {
	t.Helper()
	d, err := l.AllowN(context.Background(), "user", 1)
	if err != nil {
		t.Fatalf("%s: %v", label, err)
	}
	if d.Allowed {
		t.Fatalf("%s: allowed, want denied", label)
	}
	return d
}

func TestAllowN_ExhaustsLimit(t *testing.T) {
	_, l, _ := newTestLimiter(t, slidingwindowlog.Config{Limit: 5, Window: time.Minute})

	for i := 1; i <= 5; i++ {
		d := mustAllow(t, l, fmt.Sprintf("call %d", i))
		if want := int64(5 - i); d.Remaining != want {
			t.Errorf("call %d: Remaining = %d, want %d", i, d.Remaining, want)
		}
	}

	d := mustDeny(t, l, "sixth call")
	if d.Remaining != 0 {
		t.Errorf("Remaining = %d, want 0", d.Remaining)
	}
	if d.RetryAfter <= 0 {
		t.Errorf("RetryAfter = %s, want positive", d.RetryAfter)
	}
}

// The clock is frozen, so every request here lands on the same millisecond.
//
// The proof has to be the third request, not the reported Remaining: that number
// is arithmetic on the count read before the write, so it reads correctly even
// when two entries collide. Only the denial shows that both of the first two are
// really in the log.
func TestAllowN_CountsTwoRequestsInTheSameMillisecond(t *testing.T) {
	_, l, _ := newTestLimiter(t, slidingwindowlog.Config{Limit: 2, Window: time.Minute})

	mustAllow(t, l, "first")
	mustAllow(t, l, "second")
	mustDeny(t, l, "third, so both earlier requests are in the log")
}

// The defining property: quota comes back one request at a time, as each
// individual entry ages out. A fixed window instead returns the whole allowance
// in one step when its window rolls.
func TestAllowN_SlidesInsteadOfStepping(t *testing.T) {
	_, l, advance := newTestLimiter(t, slidingwindowlog.Config{
		Limit:  3,
		Window: 3 * time.Second,
	})

	mustAllow(t, l, "t=0s")
	advance(time.Second)
	mustAllow(t, l, "t=1s")
	advance(time.Second)
	mustAllow(t, l, "t=2s")
	mustDeny(t, l, "t=2s, quota spent")

	// Past t=3s the first entry has left the window, and exactly one slot opens.
	advance(1100 * time.Millisecond)
	mustAllow(t, l, "t=3.1s, first entry expired")
	mustDeny(t, l, "t=3.1s, only one slot was freed")
}

// The same sequence that TestAllowN_AllowsDoubleLimitAcrossWindowBoundary feeds
// to the fixed window counter, which admits all ten. Here the second burst is
// refused: the window moved with the requests instead of resetting under them.
// One input, two algorithms, opposite verdicts — the reason both are in the repo.
func TestAllowN_DeniesTheBurstFixedWindowWouldAllow(t *testing.T) {
	_, l, advance := newTestLimiter(t, slidingwindowlog.Config{
		Limit:  5,
		Window: time.Minute,
	})

	advance(59 * time.Second)
	for i := 1; i <= 5; i++ {
		mustAllow(t, l, "end of the first minute")
	}

	advance(2 * time.Second)
	for i := 1; i <= 5; i++ {
		d := mustDeny(t, l, "start of the second minute")
		if d.Remaining != 0 {
			t.Errorf("Remaining = %d, want 0", d.Remaining)
		}
	}
}

func TestAllowN_CostAboveRemainingIsRejectedNotClamped(t *testing.T) {
	_, l, _ := newTestLimiter(t, slidingwindowlog.Config{Limit: 5, Window: time.Minute})
	ctx := context.Background()

	// A cost of 3 must add three distinct entries, not one.
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

func TestAllowN_ReportsExactResetAndRetry(t *testing.T) {
	_, l, advance := newTestLimiter(t, slidingwindowlog.Config{Limit: 2, Window: time.Minute})

	d := mustAllow(t, l, "first")
	if d.Limit != 2 || d.Remaining != 1 {
		t.Errorf("first: Limit = %d, Remaining = %d; want 2, 1", d.Limit, d.Remaining)
	}
	if d.ResetAfter != time.Minute {
		t.Errorf("first: ResetAfter = %s, want %s", d.ResetAfter, time.Minute)
	}
	if d.RetryAfter != 0 {
		t.Errorf("first: RetryAfter = %s, want 0 while allowed", d.RetryAfter)
	}

	advance(20 * time.Second)

	// ResetAfter is measured from the newest entry, which was just written, so it
	// is a whole window again. A fixed window would report the remainder of the
	// current slot instead.
	d = mustAllow(t, l, "second")
	if d.ResetAfter != time.Minute {
		t.Errorf("second: ResetAfter = %s, want %s", d.ResetAfter, time.Minute)
	}

	// The wait is until the oldest entry leaves: it landed at t=0 and expires at
	// t=60s, and the clock is at t=20s.
	d = mustDeny(t, l, "third")
	if want := 40 * time.Second; d.RetryAfter != want {
		t.Errorf("third: RetryAfter = %s, want %s", d.RetryAfter, want)
	}
}

func TestAllowN_ReturnsBackendUnavailableWhenRedisIsDown(t *testing.T) {
	s, l, _ := newTestLimiter(t, slidingwindowlog.Config{Limit: 5, Window: time.Minute})
	ctx := context.Background()

	// Prove the happy path works first: without it, a helper that always failed
	// would make this test pass for entirely the wrong reason.
	mustAllow(t, l, "before the outage")

	s.Close()

	_, err := l.AllowN(ctx, "user", 1)
	if err == nil {
		t.Fatal("no error with Redis down")
	}
	if !errors.Is(err, limiter.ErrBackendUnavailable) {
		t.Errorf("err = %v, want it to wrap ErrBackendUnavailable", err)
	}
}

func TestReset_ReturnsBackendUnavailableWhenRedisIsDown(t *testing.T) {
	s, l, _ := newTestLimiter(t, slidingwindowlog.Config{Limit: 5, Window: time.Minute})
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

func TestReset_ClearsTheLog(t *testing.T) {
	_, l, _ := newTestLimiter(t, slidingwindowlog.Config{Limit: 2, Window: time.Minute})
	ctx := context.Background()

	mustAllow(t, l, "first")
	mustAllow(t, l, "second")
	mustDeny(t, l, "third")

	if err := l.Reset(ctx, "user"); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	if d := mustAllow(t, l, "after reset"); d.Remaining != 1 {
		t.Errorf("after reset: Remaining = %d, want 1", d.Remaining)
	}
}
