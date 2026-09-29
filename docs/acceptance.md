# Acceptance criteria

One section per phase. A phase's criteria are written **before** its first line of code,
and the phase is not done until every box is ticked by an actually-executed check.

---

## Phase 00 — Scaffold

Verified retroactively; the criteria were not written up front, which is the gap this
file exists to close.

- [x] `go build ./...` and `go vet ./...` clean, `gofmt -l .` empty
- [x] `make up` blocks until Redis answers PING (healthcheck + `--wait`)
- [x] Redis reachable → `GET /healthz` returns `200 {"status":"ok"}`
- [x] Redis stopped → `503`, and **the server process stays alive**
- [x] Redis restarted → `200` again with no server restart (pool reconnects)
- [x] `POST /healthz` → `405` with `Allow: GET, HEAD`
- [x] Unknown path → `404`
- [x] `ADDR=:9090` honoured; `ADDR=` (set but empty) falls back to `:8080`
- [x] `Ctrl-C` logs `shutting down` and exits without panic
- [x] **Drain proof**: with a 3s sleep in the handler, a request in flight during
      `Ctrl-C` completes with `200` before the process exits

Both defects carried forward from this phase are now closed: `httpx.WriteJSON`
replaced `http.Error`, and `golangci-lint` 2.14.0 is installed and green.

---

## Phase 01 — Limiter interface + Fixed Window Counter

### Contract

- [x] `internal/limiter/limiter.go` defines `Decision`, `Limiter`, and a sentinel
      `ErrBackendUnavailable`
- [x] That file imports **no** Redis package — it is the abstract contract, the driver
      lives in each sub-package
- [x] `fixedwindow` asserts conformance at compile time:
      `var _ limiter.Limiter = (*Limiter)(nil)`

### Algorithm

- [x] One `EVAL` per decision. No read-from-Go-then-write-to-Redis anywhere — that is a
      race by construction
- [x] The window boundary is derived from `redis.call('TIME')`, never from a timestamp
      passed down by Go
- [x] Key expiry does not slide: TTL is the remainder of the current window, or is set
      only on the call that creates the key
- [x] Redis unreachable → `AllowN` returns `ErrBackendUnavailable`, wrapped so
      `errors.Is` matches

### Unit tests (miniredis, no Docker)

- [x] `limit=5` → five allowed, sixth denied
- [x] Advancing past the window boundary frees the full quota again
- [x] **Boundary burst**: five at the end of window N plus five at the start of N+1
      lands ten requests inside less than one window. This test passes — it documents
      the algorithm's flaw rather than hiding it
- [x] The key is gone once its window has elapsed
- [x] `n > 1` is rejected when it would cross the limit, not silently clamped
- [x] `Remaining`, `ResetAfter` and `RetryAfter` are asserted, not just `Allowed`
- [x] Redis down is covered, not only the happy path

### HTTP layer

- [x] `RateLimit-Limit`, `RateLimit-Remaining`, `RateLimit-Reset` on every response,
      allowed or denied — `TestRateLimit_AllowedSetsHeadersAndCallsNext`, plus a
      live curl run against the server
- [x] `Retry-After` on `429` only — and `1`, not `0`, for a sub-second wait
- [x] `429` body is JSON **and** `Content-Type: application/json` — this is the
      Phase 00 defect being fixed, so `http.Error` cannot be used
- [x] `FailOpen: true` → request passes when Redis is down;
      `FailOpen: false` → `503`. Both directions covered by a test
      > Three cases in `TestRateLimit_FailOpenOnlyForBackendOutage`, including the
      > one that matters most: a non-backend error never opens the gate, even
      > with FailOpen set.
- [x] The rate limit key comes from a `KeyFunc`, so IP and API key are both reachable
      without touching the middleware

### Integration (real Redis, `//go:build integration`)

- [x] The same Lua script that passes under miniredis passes against `redis:7-alpine`
      > Runs against the compose Redis via `REDIS_ADDR` rather than testcontainers.
      > Same proof, one fewer dependency; revisit if CI needs its own instance.
- [x] `make test-integration` green

### Carried over from step 3

- [x] `TestReset_ReturnsBackendUnavailableWhenRedisIsDown` asserts
      `errors.Is(..., ErrBackendUnavailable)`. Mutation-checked: dropping the sentinel
      `%w` from `Reset` turns the test red
- [x] Test client sets `MaxRetries: -1` — suite went from 3.68s to 0.65s

### Gate

- [x] `go vet ./...`, `gofmt -l .` and `make test` clean
- [x] `make lint` green — requires installing `golangci-lint` first

---

## Phase 02 — Sliding Window Log

### Contract

- [x] `slidingwindowlog` asserts conformance at compile time:
      `var _ limiter.Limiter = (*Limiter)(nil)`
