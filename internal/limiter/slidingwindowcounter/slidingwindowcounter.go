// Package slidingwindowcounter estimates the rate from two fixed-window counters
// rather than recording every request.
//
// It weights the previous window's count by how much of that window is still in
// view, which costs O(1) memory per key where slidingwindowlog costs O(limit).
// The weighting assumes the previous window's traffic was spread evenly across
// it, so a burst confined to part of that window is misread. That error is the
// price of the memory, and it is why this, not the log, is what usually ships.
package slidingwindowcounter

import (
	"context"
	_ "embed"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/sorenhoang/go-ratelimiter/internal/limiter"
)

//go:embed script.lua
var scriptSource string

//go:embed reset.lua
var resetSource string

var (
	script      = redis.NewScript(scriptSource)
	resetScript = redis.NewScript(resetSource)
)

const prefix = "rl:swc:"

// Config is the limit this limiter enforces.
type Config struct {
	// Limit is how much quota one window holds.
	Limit int64
	// Window is how far back the estimate looks. Must be at least a
	// millisecond, since the script works in whole milliseconds.
	Window time.Duration
}

// Limiter is a sliding window counter backed by Redis.
type Limiter struct {
	rdb redis.Scripter
	cfg Config
}

var _ limiter.Limiter = (*Limiter)(nil)

// New returns a Limiter, or an error if the config could not produce sensible
// decisions.
func New(rdb redis.Scripter, cfg Config) (*Limiter, error) {
	if cfg.Limit <= 0 {
		return nil, fmt.Errorf("slidingwindowcounter: invalid limit: %d", cfg.Limit)
	}
	// Anything under a millisecond truncates to a zero window, and the script
	// would divide by it.
	if cfg.Window < time.Millisecond {
		return nil, fmt.Errorf("slidingwindowcounter: invalid window: %s", cfg.Window)
	}

	return &Limiter{rdb: rdb, cfg: cfg}, nil
}

// Name identifies this limiter, and doubles as its URL segment.
func (l *Limiter) Name() string {
	return "slidingwindowcounter"
}

// AllowN decides whether key may spend n of its quota.
//
// Being refused is not an error: it comes back as a Decision with Allowed
// false. An error means no decision could be made at all.
func (l *Limiter) AllowN(ctx context.Context, key string, n int64) (limiter.Decision, error) {
	if n <= 0 {
		return limiter.Decision{}, fmt.Errorf("slidingwindowcounter: invalid n: %d", n)
	}

	keys := []string{prefix + key}
	vals, err := script.Run(ctx, l.rdb, keys,
		l.cfg.Window.Milliseconds(), l.cfg.Limit, n,
	).Int64Slice()
	if err != nil {
		return limiter.Decision{}, fmt.Errorf("slidingwindowcounter: allow %s: %w: %w",
			key, limiter.ErrBackendUnavailable, err)
	}
	if len(vals) != 4 {
		return limiter.Decision{}, fmt.Errorf("slidingwindowcounter: script returned %d values, want 4: %w",
			len(vals), limiter.ErrBackendUnavailable)
	}

	return limiter.Decision{
		Allowed:    vals[0] == 1,
		Limit:      l.cfg.Limit,
		Remaining:  vals[1],
		ResetAfter: time.Duration(vals[2]) * time.Millisecond,
		RetryAfter: time.Duration(vals[3]) * time.Millisecond,
	}, nil
}

// Reset clears both counters the estimate reads, so the key starts over.
func (l *Limiter) Reset(ctx context.Context, key string) error {
	keys := []string{prefix + key}
	if err := resetScript.Run(ctx, l.rdb, keys, l.cfg.Window.Milliseconds()).Err(); err != nil {
		return fmt.Errorf("slidingwindowcounter: reset %s: %w: %w",
			key, limiter.ErrBackendUnavailable, err)
	}
	return nil
}
