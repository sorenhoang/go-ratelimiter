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

type Config struct {
	Limit  int64
	Window time.Duration
}

type Limiter struct {
	rdb redis.Scripter
	cfg Config
}

var _ limiter.Limiter = (*Limiter)(nil)

func (l *Limiter) Name() string {
	return "slidingwindowlog"
}

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

func newToken() string {
	randNum := rand.Uint64()
	return strconv.FormatUint(randNum, 36)
}

func (l *Limiter) Reset(ctx context.Context, key string) error {
	err := resetScript.Run(ctx, l.rdb, []string{prefix + key}).Err()
	if err != nil {
		return fmt.Errorf("slidingwindowlog: failed to reset key %s: %w %w", key, err, limiter.ErrBackendUnavailable)
	}
	return nil
}

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