- [x] **Nothing in `internal/limiter`, `internal/httpx` or `internal/api` changes.**
      A second algorithm dropping in without touching the abstraction is the proof
      that phase 01 drew the seams in the right place. If any of those files needs
      an edit, say why rather than quietly making it

### Algorithm

- [x] One `EVAL` per decision, clock from `redis.call('TIME')`
- [x] Sorted set members are unique per request. Using the timestamp as the member
      makes two requests in the same millisecond overwrite each other, the count
      runs low, and traffic leaks past the limit
- [x] The key carries a TTL, so a caller that goes quiet stops costing memory
- [x] `RetryAfter` is derived from the entry that actually has to expire, not
      approximated — this algorithm knows the answer exactly, unlike phase 03

### Unit tests (miniredis)

- [x] `limit=5` → five allowed, sixth denied
- [x] **Slides, not steps**: after the window has half passed, quota frees one
      request at a time rather than all at once. This is the behaviour fixed window
      cannot produce
- [x] **Contrast test**: the exact scenario from
      `TestAllowN_AllowsDoubleLimitAcrossWindowBoundary` — five requests at the end
      of a window, five just after — is **denied** here. Same input, opposite
      verdict, and the reason this algorithm is in the repo
- [x] Two requests inside the same millisecond are both counted
      > Asserted through the denial of the next request, not through `Remaining`:
      > that number is arithmetic on the count read before the write, so it reads
      > correctly even when two entries collide. Mutation-checked.
- [x] `n > 1` is rejected when it would cross the limit, not silently clamped
- [x] `Remaining`, `ResetAfter` and `RetryAfter` asserted against exact values
- [x] Redis down → `ErrBackendUnavailable`, for both `AllowN` and `Reset`

### HTTP

- [x] `POST /api/limiters/slidingwindowlog/check` works with no middleware or
      router changes beyond adding the limiter to the slice in `main.go`
      > Confirmed: `cmd/server/main.go` was the only file touched. Live, the two
      > limiters answer the same 7 requests with Reset 1 vs 10 and retry_after
      > 1 vs 10 — fixed window returns the whole quota when its slot rolls,
      > the log frees one entry at a time.

### Integration (real Redis)

- [x] `make test-integration` green, covering the same-millisecond case that
      miniredis might resolve differently from real Redis
      > A tight loop on real Redis produced 5 entries across 3 distinct
      > milliseconds, so the collision case was genuinely exercised. The test
      > reads the sorted set directly rather than trusting Remaining.
      > Mutation-checked: a constant token leaves 1 entry instead of 5.

### Gate

- [x] `go vet`, `gofmt`, `make test`, `make lint` all clean
- [x] README comparison table gains a row for this pattern, including its
      O(limit) memory cost — the reason production usually picks phase 03 instead

---

## Phase 03 — Sliding Window Counter

The approximation that ships. Two counters instead of one entry per request,
so O(1) memory, at the cost of an estimate rather than an exact answer.

### Contract

- [x] `slidingwindowcounter` asserts conformance at compile time
- [x] **Nothing in `internal/limiter`, `internal/httpx` or `internal/api` changes.**
      Two phases running says the seams hold; a third says it was not luck

### Algorithm

- [x] One `EVAL` per decision, clock from `redis.call('TIME')`
- [x] Reads exactly two counters — the current window and the one before it —
      and weights the previous one by how much of it is still in view
- [x] TTL is **`2 * window`**, not `window`. The previous counter has to outlive
      its own window or the current one has nothing to weight
- [x] The weighted estimate keeps its fraction through the comparison. The
      verdict is decided inside Lua, so unlike phase 04 no float has to cross
      back to Go — but rounding the estimate before comparing flips decisions at
      the boundary: an estimate of `4.5` against `limit 5, cost 1` must be
      refused, where a floored `4` would be admitted
- [x] `RetryAfter` is an approximation here, unlike phase 02, and the code says
      so rather than implying a precision it does not have

### Unit tests (miniredis)

- [x] `limit=5` → five allowed, sixth denied
- [x] The fixed-window boundary burst is **denied**, as in phase 02
- [x] Weight decay: fill the previous window, advance half a window, and the
      remaining quota is about half the limit
- [x] A case where truncating the estimate instead of carrying the fraction would
      flip the verdict — the test that fails if the float is lost
- [x] The previous window's counter is still readable from the current window,
      and both are gone once two windows have passed
- [x] `n > 1` is rejected when it would cross the limit, not silently clamped
- [x] Redis down → `ErrBackendUnavailable`, for both `AllowN` and `Reset`

### Divergence against phase 02

- [x] One deterministic request sequence is replayed through both this limiter
      and `slidingwindowlog`, and the share of decisions that disagree is
      asserted below a threshold. The threshold is chosen **after** measuring,
      not guessed beforehand, and the test logs the measured rate so a
      regression shows up as a number rather than a pass/fail
      > Measured over 3000 requests against `limit 10` per second:
      > **6.87%** when arrivals crowd the limit (~8/s), **0.03%** when they run
      > well under it (~3/s). The plan's "under 1% on real traffic" holds only
      > for the second shape; pressed against the limit the estimate is an order
      > of magnitude worse. Thresholds set at 8% and 1% from these numbers.
