import { useEffect, useRef, useState } from 'react'
import { useApiData } from '../api/useApiData'
import { byId, fmtBytes, fmtDateTime, fmtDuration, fmtElapsed, taskEngine } from '../api/view'
import type { ApiClient } from '../api/client'
import type { Connection, Run, RunPhase, RunProgressEvent, Task } from '../api/types'
import { useAuth, useCan } from '../auth'
import { LiveMeter, MONO, StatusBadge, btnPrimary, btnSecondary } from './dc'

type DrawerData = { run: Run; tasks: Task[]; connections: Connection[] }

interface LiveState {
  lines: string[]
  phase?: RunPhase
  bytes: number
  total: number
  bps: number
  active: boolean
}

const IDLE_LIVE: LiveState = { lines: [], bytes: 0, total: 0, bps: 0, active: false }

function reduceEvent(s: LiveState, ev: RunProgressEvent): LiveState {
  switch (ev.kind) {
    case 'log':
      return ev.message ? { ...s, lines: [...s.lines, ev.message], phase: ev.phase ?? s.phase } : s
    case 'phase':
      return { ...s, phase: ev.phase ?? s.phase }
    case 'total':
      return { ...s, total: ev.total ?? s.total }
    case 'bytes':
      return { ...s, bytes: ev.bytes ?? s.bytes, total: ev.total || s.total, bps: ev.throughput_bps ?? 0 }
    case 'status':
      return { ...s, active: false, bps: 0 }
    default:
      return s
  }
}

const isActive = (run?: Run) => run?.status === 'running' || run?.status === 'queued'

// useRunStream opens the SSE progress stream for an active run and accumulates
// live log lines, the current phase and byte/throughput counters. onTerminal
// fires once when the run reaches a terminal status (so the drawer can refetch
// the finished run for its artifact/checksum).
function useRunStream(api: ApiClient, run: Run | undefined, onTerminal: () => void): LiveState {
  const [st, setSt] = useState<LiveState>(IDLE_LIVE)
  const termRef = useRef(onTerminal)
  termRef.current = onTerminal
  const runId = run?.id
  const active = isActive(run)
  useEffect(() => {
    if (!runId || !active) return
    const ctrl = new AbortController()
    setSt({ ...IDLE_LIVE, active: true })
    api
      .streamRun(
        runId,
        (ev) => {
          setSt((s) => reduceEvent(s, ev))
          if (ev.kind === 'status') termRef.current()
        },
        ctrl.signal,
      )
      .catch(() => {})
    return () => ctrl.abort()
  }, [api, runId, active])
  return st
}

// useTicker returns a wall-clock timestamp that advances every second while
// enabled — drives the live elapsed timer without a prop from the parent.
function useTicker(enabled: boolean): number {
  const [now, setNow] = useState(() => Date.now())
  useEffect(() => {
    if (!enabled) return
    const t = setInterval(() => setNow(Date.now()), 1000)
    return () => clearInterval(t)
  }, [enabled])
  return now
}

