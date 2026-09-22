import { useEffect, useMemo, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { useApiData } from '../api/useApiData'
import { isActiveRun, useRunsPoll } from '../api/useRunsPoll'
import { activityBuckets, byId, fmtBytes, fmtDateTime, fmtDuration, fmtElapsed, fmtTime, nextRunIn, scheduledTasks, taskEngine } from '../api/view'
import type { Connection, Run, Storage, Task, WatchdogAlert } from '../api/types'
import { useAuth } from '../auth'
import { Layout } from '../ui/Layout'
import { RunDrawer } from '../ui/RunDrawer'
import { EngineDot, MONO, StatusBadge, btnPrimary, engineColor } from '../ui/dc'

type Meta = {
  tasks: Task[]
  connections: Connection[]
  storages: Storage[]
  alerts: WatchdogAlert[]
  // null when the watchdog could not be reached. Distinct from an empty list:
  // "we could not check" must never render as "nothing is wrong".
  alertsError: string | null
}

// How often the slow-moving entities (tasks, connections, storages) are
// re-read, and how often "in 14 h" style countdowns are recomputed. Runs have
// their own, faster cadence in useRunsPoll.
const META_REFRESH_MS = 30_000

export default function Dashboard() {
  const { api, logout } = useAuth()
  const navigate = useNavigate()
  const [selected, setSelected] = useState<number | null>(null)

  // One ticker drives both the metadata refresh and every "now"-relative label,
  // so the page keeps up with the scheduler without a reload.
  const [now, setNow] = useState(() => Date.now())
  useEffect(() => {
    const id = setInterval(() => setNow(Date.now()), META_REFRESH_MS)
    return () => clearInterval(id)
  }, [])

  const { data, error, loading } = useApiData<Meta>(async () => {
    const [tasks, connections, storages, alerts] = await Promise.all([
      api.listTasks(),
      api.listConnections(),
      api.listStorages(),
      // Staleness is judged server-side: the same thresholds drive the
      // notifications, so the panel and the alerts that go out agree. A failure
      // here must not sink the whole dashboard, but it is reported rather than
      // swallowed — see alertsError.
      api.listWatchdogAlerts().then(
        (list) => ({ list, error: null as string | null }),
        (e: unknown) => ({ list: [] as WatchdogAlert[], error: e instanceof Error ? e.message : String(e) }),
      ),
    ])
    return { tasks, connections, storages, alerts: alerts.list, alertsError: alerts.error }
  }, [api, now])

  // The full run history seeds the totals (stored bytes, activity chart); the
  // poll then keeps the newest page live on top of it.
  const { data: seedRuns } = useApiData<Run[]>(() => api.listRuns(), [api])
  const { runs, nowMs } = useRunsPoll(api, seedRuns ?? undefined)

  const tasks = data?.tasks ?? []
  const storages = data?.storages ?? []
  const connById = useMemo(() => byId(data?.connections ?? []), [data])
  const taskById = useMemo(() => byId(tasks), [tasks])

  const active = tasks.filter((t) => t.enabled).length
  const paused = tasks.length - active
  const recentRuns24 = runs.filter((r) => {
    const t = Date.parse(r.started_at ?? r.created_at)
    return !Number.isNaN(t) && now - t <= 24 * 60 * 60 * 1000
  })
  const success24 = recentRuns24.filter((r) => r.status === 'success').length
  const failed24 = recentRuns24.filter((r) => r.status === 'failed').length
  const finished24 = success24 + failed24
  const rate24 = finished24 ? Math.round((success24 / finished24) * 100) : 0
  const totalStored = runs.reduce((sum, r) => sum + (r.artifact?.size ?? 0), 0)

  const buckets = activityBuckets(runs, 14, now)
  const maxTotal = Math.max(1, ...buckets.map((b) => b.total))
  // The chart caption describes the charted window, not the whole history.
  const windowRuns = buckets.reduce((sum, b) => sum + b.total, 0)
  const windowFails = buckets.reduce((sum, b) => sum + b.fail, 0)
  // The next task to fire is the soonest one, not the first in id order — and a
  // paused task has no next_run at all, so it can never show up here.
  const upcoming = scheduledTasks(tasks)
  const nextTask = upcoming[0]
  const nextIn = nextTask ? nextRunIn(nextTask, now) : null

  const kpis: Kpi[] = [
    { label: 'АКТИВНЫХ ЗАДАЧ', value: String(active), unit: '', color: 'var(--ink)', sub: `${paused} на паузе`, subColor: 'var(--ink-3)' },
    { label: 'УСПЕШНОСТЬ 24Ч', value: finished24 ? String(rate24) : '—', unit: finished24 ? '%' : '', color: 'var(--ok)', sub: finished24 ? `${success24} из ${finished24}` : 'нет завершённых', subColor: 'var(--ink-3)', donut: `conic-gradient(var(--ok) 0 ${rate24}%, var(--panel-2) ${rate24}% 100%)` },
    { label: 'ОШИБКИ 24Ч', value: String(failed24), unit: '', color: failed24 ? 'var(--err)' : 'var(--ink)', sub: failed24 ? failedTaskName(recentRuns24, taskById) : 'нет ошибок', subColor: failed24 ? 'var(--err)' : 'var(--ink-3)', spark: sparkFrom(buckets.map((b) => b.fail), 'var(--err)') },
    { label: 'ЗАНЯТО', value: storageValue(totalStored), unit: storageUnit(totalStored), color: 'var(--ink)', sub: storages.length ? `${storages.length} ${ruPlural(storages.length, 'хранилище', 'хранилища', 'хранилищ')}` : 'нет хранилищ', subColor: 'var(--ink-3)', spark: sparkFrom(cumulative(buckets.map((b) => b.bytes)), 'var(--accent)') },
    { label: 'СЛЕД. ЗАПУСК', value: nextIn?.value ?? '—', unit: nextIn?.unit ?? '', color: 'var(--accent)', sub: nextTask?.name ?? 'нет запланированных задач', subColor: 'var(--ink-3)' },
  ]

  const alerts = data?.alerts ?? []
  const alertsError = data?.alertsError ?? null
  const storagesMini = buildStoragesMini(storages, runs)
  const dashboardAction = (
    <button style={{ ...btnPrimary, padding: '8px 15px' }} onClick={() => navigate('/tasks')}>
      <span style={{ fontSize: 14, lineHeight: 1 }}>▶</span>Запустить задачу
    </button>
  )

  return (
    <Layout title="Дашборд" subtitle="сводка узла" onLogout={logout} actions={dashboardAction}>
      {loading && <div style={LOADING}>Загрузка…</div>}
      {error && <div style={ERRSTYLE} role="alert">{error}</div>}
      {!loading && !error && (
        <div className="dc-fade">
          {/* KPI strip */}
          <div style={{ display: 'flex', borderBottom: '1px solid var(--line)', background: 'var(--panel)', marginLeft: -1 }}>
            {kpis.map((k) => (
              <div key={k.label} style={{ flex: 1, padding: '18px 22px', borderLeft: '1px solid var(--line)' }}>
                <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: 14 }}>
                  <span style={{ fontSize: 10.5, fontWeight: 600, letterSpacing: '0.06em', color: 'var(--ink-3)', fontFamily: MONO }}>{k.label}</span>
                  {k.donut && (
                    <div style={{ width: 24, height: 24, borderRadius: '50%', background: k.donut, display: 'flex', alignItems: 'center', justifyContent: 'center' }}>
                      <div style={{ width: 15, height: 15, borderRadius: '50%', background: 'var(--panel)' }} />
                    </div>
                  )}
                </div>
                <div style={{ display: 'flex', alignItems: 'baseline', gap: 5 }}>
                  <span style={{ fontFamily: MONO, fontSize: 30, fontWeight: 600, color: k.color, letterSpacing: '-0.03em', fontVariantNumeric: 'tabular-nums' }}>{k.value}</span>
                  <span style={{ fontSize: 13, color: 'var(--ink-3)' }}>{k.unit}</span>
                </div>
                <div style={{ display: 'flex', alignItems: 'flex-end', justifyContent: 'space-between', marginTop: 10, gap: 10 }}>
                  <span style={{ fontSize: 11, color: k.subColor, fontFamily: MONO }}>{k.sub}</span>
                  {k.spark && <SparkBars bars={k.spark} />}
                </div>
              </div>
            ))}
          </div>

          <div style={{ display: 'grid', gridTemplateColumns: '1.85fr 1fr', gap: 0, alignItems: 'stretch' }}>
            <div style={{ borderRight: '1px solid var(--line)', minWidth: 0 }}>
              {/* Activity */}
              <div style={{ padding: '20px 24px', borderBottom: '1px solid var(--line)' }}>
                <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: 18 }}>
                  <div>
                    <div style={{ fontSize: 13.5, fontWeight: 600, color: 'var(--ink)' }}>Активность запусков</div>
                    <div style={{ fontSize: 11, color: 'var(--ink-3)', marginTop: 3, fontFamily: MONO }}>
                      14 дней · {windowRuns} запусков · {windowFails} ошибок
                    </div>
                  </div>
                  <div style={{ display: 'flex', gap: 16, fontSize: 11, color: 'var(--ink-2)' }}>
                    <div style={{ display: 'flex', alignItems: 'center', gap: 6 }}><div style={{ width: 8, height: 8, borderRadius: 2, background: 'var(--ok)' }} />успешно</div>
                    <div style={{ display: 'flex', alignItems: 'center', gap: 6 }}><div style={{ width: 8, height: 8, borderRadius: 2, background: 'var(--err)' }} />ошибка</div>
                  </div>
                </div>
                {buckets.length === 0 ? (
                  <div style={{ color: 'var(--ink-3)', fontSize: 12.5, padding: '20px 0' }}>Пока нет запусков для графика.</div>
                ) : (
                  <>
                    <div style={{ display: 'flex', alignItems: 'flex-end', gap: 6, height: 104, marginBottom: 9, borderBottom: '1px solid var(--line)', paddingBottom: 1 }}>
                      {buckets.map((b) => {
                        const okH = Math.round((b.ok / maxTotal) * 92)
                        const failH = Math.round((b.fail / maxTotal) * 92)
                        return (
                          <div key={b.key} title={`${b.label} — успешно: ${b.ok}, ошибок: ${b.fail}`} style={{ flex: 1, display: 'flex', flexDirection: 'column', justifyContent: 'flex-end', height: '100%', gap: 2 }}>
                            {failH > 0 && <div style={{ width: '100%', background: 'var(--err)', borderRadius: '2px 2px 0 0', height: failH }} />}
                            {okH > 0 && <div style={{ width: '100%', background: 'var(--ok)', borderRadius: failH ? '0' : '2px 2px 0 0', height: okH }} />}
                          </div>
                        )
                      })}
                    </div>
                    <div style={{ display: 'flex', gap: 6 }}>
                      {buckets.map((b, i) => (
                        <div key={b.key} style={{ flex: 1, textAlign: 'center', fontSize: 9.5, color: 'var(--ink-3)', fontFamily: MONO }}>{i % 2 === 0 ? b.label : ''}</div>
                      ))}
                    </div>
                  </>
                )}
              </div>

              {/* Recent runs */}
              <div>
                <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', padding: '16px 24px 10px 24px' }}>
                  <div style={{ fontSize: 13.5, fontWeight: 600, color: 'var(--ink)' }}>Последние запуски</div>
                  <div onClick={() => navigate('/history')} style={{ fontSize: 12, color: 'var(--accent)', fontFamily: MONO, cursor: 'pointer' }}>вся история →</div>
                </div>
                {runs.length === 0 ? (
                  <div style={{ color: 'var(--ink-3)', fontSize: 13, padding: '20px 24px' }}>Запусков пока нет.</div>
                ) : (
                  <>
                    <div style={{ display: 'grid', gridTemplateColumns: RUN_COLS, padding: '8px 24px', fontSize: 10, fontWeight: 600, letterSpacing: '0.05em', color: 'var(--ink-3)', borderTop: '1px solid var(--line)', borderBottom: '1px solid var(--line)', fontFamily: MONO }}>
                      <div>ЗАДАЧА</div><div>СУБД</div><div>НАЧАЛО</div><div>ДЛИТ.</div><div style={{ textAlign: 'right' }}>РАЗМЕР</div><div style={{ textAlign: 'right' }}>СТАТУС</div>
                    </div>
                    {runs.slice(0, 8).map((r) => {
                      const task = taskById.get(r.task_id)
                      const engine = taskEngine(task, connById)
                      return (
                        <div key={r.id} onClick={() => setSelected(r.id)} className="dc-h-panel" style={{ display: 'grid', gridTemplateColumns: RUN_COLS, padding: '11px 24px', fontSize: 12.5, borderBottom: '1px solid var(--line)', cursor: 'pointer', alignItems: 'center' }}>
                          <div style={{ display: 'flex', alignItems: 'center', gap: 8, minWidth: 0 }}>
                            {r.attempt > 1 && <span style={{ fontFamily: MONO, fontSize: 9.5, fontWeight: 600, color: 'var(--err)', border: '1px solid var(--err)', padding: '0 4px', borderRadius: 3 }}>#{r.attempt}</span>}
                            <span style={{ fontWeight: 500, color: 'var(--ink)', whiteSpace: 'nowrap', overflow: 'hidden', textOverflow: 'ellipsis' }}>{task?.name ?? `задача #${r.task_id}`}</span>
                          </div>
                          <div style={{ display: 'flex', alignItems: 'center', gap: 7 }}>
                            {engine ? <EngineDot engine={engine} /> : null}
                            <span style={{ fontFamily: MONO, color: 'var(--ink-2)', fontSize: 11.5 }}>{engine || '—'}</span>
                          </div>
                          <div style={{ fontFamily: MONO, fontSize: 11.5, color: 'var(--ink-2)' }}>{fmtTime(r.started_at ?? r.created_at)}</div>
                          <div style={{ fontFamily: MONO, fontSize: 11.5, color: isActiveRun(r) ? 'var(--run)' : 'var(--ink-2)' }}>
                            {isActiveRun(r) ? fmtElapsed(r.started_at ?? r.created_at, nowMs) : fmtDuration(r)}
                          </div>
                          <div style={{ fontFamily: MONO, fontSize: 11.5, color: 'var(--ink-2)', textAlign: 'right' }}>{fmtBytes(r.artifact?.size)}</div>
                          <div style={{ textAlign: 'right' }}><StatusBadge status={r.status} /></div>
                        </div>
                      )
                    })}
                  </>
                )}
              </div>
            </div>

            <div style={{ minWidth: 0 }}>
              <div style={{ padding: '18px 22px', borderBottom: '1px solid var(--line)', background: 'var(--warn-bg)' }}>
                <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: 12 }}>
                  <div style={{ display: 'flex', alignItems: 'center', gap: 9 }}>
                    <div className="dc-pulse" style={{ width: 8, height: 8, borderRadius: '50%', background: 'var(--warn)' }} />
                    <div style={{ fontSize: 13, fontWeight: 600, color: 'var(--ink)' }}>Наблюдение (watchdog)</div>
                  </div>
                  <span style={{ fontFamily: MONO, fontSize: 11, fontWeight: 600, color: 'var(--warn)' }}>
                    {alertsError ? 'нет данных' : `${alerts.length} ${ruPlural(alerts.length, 'алерт', 'алерта', 'алертов')}`}
                  </span>
                </div>
                {alertsError ? (
                  <div style={{ padding: '9px 0', borderTop: '1px solid var(--line-2)' }} role="alert">
                    <div style={{ fontSize: 12.5, color: 'var(--err)', fontWeight: 500 }}>Не удалось проверить свежесть бэкапов</div>
                    <div style={{ fontSize: 11, color: 'var(--ink-2)', marginTop: 3 }}>{alertsError}</div>
                  </div>
                ) : alerts.length ? alerts.slice(0, 3).map((a) => (
                  <div key={a.task_id} style={{ padding: '9px 0', borderTop: '1px solid var(--line-2)' }}>
                    <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: 10 }}>
                      <div style={{ fontSize: 12.5, color: 'var(--ink)', fontWeight: 500, minWidth: 0, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{a.task}</div>
                      <div style={{ fontSize: 11.5, color: 'var(--warn)', fontFamily: MONO, fontWeight: 600, flexShrink: 0 }}>{fmtAge(a.since_sec)}</div>
                    </div>
                    <div style={{ fontSize: 11, color: 'var(--ink-2)', marginTop: 3 }}>{alertReason(a)}</div>
                  </div>
                )) : <div style={{ padding: '9px 0', borderTop: '1px solid var(--line-2)', fontSize: 12, color: 'var(--ink-2)' }}>Нет активных алертов.</div>}
              </div>

              <div style={{ padding: '18px 22px', borderBottom: '1px solid var(--line)' }}>
                <div style={{ fontSize: 13, fontWeight: 600, color: 'var(--ink)', marginBottom: 12 }}>Ближайшие запуски</div>
                {upcoming.length ? upcoming.slice(0, 5).map((t) => {
                  const engine = taskEngine(t, connById)
                  const countdown = nextRunIn(t, now)
                  return (
                    <div key={t.id} style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', padding: '8px 0', borderTop: '1px solid var(--line)' }}>
                      <div style={{ minWidth: 0, display: 'flex', alignItems: 'center', gap: 9 }}>
                        <div style={{ width: 6, height: 6, borderRadius: 2, background: engine ? engineColor(engine) : 'var(--ink-3)', flexShrink: 0 }} />
                        <div style={{ minWidth: 0 }}>
                          <div style={{ fontSize: 12.5, color: 'var(--ink)', fontWeight: 500, whiteSpace: 'nowrap', overflow: 'hidden', textOverflow: 'ellipsis' }}>{t.name}</div>
                          <div style={{ fontSize: 10.5, color: 'var(--ink-3)', fontFamily: MONO, marginTop: 2 }}>{t.cron} · {fmtDateTime(t.next_run)}</div>
                        </div>
                      </div>
                      <div style={{ fontFamily: MONO, fontSize: 12, color: 'var(--accent)', flexShrink: 0, marginLeft: 10, fontWeight: 500 }}>{countdown ? `${countdown.value} ${countdown.unit}` : '—'}</div>
                    </div>
                  )
                }) : <div style={{ padding: '8px 0', borderTop: '1px solid var(--line)', fontSize: 12, color: 'var(--ink-2)' }}>Нет запланированных задач.</div>}
              </div>

              <div style={{ padding: '18px 22px' }}>
                <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: 12 }}>
                  <div style={{ fontSize: 13, fontWeight: 600, color: 'var(--ink)' }}>Хранилища</div>
                  <span style={{ fontFamily: MONO, fontSize: 11, color: 'var(--ink-3)' }}>{fmtBytes(totalStored)}</span>
                </div>
                {storagesMini.length ? storagesMini.map((s) => (
                  <div key={s.name} style={{ marginBottom: 12 }}>
                    <div style={{ display: 'flex', justifyContent: 'space-between', marginBottom: 6, gap: 10 }}>
                      <span style={{ fontFamily: MONO, fontSize: 11.5, color: 'var(--ink-2)', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{s.name}</span>
                      <span style={{ fontFamily: MONO, color: 'var(--ink-3)', fontSize: 11, flexShrink: 0 }}>{s.usedLabel}</span>
                    </div>
                    <div style={{ height: 6, borderRadius: 3, background: 'var(--panel-2)', overflow: 'hidden' }}>
                      <div style={{ height: '100%', borderRadius: 3, background: 'var(--accent)', width: `${s.pct}%` }} />
                    </div>
                  </div>
                )) : <div style={{ fontSize: 12, color: 'var(--ink-2)' }}>Хранилищ пока нет.</div>}
              </div>
            </div>
          </div>
        </div>
      )}

      {selected !== null && <RunDrawer runId={selected} onClose={() => setSelected(null)} />}
    </Layout>
  )
}

