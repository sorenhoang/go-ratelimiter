# go-ratelimiter

Five rate limiting algorithms implemented in Go against Redis with Lua, behind one
interface, plus a React harness that fires identical traffic at all of them so the
differences between the algorithms become visible instead of theoretical.

This is a learning repository. The goal is not a library to import — it is to write
each algorithm by hand, hit the real gotchas, and end up with a comparison table
backed by actual benchmarks.

> **Status: four of five algorithms built.** Fixed Window Counter, Sliding
> Window Log, Sliding Window Counter and Token Bucket all run, behind one
> interface and one HTTP middleware. Only Leaky Bucket is left. The build
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
| Sliding Window Log | **O(limit)** — one sorted set entry per request | Exact by construction | None; the window moves with the requests | Exact — read off the entry that has to expire |
| Sliding Window Counter | O(1) — two integers | **6.9% of decisions differ from the log** when traffic crowds the limit, 0.03% when it does not | None | Approximate — solves for when the falling estimate clears the limit |
| Token Bucket | O(1) — two hash fields, one of them fractional | Exact | **The whole capacity, on purpose** — a caller who was quiet may spend the quiet time | Exact — the deficit over the rate, and the only one a client can act on precisely |

The same 7 requests, every limiter set to the same sustained rate of five per
ten seconds:

| | `fixedwindow` | `slidingwindowlog` | `slidingwindowcounter` | `tokenbucket` |
|---|---|---|---|---|
| `Retry-After` on 429 | `6` | `10` | `5` | **`2`** |

Each answer falls straight out of how the limiter stores state. Fixed window
points at its next boundary and then returns the whole allowance at once. The
log waits for its oldest entry to age out, which frees exactly one slot. The
counter solves for when its estimate decays past the limit. The bucket knows
that at half a token per second the next one lands in two seconds — the only
answer here a client can act on precisely, and the only one that does not
invite a fresh burst the moment the wait is over.

`TestAllowN_DeniesTheBurstFixedWindowWouldAllow` exists in both phase 02 and
phase 03: one request sequence, the opposite verdict from phase 01.

### What the approximation actually costs

`TestDivergence_AgainstSlidingWindowLog` replays one request stream through the
counter and through the log, which is exact by construction, and counts the
disagreements. Over 3000 requests against a limit of 10 per second:

| Arrival rate | Decisions that differ |
|---|---|
| ~3/s — well under the limit | **0.03%** |
| ~8/s — crowding the limit | **6.87%** |

The usual claim that this lands within a percent of exact holds only for the
first shape. Pressed against the limit it is an order of magnitude worse — and
that is precisely when a rate limiter is doing its job, so the number to quote
is the second one.

It errs **strict**: 113 refusals the log would have admitted against 93
admissions it would have refused. The approximation is conservative rather than
a hole to drive through, which is what makes trading phase 02's O(limit) memory
for it a question of user experience rather than of security.

### The burst nobody else allows

`TestBurst_TokenBucketAdmitsWhatTheLogRefuses` puts a token bucket and a sliding
window log at the same sustained rate — one request per second — and sends a
burst of ten at once:

| | Admitted |
|---|---|
| Token Bucket, capacity 10 refilling 1/s | **10** |
| Sliding Window Log, limit 1 per second | **1** |

Over a minute the two pass roughly the same traffic. They disagree entirely
about a caller who has been quiet and arrives with a backlog. Every other
limiter here treats that burst as the thing to prevent; the bucket treats the
quiet time as credit the caller earned.

That is also the phase where the state is fractional. At half a token per second
Redis holds `0.404` of a token between calls, read straight out of the hash in
the integration test. Round it away anywhere and a bucket refilling slower than
one token per second never fills at all — the caller is locked out for good,
with no error raised anywhere.

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
- [x] **03** Sliding Window Counter + divergence test
- [x] **04** Token Bucket
- [ ] **05** Leaky Bucket — Redis meter + pure-Go queue
- [ ] **06** React harness with the Compare tab
- [ ] **07** Benchmarks + comparison table
