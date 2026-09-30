import { useState } from 'react'
import { type LimiterInfo, type Outcome, QUEUE_WAIT_URL, reset, send } from './api'
import { Controls } from './Controls'
import { summarise } from './summary'
import { Timeline } from './Timeline'
import { plannedOffsets, run, type Mode } from './traffic'

type Props = {
  /** Absent for the queue, which is not a limiter and has no entry in the list. */
  limiter?: LimiterInfo
}

export function LimiterPanel({ limiter }: Props) {
  const isQueue = limiter === undefined
  const checkURL = limiter?.check_url ?? QUEUE_WAIT_URL

  const [mode, setMode] = useState<Mode>({ kind: 'burst', count: 10 })
  const [outcomes, setOutcomes] = useState<Outcome[]>([])
  const [running, setRunning] = useState(false)
  const [error, setError] = useState<string | null>(null)

  async function fire() {
    setRunning(true)
    setError(null)
    setOutcomes([])

    const collected: Outcome[] = []
    try {
      await run(
        mode,
        (startedAt) => send(checkURL, startedAt),
        (outcome) => {
          collected.push(outcome)
          // Replaced rather than appended, so a run of two hundred does not
          // schedule two hundred renders.
          setOutcomes([...collected])
        },
      )
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : String(err))
    } finally {
      setRunning(false)
    }
  }

  async function clear() {
    setError(null)
    try {
      if (limiter !== undefined) {
        await reset(limiter.reset_url)
      }
      setOutcomes([])
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : String(err))
    }
  }

  const stats = summarise(outcomes)

  // The axis covers whichever is longer: the run that was asked for, so marks
  // appear in place while it fills rather than sliding leftward, or what has
  // actually landed, since a burst compresses into a couple of milliseconds and
  // a fixed floor would stack all of it on the origin.
  const planned = plannedOffsets(mode)
  const spanMs = Math.max(
    planned.length > 0 ? planned[planned.length - 1] : 0,
    ...outcomes.map((o) => o.at + o.tookMs),
    1,
  )

  return (
    <section>
      <Controls
        mode={mode}
        onMode={setMode}
        onFire={fire}
        onReset={clear}
        running={running}
        resettable={!isQueue}
      />

      {error !== null && <p className="error">{error}</p>}

      {isQueue && (
        <p className="note">
          The queue does not refuse requests until it is full — it makes them wait.
          The bars are how long each caller waited, and they are the answer here.
          It is also the one limiter that is not per caller: it shapes the whole
          flow through its endpoint.
        </p>
      )}

      <Timeline outcomes={outcomes} spanMs={spanMs} showWaits={isQueue} />

      <dl className="stats">
        <div>
          <dt>allowed</dt>
          <dd className="num ok">{stats.allowed}</dd>
        </div>
        <div>
          <dt>{isQueue ? 'refused (queue full)' : 'refused'}</dt>
          <dd className="num bad">{stats.refused}</dd>
        </div>
        {mode.kind === 'sustained' && (
          // Only meaningful for a sustained run, where it answers "did the
          // generator actually deliver the rate it was set to". For a burst it
          // divides ten requests by half a millisecond and reports a number
          // that is arithmetically true and says nothing.
          <div>
            <dt>rate achieved</dt>
            <dd className="num">{stats.achieved.toFixed(1)}/s</dd>
          </div>
        )}
        {isQueue && (
          <div>
            <dt>longest wait</dt>
            <dd className="num warn">{(stats.longestWait / 1000).toFixed(2)}s</dd>
          </div>
        )}
        {!isQueue && outcomes.length > 0 && (
          <div>
            <dt>retry-after, last refusal</dt>
            <dd className="num">
              {outcomes.filter((o) => !o.allowed).at(-1)?.retryAfter ?? '—'}
              {outcomes.some((o) => !o.allowed) ? 's' : ''}
            </dd>
          </div>
        )}
      </dl>
    </section>
  )
}
