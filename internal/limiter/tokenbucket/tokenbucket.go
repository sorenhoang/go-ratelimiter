// Package tokenbucket admits bursts on purpose.
//
// A caller accumulates tokens up to a capacity and spends them per request, so
// one who has been quiet can fire the whole capacity at once and then settle
// into the refill rate. Every other limiter in this repo treats that burst as
// the flaw to prevent; here it is the feature, which makes this the usual choice
// for an API that wants to smooth sustained load without punishing a client for
// arriving with a backlog.
//
// State is one hash of two fields per key, refilled lazily when it is read.
package tokenbucket

import (
	"context"
	_ "embed"
	"fmt"
	"math"
	"strconv"
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

const prefix = "rl:tb:"

// Config describes the bucket.
//
// Unlike the window-based limiters there is no window here: the pair of numbers
// below sets both the burst a caller may take and the rate it sustains.
type Config struct {
	// Capacity is the most tokens a bucket holds, and so the largest burst a
	// caller can spend at once.
	Capacity int64
	// RefillPerSecond is how fast tokens come back, and may be fractional: a
	// rate below one token per second is the point of keeping the partial token
	// in Redis rather than rounding it away.
	RefillPerSecond float64
}

// Limiter is a token bucket backed by Redis.
type Limiter struct {
	rdb redis.Scripter
	cfg Config
	// Formatted once, so the script always parses the same decimal string
	// instead of whatever the client happens to render the float as.
	rate string
}

var _ limiter.Limiter = (*Limiter)(nil)

// New returns a Limiter, or an error if the config could not produce sensible
// decisions.
func New(rdb redis.Scripter, cfg Config) (*Limiter, error) {
	if cfg.Capacity <= 0 {
		return nil, fmt.Errorf("tokenbucket: invalid capacity: %d", cfg.Capacity)
	}
	// NaN fails every comparison, so it would slip past a plain `<= 0` and then
	// poison every arithmetic result in the script without erroring anywhere.
	if math.IsNaN(cfg.RefillPerSecond) || math.IsInf(cfg.RefillPerSecond, 0) || cfg.RefillPerSecond <= 0 {
		return nil, fmt.Errorf("tokenbucket: invalid refill rate: %v", cfg.RefillPerSecond)
	}

	return &Limiter{
		rdb:  rdb,
		cfg:  cfg,
		rate: strconv.FormatFloat(cfg.RefillPerSecond, 'f', -1, 64),
	}, nil
}

// Name identifies this limiter, and doubles as its URL segment.
func (l *Limiter) Name() string {
	return "tokenbucket"
}

// AllowN decides whether key may spend n tokens.
//
// Decision.Limit carries the capacity, which is the closest thing this algorithm
// has to a limit: it is the largest burst allowed, not an allowance per window.
// Remaining is whole tokens only — a partial token exists in Redis but cannot be
// spent, so reporting it would overstate what the caller can do.
func (l *Limiter) AllowN(ctx context.Context, key string, n int64) (limiter.Decision, error) {
	if n <= 0 {
		return limiter.Decision{}, fmt.Errorf("tokenbucket: invalid n: %d", n)
	}

	keys := []string{prefix + key}
	vals, err := script.Run(ctx, l.rdb, keys,
		l.cfg.Capacity, l.rate, n,
	).Int64Slice()
	if err != nil {
		return limiter.Decision{}, fmt.Errorf("tokenbucket: allow %s: %w: %w",
			key, limiter.ErrBackendUnavailable, err)
	}
	if len(vals) != 4 {
		return limiter.Decision{}, fmt.Errorf("tokenbucket: script returned %d values, want 4: %w",
			len(vals), limiter.ErrBackendUnavailable)
	}

	return limiter.Decision{
		Allowed:    vals[0] == 1,
		Limit:      l.cfg.Capacity,
		Remaining:  vals[1],
		ResetAfter: time.Duration(vals[2]) * time.Millisecond,
		RetryAfter: time.Duration(vals[3]) * time.Millisecond,
	}, nil
}

// Reset drops the bucket, so the next request meets a full one.
func (l *Limiter) Reset(ctx context.Context, key string) error {
	keys := []string{prefix + key}
	if err := resetScript.Run(ctx, l.rdb, keys).Err(); err != nil {
		return fmt.Errorf("tokenbucket: reset %s: %w: %w",
			key, limiter.ErrBackendUnavailable, err)
	}
	return nil
}
