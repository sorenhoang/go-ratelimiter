import type { Outcome } from './api'

/**
 * summarise reduces a run to the few numbers worth reading off it.
 *
 * `achieved` is the rate actually delivered, which is not always the one that
 * was asked for — the reason the scheduler has a test of its own.
 */
export function summarise(outcomes: Outcome[]) {
  const allowed = outcomes.filter((o) => o.allowed).length
  const refused = outcomes.length - allowed

  const spanMs =
    outcomes.length > 1 ? outcomes[outcomes.length - 1].at - outcomes[0].at : 0
  const achieved = spanMs > 0 ? (outcomes.length - 1) / (spanMs / 1000) : 0

  const waits = outcomes.filter((o) => o.allowed).map((o) => o.tookMs)
  const longestWait = waits.length > 0 ? Math.max(...waits) : 0

  return { allowed, refused, achieved, longestWait }
}
