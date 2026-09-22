# go-ratelimiter

Five rate limiting algorithms implemented in Go against Redis with Lua, behind one
interface, plus a React harness that fires identical traffic at all of them so the
differences between the algorithms become visible instead of theoretical.

This is a learning repository. The goal is not a library to import — it is to write
each algorithm by hand, hit the real gotchas, and end up with a comparison table
backed by actual benchmarks.

> **Status: planning.** No code yet. The full build plan lives in
> [`docs/plan.html`](docs/plan.html).

## The algorithms

Built in this order. Fixed Window comes first because it drags the whole scaffold —
interface, middleware, headers, Lua helpers, test harness — into existence on the
easiest algorithm.

| # | Pattern | Redis structure | Why it is here |
|---|---------|-----------------|----------------|
| 01 | Fixed Window Counter | String counter per window | The one that is wrong on purpose — demonstrates the 2× boundary burst |
| 02 | Sliding Window Log | Sorted set of timestamps | Exact by construction; becomes the ground truth for the others |
| 03 | Sliding Window Counter | Two string counters, weighted | The approximation that ships — Cloudflare runs this |
| 04 | Token Bucket | Hash `{tokens, last_refill}` | Burst is a feature here, not an accident |
| 05 | Leaky Bucket | Hash `{level, last_leak}` + a pure-Go queue variant | Smooths traffic; the queue variant is the Go concurrency exercise |

## Design decisions

- **The clock lives in Redis.** Every script reads `redis.call('TIME')` rather than
  receiving `time.Now()` from Go. With several application servers, clock skew means
  each node computes a different window boundary for the same key.
- **One module, one server, five packages.** Each algorithm is its own package
  implementing a shared `Limiter` interface, all mounted on a single server at `:8080`.
- **Limiters take `redis.Scripter`, not `*redis.Client`** — the interface go-redis
  already exports, so `Client`, `ClusterClient`, `Ring` and `Pipeline` all work.
- **Redis being down is a policy question, not an assumption.** `AllowN` returns
  `ErrBackendUnavailable`; the middleware decides fail-open or fail-closed via config.
- **No router library.** Since Go 1.22 the standard `ServeMux` matches
  `"POST /api/limiters/{pattern}/check"` and exposes path values.

## Layout

```
cmd/server/            wiring + graceful shutdown
internal/limiter/      the Limiter interface and the five implementations
internal/httpx/        RateLimit middleware, key extraction, RateLimit-* headers
internal/api/          router and handlers
web/                   Vite + React + TS harness
docs/plan.html         the full build plan
```

## Running it

Planned interface — none of this works yet.

```bash
docker compose up -d        # redis:7-alpine
make run                    # server on :8080
make test                   # unit tests, miniredis, no Docker needed
make test-integration       # against real Redis, behind //go:build integration
make bench                  # latency and allocations per AllowN
```

```bash
cd web && npm install && npm run dev
```

## Testing

Two layers, both kept on purpose. `miniredis` runs Lua through a Go interpreter rather
than Redis's own, so float formatting, `TIME` resolution and reply types diverge at the
edges — unit tests give a fast loop, integration tests prove the scripts behave on real
Redis.

The most valuable test in the repository compares Sliding Window Counter against
Sliding Window Log on one generated traffic sequence and asserts the divergence stays
under a threshold. That number is the whole point of the approximation.

## Progress

- [x] **00** Scaffold — go.mod, docker-compose, Makefile, golangci-lint
- [ ] **01** `Limiter` interface + Fixed Window + middleware + first tests
- [ ] **02** Sliding Window Log
- [ ] **03** Sliding Window Counter + divergence test
- [ ] **04** Token Bucket
- [ ] **05** Leaky Bucket — Redis meter + pure-Go queue
- [ ] **06** React harness with the Compare tab
- [ ] **07** Benchmarks + comparison table
