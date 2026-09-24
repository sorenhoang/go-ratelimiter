package limiter

import (
	"context"
	"errors"
	"time"
)

// Decision represents the result of a rate limit check.
type Decision struct {
	Allowed    bool
	Limit      int64
	Remaining  int64
	ResetAfter time.Duration
	// RetryAfter is the duration after which the client should retry the request.
	RetryAfter time.Duration
}

// Limiter is an interface for rate limiting.
type Limiter interface {
	AllowN(ctx context.Context, key string, n int64) (Decision, error)
	Name() string
	Reset(ctx context.Context, key string) error
}

var ErrBackendUnavailable = errors.New("limiter: backend unavailable")
