package leakybucket_test

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/sorenhoang/go-ratelimiter/internal/limiter/leakybucket"
)

func newTestQueue(t *testing.T, cfg leakybucket.QueueConfig) *leakybucket.Queue {
	t.Helper()

	q, err := leakybucket.NewQueue(cfg)
	if err != nil {
		t.Fatalf("NewQueue: %v", err)
	}
	// Registered before the test can fail, so goleak in TestMain does not see a
	// worker left behind by a t.Fatal somewhere below.
	t.Cleanup(func() { _ = q.Close() })

	return q
}

// The property that makes this traffic shaping rather than rate limiting: the
// output is evenly spaced.
//
// Asserting the total duration instead would pass an implementation that
// released all ten at once and then slept, which is precisely the behaviour a
// queue exists to prevent.
func TestQueue_ReleasesEvenlySpaced(t *testing.T) {
	const (
		rate     = 50.0
		interval = time.Second / time.Duration(rate)
		releases = 8
	)
	q := newTestQueue(t, leakybucket.QueueConfig{ReleasePerSecond: rate, Capacity: 4})
	ctx := context.Background()

	// The first wait lands anywhere inside the current interval, so timing
	// starts from the first release rather than from here.
	if err := q.Wait(ctx); err != nil {
		t.Fatalf("first wait: %v", err)
	}

	prev := time.Now()
	gaps := make([]time.Duration, 0, releases)
	for i := 0; i < releases; i++ {
		if err := q.Wait(ctx); err != nil {
			t.Fatalf("wait %d: %v", i, err)
		}
		now := time.Now()
		gaps = append(gaps, now.Sub(prev))
		prev = now
	}

	t.Logf("interval %s, gaps %v", interval, gaps)
	for i, gap := range gaps {
		// Loose enough for a busy machine, tight enough that a clumped release
		// or an accumulated burst fails.
		if gap < interval/2 || gap > interval*3 {
			t.Errorf("gap %d = %s, want roughly %s", i, gap, interval)
		}
	}
}

// Idle time earns no credit. A token bucket would hand back a burst here; a
// queue must not, and that difference is the unbuffered permit channel.
func TestQueue_IdleTimeDoesNotBuyABurst(t *testing.T) {
	const (
		rate     = 20.0
		interval = time.Second / time.Duration(rate)
	)
	q := newTestQueue(t, leakybucket.QueueConfig{ReleasePerSecond: rate, Capacity: 4})
	ctx := context.Background()

	// Five intervals of nobody waiting. Permits tick and are dropped.
	time.Sleep(5 * interval)

	start := time.Now()
	for i := 0; i < 3; i++ {
		if err := q.Wait(ctx); err != nil {
			t.Fatalf("wait %d: %v", i, err)
		}
	}
	took := time.Since(start)

	// Three releases cannot be faster than two intervals apart. Had the permits
	// accumulated, all three would have come back at once.
	if min := 2 * interval * 8 / 10; took < min {
		t.Errorf("three releases after idling took %s, want at least %s — permits accumulated",
			took, min)
	}
	t.Logf("three releases after five idle intervals took %s", took)
}

func TestQueue_RefusesWhenFull(t *testing.T) {
	// Slow enough that the occupant is still waiting when the next caller
	// arrives.
	q := newTestQueue(t, leakybucket.QueueConfig{ReleasePerSecond: 1, Capacity: 1})

	occupied := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		close(occupied)
		_ = q.Wait(context.Background())
	}()
	<-occupied
	// Give the occupant time to actually take the slot.
	time.Sleep(30 * time.Millisecond)

	start := time.Now()
	err := q.Wait(context.Background())
	if !errors.Is(err, leakybucket.ErrQueueFull) {
		t.Errorf("err = %v, want ErrQueueFull", err)
	}
	// Fast, not parked: the point of the error is that the caller learns at once.
	if took := time.Since(start); took > 50*time.Millisecond {
		t.Errorf("refusal took %s, want it to be immediate", took)
	}

	_ = q.Close()
	wg.Wait()
}

