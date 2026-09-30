import type { Outcome } from './api'

export type Mode =
  | { kind: 'single' }
  | { kind: 'burst'; count: number }
  | { kind: 'sustained'; ratePerSecond: number; seconds: number }

/**
 * plannedOffsets returns when each request should be sent, in milliseconds from
 * the start of the run.
 *
 * Offsets are absolute rather than gaps between requests. Sleeping a fixed gap
 * each time accumulates every scheduling delay, so a run that asks for ten a
 * second lands short of it and drifts further the longer it goes.
 */
export function plannedOffsets(mode: Mode): number[] {
  switch (mode.kind) {
    case 'single':
      return [0]

    case 'burst':
      // All at once. A burst is a burst.
      return Array.from({ length: Math.max(0, mode.count) }, () => 0)

    case 'sustained': {
      if (mode.ratePerSecond <= 0 || mode.seconds <= 0) {
        return []
      }
      const interval = 1000 / mode.ratePerSecond
      const total = Math.floor(mode.ratePerSecond * mode.seconds)
      return Array.from({ length: total }, (_, i) => i * interval)
    }
  }
}

function sleepUntil(deadline: number, now: () => number): Promise<void> {
  const wait = deadline - now()
  if (wait <= 0) {
    return Promise.resolve()
  }
  return new Promise((resolve) => setTimeout(resolve, wait))
}

export type RunOptions = {
  /** Injectable for tests; defaults to performance.now. */
  now?: () => number
}

/**
 * run fires the planned requests and reports each outcome as it lands.
 *
 * Requests are started on schedule and awaited only at the end. Awaiting each
 * response before scheduling the next would tie the delivered rate to the
 * server's latency: a hundred milliseconds per response caps the run at ten a
 * second however high the rate is set, and every chart drawn from it would show
 * a rate the server never saw.
 */
export async function run(
  mode: Mode,
  sendOne: (startedAt: number) => Promise<Outcome>,
  onOutcome: (outcome: Outcome) => void,
  options: RunOptions = {},
): Promise<void> {
  const now = options.now ?? (() => performance.now())
  const offsets = plannedOffsets(mode)
  if (offsets.length === 0) {
    return
  }

  const startedAt = now()
  const inFlight: Promise<void>[] = []

  for (const offset of offsets) {
    await sleepUntil(startedAt + offset, now)

    inFlight.push(
      sendOne(startedAt)
        .then(onOutcome)
        .catch(() => {
          // A transport failure is not an outcome to plot, and one dead request
          // must not abandon the rest of the run.
        }),
    )
  }

  await Promise.all(inFlight)
}
