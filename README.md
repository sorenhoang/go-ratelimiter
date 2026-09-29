# go-ratelimiter

Five rate limiting algorithms implemented in Go against Redis with Lua, behind one
interface, plus a React harness that fires identical traffic at all of them so the
differences between the algorithms become visible instead of theoretical.

This is a learning repository. The goal is not a library to import — it is to write
each algorithm by hand, hit the real gotchas, and end up with a comparison table
backed by actual benchmarks.

> **Status: two of five algorithms built.** Fixed Window Counter and Sliding
> Window Log both run, behind one interface and one HTTP middleware. The build
> plan lives in [`docs/plan.html`](docs/plan.html), and what each phase had to
> prove before it was called done is in [`docs/acceptance.md`](docs/acceptance.md).

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

### Measured so far

Filled in as each one lands, from the tests and from live runs rather than from
the textbook.

| Pattern | Memory per key | Accuracy | The burst it allows | `RetryAfter` |
|---------|----------------|----------|---------------------|--------------|
| Fixed Window Counter | O(1) — one integer | Admits up to **2× the limit** around a boundary | Whole quota returns at once when the slot rolls | Exact, but only ever "until this slot ends" |
| Sliding Window Log | **O(limit)** — one sorted set entry per request | Exact by construction | None; the window moves with the requests | Exact — read off the entry that actually has to expire |

The same 7 requests at `limit=5, window=10s`, nine seconds into a window:

| | `fixedwindow` | `slidingwindowlog` |
|---|---|---|
| `RateLimit-Reset` | `1` | `10` |
| `retry_after` on 429 | `1` | `10` |

Fixed window says "wait one second" because its slot is about to roll and hand
back the entire allowance. The log says "wait ten" because its oldest entry has
to age out, and that frees exactly one slot. Both are right about their own
definition, and `TestAllowN_DeniesTheBurstFixedWindowWouldAllow` pins the
difference down: one request sequence, opposite verdicts.

That O(limit) row is the catch. At `limit=10000` across a million callers the log
is an enormous amount of sorted set, which is why production usually reaches for
the counter approximation in phase 03 instead.

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

```bash
docker compose up -d        # redis:7-alpine
make run                    # server on :8080
make test                   # unit tests, miniredis, no Docker needed
make test-integration       # against real Redis, behind //go:build integration
make bench                  # latency and allocations per AllowN
```

Fire at a limiter and watch the quota drain:

```bash
for i in $(seq 1 7); do curl -s -X POST localhost:8080/api/limiters/fixedwindow/check; echo; done
```

Swap `fixedwindow` for `slidingwindowlog` to see the same requests answered
differently. `POST /api/limiters/{name}/reset` clears a counter so you need not
wait out a window.

The React harness is phase 06 and does not exist yet.

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
- [x] **01** `Limiter` interface + Fixed Window + middleware + first tests
- [x] **02** Sliding Window Log
- [ ] **03** Sliding Window Counter + divergence test
- [ ] **04** Token Bucket
- [ ] **05** Leaky Bucket — Redis meter + pure-Go queue
- [ ] **06** React harness with the Compare tab
- [ ] **07** Benchmarks + comparison table
