package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/sorenhoang/go-ratelimiter/internal/httpx"
	"github.com/sorenhoang/go-ratelimiter/internal/limiter/leakybucket"
)

type waitResponse struct {
	Status   string `json:"status"`
	WaitedMs int64  `json:"waited_ms"`
}

// MountQueue adds the queue's own route.
//
// It is mounted separately from New's limiters, and deliberately so. Everything
// New mounts goes behind the rate limit middleware, which asks a limiter for a
// verdict and either calls the handler or writes 429. A queue has no verdict to
// give: it makes the caller wait and then lets them through. Squeezing it into
// that middleware would mean either a limiter whose AllowN blocks, or a
// middleware that sometimes waits — and both hide the distinction that makes
// the queue worth having.
//
// The parameter is the concrete *leakybucket.Queue rather than an interface.
// There is one queue type, and this handler needs its error values to map them
// to status codes, so an interface here would decouple the call and not the
// import.
func MountQueue(mux *http.ServeMux, q *leakybucket.Queue) {
	mux.Handle("POST /api/queue/wait", queueHandler(q))
}

func queueHandler(q *leakybucket.Queue) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		err := q.Wait(r.Context())
		waited := time.Since(start).Milliseconds()

		switch {
		case err == nil:
			httpx.WriteJSON(w, http.StatusOK, waitResponse{
				Status:   "released",
				WaitedMs: waited,
			})

		case errors.Is(err, leakybucket.ErrQueueFull):
			// The caller sent more than there is room to hold, which is the same
			// thing a limiter means by 429 -- it just arrives without a
			// RateLimit-* header, because a queue has no quota to report.
			httpx.WriteJSON(w, http.StatusTooManyRequests, httpx.ErrorBody{
				Error: "queue is full",
			})

		case errors.Is(err, leakybucket.ErrQueueClosed):
			httpx.WriteJSON(w, http.StatusServiceUnavailable, httpx.ErrorBody{
				Error: "queue is shutting down",
			})

		default:
			// The context was cancelled, so the client has already gone. Writing
			// a response would be shouting at a closed connection; net/http
			// logs a superfluous WriteHeader if the handler tries.
			return
		}
	})
}