- [x] The direction of the error is recorded: whether the estimate is more
      likely to admit traffic it should refuse, or refuse traffic it should admit
      > It errs **strict** in both shapes: 113 refusals the log would have
      > admitted against 93 admissions it would have refused, in the crowded
      > case. So the approximation is conservative rather than a hole to drive
      > through — worth knowing before choosing it over phase 02.

### HTTP, integration, gate

- [x] `POST /api/limiters/slidingwindowcounter/check` works with no change
      beyond adding the limiter to the slice in `main.go`
      > Confirmed: `cmd/server/main.go` was again the only file touched. Live,
      > the three answer the same seven requests with Reset 6 / 10 / 16 and
      > Retry-After 6 / 10 / 6 — each one's idea of "wait this long" falling
      > straight out of how it stores state.
- [x] `make test-integration` green against real Redis
      > Checks the two places this algorithm would drift between miniredis and
      > the real server: the TTL that keeps the previous counter alive, read
      > back with PTTL, and the weighting itself on a real clock — the quota
      > stays shut just into the next window and reopens three quarters through
      > it. Mutation-checked: shortening the TTL to one window turns both red.
- [x] `go vet`, `gofmt`, `make test`, `make lint` all clean
- [x] README comparison table gains a row, including the measured divergence

---

## Phase 04 — Token Bucket

The first one where a burst is a feature rather than a defect, and the first
whose state is fractional.

### Contract

- [x] `tokenbucket` asserts conformance at compile time
- [x] `Config` is **not** `{Limit, Window}`. It is a capacity and a refill rate,
      and `Decision.Limit` carries the capacity. Three phases sharing a config
      shape was the coincidence; the interface has to survive one that does not
- [ ] **Nothing in `internal/limiter`, `internal/httpx` or `internal/api`
      changes.** Especially here, since this is the phase that tests whether
      `Decision` was the right abstraction or merely a fitting one

### Algorithm

- [x] One `EVAL` per decision, clock from `redis.call('TIME')`
- [x] Refill is lazy — computed from elapsed time on read. No ticker, no
      background goroutine, nothing to supervise
- [x] A key that does not exist means a **full** bucket. Letting a missing hash
      read as zero would refuse every caller's first ever request
- [x] **Fractional tokens survive a round trip through Redis.** This is the phase
      where the float is persisted rather than compared and discarded, so how
      Redis stores and returns it must be established **by test**, not assumed.
      Losing `0.7` of a token every call silently starves the bucket
- [x] TTL is the time to refill from empty, so a caller that goes quiet costs
      nothing and comes back to a full bucket — which is the same answer the
      algorithm would have given anyway
- [x] `RetryAfter` is exact here: the deficit divided by the rate

### Unit tests (miniredis)

- [x] **Burst**: `capacity` requests back to back are all admitted, the next is
      refused. No other limiter in this repo does that on purpose
- [x] Refill: advance a known time and exactly the expected number more fit
- [x] Ceiling: the refill stops at the capacity
      > Rewritten after a mutation survived the first version. Advancing an hour
      > also blows past the TTL, so the key expires and the next caller meets a
      > fresh full bucket through the missing-key branch — the ceiling was never
      > reached. It now spends two, waits four seconds against a five second
      > TTL, and checks that three in hand plus four refilled is five, not seven.
- [x] **Fractional refill**: at half a token per second, one second buys half a
      token and a cost of 1 is refused; another second buys the rest and it
      passes. This is the test that fails if the fraction is lost anywhere
- [x] A caller's first ever request is admitted
- [x] `n > 1` is rejected when it would overdraw, not silently clamped
- [x] `RetryAfter` asserted against the exact deficit over rate
- [x] Redis down → `ErrBackendUnavailable`, for both `AllowN` and `Reset`

### Against the earlier phases

- [x] A test that records what the others cannot do: the same burst of
      `capacity` requests that token bucket admits by design is refused by
      `slidingwindowlog` at an equivalent limit
      > Both sustain one request per second. A burst of ten arrives: the bucket
      > admits all ten, the log admits one. Near-identical throughput over a
      > minute, opposite answers to a caller who saved up.

### HTTP, integration, gate

- [ ] `POST /api/limiters/tokenbucket/check` works with no change beyond adding
      the limiter to the slice in `main.go`
- [ ] `make test-integration` green, including the fractional case against real
      Redis rather than miniredis' Lua interpreter
- [ ] `go vet`, `gofmt`, `make test`, `make lint` all clean
- [ ] README comparison table gains a row, including the burst it allows on
      purpose and the memory that buys