func TestQueue_CancelledCallerFreesItsPlaceAtOnce(t *testing.T) {
	q := newTestQueue(t, leakybucket.QueueConfig{ReleasePerSecond: 1, Capacity: 1})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	err := q.Wait(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}

	// The place has to be free immediately, not held until the turn that caller
	// gave up on would have arrived. Otherwise a queue fills with callers who
	// have already gone.
	if n := q.Len(); n != 0 {
		t.Errorf("queue holds %d places after the caller gave up, want 0", n)
	}

	// And a capacity of one means the next caller can only get in if the place
	// really was released.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel2()
	if err := q.Wait(ctx2); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("second caller: err = %v, want DeadlineExceeded rather than ErrQueueFull", err)
	}
}

func TestQueue_CloseReleasesWaiters(t *testing.T) {
	q, err := leakybucket.NewQueue(leakybucket.QueueConfig{ReleasePerSecond: 1, Capacity: 4})
	if err != nil {
		t.Fatalf("NewQueue: %v", err)
	}

	const waiters = 3
	errs := make(chan error, waiters)
	for i := 0; i < waiters; i++ {
		go func() { errs <- q.Wait(context.Background()) }()
	}
	time.Sleep(30 * time.Millisecond)

	if err := q.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	for i := 0; i < waiters; i++ {
		select {
		case err := <-errs:
			if !errors.Is(err, leakybucket.ErrQueueClosed) {
				t.Errorf("waiter %d: err = %v, want ErrQueueClosed", i, err)
			}
		case <-time.After(time.Second):
			t.Fatalf("waiter %d never returned after Close", i)
		}
	}

	// Idempotent, and a late caller is refused rather than parked forever.
	if err := q.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	if err := q.Wait(context.Background()); !errors.Is(err, leakybucket.ErrQueueClosed) {
		t.Errorf("wait after Close: err = %v, want ErrQueueClosed", err)
	}
}

// Runs the queue hard from many goroutines so the race detector has something
// to look at, and checks every caller got exactly one of the two answers.
func TestQueue_ConcurrentCallers(t *testing.T) {
	const (
		callers  = 40
		capacity = 8
	)
	q := newTestQueue(t, leakybucket.QueueConfig{ReleasePerSecond: 200, Capacity: capacity})

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	var (
		mu       sync.Mutex
		released int
		full     int
		other    int
		wg       sync.WaitGroup
	)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := q.Wait(ctx)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				released++
			case errors.Is(err, leakybucket.ErrQueueFull):
				full++
			default:
				other++
			}
		}()
	}
	wg.Wait()

	t.Logf("%d callers: %d released, %d refused as full, %d otherwise", callers, released, full, other)

	if released+full+other != callers {
		t.Errorf("accounted for %d of %d callers", released+full+other, callers)
	}
	if released == 0 {
		t.Error("nobody got through")
	}
	if other != 0 {
		t.Errorf("%d callers got an unexpected error", other)
	}
}

func TestNewQueue_RejectsUnusableConfig(t *testing.T) {
	tests := []struct {
		name string
		cfg  leakybucket.QueueConfig
	}{
		{"zero capacity", leakybucket.QueueConfig{ReleasePerSecond: 1, Capacity: 0}},
		{"negative capacity", leakybucket.QueueConfig{ReleasePerSecond: 1, Capacity: -1}},
		{"zero rate", leakybucket.QueueConfig{ReleasePerSecond: 0, Capacity: 1}},
		{"negative rate", leakybucket.QueueConfig{ReleasePerSecond: -1, Capacity: 1}},
		// NaN fails every comparison, so a plain bounds check waves it through.
		{"NaN rate", leakybucket.QueueConfig{ReleasePerSecond: math.NaN(), Capacity: 1}},
		{"infinite rate", leakybucket.QueueConfig{ReleasePerSecond: math.Inf(1), Capacity: 1}},
		// Fast enough that the interval truncates to zero, which would panic
		// inside time.NewTicker.
		{"rate too fast to schedule", leakybucket.QueueConfig{ReleasePerSecond: 1e10, Capacity: 1}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			q, err := leakybucket.NewQueue(tc.cfg)
			if err == nil {
				_ = q.Close()
				t.Errorf("NewQueue(%+v) succeeded, want an error", tc.cfg)
			}
		})
	}
}
