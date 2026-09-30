import { useEffect, useState } from 'react'
import { fetchLimiters, type LimiterInfo } from './api'
import './App.css'

/** The queue is a tab like the others but not a limiter, so it carries no info. */
const QUEUE_TAB = 'queue'
const COMPARE_TAB = 'compare'

export default function App() {
  const [limiters, setLimiters] = useState<LimiterInfo[]>([])
  const [error, setError] = useState<string | null>(null)
  const [tab, setTab] = useState<string>(COMPARE_TAB)

  useEffect(() => {
    fetchLimiters()
      .then((list) => {
        setLimiters(list)
        setError(null)
      })
      .catch((err: unknown) => {
        setLimiters([])
        setError(err instanceof Error ? err.message : String(err))
      })
  }, [])

  return (
    <div className="app">
      <header className="masthead">
        <h1>Rate limiter harness</h1>
        <p className="sub">
          Five algorithms and one queue, behind the same Go server. Fire traffic at
          them and watch where they disagree.
        </p>
      </header>

      {error !== null && (
        <p className="error">
          Cannot reach the API: <code>{error}</code>. Start it with{' '}
          <code>make up &amp;&amp; make run</code>.
        </p>
      )}

      <nav className="tabs" role="tablist">
        <button
          role="tab"
          aria-selected={tab === COMPARE_TAB}
          className={tab === COMPARE_TAB ? 'tab on' : 'tab'}
          onClick={() => setTab(COMPARE_TAB)}
        >
          Compare
        </button>
        {limiters.map((l) => (
          <button
            key={l.name}
            role="tab"
            aria-selected={tab === l.name}
            className={tab === l.name ? 'tab on' : 'tab'}
            onClick={() => setTab(l.name)}
          >
            {l.name}
          </button>
        ))}
        <button
          role="tab"
          aria-selected={tab === QUEUE_TAB}
          className={tab === QUEUE_TAB ? 'tab on' : 'tab'}
          onClick={() => setTab(QUEUE_TAB)}
        >
          queue
        </button>
      </nav>

      <main className="panel">
        <p className="todo">
          <code>{tab}</code> — the traffic generator and the timeline arrive in the
          next steps. The tabs above came from <code>GET /api/limiters</code>, so
          the proxy and the endpoint are both working.
        </p>
      </main>
    </div>
  )
}
