import { useMemo, useState } from 'react'
import { ApiError } from '../api/client'
import { useApiData } from '../api/useApiData'
import { byId, fmtBytes, fmtDateTime, taskEngine } from '../api/view'
import type { Connection, RetentionSummary, Sweep, Task } from '../api/types'
import { useAuth, useCan } from '../auth'
import { Layout } from '../ui/Layout'
import { EngineDot, MONO, StatusBadge, btnPrimary } from '../ui/dc'
import { formatPolicy } from '../ui/retention'

type Data = { summary: RetentionSummary; sweeps: Sweep[]; tasks: Task[]; connections: Connection[] }

// Re-exported so the page stays the place tests reach for policy formatting,
// while the wizard and the editor share the same implementation.
export { formatPolicy }

export default function Retention() {
  const { api, logout } = useAuth()
  const canWrite = useCan('operator')
  const [reload, setReload] = useState(0)
  const [sweeping, setSweeping] = useState(false)
  const [sweepErr, setSweepErr] = useState<string | null>(null)

  const { data, error, loading } = useApiData<Data>(async () => {
    const [summary, sweeps, tasks, connections] = await Promise.all([
      api.getRetention(),
      api.listSweeps(20),
      api.listTasks(),
      api.listConnections(),
    ])
    return { summary, sweeps, tasks, connections }
  }, [api, reload])

  const summary = data?.summary
  const sweeps = data?.sweeps ?? []
  const connById = useMemo(() => byId(data?.connections ?? []), [data])
  const taskById = useMemo(() => byId(data?.tasks ?? []), [data])

  async function sweepNow() {
    setSweeping(true)
    setSweepErr(null)
    try {
      await api.runSweep()
      setReload((n) => n + 1)
    } catch (e) {
      // 409 is not a failure: the scheduled sweep is already doing this work.
      // Reload anyway, so its result appears as soon as it lands.
      if (e instanceof ApiError && e.status === 409) {
        setSweepErr('Очистка уже выполняется — дождитесь её завершения.')
        setReload((n) => n + 1)
      } else {
        setSweepErr(e instanceof Error ? e.message : String(e))
      }
    } finally {
      setSweeping(false)
    }
  }

  const last = summary?.last_sweep
  const sweepBtn = canWrite ? (
    <button style={{ ...btnPrimary, opacity: sweeping ? 0.6 : 1 }} disabled={sweeping} onClick={sweepNow}>
      <span style={{ fontSize: 14, lineHeight: 1 }}>↻</span>{sweeping ? 'Очистка…' : 'Очистить сейчас'}
    </button>
  ) : null

  const cards: SummaryCard[] = [
    {
      label: 'РАСПИСАНИЕ ОЧИСТКИ',
      value: summary?.cron || 'выключено',
      unit: '',
      color: summary?.cron ? 'var(--ink)' : 'var(--ink-3)',
      sub: summary?.next_sweep ? `следующая — ${fmtDateTime(summary.next_sweep)}` : 'запуск только вручную',
    },
    {
      label: 'ПОСЛЕДНИЙ ПРОГОН',
      value: last ? String(last.deleted_count) : '—',
      unit: last ? 'удалено' : '',
      color: last?.status === 'failed' ? 'var(--err)' : 'var(--ok)',
      sub: last ? `освобождено ${fmtBytes(last.freed_bytes)} · ${fmtDateTime(last.finished_at)}` : 'очисток ещё не было',
    },
    {
      label: 'ORPHAN-ОБЪЕКТЫ',
      value: last ? String(last.orphan_count) : '—',
      unit: last ? 'найдено' : '',
      color: last && last.orphan_count > 0 ? 'var(--warn)' : 'var(--ink)',
      sub: 'в хранилище, но не в каталоге',
    },
  ]

  return (
    <Layout
      title="Политики хранения"
      subtitle={summary?.cron ? `очистка по ${summary.cron}` : 'очистка по расписанию выключена'}
      onLogout={logout}
      actions={sweepBtn}
    >
      {loading && <div style={MUTED}>Загрузка…</div>}
      {error && <div style={ERRSTYLE} role="alert">{error}</div>}
      {!loading && !error && summary && (
        <div style={{ padding: '24px 26px' }} className="dc-fade">
          {sweepErr && (
            <div style={{ ...ERRSTYLE, padding: '0 0 16px 0' }} role="alert">{sweepErr}</div>
          )}

          <div style={{ display: 'grid', gridTemplateColumns: 'repeat(3, 1fr)', gap: 14, marginBottom: 22 }}>
            {cards.map((c) => (
              <div key={c.label} style={{ border: '1px solid var(--line)', borderRadius: 11, padding: '16px 18px', background: 'var(--panel)' }}>
                <div style={{ fontSize: 10.5, fontWeight: 600, letterSpacing: '0.06em', color: 'var(--ink-3)', fontFamily: MONO, marginBottom: 12 }}>{c.label}</div>
                <div style={{ display: 'flex', alignItems: 'baseline', gap: 6 }}>
                  <span style={{ fontFamily: MONO, fontSize: 22, fontWeight: 600, color: c.color }}>{c.value}</span>
                  <span style={{ fontSize: 12, color: 'var(--ink-3)' }}>{c.unit}</span>
                </div>
                <div style={{ fontSize: 11.5, color: 'var(--ink-3)', marginTop: 8, fontFamily: MONO }}>{c.sub}</div>
              </div>
            ))}
          </div>

          {last?.status === 'failed' && last.error && (
            <div style={{ border: '1px solid var(--err)', background: 'var(--err-bg)', borderRadius: 10, padding: '12px 16px', marginBottom: 22, fontSize: 12.5, color: 'var(--ink)' }} role="alert">
              <div style={{ fontWeight: 600, marginBottom: 4 }}>Последняя очистка завершилась ошибкой</div>
              <div style={{ fontFamily: MONO, fontSize: 11.5, color: 'var(--ink-2)', whiteSpace: 'pre-wrap' }}>{last.error}</div>
            </div>
          )}

          <div style={{ fontSize: 13, fontWeight: 600, color: 'var(--ink)', marginBottom: 12 }}>Политики по задачам</div>
          <div style={{ border: '1px solid var(--line)', borderRadius: 10, overflow: 'hidden', background: 'var(--panel)', marginBottom: 26 }}>
            <div style={{ display: 'grid', gridTemplateColumns: POLICY_COLS, padding: '10px 20px', fontSize: 10, fontWeight: 600, letterSpacing: '0.05em', color: 'var(--ink-3)', borderBottom: '1px solid var(--line)', fontFamily: MONO }}>
              <div>ЗАДАЧА</div><div>ПОЛИТИКА</div>
              <div style={{ textAlign: 'right' }}>АРТЕФАКТОВ</div>
              <div style={{ textAlign: 'right' }}>ЗАНЯТО</div>
              <div style={{ textAlign: 'right' }}>СЛЕД. ОЧИСТКА</div>
            </div>
            {summary.tasks.length === 0 ? (
              <div style={{ padding: '20px', fontSize: 12.5, color: 'var(--ink-3)' }}>Задач пока нет.</div>
            ) : summary.tasks.map((row) => {
              const policy = formatPolicy(row.retention)
              const engine = taskEngine(taskById.get(row.task_id), connById)
              return (
                <div key={row.task_id} style={{ display: 'grid', gridTemplateColumns: POLICY_COLS, padding: '12px 20px', fontSize: 12.5, borderBottom: '1px solid var(--line)', alignItems: 'center' }}>
                  <div style={{ display: 'flex', alignItems: 'center', gap: 8, minWidth: 0 }}>
                    {engine ? <EngineDot engine={engine} /> : null}
                    <span style={{ fontWeight: 500, color: row.enabled ? 'var(--ink)' : 'var(--ink-3)', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{row.name}</span>
                  </div>
                  <div>
                    {policy ? (
                      <span style={{ fontFamily: MONO, fontSize: 11, color: 'var(--ink-2)', background: 'var(--panel-2)', border: '1px solid var(--line)', padding: '2px 8px', borderRadius: 5 }}>{policy}</span>
                    ) : (
                      // No policy is not "nothing to show": it means this task's
                      // artifacts are never pruned, which is worth flagging.
                      <span title="Артефакты этой задачи не удаляются" style={{ fontFamily: MONO, fontSize: 11, color: 'var(--warn)', background: 'var(--warn-bg)', border: '1px solid var(--warn)', padding: '2px 8px', borderRadius: 5 }}>не настроена</span>
                    )}
                  </div>
                  <div style={{ fontFamily: MONO, fontSize: 11.5, color: 'var(--ink-2)', textAlign: 'right' }}>{row.artifacts}</div>
                  <div style={{ fontFamily: MONO, fontSize: 11.5, color: 'var(--ink-2)', textAlign: 'right' }}>{fmtBytes(row.bytes)}</div>
                  <div style={{ fontFamily: MONO, fontSize: 11, color: 'var(--ink-3)', textAlign: 'right' }}>
                    {policy ? fmtDateTime(summary.next_sweep) : '—'}
                  </div>
                </div>
              )
            })}
          </div>

          <div style={{ fontSize: 13, fontWeight: 600, color: 'var(--ink)', marginBottom: 12 }}>История очисток</div>
          <div style={{ border: '1px solid var(--line)', borderRadius: 10, overflow: 'hidden', background: 'var(--panel)' }}>
            {sweeps.length === 0 ? (
              <div style={{ padding: '20px', fontSize: 12.5, color: 'var(--ink-3)' }}>Очисток пока не было.</div>
            ) : sweeps.map((s) => (
              <div key={s.id} style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: 16, padding: '12px 20px', borderBottom: '1px solid var(--line)' }}>
                <div style={{ display: 'flex', alignItems: 'center', gap: 14, minWidth: 0 }}>
                  <span style={{ fontFamily: MONO, fontSize: 12, color: 'var(--ink)', minWidth: 110 }}>{fmtDateTime(s.finished_at)}</span>
                  <StatusBadge status={s.status} />
                  {s.source === 'manual' && (
                    <span style={{ fontFamily: MONO, fontSize: 10, color: 'var(--ink-3)', border: '1px solid var(--line-2)', padding: '1px 6px', borderRadius: 4 }}>вручную</span>
                  )}
                  {s.error && (
                    <span title={s.error} style={{ fontSize: 11.5, color: 'var(--err)', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{s.error}</span>
                  )}
                </div>
                <div style={{ display: 'flex', alignItems: 'center', gap: 24, fontFamily: MONO, fontSize: 11.5, color: 'var(--ink-2)', flexShrink: 0 }}>
                  <span>удалено <span style={{ color: 'var(--ink)' }}>{s.deleted_count}</span></span>
                  <span>освобождено <span style={{ color: 'var(--ink)' }}>{fmtBytes(s.freed_bytes)}</span></span>
                  <span>orphan <span style={{ color: 'var(--ink)' }}>{s.orphan_count}</span></span>
                </div>
              </div>
            ))}
          </div>
        </div>
      )}
    </Layout>
  )
}

interface SummaryCard { label: string; value: string; unit: string; color: string; sub: string }

const POLICY_COLS = '1.7fr 1.2fr 0.9fr 0.9fr 1fr'
const MUTED = { padding: '40px 26px', color: 'var(--ink-3)', fontSize: 13 } as const
const ERRSTYLE = { padding: '40px 26px', color: 'var(--err)', fontSize: 13 } as const
