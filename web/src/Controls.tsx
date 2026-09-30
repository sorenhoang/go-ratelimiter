import type { Mode } from './traffic'

type Props = {
  mode: Mode
  onMode: (mode: Mode) => void
  onFire: () => void
  onReset: () => void
  running: boolean
  /** The queue has nothing to reset: it holds no per-caller state. */
  resettable?: boolean
}

export function Controls({
  mode,
  onMode,
  onFire,
  onReset,
  running,
  resettable = true,
}: Props) {
  return (
    <div className="controls">
      <div className="modes" role="group" aria-label="Traffic pattern">
        <button
          className={mode.kind === 'single' ? 'mode on' : 'mode'}
          onClick={() => onMode({ kind: 'single' })}
          disabled={running}
        >
          Single
        </button>
        <button
          className={mode.kind === 'burst' ? 'mode on' : 'mode'}
          onClick={() => onMode({ kind: 'burst', count: 10 })}
          disabled={running}
        >
          Burst
        </button>
        <button
          className={mode.kind === 'sustained' ? 'mode on' : 'mode'}
          onClick={() => onMode({ kind: 'sustained', ratePerSecond: 4, seconds: 5 })}
          disabled={running}
        >
          Sustained
        </button>
      </div>

      {mode.kind === 'burst' && (
        <label className="field">
          requests
          <input
            type="number"
            min={1}
            max={200}
            value={mode.count}
            disabled={running}
            onChange={(e) =>
              onMode({ kind: 'burst', count: clamp(e.target.value, 1, 200, 10) })
            }
          />
        </label>
      )}

      {mode.kind === 'sustained' && (
        <>
          <label className="field">
            per second
            <input
              type="number"
              min={0.5}
              max={100}
              step={0.5}
              value={mode.ratePerSecond}
              disabled={running}
              onChange={(e) =>
                onMode({ ...mode, ratePerSecond: clamp(e.target.value, 0.5, 100, 4) })
              }
            />
          </label>
          <label className="field">
            seconds
            <input
              type="number"
              min={1}
              max={60}
              value={mode.seconds}
              disabled={running}
              onChange={(e) => onMode({ ...mode, seconds: clamp(e.target.value, 1, 60, 5) })}
            />
          </label>
        </>
      )}

      <div className="spacer" />

      <button onClick={onFire} disabled={running} className="fire">
        {running ? 'Firing…' : 'Fire'}
      </button>
      {resettable && (
        <button onClick={onReset} disabled={running}>
          Reset state
        </button>
      )}
    </div>
  )
}

/**
 * Number inputs hand back a string, and an empty one parses to NaN. Left
 * unchecked that reaches the scheduler and plans a run of NaN requests.
 */
function clamp(raw: string, min: number, max: number, fallback: number): number {
  const n = Number(raw)
  if (!Number.isFinite(n)) {
    return fallback
  }
  return Math.min(max, Math.max(min, n))
}
