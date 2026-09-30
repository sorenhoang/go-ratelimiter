import { describe, expect, it } from 'vitest'
import type { Outcome } from './api'
import { plannedOffsets, run } from './traffic'

describe('plannedOffsets', () => {
  it('sends one request for a single', () => {
    expect(plannedOffsets({ kind: 'single' })).toEqual([0])
  })

  it('sends a burst all at once', () => {
    expect(plannedOffsets({ kind: 'burst', count: 4 })).toEqual([0, 0, 0, 0])
  })

  it('spaces a sustained run evenly', () => {
    expect(
      plannedOffsets({ kind: 'sustained', ratePerSecond: 4, seconds: 1 }),
    ).toEqual([0, 250, 500, 750])
  })

  it('measures offsets from the start rather than from each other', () => {
    // The distinction that keeps a long run from drifting: the tenth request is
    // planned for 900ms after the start, not for nine consecutive 100ms waits.
    const offsets = plannedOffsets({ kind: 'sustained', ratePerSecond: 10, seconds: 1 })
    expect(offsets).toHaveLength(10)
    expect(offsets[9]).toBe(900)
  })

  it('handles a fractional rate', () => {
    expect(
      plannedOffsets({ kind: 'sustained', ratePerSecond: 2.5, seconds: 2 }),
    ).toEqual([0, 400, 800, 1200, 1600])
  })

  it('returns nothing for a rate or duration of zero', () => {
    expect(plannedOffsets({ kind: 'sustained', ratePerSecond: 0, seconds: 1 })).toEqual([])
    expect(plannedOffsets({ kind: 'sustained', ratePerSecond: 10, seconds: 0 })).toEqual([])
    expect(plannedOffsets({ kind: 'burst', count: 0 })).toEqual([])
  })
})

function stubOutcome(at: number): Outcome {
  return { at, tookMs: 0, status: 200, allowed: true, remaining: null, retryAfter: null }
}

describe('run', () => {
  /**
   * The failure this guards against is invisible on screen.
   *
   * If the runner awaited each response before scheduling the next, the
   * delivered rate would be capped by the server's latency instead of the
   * configured rate — and every chart in the harness would show a rate the
   * server never received, with nothing to indicate it.
   *
   * The stub below answers slower than the interval asks for. A sequential
   * implementation would manage about five sends in the window; a correct one
   * manages ten.
   */
  it('keeps its rate when responses are slower than the interval', async () => {
    const responseMs = 100
    const sentAt: number[] = []

    await run(
      { kind: 'sustained', ratePerSecond: 20, seconds: 0.5 },
      async (startedAt) => {
        sentAt.push(performance.now() - startedAt)
        await new Promise((resolve) => setTimeout(resolve, responseMs))
        return stubOutcome(0)
      },
      () => {},
    )

    // Ten planned at 20/s for half a second, and all ten have to have gone out.
    expect(sentAt).toHaveLength(10)

    // Spaced by the interval, not by the response time. Generous enough for a
    // busy machine, tight enough that 100ms spacing fails.
    const gaps = sentAt.slice(1).map((t, i) => t - sentAt[i])
    for (const gap of gaps) {
      expect(gap).toBeLessThan(responseMs * 0.8)
    }

    // And the whole run took about its stated duration rather than twice it.
    expect(sentAt[sentAt.length - 1]).toBeLessThan(700)
  })

  it('reports every outcome and survives a failing request', async () => {
    const seen: Outcome[] = []
    let call = 0

    await run(
      { kind: 'burst', count: 5 },
      async () => {
        call++
        if (call === 3) {
          throw new Error('transport died')
        }
        return stubOutcome(call)
      },
      (outcome) => seen.push(outcome),
    )

    // Four of five landed; the failure is dropped rather than aborting the run,
    // because one dead connection should not end the experiment.
    expect(seen).toHaveLength(4)
  })

  it('waits for the last response before returning', async () => {
    let settled = 0

    await run(
      { kind: 'burst', count: 3 },
      async () => {
        await new Promise((resolve) => setTimeout(resolve, 20))
        settled++
        return stubOutcome(0)
      },
      () => {},
    )

    // Returning early would let a caller clear the chart while requests were
    // still in flight.
    expect(settled).toBe(3)
  })
})