// RunDrawer fetches one run and its display context. Styling is ported from the
// comp's drawer, while pipeline stages are derived from current DTOs.
export function RunDrawer({ runId, onClose }: { runId: number; onClose: () => void }) {
  const { api } = useAuth()
  // Downloading is operator+: a viewer may see that a backup happened, not pull
  // down a copy of the production database.
  const canDownload = useCan('operator')
  const [reload, setReload] = useState(0)
  const { data, error, loading } = useApiData<DrawerData>(async () => {
    const [run, tasks, connections] = await Promise.all([
      api.getRun(runId),
      api.listTasks(),
      api.listConnections(),
    ])
    return { run, tasks, connections }
  }, [runId, reload])
  const [hint, setHint] = useState<string | null>(null)
  const [actionErr, setActionErr] = useState<string | null>(null)
  const run = data?.run
  // When a live run finishes, refetch it once to pick up the artifact/checksum.
  const live = useRunStream(api, run, () => setReload((n) => n + 1))
  const now = useTicker(live.active)
  const task = run ? byId(data?.tasks ?? []).get(run.task_id) : undefined
  const connById = byId(data?.connections ?? [])
  const engine = taskEngine(task, connById)
  const stages = run && task ? buildStages(run, task, connById.get(task.connection_id)?.connector_type ?? 'direct', live.active ? live.phase : undefined, live) : []
  const duration = run && live.active ? fmtElapsed(run.started_at ?? run.created_at, now) : run ? fmtDuration(run) : '—'
  const logText = live.lines.length ? live.lines.join('\n') : run?.log ?? ''

  async function showRestoreHint() {
    if (!run) return
    setActionErr(null)
    try {
      const { restore_hint } = await api.restoreHint(run.task_id)
      setHint(restore_hint)
    } catch (e) {
      setActionErr(e instanceof Error ? e.message : String(e))
    }
  }

  async function downloadArtifact() {
    if (!run) return
    setActionErr(null)
    try {
      const blob = await api.downloadRunArtifact(run.id)
      const url = URL.createObjectURL(blob)
      const a = document.createElement('a')
      a.href = url
      a.download = run.artifact?.key.split('/').pop() ?? `run-${run.id}.dump`
      document.body.appendChild(a)
      a.click()
      a.remove()
      URL.revokeObjectURL(url)
    } catch (e) {
      setActionErr(e instanceof Error ? e.message : String(e))
    }
  }

  const label = (t: string) => (
    <div style={{ fontSize: 10, color: 'var(--ink-3)', fontWeight: 600, letterSpacing: '0.04em', marginBottom: 5, fontFamily: MONO }}>{t}</div>
  )
  const value = (v: string) => (
    <div style={{ fontFamily: MONO, fontSize: 12.5, color: 'var(--ink)' }}>{v}</div>
  )

  return (
    <div onClick={onClose} style={{ position: 'fixed', inset: 0, background: 'var(--overlay)', zIndex: 20 }}>
      <aside
        role="dialog"
        aria-label={`Запуск #${runId}`}
        onClick={(e) => e.stopPropagation()}
        style={{ position: 'fixed', top: 0, right: 0, bottom: 0, width: 480, maxWidth: '100%', background: 'var(--app)', borderLeft: '1px solid var(--line-2)', zIndex: 21, display: 'flex', flexDirection: 'column', boxShadow: '-24px 0 50px oklch(0 0 0 / 0.4)', animation: 'drawer-in 0.2s ease' }}
      >
        <div style={{ display: 'flex', alignItems: 'flex-start', justifyContent: 'space-between', padding: '20px 24px', borderBottom: '1px solid var(--line)' }}>
          <div>
            <div style={{ display: 'flex', alignItems: 'center', gap: 10 }}>
              <span style={{ fontSize: 16, fontWeight: 700, color: 'var(--ink)' }}>{task?.name ?? `Запуск #${runId}`}</span>
              {run && <StatusBadge status={run.status} />}
            </div>
            {run && (
              <div style={{ fontFamily: MONO, fontSize: 11.5, color: 'var(--ink-3)', marginTop: 5 }}>
                запуск #{run.id} · {fmtDateTime(run.started_at ?? run.created_at)}
              </div>
            )}
          </div>
          <button type="button" onClick={onClose} aria-label="Закрыть" className="dc-reset dc-h-panel2" style={{ width: 30, height: 30, borderRadius: 6, display: 'flex', alignItems: 'center', justifyContent: 'center', cursor: 'pointer', color: 'var(--ink-2)', fontSize: 15, flexShrink: 0 }}>✕</button>
        </div>

        <div style={{ flex: 1, overflowY: 'auto', display: 'flex', flexDirection: 'column' }}>
          {loading && <div style={{ padding: '24px', color: 'var(--ink-3)', fontSize: 13 }}>Загрузка…</div>}
          {error && <div role="alert" style={{ padding: '24px', color: 'var(--err)', fontSize: 13 }}>{error}</div>}
          {run && (
            <>
              <div style={{ padding: '18px 24px', borderBottom: '1px solid var(--line)' }}>
                <div style={{ fontSize: 10, fontWeight: 600, letterSpacing: '0.06em', color: 'var(--ink-3)', marginBottom: 12, fontFamily: MONO }}>ПАЙПЛАЙН</div>
                <div style={{ display: 'flex', alignItems: 'center' }}>
                  {stages.map((p, i) => (
                    <div key={p.name} style={{ flex: 1, display: 'flex', alignItems: 'center' }}>
                      <div className={p.pulse ? 'dc-pulse' : undefined} style={{ flex: 1, padding: '10px 8px', borderRadius: 7, background: p.bg, border: `1px solid ${p.border}`, textAlign: 'center', minWidth: 0 }}>
                        <div style={{ fontSize: 11.5, fontWeight: 600, color: p.titleColor }}>{p.name}</div>
                        <div style={{ fontFamily: MONO, fontSize: 9.5, color: 'var(--ink-2)', marginTop: 3, whiteSpace: 'nowrap', overflow: 'hidden', textOverflow: 'ellipsis' }}>{p.detail}</div>
                      </div>
                      {i < stages.length - 1 && <div style={{ color: 'var(--ink-3)', fontSize: 11, padding: '0 3px' }}>→</div>}
                    </div>
                  ))}
                </div>
              </div>

              <div style={{ padding: '18px 24px', display: 'grid', gridTemplateColumns: '1fr 1fr', gap: '15px 14px', borderBottom: '1px solid var(--line)' }}>
                <div>{label('СУБД')}{value(engine || '—')}</div>
                <div>{label('ДЛИТЕЛЬНОСТЬ')}{value(duration)}</div>
                <div>{label('РАЗМЕР')}{value(fmtBytes(run.artifact?.size))}</div>
                <div>{label('ПОПЫТКА')}{value(attemptLabel(run, task))}</div>
                {run.worker && <div>{label('ВОРКЕР')}{value(run.worker)}</div>}
                {live.active && (
                  <div style={{ gridColumn: '1 / -1' }}>
                    {label('ПЕРЕДАНО В STORAGE')}
                    <LiveMeter bytes={live.bytes} bps={live.bps} total={live.total} active />
                  </div>
                )}
                <div style={{ gridColumn: '1 / -1' }}>
                  {label('КЛЮЧ АРТЕФАКТА')}
                  <div style={{ fontFamily: MONO, fontSize: 11.5, color: 'var(--ink-2)', wordBreak: 'break-all', background: 'var(--panel)', padding: '8px 10px', borderRadius: 6, border: '1px solid var(--line)' }}>{run.artifact?.key ?? '—'}</div>
                </div>
                {run.artifact?.checksum && (
                  <div style={{ gridColumn: '1 / -1' }}>{label('SHA-256')}{<div style={{ fontFamily: MONO, fontSize: 11, color: 'var(--ink-3)', wordBreak: 'break-all' }}>{run.artifact.checksum}</div>}</div>
                )}
                {run.error && (
                  <div style={{ gridColumn: '1 / -1' }}>
                    {label('ОШИБКА')}
                    <div style={{ fontFamily: MONO, fontSize: 11.5, color: 'var(--err)', wordBreak: 'break-all', background: 'var(--panel)', padding: '8px 10px', borderRadius: 6, border: '1px solid var(--line)' }}>{run.error}</div>
                  </div>
                )}
              </div>

              <div style={{ padding: '16px 24px 8px 24px', fontSize: 10, color: 'var(--ink-3)', fontWeight: 600, letterSpacing: '0.04em', fontFamily: MONO, display: 'flex', alignItems: 'center', gap: 7 }}>
                ЛОГ ЗАПУСКА
                {live.active && <span className="dc-pulse-fast" style={{ width: 6, height: 6, borderRadius: '50%', background: 'var(--run)' }} />}
              </div>
              <div style={{ flex: 1, minHeight: 120, overflowY: 'auto', margin: '0 24px', padding: '12px 14px', fontFamily: MONO, fontSize: 11.5, lineHeight: 1.75, color: 'var(--ink-2)', whiteSpace: 'pre-wrap', background: 'var(--sidebar)', border: '1px solid var(--line)', borderRadius: 7 }}>
                {logText ? logText : live.active ? 'Ожидание вывода…' : 'Лог пуст.'}
              </div>

              {hint && (
                <pre style={{ margin: '0 24px 12px', padding: '12px 14px', fontFamily: MONO, fontSize: 11.5, color: 'var(--ink-2)', whiteSpace: 'pre-wrap', background: 'var(--panel)', border: '1px solid var(--line)', borderRadius: 7 }}>{hint}</pre>
              )}
              {actionErr && <div role="alert" style={{ margin: '0 24px 12px', color: 'var(--err)', fontSize: 12.5 }}>{actionErr}</div>}
            </>
          )}
        </div>

        <div style={{ padding: '16px 24px', borderTop: '1px solid var(--line)', display: 'flex', gap: 10 }}>
          {canDownload && (
            <button type="button" onClick={downloadArtifact} disabled={!run || !run.artifact} style={{ ...btnPrimary, flex: 1, justifyContent: 'center', padding: 10, opacity: run && run.artifact ? 1 : 0.5 }}>Скачать артефакт</button>
          )}
          <button type="button" onClick={showRestoreHint} disabled={!run} style={{ ...btnSecondary, padding: '10px 16px', color: 'var(--ink)', opacity: run ? 1 : 0.5 }}>Команда restore</button>
        </div>
      </aside>
    </div>
  )
}

