/**
 * tickStep picks a round interval giving a handful of ticks across the span.
 *
 * A fixed one-second tick is wrong at both ends of the range this harness
 * covers: a burst of ten lands inside a couple of milliseconds, where every
 * mark would stack on the origin, and a sustained run can go a minute, where
 * sixty ticks turn the axis into a smear.
 */
export function tickStep(spanMs: number): number {
  const rough = spanMs / 6
  const steps = [1, 2, 5, 10, 20, 50, 100, 200, 500, 1000, 2000, 5000, 10_000, 30_000]
  return steps.find((s) => s >= rough) ?? 60_000
}

/** Labels in milliseconds while that reads better than a fraction of a second. */
export function tickLabel(ms: number, step: number): string {
  if (step < 1000) {
    return `${Math.round(ms)}ms`
  }
  return `${+(ms / 1000).toFixed(1)}s`
}
