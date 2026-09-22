import { useMemo, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { useApiData } from '../api/useApiData'
import { useRunsPoll, isActiveRun } from '../api/useRunsPoll'
import { byId, fmtDateTime, fmtElapsed, latestRunByTask, taskEngine } from '../api/view'
import type { Connection, Run, Storage, Task } from '../api/types'
import { useAuth, useCan } from '../auth'
import { Layout } from '../ui/Layout'
import { EngineDot, MONO, StatusBadge, btnPrimary } from '../ui/dc'

type Data = { tasks: Task[]; runs: Run[]; connections: Connection[]; storages: Storage[] }

export default function Tasks() {
  const { api, logout } = useAuth()
  // Server-enforced; hiding the button only avoids offering a guaranteed 403.
  const canWrite = useCan('operator')
  const navigate = useNavigate()
  const [reload, setReload] = useState(0)
  const [busyId, setBusyId] = useState<number | null>(null)

  const { data, error, loading } = useApiData<Data>(async () => {
    const [tasks, runs, connections, storages] = await Promise.all([
      api.listTasks(),
      api.listRuns(),
      api.listConnections(),
      api.listStorages(),
    ])
    return { tasks, runs, connections, storages }
  }, [reload])

  const tasks = data?.tasks ?? []
  const connById = useMemo(() => byId(data?.connections ?? []), [data])

  // Live run statuses: seeded from the initial load, then polled while any run is
  // active so the badges flip to a terminal state without a full-page reload.
  const { runs, nowMs: now } = useRunsPoll(api, data?.runs)
  const lastRun = useMemo(() => latestRunByTask(runs), [runs])

  async function runNow(id: number) {
    setBusyId(id)
    try {
      await api.runTask(id)
      setReload((n) => n + 1)
    } finally {
      setBusyId(null)
    }
  }

  const createBtn = canWrite ? (
    <button style={btnPrimary} onClick={() => navigate('/tasks/new')}>
      <span style={{ fontSize: 14, lineHeight: 1 }}>＋</span>Создать задачу
    </button>
  ) : null

  return (
    <Layout title="Задачи" subtitle={`${tasks.length} задач`} onLogout={logout} actions={createBtn}>
      {loading && <div style={MUTED}>Загрузка…</div>}
      {error && <div style={ERRSTYLE} role="alert">{error}</div>}
      {!loading && !error && (
        <div style={{ padding: '24px 26px' }} className="dc-fade">
          {tasks.length === 0 ? (
            <div style={MUTED}>Задач пока нет — создайте первую.</div>
          ) : (
            <div style={{ border: '1px solid var(--line)', borderRadius: 10, overflow: 'hidden', background: 'var(--panel)' }}>
              <div style={{ display: 'grid', gridTemplateColumns: COLS, padding: '11px 20px', fontSize: 10, fontWeight: 600, letterSpacing: '0.05em', color: 'var(--ink-3)', borderBottom: '1px solid var(--line)', fontFamily: MONO }}>
                <div>ЗАДАЧА</div><div>СУБД</div><div>РАСПИСАНИЕ</div><div>ПОСЛ. ЗАПУСК</div><div>КОДЕК</div><div style={{ textAlign: 'right' }}>ВКЛ.</div><div />
              </div>
              {tasks.map((t) => {
                const conn = connById.get(t.connection_id)
                const engine = taskEngine(t, connById)
                const run = lastRun.get(t.id)
                return (
                  <div
                    key={t.id}
                    role="button"
                    tabIndex={0}
                    onClick={() => navigate(`/tasks/${t.id}/edit`)}
                    onKeyDown={(e) => {
                      if (e.key === 'Enter' || e.key === ' ') navigate(`/tasks/${t.id}/edit`)
                    }}
                    className="dc-h-panel2"
                    style={{ display: 'grid', gridTemplateColumns: COLS, padding: '14px 20px', fontSize: 12.5, borderBottom: '1px solid var(--line)', alignItems: 'center', cursor: 'pointer', transition: 'background 0.15s ease' }}
                  >
                    <div style={{ minWidth: 0 }}>
                      <div style={{ fontWeight: 600, color: 'var(--ink)' }}>{t.name}</div>
                      <div style={{ fontSize: 11, color: 'var(--ink-3)', fontFamily: MONO, marginTop: 3 }}>
                        {conn ? `${conn.name} · ${conn.connector_type}` : `conn #${t.connection_id}`}
                      </div>
                    </div>
                    <div style={{ display: 'flex', alignItems: 'center', gap: 7 }}>
                      {engine ? <EngineDot engine={engine} /> : null}
                      <span style={{ fontFamily: MONO, color: 'var(--ink-2)', fontSize: 11.5 }}>{engine || '—'}</span>
                    </div>
                    <div style={{ fontFamily: MONO, fontSize: 11.5, color: 'var(--ink-2)' }}>{t.cron}</div>
                    <div>
                      {run ? (
                        <>
                          <StatusBadge status={run.status} />
                          <div style={{ fontSize: 10.5, color: isActiveRun(run) ? 'var(--run)' : 'var(--ink-3)', fontFamily: MONO, marginTop: 4 }}>
                            {isActiveRun(run) ? fmtElapsed(run.started_at ?? run.created_at, now) : fmtDateTime(run.started_at ?? run.created_at)}
                          </div>
                        </>
                      ) : (
                        <span style={{ color: 'var(--ink-3)' }}>—</span>
                      )}
                    </div>
                    <div style={{ fontFamily: MONO, fontSize: 11, color: 'var(--ink-2)' }}>{t.codec_chain.length ? t.codec_chain.join(' → ') : '—'}</div>
                    <div style={{ textAlign: 'right' }}>
                      <span style={{ display: 'inline-flex', alignItems: 'center', gap: 6, padding: '3px 9px', borderRadius: 5, fontSize: 11, fontWeight: 600, background: t.enabled ? 'var(--ok-bg)' : 'var(--skip-bg)', color: t.enabled ? 'var(--ok)' : 'var(--skip)' }}>
                        {t.enabled ? 'включена' : 'на паузе'}
                      </span>
                    </div>
                    <div style={{ display: 'flex', justifyContent: 'flex-end', gap: 6 }}>
                      {canWrite && (
                      <button
                        type="button"
                        title="Запустить сейчас"
                        onClick={(e) => {
                          e.stopPropagation()
                          runNow(t.id)
                        }}
                        disabled={busyId === t.id}
                        className="dc-reset dc-h-panel2"
                        style={{ width: 28, height: 24, borderRadius: 6, display: 'flex', alignItems: 'center', justifyContent: 'center', color: 'var(--accent)', cursor: busyId === t.id ? 'default' : 'pointer', fontSize: 12, opacity: busyId === t.id ? 0.5 : 1 }}
                      >▶</button>
                      )}
                    </div>
                  </div>
                )
              })}
            </div>
          )}
        </div>
      )}
    </Layout>
  )
}

const COLS = '1.9fr 0.8fr 1fr 1.1fr 1fr 0.7fr 0.7fr'
const MUTED = { padding: '40px 26px', color: 'var(--ink-3)', fontSize: 13 } as const
const ERRSTYLE = { padding: '40px 26px', color: 'var(--err)', fontSize: 13 } as const