interface StageView {
  name: string
  detail: string
  bg: string
  border: string
  titleColor: string
  pulse?: boolean
}

type StageState = 'ok' | 'fail' | 'skip' | 'active'

function stage(name: string, detail: string, state: StageState): StageView {
  if (state === 'ok') return { name, detail, bg: 'var(--ok-bg)', border: 'var(--ok)', titleColor: 'var(--ok)' }
  if (state === 'fail') return { name, detail, bg: 'var(--err-bg)', border: 'var(--err)', titleColor: 'var(--err)' }
  if (state === 'active') return { name, detail, bg: 'var(--run-bg)', border: 'var(--run)', titleColor: 'var(--run)', pulse: true }
  return { name, detail, bg: 'var(--skip-bg)', border: 'var(--line-2)', titleColor: 'var(--ink-3)' }
}

// phaseStates maps a live phase to per-stage states. The pipeline is one fused
// stream, so the `stream` phase lights the whole Dumper→Codec→Storage stretch as
// in-flight (not just one box) with the Connector already done.
// Order: [Connector, Dumper, Codec, Storage].
function phaseStates(phase: RunPhase): StageState[] {
  switch (phase) {
    case 'queued':
      return ['skip', 'skip', 'skip', 'skip']
    case 'resolve':
      return ['active', 'skip', 'skip', 'skip']
    case 'stream':
      return ['ok', 'active', 'active', 'active']
    case 'record':
      return ['ok', 'ok', 'ok', 'active']
  }
}

