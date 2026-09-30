import type { Outcome } from './api'
import { tickLabel, tickStep } from './ticks'

/** Drawn in a fixed coordinate space and scaled by CSS. */
const W = 1000
const ROW_H = 44
const AXIS_H = 18

type Props = {
  outcomes: Outcome[]
  /** The span the axis covers, in milliseconds. */
  spanMs: number
  /** Row label, shown on the left in the stacked comparison. */
  label?: string
  /**
   * Draw how long each request waited before it was served. Meaningless for the
   * five limiters, which answer at once; the whole answer for the queue.
   */
  showWaits?: boolean
}

export function Timeline({ outcomes, spanMs, label, showWaits = false }: Props) {
  const span = Math.max(spanMs, 1)
  const x = (ms: number) => (ms / span) * W
  const mid = ROW_H / 2

  const step = tickStep(span)
  const ticks: number[] = []
  for (let t = 0; t <= span + step / 2; t += step) {
    ticks.push(t)
  }

  const allowed = outcomes.filter((o) => o.allowed).length

  return (
    <div className="timeline">
      {label !== undefined && <span className="timeline-label">{label}</span>}
      <svg
        viewBox={`0 0 ${W} ${ROW_H + AXIS_H}`}
        preserveAspectRatio="none"
        role="img"
        aria-label={`${label ?? 'run'}: ${allowed} allowed, ${
          outcomes.length - allowed
        } refused over ${Math.round(span)}ms`}
      >
        <line x1={0} y1={mid} x2={W} y2={mid} className="tl-baseline" />

        {ticks.map((t) => (
          <g key={t}>
            <line x1={x(t)} y1={mid - 5} x2={x(t)} y2={mid + 5} className="tl-tick" />
            <text x={x(t)} y={ROW_H + AXIS_H - 5} className="tl-tick-label">
              {tickLabel(t, step)}
            </text>
          </g>
        ))}

        {outcomes.map((o, i) => {
          const cx = x(o.at)

          // The queue answers with a duration, so the bar is the answer and the
          // mark only says where it ended. Drawing a queued request that took
          // four seconds the same as one allowed instantly would misreport the
          // algorithm.
          const bar =
            showWaits && o.tookMs > 0 ? (
              <rect
                x={cx}
                y={mid - 3}
                width={Math.max(x(o.tookMs), 1)}
                height={6}
                className="tl-wait"
              />
            ) : null

          const end = showWaits ? x(o.at + o.tookMs) : cx

          return (
            <g key={i}>
              {bar}
              {o.allowed ? (
                <circle cx={end} cy={mid} r={4} className="tl-allowed" />
              ) : (
                // A cross, not a differently coloured dot: the two outcomes have
                // to be told apart without relying on colour.
                <g className="tl-refused">
                  <line x1={end - 4} y1={mid - 4} x2={end + 4} y2={mid + 4} />
                  <line x1={end - 4} y1={mid + 4} x2={end + 4} y2={mid - 4} />
                </g>
              )}
            </g>
          )
        })}
      </svg>
    </div>
  )
}
