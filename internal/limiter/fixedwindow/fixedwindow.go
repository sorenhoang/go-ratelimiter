// Package fixedwindow counts requests against a fixed slot of time.
//
// It is the cheapest thing in this repo -- one integer per caller, 72 bytes in
// Redis however much traffic passes through it -- and the least accurate. A
// caller can take twice the limit by spending one window's allowance at its end
// and the next one's at its start, which the algorithm permits by definition
// rather than by mistake.
//
// It is kept for exactly that. TestAllowN_AllowsDoubleLimitAcrossWindowBoundary
// asserts the flaw rather than hiding it, and every later algorithm here has a
// test feeding it the same sequence and reaching the opposite verdict.
package fixedwindow

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

const prefix = "rl:fw:"

// Config is the limit this limiter enforces.
type Config struct {
	// Limit is how many requests one window admits.
	Limit int64
	// Window is the length of a slot. Must be at least a millisecond, since the
	// script works in whole milliseconds and anything shorter truncates to zero.
	Window time.Duration
}

// Limiter is a fixed window counter backed by Redis.
type Limiter struct {
	rdb redis.Scripter
	cfg Config
}

var _ limiter.Limiter = (*Limiter)(nil)

// Name identifies this limiter, and doubles as its URL segment.
func (l *Limiter) Name() string {
	return "fixedwindow"
}

// New returns a Limiter, or an error if the config could not produce sensible
// decisions.
func New(rdb redis.Scripter, cfg Config) (*Limiter, error) {
	if cfg.Limit <= 0 {
		return nil, fmt.Errorf("fixedwindow: invalid limit: %d", cfg.Limit)
	}

	if cfg.Window < time.Millisecond {
		return nil, fmt.Errorf("fixedwindow: invalid window: %s", cfg.Window)
	}
	return &Limiter{
		rdb: rdb,
		cfg: cfg,
	}, nil
}

// Reset clears the counter for the window that is current right now, which is
// the only one AllowN reads.
func (l *Limiter) Reset(ctx context.Context, key string) error {
	if err := resetScript.Run(ctx, l.rdb, []string{prefix + key}, l.cfg.Window.Milliseconds()).Err(); err != nil {
		return fmt.Errorf("fixedwindow: failed to reset limit for key %s: %w %w", key, err, limiter.ErrBackendUnavailable)
	}
	return nil
}

// AllowN decides whether key may spend n of this window's allowance.
//
// Being refused is not an error: it comes back as a Decision with Allowed
// false. An error means no decision could be made at all.
//
// ResetAfter is the remainder of the current window, and also when the entire
// allowance returns at once. That step is what lets a caller take double the
// limit across a boundary.
func (l *Limiter) AllowN(ctx context.Context, key string, n int64) (limiter.Decision, error) {
	if n <= 0 {
		return limiter.Decision{}, fmt.Errorf("fixedwindow: invalid n: %d", n)
	}

	vals, err := script.Run(ctx, l.rdb, []string{prefix + key}, l.cfg.Window.Milliseconds(), l.cfg.Limit, n).Int64Slice()
	if err != nil {
		return limiter.Decision{}, fmt.Errorf("fixedwindow: failed to check limit for key %s: %w %w", key, err, limiter.ErrBackendUnavailable)
	}

	if len(vals) != 4 {
		return limiter.Decision{}, fmt.Errorf("fixedwindow: unexpected number of return values from script: %d %w", len(vals), limiter.ErrBackendUnavailable)
	}

	decision := limiter.Decision{
		Allowed:    vals[0] == 1,
		Limit:      l.cfg.Limit,
		Remaining:  vals[1],
		ResetAfter: time.Duration(vals[2]) * time.Millisecond,
		RetryAfter: time.Duration(vals[3]) * time.Millisecond,
	}

	return decision, nil
}
