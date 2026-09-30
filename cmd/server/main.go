package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/sorenhoang/go-ratelimiter/internal/api"
	"github.com/sorenhoang/go-ratelimiter/internal/httpx"
	"github.com/sorenhoang/go-ratelimiter/internal/limiter"
	"github.com/sorenhoang/go-ratelimiter/internal/limiter/fixedwindow"
	"github.com/sorenhoang/go-ratelimiter/internal/limiter/leakybucket"
	"github.com/sorenhoang/go-ratelimiter/internal/limiter/slidingwindowcounter"
	"github.com/sorenhoang/go-ratelimiter/internal/limiter/slidingwindowlog"
	"github.com/sorenhoang/go-ratelimiter/internal/limiter/tokenbucket"
)

const shutdownTimeout = 10 * time.Second

type healthResponse struct {
	Status string `json:"status"`
}

const (
	limitPerWindow = 5
	window         = 10 * time.Second

	// The same sustained rate as the window limiters above -- five per ten
	// seconds -- so a live comparison differs only in how each one treats a
	// caller who arrives with a backlog.
	bucketCapacity        = limitPerWindow
	bucketRefillPerSecond = 0.5

	// The queue shapes the aggregate flow through its endpoint rather than any
	// one caller's share, so its rate is a throughput rather than a per-client
	// allowance. Capacity is how many callers may be kept waiting.
	queueReleasePerSecond = 2
	queueCapacity         = 20
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))

	rdb := redis.NewClient(&redis.Options{
		Addr: env("REDIS_ADDR", "localhost:6379"),
	})
	defer func() {
		if err := rdb.Close(); err != nil {
			log.Error("failed to close redis client", "error", err)
		}
	}()

	fw, err := fixedwindow.New(rdb, fixedwindow.Config{
		Limit:  limitPerWindow,
		Window: window,
	})
	if err != nil {
		log.Error("failed to create fixed window limiter", "error", err)
		os.Exit(1)
	}

	swl, err := slidingwindowlog.New(rdb, slidingwindowlog.Config{
		Limit:  limitPerWindow,
		Window: window,
	})
	if err != nil {
		log.Error("failed to create sliding window log limiter", "error", err)
		os.Exit(1)
	}

	swc, err := slidingwindowcounter.New(rdb, slidingwindowcounter.Config{
		Limit:  limitPerWindow,
		Window: window,
	})
	if err != nil {
		log.Error("failed to create sliding window counter limiter", "error", err)
		os.Exit(1)
	}

	tb, err := tokenbucket.New(rdb, tokenbucket.Config{
		Capacity:        bucketCapacity,
		RefillPerSecond: bucketRefillPerSecond,
	})
	if err != nil {
		log.Error("failed to create token bucket limiter", "error", err)
		os.Exit(1)
	}

	meter, err := leakybucket.NewMeter(rdb, leakybucket.MeterConfig{
		Capacity:      bucketCapacity,
		LeakPerSecond: bucketRefillPerSecond,
	})
	if err != nil {
		log.Error("failed to create leaky bucket meter", "error", err)
		os.Exit(1)
	}

	queue, err := leakybucket.NewQueue(leakybucket.QueueConfig{
		ReleasePerSecond: queueReleasePerSecond,
		Capacity:         queueCapacity,
	})
	if err != nil {
		log.Error("failed to create leaky bucket queue", "error", err)
		os.Exit(1)
	}
	// Closed before the Redis client, since callers still waiting have to be
	// released before anything they might touch on the way out goes away.
	defer func() {
		if err := queue.Close(); err != nil {
			log.Error("failed to close queue", "error", err)
		}
	}()

	mux := api.New([]limiter.Limiter{fw, swl, swc, tb, meter}, true)
	api.MountQueue(mux, queue)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := rdb.Ping(r.Context()).Err(); err != nil {
			log.Error("redis ping failed", "err", err)
			httpx.WriteJSON(w, http.StatusServiceUnavailable, httpx.ErrorBody{
				Error: "redis unreachable",
			})
			return
		}

		httpx.WriteJSON(w, http.StatusOK, healthResponse{Status: "ok"})
	})

	srv := &http.Server{
		Addr:              env("ADDR", ":8080"),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	// Serve until SIGINT/SIGTERM, then drain in-flight requests.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Info("listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("server error", "error", err)
			stop()
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("failed to shutdown server", "error", err)
	}

}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
