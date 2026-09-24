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
