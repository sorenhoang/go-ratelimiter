// The Go server speaks snake_case; these types mirror it exactly rather than
// converting, because the harness only reads and one naming convention across
// the boundary is cheaper than a conversion layer that can disagree with it.

export type LimiterInfo = {
  name: string
  check_url: string
  reset_url: string
}

/** The queue has no entry in /api/limiters: it is not a Limiter. */
export const QUEUE_WAIT_URL = '/api/queue/wait'

export async function fetchLimiters(): Promise<LimiterInfo[]> {
  const res = await fetch('/api/limiters')
  if (!res.ok) {
    throw new Error(`GET /api/limiters: ${res.status}`)
  }
  const body = (await res.json()) as { limiters?: LimiterInfo[] }
  return body.limiters ?? []
}

/** One recorded request, as the harness saw it. */
export type Outcome = {
  /** Milliseconds since the run started, so runs line up on a shared axis. */
  at: number
  /** How long the request itself took. Only interesting for the queue. */
  tookMs: number
  status: number
  allowed: boolean
  /** From RateLimit-Remaining, absent on the queue and the fail-open path. */
  remaining: number | null
  /** From Retry-After, in seconds. Absent unless refused. */
  retryAfter: number | null
}

function header(res: Response, name: string): number | null {
  const raw = res.headers.get(name)
  if (raw === null) {
    return null
  }
  const n = Number(raw)
  return Number.isFinite(n) ? n : null
}

/**
 * send fires one request and records what came back.
 *
 * A refusal is not an error here: 429 is an answer, and the point of the
 * harness is to plot it. Only a transport failure throws.
 */
export async function send(url: string, startedAt: number): Promise<Outcome> {
  const sentAt = performance.now()
  const res = await fetch(url, { method: 'POST' })
  const tookMs = performance.now() - sentAt

  return {
    at: sentAt - startedAt,
    tookMs,
    status: res.status,
    allowed: res.ok,
    remaining: header(res, 'RateLimit-Remaining'),
    retryAfter: header(res, 'Retry-After'),
  }
}

export async function reset(url: string): Promise<void> {
  const res = await fetch(url, { method: 'POST' })
  if (!res.ok) {
    throw new Error(`POST ${url}: ${res.status}`)
  }
}
