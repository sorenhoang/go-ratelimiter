# go-ratelimiter

Five rate limiting algorithms implemented in Go against Redis with Lua, behind one
interface, plus a React harness that fires identical traffic at all of them so the
differences between the algorithms become visible instead of theoretical.

This is a learning repository. The goal is not a library to import — it is to write
each algorithm by hand, hit the real gotchas, and end up with a comparison table
backed by actual benchmarks.

> **Status: complete.** Five algorithms, six implementations, a harness that
> fires one traffic pattern at all of them and draws the answers on one axis,
> and measurements for the two columns that decide between them. The build
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
| Leaky Bucket — meter | O(1) — two hash fields, one of them fractional | Exact | The whole capacity, same as above | Exact — the overflow over the rate |
| Leaky Bucket — queue | O(capacity) — one waiting caller per place | Exact | None — nothing is refused until the queue itself is full | Not applicable: it does not refuse, it waits |

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

### What a decision costs

`make bench` against a local Redis, and `TestMemoryPerKey` asking Redis what one
busy caller occupies after 2000 requests:

| | ns per decision | allocs | bytes in Redis, per key |
|---|---|---|---|
| *a round trip doing nothing* | *135,136* | *6* | — |
| Fixed Window Counter | 145,526 | 18 | **72** |
| Sliding Window Log | 151,112 | 20 | **196,392** |
| Sliding Window Counter | 149,998 | 18 | **72** |
| Token Bucket | 141,068 | 18 | **112** |
| Leaky Bucket — meter | 147,556 | 18 | **104** |

Two things fall out of that, and the second is the useful one.

**Nobody here is slow.** The round trip is 135µs and the scripts add six to
sixteen on top of it. Between the limiters the spread is about 10µs against a
run-to-run variance of 6µs on a single benchmark, so the ordering above is
suggestive at best and not something to choose on.

**The log costs 2700× the memory, and nothing else.** It is not slower — sorted
set operations are logarithmic and the set had to reach 2000 entries before the
number above was taken. The entire price of being exact is the last column.
That is the trade, stated in bytes: one caller, one key, 196KB against 72.

### Which one to reach for

| If you want to | Use |
|---|---|
| The cheapest thing that mostly works | **Fixed Window Counter** — and accept that a client can take double the limit across a boundary |
| An exact answer, and you can afford the memory | **Sliding Window Log** |
| An exact-enough answer at O(1), which is what most APIs ship | **Sliding Window Counter** — about 7% of decisions differ from exact when traffic crowds the limit, and it errs strict |
| To let a quiet client spend what it saved up | **Token Bucket**, or the **Leaky Bucket meter**, which is the same algorithm read from the other side |
| To smooth traffic rather than reject it | **Leaky Bucket queue** — the only one here that makes callers wait instead of turning them away |

Two of the five are the same algorithm. `TestDuality_MeterMatchesTokenBucket`
replays a fixed sequence through the token bucket and the leaky bucket meter at a
matching capacity and rate, and they agree on every verdict, every `Remaining`
and every `RetryAfter`, with `level + tokens` summing to the capacity throughout.
Pick whichever reads better for the question you are asking — "how full is this
client's bucket" or "how much credit is left".

The queue is the odd one out, and the only place the shared interface stops. It
does not implement `limiter.Limiter`, because `AllowN` asks "may this proceed?"
and answers now while `Wait` answers "not yet" and then "now". It is also the
only one that is not per caller: each queue owns a goroutine and a ticker, so one
per client would grow without bound, and it therefore smooths the aggregate flow
through an endpoint rather than any one client's share.

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

### The harness

With the server running, in another terminal:

```bash
cd web && npm install && npm run dev
```

Then open http://localhost:5173. Vite forwards `/api` to the Go server, so
there is no CORS to configure and the server needs no development mode.

One tab per limiter, and a **Compare** tab that fires a single traffic pattern
at all six at once and stacks the timelines on a shared axis. Four requests a
second for five seconds looks like this:

| | allowed / refused |
|---|---|
| Fixed Window Counter | 10 / 10 — in two blocks, because the whole quota returns at once when the slot rolls |
| Sliding Window Log | 5 / 15 — five, then nothing until the first of them ages out |
| Sliding Window Counter | 7 / 13 — between the two, which is what an approximation should look like |
| Token Bucket | 8 / 12 |
| Leaky Bucket — meter | 8 / 12 — the same row twice, which is the duality made visible |
| Leaky Bucket — queue | 20 / 0, and the last caller waited 5.0s |

The last row is drawn differently on purpose. The queue refuses nothing until
it is full; it makes callers wait, so its marks carry a bar for the wait and its
headline number is the longest one rather than a count of refusals.

```bash
cd web && npm run build    # tsc -b, which fails on an unused symbol
cd web && npm run test     # the traffic scheduler
cd web && npm run lint
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
- [x] **01** `Limiter` interface + Fixed Window + middleware + first tests
- [x] **02** Sliding Window Log
- [x] **03** Sliding Window Counter + divergence test
- [x] **04** Token Bucket
- [x] **05** Leaky Bucket — Redis meter + pure-Go queue
- [x] **06** React harness with the Compare tab
- [x] **07** Benchmarks — latency, allocations, and bytes in Redis per key
