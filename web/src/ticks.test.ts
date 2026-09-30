import { describe, expect, it } from 'vitest'
import { tickLabel, tickStep } from './ticks'

describe('tickStep', () => {
  it('keeps the tick count in a readable range across the whole span this harness covers', () => {
    // From a burst that lands in a couple of milliseconds to a minute-long
    // sustained run. The first version of the chart used a fixed one second
    // tick, which stacked an entire burst on the origin with no axis to read.
    for (const span of [2, 8, 40, 250, 1_000, 5_000, 30_000, 60_000]) {
      const count = Math.floor(span / tickStep(span)) + 1
      expect(count, `${span}ms produced ${count} ticks`).toBeGreaterThanOrEqual(2)
      expect(count, `${span}ms produced ${count} ticks`).toBeLessThanOrEqual(9)
    }
  })

  it('returns a round number rather than an arbitrary division', () => {
    expect(tickStep(1_000)).toBe(200)
    expect(tickStep(6_000)).toBe(1000)
  })
})

describe('tickLabel', () => {
  it('uses milliseconds below a second, where fractions of a second read badly', () => {
    expect(tickLabel(0, 50)).toBe('0ms')
    expect(tickLabel(150, 50)).toBe('150ms')
  })

  it('uses seconds above that, and trims a trailing zero', () => {
    expect(tickLabel(2_000, 1000)).toBe('2s')
    expect(tickLabel(2_500, 1000)).toBe('2.5s')
  })
})
