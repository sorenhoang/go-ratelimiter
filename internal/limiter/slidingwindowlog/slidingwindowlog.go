// Package slidingwindowlog records every request and counts the ones still
// inside the window.
//
// That makes it exact by construction, which is why it serves as the ground
// truth the approximations are measured against: the divergence test in phase
// 03 replays one request stream through both and counts where they disagree.
//
// The price is memory, not speed. Sorted set operations are logarithmic, so it
// benchmarks no slower than the counters -- every limiter here answers in about
// the time of one Redis round trip. But one key holding 2000 requests occupies
// around 196KB where a counter occupies 72 bytes, and that ratio is the entire
// argument for using something else.
package slidingwindowlog

import (
	"context"
	_ "embed"
	"fmt"
	"math/rand/v2"
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

const prefix = "rl:swl:"

// Config is the limit this limiter enforces.
type Config struct {
	// Limit is how many requests may fall inside the trailing window. It is
	// also how many entries one key holds, so it sets the memory cost directly.
	Limit int64
	// Window is how far back the count reaches. Must be at least a millisecond,
	// since the script works in whole milliseconds.
	Window time.Duration
}

// Limiter is a sliding window log backed by Redis.
type Limiter struct {
	rdb redis.Scripter
	cfg Config
}

var _ limiter.Limiter = (*Limiter)(nil)

// Name identifies this limiter, and doubles as its URL segment.
func (l *Limiter) Name() string {
	return "slidingwindowlog"
}

// New returns a Limiter, or an error if the config could not produce sensible
// decisions.
func New(rdb redis.Scripter, cfg Config) (*Limiter, error) {
	if cfg.Limit <= 0 {
		return nil, fmt.Errorf("slidingwindowlog: invalid limit: %d", cfg.Limit)
	}

	if cfg.Window < time.Millisecond {
		return nil, fmt.Errorf("slidingwindowlog: invalid window: %s", cfg.Window)
	}
	return &Limiter{
		rdb: rdb,
		cfg: cfg,
	}, nil
}

// newToken returns a value unique to one request, used as its sorted set member.
//
// Scoring by time alone is the trap this exists to avoid: two requests landing
// in the same millisecond would carry the same member, ZADD would overwrite
// instead of adding, the count would run low and traffic would leak past the
// limit. A counter collides the same way once two application servers are
// running, and crypto/rand would be a syscall per request for a value that has
// to be distinct rather than unguessable.
func newToken() string {
	randNum := rand.Uint64()
	return strconv.FormatUint(randNum, 36)
}

// Reset drops the whole log for one key.
func (l *Limiter) Reset(ctx context.Context, key string) error {
	err := resetScript.Run(ctx, l.rdb, []string{prefix + key}).Err()
	if err != nil {
		return fmt.Errorf("slidingwindowlog: failed to reset key %s: %w %w", key, err, limiter.ErrBackendUnavailable)
	}
	return nil
}

// AllowN decides whether key may spend n of its allowance.
//
// Being refused is not an error: it comes back as a Decision with Allowed
// false. An error means no decision could be made at all.
//
// RetryAfter is exact here, read off the entry that actually has to expire.
// The counter in phase 03 can only estimate the same number, and says so.
func (l *Limiter) AllowN(ctx context.Context, key string, n int64) (limiter.Decision, error) {
	if n <= 0 {
		return limiter.Decision{}, fmt.Errorf("slidingwindowlog: invalid n: %d", n)
	}

	token := newToken()
	keys := []string{prefix + key}
	vals, err := script.Run(ctx, l.rdb, keys,
		l.cfg.Window.Milliseconds(), l.cfg.Limit, n, token,
	).Int64Slice()

	if err != nil {
		return limiter.Decision{}, fmt.Errorf("slidingwindowlog: failed to check limit for key %s: %w %w", key, err, limiter.ErrBackendUnavailable)
	}

	if len(vals) != 4 {
		return limiter.Decision{}, fmt.Errorf("slidingwindowlog: unexpected number of return values from script: %d %w", len(vals), limiter.ErrBackendUnavailable)
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
