// Package limiter is the contract every rate limiting algorithm in this repo
// satisfies, and the reason a new one can be added without touching anything
// else.
//
// Five algorithms implement it, and four of them arrived after it was written.
// The last of those, tokenbucket, has a config sharing no field with the other
// three — a capacity and a refill rate rather than a limit and a window — and
// needed no change here to carry it. That is the evidence the seam is in the
// right place.
//
// It is also worth knowing where it stops. leakybucket.Queue deliberately does
// not implement Limiter: AllowN asks "may this proceed?" and answers now, while
// a queue answers "not yet" and then "now". Bending one into the other would
// hide the only thing that makes that variant worth having.
//
// This package imports no Redis. The drivers live in the sub-packages, which is
// what lets a test substitute a stub with no backend at all.
package limiter

import (
	"context"
	"errors"
	"time"
)

// Decision is the answer to one rate limit check.
//
// A refusal is a Decision with Allowed false, not an error. An error from
// AllowN means no decision could be reached, which is a different situation
// and the one fail-open exists for.
type Decision struct {
	// Allowed reports whether the request may proceed.
	Allowed bool
	// Limit is the quota in force. It travels in the Decision rather than being
	// read from config so the HTTP layer can write RateLimit-Limit without
	// knowing which algorithm answered.
	Limit int64
	// Remaining is what is left of that quota, in whole units. A partial unit
	// may exist in Redis but cannot pay for a request, so reporting it would
	// overstate what the caller can do.
	Remaining int64
	// ResetAfter is how long until the quota is whole again.
	ResetAfter time.Duration
	// RetryAfter is how long until this particular request would be admitted,
	// and is zero whenever Allowed is true.
	//
	// It is separate from ResetAfter because the two differ once an algorithm
	// refills gradually. A token bucket of ten at one per second, empty, can
	// serve one request in a second while taking ten to fill: Retry-After wants
	// the first number and RateLimit-Reset the second.
	RetryAfter time.Duration
}

// Limiter decides whether a key may spend some of its quota.
//
// Implementations are safe for concurrent use: every decision is one atomic
// operation on the backend rather than a read followed by a write.
type Limiter interface {
	// AllowN reports whether key may spend n units now.
	AllowN(ctx context.Context, key string, n int64) (Decision, error)
	// Name identifies the algorithm, and doubles as its URL segment so the
	// route and the limiter cannot drift apart.
	Name() string
	// Reset clears everything the limiter holds for one key.
	Reset(ctx context.Context, key string) error
}

// ErrBackendUnavailable wraps every error that means the backend could not be
// reached, so a caller can tell an outage apart from a bug.
//
// The distinction is the whole basis of fail-open. Letting any error through
// the open gate would hide a mistake of our own behind the traffic it admitted,
// which is why the middleware checks for this sentinel specifically rather than
// for a non-nil error.
var ErrBackendUnavailable = errors.New("limiter: backend unavailable")
