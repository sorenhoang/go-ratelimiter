package slidingwindowcounter_test

import (
	"context"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/sorenhoang/go-ratelimiter/internal/limiter/slidingwindowcounter"
	"github.com/sorenhoang/go-ratelimiter/internal/limiter/slidingwindowlog"
)

// TestDivergence_AgainstSlidingWindowLog replays one request stream through both
// limiters and counts how often they disagree.
//
// slidingwindowlog is the ground truth: it records every request, so its verdict
// is exact by construction. This limiter estimates instead, and the gap between
// the two is what the O(limit) memory of phase 02 actually buys.
//
// Both see every arrival and each decides on its own, so their states drift
// apart as a consequence of their own verdicts. That is deliberate — the
// question is what would have happened had each been deployed, not how they
// compare on identical internal state.
func TestDivergence_AgainstSlidingWindowLog(t *testing.T) {
	const (
		requests = 3000
		window   = time.Second
		limit    = 10
	)

	// How close the arrival rate runs to the limit is what decides the error, so
	// measure both a stream that crowds the limit and one that does not. Each
	// threshold was set from the measurement this test prints, not guessed
	// before it.
	tests := []struct {
		name   string
		maxGap time.Duration
		// ~requests per second, against a limit of 10 per second.
		about string
		max   float64
	}{
		{name: "crowding the limit", maxGap: 250 * time.Millisecond, about: "~8/s", max: 0.08},
		{name: "well under the limit", maxGap: 600 * time.Millisecond, about: "~3/s", max: 0.01},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			lenient, strict := runDivergence(t, requests, window, limit, tc.maxGap)
			disagreed := lenient + strict
			rate := float64(disagreed) / float64(requests)

			t.Logf("%s (%s): disagreed on %d of %d (%.2f%%)", tc.name, tc.about, disagreed, requests, rate*100)
			t.Logf("  admitted %d the log refused, refused %d the log admitted", lenient, strict)

			if rate > tc.max {
				t.Errorf("divergence %.2f%% exceeds the accepted %.2f%%", rate*100, tc.max*100)
			}

			// Which way it errs decides whether the estimate is a security
			// problem or merely an annoyance, so record it rather than leaving
			// it to be rediscovered.
			switch {
			case lenient > strict:
				t.Log("  errs toward admitting traffic the exact answer refuses")
			case strict > lenient:
				t.Log("  errs toward refusing traffic the exact answer admits")
			}
		})
	}
}

func runDivergence(t *testing.T, requests int, window time.Duration, limit int64, maxGap time.Duration) (lenient, strict int) {
	t.Helper()

	s, err := miniredis.Run()
	if err != nil {
		t.Fatalf("start miniredis: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	s.SetTime(baseTime)
	rdb := redis.NewClient(&redis.Options{Addr: s.Addr(), MaxRetries: -1})

	counter, err := slidingwindowcounter.New(rdb, slidingwindowcounter.Config{Limit: limit, Window: window})
	if err != nil {
		t.Fatalf("counter: %v", err)
	}
	exact, err := slidingwindowlog.New(rdb, slidingwindowlog.Config{Limit: limit, Window: window})
	if err != nil {
		t.Fatalf("log: %v", err)
	}

	// Fixed seed: the measured rate has to mean the same thing on every run.
	rng := rand.New(rand.NewPCG(0x5eed, 0x1337))
	ctx := context.Background()
	elapsed := time.Duration(0)

	for i := 0; i < requests; i++ {
		step := time.Duration(rng.IntN(int(maxGap.Milliseconds()))) * time.Millisecond
		elapsed += step
		s.SetTime(baseTime.Add(elapsed))
		s.FastForward(step)

		approx, err := counter.AllowN(ctx, "user", 1)
		if err != nil {
			t.Fatalf("request %d, counter: %v", i, err)
		}
		truth, err := exact.AllowN(ctx, "user", 1)
		if err != nil {
			t.Fatalf("request %d, log: %v", i, err)
		}

		switch {
		case approx.Allowed && !truth.Allowed:
			lenient++
		case !approx.Allowed && truth.Allowed:
			strict++
		}
	}

	return lenient, strict
}