interface Kpi { label: string; value: string; unit: string; color: string; sub: string; subColor: string; donut?: string; spark?: Spark[] }
interface Spark { h: number; color: string }
interface StorageMini { name: string; usedLabel: string; pct: number; used: number }

function SparkBars({ bars }: { bars: Spark[] }) {
  return (
    <div style={{ display: 'flex', alignItems: 'flex-end', gap: 2, height: 20 }}>
      {bars.map((s, i) => <div key={i} style={{ width: 3, borderRadius: 1, background: s.color, height: s.h }} />)}
    </div>
  )
}

// sparkFrom scales real per-day values to bar heights. An all-zero series draws
// a flat baseline rather than nothing, so the tile keeps its shape.
function sparkFrom(values: number[], color: string): Spark[] | undefined {
  if (values.length === 0) return undefined
  const max = Math.max(...values)
  return values.map((v) => ({ h: Math.max(2, Math.round((max ? v / max : 0) * 20)), color }))
}

// Running total, so a "stored bytes" spark shows growth rather than daily spikes.
function cumulative(values: number[]): number[] {
  let sum = 0
  return values.map((v) => (sum += v))
}

function failedTaskName(runs: Run[], taskById: Map<number, Task>): string {
  const run = runs.find((r) => r.status === 'failed')
  return run ? taskById.get(run.task_id)?.name ?? `задача #${run.task_id}` : 'требуют внимания'
}

