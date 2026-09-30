// Package api mounts the limiters and the queue on one mux.
//
// The two are mounted by different routes on purpose. Everything New mounts
// goes behind the rate limit middleware, which asks for a verdict and either
// calls the handler or writes 429. A queue has no verdict to give -- it waits,
// then lets the caller through -- so MountQueue gives it a route of its own
// rather than forcing a limiter whose AllowN blocks or a middleware that
// sometimes waits.
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

type limiterInfo struct {
	Name     string `json:"name"`
	CheckURL string `json:"check_url"`
	ResetURL string `json:"reset_url"`
}

type limitersResponse struct {
	Limiters []limiterInfo `json:"limiters"`
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
	info := make([]limiterInfo, 0, len(limiters))

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

		info = append(info, limiterInfo{
			Name:     l.Name(),
			CheckURL: base + "/check",
			ResetURL: base + "/reset",
		})
	}

	// Built from the same loop that mounts the routes, so a client discovers
	// what is actually served rather than a list that can drift out of step
	// with it.
	mux.Handle("GET /api/limiters", listHandler(info))

	return mux
}

// listHandler reports the mounted limiters.
//
// It carries no configured limit, deliberately. The Limiter interface does not
// expose one, and adding a method for this single caller would be the wrong
// trade -- a client learns the real limit from the RateLimit-Limit header on
// its first request, which is what the server actually enforces rather than
// what a config claims.
func listHandler(info []limiterInfo) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, limitersResponse{Limiters: info})
	})
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
