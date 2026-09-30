import { useEffect, useState } from 'react'
import './App.css'
import { fetchLimiters, type LimiterInfo } from './api'
import { ComparePanel } from './ComparePanel'
import { LimiterPanel } from './LimiterPanel'

const QUEUE_TAB = 'queue'
const COMPARE_TAB = 'compare'

export default function App() {
  const [limiters, setLimiters] = useState<LimiterInfo[]>([])
  const [error, setError] = useState<string | null>(null)
  const [tab, setTab] = useState<string>(QUEUE_TAB)

  useEffect(() => {
    fetchLimiters()
      .then((list) => {
        setLimiters(list)
        setError(null)
        if (list.length > 0) {
          setTab(list[0].name)
        }
      })
      .catch((err: unknown) => {
        setLimiters([])
        setError(err instanceof Error ? err.message : String(err))
      })
  }, [])

  const tabs = [...limiters.map((l) => l.name), QUEUE_TAB, COMPARE_TAB]
  const selected = limiters.find((l) => l.name === tab)

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
        {tabs.map((name) => (
          <button
            key={name}
            role="tab"
            aria-selected={tab === name}
            className={tab === name ? 'tab on' : 'tab'}
            onClick={() => setTab(name)}
          >
            {name}
          </button>
        ))}
      </nav>

      <main className="panel">
        {tab === COMPARE_TAB ? (
          <ComparePanel limiters={limiters} />
        ) : (
          // Keyed by tab, so switching tabs starts a clean panel instead of
          // showing one limiter's marks under another's name.
          <LimiterPanel key={tab} limiter={selected} />
        )}
      </main>
    </div>
  )
}