function storageValue(bytes: number): string {
  const [value] = fmtBytes(bytes).split(' ')
  return value
}

function storageUnit(bytes: number): string {
  const [, unit = 'Б'] = fmtBytes(bytes).split(' ')
  return unit === '—' ? '' : unit
}

// fmtAge renders a watchdog gap or threshold compactly. Sub-hour values are
// real — watchdog_sec accepts any non-negative number — so they must read as
// minutes rather than being rounded up into a misleading "1 ч".
export function fmtAge(sec: number): string {
  if (sec < 60) return `${Math.max(1, Math.round(sec))} с`
  if (sec < 3600) return `${Math.round(sec / 60)} мин`
  const hours = Math.floor(sec / 3600)
  if (hours < 48) return `${hours} ч`
  return `${Math.floor(hours / 24)} д`
}

// alertReason localises the server's cause. The server sends English (it also
// goes to Telegram/webhook, where the operator may not be reading Russian), so
// the UI states it in its own terms rather than echoing the wire text.
export function alertReason(a: WatchdogAlert): string {
  if (!a.last_success) return 'успешных бэкапов ещё не было'
  return `превышен порог watchdog (${fmtAge(a.threshold_sec)})`
}

// Storage bars show each storage's share of the bytes Duskrun has actually
// written. Real capacity is not something the backend reports, so the bar is
// explicitly relative — no invented disk size.
function buildStoragesMini(storages: Storage[], runs: Run[]): StorageMini[] {
  const usedById = new Map<number, number>()
  for (const r of runs) {
    if (!r.artifact) continue
    usedById.set(r.artifact.storage_id, (usedById.get(r.artifact.storage_id) ?? 0) + r.artifact.size)
  }
  const total = [...usedById.values()].reduce((a, b) => a + b, 0)
  return storages
    .map((s) => {
      const used = usedById.get(s.id) ?? 0
      return { name: s.name, usedLabel: fmtBytes(used), pct: total ? Math.round((used / total) * 100) : 0, used }
    })
    .sort((a, b) => b.used - a.used)
    .slice(0, 3)
}

function ruPlural(n: number, one: string, few: string, many: string): string {
  const mod10 = n % 10
  const mod100 = n % 100
  if (mod10 === 1 && mod100 !== 11) return one
  if (mod10 >= 2 && mod10 <= 4 && (mod100 < 12 || mod100 > 14)) return few
  return many
}

const RUN_COLS = '1.7fr 0.9fr 0.8fr 0.8fr 0.7fr 1fr'
const LOADING = { padding: '40px 26px', color: 'var(--ink-3)', fontSize: 13 } as const
const ERRSTYLE = { padding: '40px 26px', color: 'var(--err)', fontSize: 13 } as const
