package httpx

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/sorenhoang/go-ratelimiter/internal/limiter"
)

// Config wires one limiter into the HTTP layer.
type Config struct {
	// Limiter is the algorithm to consult. It is an interface, so the same
	// middleware serves every pattern in this repo.
	Limiter limiter.Limiter
	// KeyFunc decides what the limit applies to. Defaults to KeyByIP.
	KeyFunc KeyFunc
	// FailOpen lets requests through when the backend is unreachable. Leave it
	// false to reject them instead.
	FailOpen bool
	// Cost is how much quota one request spends. Defaults to 1.
	Cost int64
}

// RateLimit returns middleware that consults cfg.Limiter before calling next.
//
// A denied request never reaches next. When the backend is unreachable and
// cfg.FailOpen is set, the request passes carrying no RateLimit-* headers at
// all: the limit and the remaining quota are unknown here, and inventing
// numbers would lie to the client.
func RateLimit(cfg Config) func(http.Handler) http.Handler {
	// Resolved once at wiring time rather than on every request. A nil KeyFunc
	// panics per request; a zero Cost makes AllowN reject every call with
	// "invalid n", which together with FailOpen would quietly turn the whole
	// rate limiter into a no-op.
	keyFn := cfg.KeyFunc
	if keyFn == nil {
		keyFn = KeyByIP
	}
	cost := cfg.Cost
	if cost <= 0 {
		cost = 1
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			decision, err := cfg.Limiter.AllowN(r.Context(), keyFn(r), cost)
			if err != nil {
				// Only a backend outage may fail open. Any other error is a bug
				// on our side, and letting traffic through to hide it is worse
				// than saying so.
				if errors.Is(err, limiter.ErrBackendUnavailable) && cfg.FailOpen {
					next.ServeHTTP(w, r)
					return
				}
				WriteJSON(w, http.StatusServiceUnavailable, ErrorBody{
					Error: "rate limiter unavailable",
				})
				return
			}

			// Headers must be set before WriteJSON: it calls WriteHeader, and
			// after that the header map is frozen and further writes are
			// silently dropped.
			h := w.Header()
			h.Set("RateLimit-Limit", strconv.FormatInt(decision.Limit, 10))
			h.Set("RateLimit-Remaining", strconv.FormatInt(decision.Remaining, 10))
			h.Set("RateLimit-Reset", strconv.FormatInt(ceilSeconds(decision.ResetAfter), 10))

			if !decision.Allowed {
				retry := ceilSeconds(decision.RetryAfter)
				h.Set("Retry-After", strconv.FormatInt(retry, 10))
				WriteJSON(w, http.StatusTooManyRequests, ErrorBody{
					Error:      "rate limit exceeded",
					RetryAfter: retry,
				})
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// ceilSeconds rounds a wait up to whole seconds.
//
// Truncating instead would report 0 for anything under a second, telling the
// client to retry immediately. It gets denied again, reads 0 again, and becomes
// a busy loop hammering the server the limiter exists to protect.
func ceilSeconds(d time.Duration) int64 {
	if d <= 0 {
		return 0
	}
	return int64((d + time.Second - 1) / time.Second)
}