function buildStages(run: Run, task: Task, connector: string, phase?: RunPhase, live?: LiveState): StageView[] {
  const codec = task.codec_chain.length ? task.codec_chain.join(' → ') : 'без обработки'
  let states: StageState[]
  if (phase) {
    // Live run: drive the stages from the current phase.
    states = phaseStates(phase)
  } else if (run.status === 'success') {
    states = ['ok', 'ok', 'ok', 'ok']
  } else if (run.status === 'failed') {
    states = ['ok', 'fail', 'skip', 'skip']
  } else if (run.status === 'running' || run.status === 'queued') {
    // Active but no phase event yet — assume the stream is in flight.
    states = ['ok', 'active', 'active', 'active']
  } else {
    states = ['skip', 'skip', 'skip', 'skip']
  }
  // While streaming, the Storage box is the frontier where bytes land: surface the
  // live transferred amount (and % if an estimate exists) right on it.
  let storageDetail = `storage #${task.storage_id}`
  if (live?.active && live.bytes > 0) {
    storageDetail = fmtBytes(live.bytes)
    if (live.total > 0) storageDetail += ` · ≈${Math.min(99, Math.round((live.bytes / live.total) * 100))}%`
  }
  return [
    stage('Connector', connector, states[0]),
    stage('Dumper', dumperLabel(task), states[1]),
    stage('Codec', codec, states[2]),
    stage('Storage', storageDetail, states[3]),
  ]
}

function dumperLabel(task: Task): string {
  const opts = task.dumper_opts && typeof task.dumper_opts === 'object' ? task.dumper_opts as Record<string, unknown> : {}
  return typeof opts.tool === 'string' ? opts.tool : 'dump'
}

function attemptLabel(run: Run, task?: Task): string {
  const max = task?.retries != null ? task.retries + 1 : run.attempt
  return `${run.attempt} / ${Math.max(run.attempt, max)}`
}
