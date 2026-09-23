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
- [ ] Unknown path → `404`
- [ ] `ADDR=:9090` honoured; `ADDR=` (set but empty) falls back to `:8080`
- [ ] `Ctrl-C` logs `shutting down` and exits without panic
- [ ] **Drain proof**: with a 3s sleep in the handler, a request in flight during
      `Ctrl-C` completes with `200` before the process exits

Known defects carried forward, to fix in Phase 01:
- `http.Error` sends `Content-Type: text/plain` with a JSON body
- `make lint` cannot run — `golangci-lint` is not installed

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
- [ ] **Boundary burst**: five at the end of window N plus five at the start of N+1
      lands ten requests inside less than one window. This test passes — it documents
      the algorithm's flaw rather than hiding it
      > `TestAllowN_AllowsDoubleLimitAcrossWindowBoundary` exists but advances a full
      > window between the two bursts, so it proves quota reset, not the 2x burst.
      > Needs `advance(59s)` before the first burst and `advance(2s)` between.
- [ ] The key is gone once its window has elapsed
- [ ] `n > 1` is rejected when it would cross the limit, not silently clamped
- [ ] `Remaining`, `ResetAfter` and `RetryAfter` are asserted, not just `Allowed`
      > `Remaining` and `RetryAfter > 0` are covered. `ResetAfter` is never asserted
      > against an exact value, which an aligned `baseTime` makes easy.
- [x] Redis down is covered, not only the happy path

### HTTP layer

- [ ] `RateLimit-Limit`, `RateLimit-Remaining`, `RateLimit-Reset` on every response,
      allowed or denied
- [ ] `Retry-After` on `429` only
- [ ] `429` body is JSON **and** `Content-Type: application/json` — this is the
      Phase 00 defect being fixed, so `http.Error` cannot be used
- [ ] `FailOpen: true` → request passes when Redis is down;
      `FailOpen: false` → `503`. Both directions covered by a test
- [ ] The rate limit key comes from a `KeyFunc`, so IP and API key are both reachable
      without touching the middleware

### Integration (real Redis, `//go:build integration`)

- [ ] The same Lua script that passes under miniredis passes against `redis:7-alpine`
- [ ] `make test-integration` green

### Carried over from step 3

- [x] `TestReset_ReturnsBackendUnavailableWhenRedisIsDown` asserts
      `errors.Is(..., ErrBackendUnavailable)`. Mutation-checked: dropping the sentinel
      `%w` from `Reset` turns the test red
- [x] Test client sets `MaxRetries: -1` — suite went from 3.68s to 0.65s

### Gate

- [ ] `go vet ./...`, `gofmt -l .` and `make test` clean
- [ ] `make lint` green — requires installing `golangci-lint` first
