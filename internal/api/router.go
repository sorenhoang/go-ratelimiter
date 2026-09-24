package api

import (
	"net/http"

	"github.com/sorenhoang/go-ratelimiter/internal/httpx"
	"github.com/sorenhoang/go-ratelimiter/internal/limiter"
)

type checkResponse struct {
	Status  string `json:"status"`
	Limiter string `json:"limiter"`
}

type resetResponse struct {
	Status string `json:"status"`
	Key    string `json:"key"`
}

// New builds the API mux, mounting two routes for each limiter.
//
// Routes are registered per limiter rather than behind a {pattern} wildcard.
// The middleware binds at wiring time, one closure per limiter; a wildcard
// would force a lookup inside the handler and that model would collapse.
// Explicit registration also fails loudly: two limiters reporting the same
// Name() panic here at startup instead of silently overwriting one another.
//
// It returns the concrete mux so the caller can mount its own routes, such as
// a health endpoint, on the same tree.
func New(limiters []limiter.Limiter, failOpen bool) *http.ServeMux {
	mux := http.NewServeMux()

	for _, l := range limiters {
		cfg := httpx.Config{
			Limiter:  l,
			KeyFunc:  httpx.KeyByIP,
			FailOpen: failOpen,
		}
		base := "/api/limiters/" + l.Name()

		mux.Handle("POST "+base+"/check", httpx.RateLimit(cfg)(checkHandler(l)))
		// Deliberately not rate limited: a reset you cannot reach once you have
		// hit the limit is no use for the thing it exists to do.
		mux.Handle("POST "+base+"/reset", resetHandler(l))
	}

	return mux
}

// checkHandler is the protected endpoint. Reaching it means the limiter let the
// request through, so it knows nothing about rate limiting itself.
func checkHandler(l limiter.Limiter) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, checkResponse{
			Status:  "ok",
			Limiter: l.Name(),
		})
	})
}

// resetHandler clears the counter for one key so manual testing need not wait
// out a whole window. The key comes from ?key=, defaulting to the caller's own
// IP so curl needs no arguments.
func resetHandler(l limiter.Limiter) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.URL.Query().Get("key")
		if key == "" {
			key = httpx.KeyByIP(r)
		}

		if err := l.Reset(r.Context(), key); err != nil {
			httpx.WriteJSON(w, http.StatusServiceUnavailable, httpx.ErrorBody{
				Error: "reset failed",
			})
			return
		}

		httpx.WriteJSON(w, http.StatusOK, resetResponse{
			Status: "reset",
			Key:    key,
		})
	})
}
