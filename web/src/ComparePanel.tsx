import { useState } from 'react'
import { type LimiterInfo, type Outcome, QUEUE_WAIT_URL, reset, send } from './api'
import { Controls } from './Controls'
import { summarise } from './summary'
import { Timeline } from './Timeline'
import { plannedOffsets, run, type Mode } from './traffic'

const QUEUE = 'queue'

type Props = {
  limiters: LimiterInfo[]
}

/**
 * ComparePanel fires one pattern at every limiter at once and stacks the
 * results on a shared axis.
 *
 * The shared axis is the whole point. Letting each row scale to its own data
 * would line six charts up under one heading while making them incomparable,
 * which is worse than showing them separately.
 */
export function ComparePanel({ limiters }: Props) {
  const [mode, setMode] = useState<Mode>({ kind: 'sustained', ratePerSecond: 4, seconds: 5 })
  const [runs, setRuns] = useState<Record<string, Outcome[]>>({})
  const [running, setRunning] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const targets = [
    ...limiters.map((l) => ({ name: l.name, url: l.check_url, isQueue: false })),
    { name: QUEUE, url: QUEUE_WAIT_URL, isQueue: true },
  ]

  async function fire() {
    setRunning(true)
    setError(null)
    setRuns({})

    try {
      // Cleared first, or a limiter still holding state from an earlier run
      // starts the comparison part way through its own quota and the six rows
      // stop being a fair test.
      await Promise.all(limiters.map((l) => reset(l.reset_url)))

      const collected: Record<string, Outcome[]> = {}
      await Promise.all(
        targets.map((t) => {
          collected[t.name] = []
          return run(
            mode,
            (startedAt) => send(t.url, startedAt),
            (outcome) => {
              collected[t.name].push(outcome)
              setRuns({ ...collected })
            },
          )
        }),
      )
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : String(err))
    } finally {
      setRunning(false)
    }
  }

  const planned = plannedOffsets(mode)
  const spanMs = Math.max(
    planned.length > 0 ? planned[planned.length - 1] : 0,
    ...Object.values(runs).flatMap((os) => os.map((o) => o.at + o.tookMs)),
    1,
  )

  return (
    <section>
      <Controls
        mode={mode}
        onMode={setMode}
        onFire={fire}
        onReset={() => setRuns({})}
        running={running}
        resettable={false}
      />

      {error !== null && <p className="error">{error}</p>}

      <p className="note">
        One pattern, fired at all six at once, on one axis. Every limiter here is
        set to the same sustained rate, so the rows differ only in how each one
        decides — and in the queue's case, in whether it decides at all.
      </p>

      <ul className="legend">
        <li>
          <svg viewBox="0 0 12 12" aria-hidden="true">
            <circle cx={6} cy={6} r={4} className="tl-allowed" />
          </svg>
          allowed
        </li>
        <li>
          <svg viewBox="0 0 12 12" aria-hidden="true">
            <g className="tl-refused">
              <line x1={2} y1={2} x2={10} y2={10} />
              <line x1={2} y1={10} x2={10} y2={2} />
            </g>
          </svg>
          refused
        </li>
        <li>
          <svg viewBox="0 0 12 12" aria-hidden="true">
            <rect x={0} y={4} width={12} height={4} className="tl-wait" />
          </svg>
          waited (queue only)
        </li>
      </ul>

      <div className="rows">
        {targets.map((t) => {
          const outcomes = runs[t.name] ?? []
          const stats = summarise(outcomes)
          return (
            <div key={t.name} className="row">
              <Timeline
                outcomes={outcomes}
                spanMs={spanMs}
                label={t.name}
                showWaits={t.isQueue}
              />
              <p className="row-stats num">
                <span className="ok">{stats.allowed}</span>
                {' / '}
                <span className="bad">{stats.refused}</span>
                {t.isQueue && stats.longestWait > 0 && (
                  <span className="warn">
                    {' · '}
                    {(stats.longestWait / 1000).toFixed(1)}s wait
                  </span>
                )}
              </p>
            </div>
          )
        })}
      </div>
    </section>
  )
}
