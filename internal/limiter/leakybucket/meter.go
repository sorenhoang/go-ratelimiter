// Package leakybucket holds the two halves of the same idea.
//
// Meter refuses what would overflow, which makes it a rate limiter like
// everything else in this repo — and, exactly, the dual of phase 04's token
// bucket. Queue makes the caller wait instead, which makes it traffic shaping,
// and is the original sense of the name.
//
// The two do not share an interface. Meter answers now; Queue answers when the
// caller's turn arrives. See queue.go.
package leakybucket

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

//go:embed meter.lua
var meterSource string

//go:embed reset.lua
var resetSource string

var (
	meterScript = redis.NewScript(meterSource)
	resetScript = redis.NewScript(resetSource)
)

const meterPrefix = "rl:lb:"

// MeterConfig describes the bucket water pours into.
type MeterConfig struct {
	// Capacity is how much the bucket holds before it overflows.
	Capacity int64
	// LeakPerSecond is how fast it drains, and may be fractional.
	LeakPerSecond float64
}

// Meter is a leaky bucket that refuses overflow, backed by Redis.
//
// It is the token bucket seen from the other side: level = capacity - tokens,
// and every comparison maps across. Which one reads better depends on the
// question — "how full is this client's bucket" or "how much credit is left".
type Meter struct {
	rdb  redis.Scripter
	cfg  MeterConfig
	leak string
}

var _ limiter.Limiter = (*Meter)(nil)

// NewMeter returns a Meter, or an error if the config could not produce
// sensible decisions.
func NewMeter(rdb redis.Scripter, cfg MeterConfig) (*Meter, error) {
	if cfg.Capacity <= 0 {
		return nil, fmt.Errorf("leakybucket: invalid capacity: %d", cfg.Capacity)
	}
	// NaN fails every comparison, so it would slip past a plain `<= 0` and then
	// poison every arithmetic result in the script without erroring anywhere.
	if math.IsNaN(cfg.LeakPerSecond) || math.IsInf(cfg.LeakPerSecond, 0) || cfg.LeakPerSecond <= 0 {
		return nil, fmt.Errorf("leakybucket: invalid leak rate: %v", cfg.LeakPerSecond)
	}

	return &Meter{
		rdb:  rdb,
		cfg:  cfg,
		leak: strconv.FormatFloat(cfg.LeakPerSecond, 'f', -1, 64),
	}, nil
}

// Name identifies this limiter, and doubles as its URL segment.
func (m *Meter) Name() string {
	return "leakybucket"
}

// AllowN decides whether key may pour n more units into its bucket.
//
// Remaining is the headroom left, in whole units: a partial unit exists in
// Redis but cannot hold a whole request, so reporting it would overstate what
// the caller can do.
func (m *Meter) AllowN(ctx context.Context, key string, n int64) (limiter.Decision, error) {
	if n <= 0 {
		return limiter.Decision{}, fmt.Errorf("leakybucket: invalid n: %d", n)
	}

	keys := []string{meterPrefix + key}
	vals, err := meterScript.Run(ctx, m.rdb, keys,
		m.cfg.Capacity, m.leak, n,
	).Int64Slice()
	if err != nil {
		return limiter.Decision{}, fmt.Errorf("leakybucket: allow %s: %w: %w",
			key, limiter.ErrBackendUnavailable, err)
	}
	if len(vals) != 4 {
		return limiter.Decision{}, fmt.Errorf("leakybucket: script returned %d values, want 4: %w",
			len(vals), limiter.ErrBackendUnavailable)
	}

	return limiter.Decision{
		Allowed:    vals[0] == 1,
		Limit:      m.cfg.Capacity,
		Remaining:  vals[1],
		ResetAfter: time.Duration(vals[2]) * time.Millisecond,
		RetryAfter: time.Duration(vals[3]) * time.Millisecond,
	}, nil
}

// Reset empties the bucket.
func (m *Meter) Reset(ctx context.Context, key string) error {
	keys := []string{meterPrefix + key}
	if err := resetScript.Run(ctx, m.rdb, keys).Err(); err != nil {
		return fmt.Errorf("leakybucket: reset %s: %w: %w",
			key, limiter.ErrBackendUnavailable, err)
	}
	return nil
}
