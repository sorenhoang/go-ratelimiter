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
	"github.com/sorenhoang/go-ratelimiter/internal/limiter/slidingwindowlog"
)

const shutdownTimeout = 10 * time.Second

type healthResponse struct {
	Status string `json:"status"`
}

const (
	limitPerWindow = 5
	window         = 10 * time.Second
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

	mux := api.New([]limiter.Limiter{fw, swl}, true)
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
