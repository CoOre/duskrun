import { useEffect, useMemo, useRef, useState } from 'react'
import type { MutableRefObject } from 'react'
import { useSearchParams } from 'react-router-dom'
import { useApiData } from '../api/useApiData'
import { byId, fmtBytes, fmtDateTime, fmtDuration, taskEngine } from '../api/view'
import type { Connection, Run, Task } from '../api/types'
import { useAuth } from '../auth'
import { Layout } from '../ui/Layout'
import { RunDrawer } from '../ui/RunDrawer'
import { EngineDot, MONO, StatusBadge } from '../ui/dc'

type MetaData = { tasks: Task[]; connections: Connection[] }

const STATUS_CHIPS: { key: string; label: string }[] = [
  { key: 'all', label: 'все' },
  { key: 'success', label: 'успешно' },
  { key: 'failed', label: 'ошибка' },
  { key: 'running', label: 'выполняется' },
  { key: 'skipped', label: 'пропущено' },
]

export default function History() {
  const { api, logout } = useAuth()
  const [searchParams, setSearchParams] = useSearchParams()
  const [statusFilter, setStatusFilter] = useState<string>('all')
  const [engineFilter, setEngineFilter] = useState<string>('all')
  const [selected, setSelected] = useState<number | null>(null)
  const [runs, setRuns] = useState<Run[]>([])
  const [runsLoading, setRunsLoading] = useState(true)
  const [runsError, setRunsError] = useState<string | null>(null)
  const [loadingOlder, setLoadingOlder] = useState(false)
  const [loadingNewer, setLoadingNewer] = useState(false)
  const [hasOlder, setHasOlder] = useState(false)
  const [hasNewer, setHasNewer] = useState(false)
  const topSentinel = useRef<HTMLDivElement | null>(null)
  const bottomSentinel = useRef<HTMLDivElement | null>(null)
  const rowRefs = useRef<Map<number, HTMLDivElement>>(new Map())
  const scrollRoot = useRef<HTMLElement | Window | null>(null)
  const loadingOlderRef = useRef(false)
  const loadingNewerRef = useRef(false)
  const urlTimer = useRef<number | null>(null)

  const { data, error: metaError, loading: metaLoading } = useApiData<MetaData>(async () => {
    const [tasks, connections] = await Promise.all([
      api.listTasks(),
      api.listConnections(),
    ])
    return { tasks, connections }
  }, [api])

  const connById = useMemo(() => byId(data?.connections ?? []), [data])
  const taskById = useMemo(() => byId(data?.tasks ?? []), [data])

  useEffect(() => {
    let cancelled = false
    const anchor = parseRunID(searchParams.get('at'))
    setRuns([])
    setRunsLoading(true)
    setRunsError(null)
    setHasOlder(false)
    setHasNewer(false)

    api.listRuns({ status: statusParam(statusFilter), limit: PAGE_SIZE, anchor: anchor ?? undefined })
      .then((page) => {
        if (cancelled) return
        const next = sortRuns(page)
        setRuns(next)
        setHasOlder(page.length === PAGE_SIZE)
        setHasNewer(anchor !== null)
        setRunsLoading(false)
        if (anchor !== null) {
          window.requestAnimationFrame(() => rowRefs.current.get(anchor)?.scrollIntoView({ block: 'start' }))
        }
      })
      .catch((err) => {
        if (cancelled) return
        setRunsError(err instanceof Error ? err.message : String(err))
        setRunsLoading(false)
      })

    return () => {
      cancelled = true
    }
    // `at` is read only on mount/filter reload. Scroll updates replace it in the
    // URL without restarting the current lazy window.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [api, statusFilter])

  useEffect(() => {
    loadingOlderRef.current = loadingOlder
  }, [loadingOlder])

  useEffect(() => {
    loadingNewerRef.current = loadingNewer
  }, [loadingNewer])

  // Live refresh: while any loaded run is active, poll the newest page and merge
  // it by id so running rows flip to their terminal status in place.
  const hasActive = useMemo(() => runs.some((r) => r.status === 'running' || r.status === 'queued'), [runs])
  useEffect(() => {
    if (!hasActive) return
    const poll = setInterval(() => {
      api
        .listRuns({ status: statusParam(statusFilter), limit: PAGE_SIZE })
        .then((page) => setRuns((prev) => mergeRuns(prev, page)))
        .catch(() => {})
    }, 3000)
    return () => clearInterval(poll)
  }, [hasActive, api, statusFilter])

  const engines = useMemo(() => {
    const set = new Set<string>()
    for (const t of data?.tasks ?? []) {
      const e = taskEngine(t, connById)
      if (e) set.add(e)
    }
    return [...set]
  }, [data, connById])

  const engineOf = (r: Run) => taskEngine(taskById.get(r.task_id), connById)
  const shown = runs.filter(
    (r) =>
      (statusFilter === 'all' || r.status === statusFilter) &&
      (engineFilter === 'all' || engineOf(r) === engineFilter),
  )

  const engineChips = [{ key: 'all', label: 'все' }, ...engines.map((e) => ({ key: e, label: e }))]
  const loading = metaLoading || runsLoading
  const error = metaError || runsError

  async function loadOlder() {
    if (loadingOlderRef.current || !hasOlder || runs.length === 0) return
    const before = runs[runs.length - 1].id
    loadingOlderRef.current = true
    setLoadingOlder(true)
    setRunsError(null)
    try {
      const page = await api.listRuns({ status: statusParam(statusFilter), limit: PAGE_SIZE, before })
      setRuns((prev) => mergeRuns(prev, page))
      setHasOlder(page.length === PAGE_SIZE)
    } catch (err) {
      setRunsError(err instanceof Error ? err.message : String(err))
    } finally {
      loadingOlderRef.current = false
      setLoadingOlder(false)
    }
  }

  async function loadNewer() {
    if (loadingNewerRef.current || !hasNewer || runs.length === 0) return
    const after = runs[0].id
    const root = scrollRoot.current
    const beforeHeight = scrollHeight(root)
    loadingNewerRef.current = true
    setLoadingNewer(true)
    setRunsError(null)
    try {
      const page = await api.listRuns({ status: statusParam(statusFilter), limit: PAGE_SIZE, after })
      setRuns((prev) => mergeRuns(prev, page))
      setHasNewer(page.length === PAGE_SIZE)
      window.requestAnimationFrame(() => {
        const delta = scrollHeight(root) - beforeHeight
        if (delta > 0) scrollByRoot(root, delta)
      })
    } catch (err) {
      setRunsError(err instanceof Error ? err.message : String(err))
    } finally {
      loadingNewerRef.current = false
      setLoadingNewer(false)
    }
  }

  useEffect(() => {
    const root = getScrollParent(bottomSentinel.current)
    scrollRoot.current = root
    const onScroll = () => {
      const top = topSentinel.current?.getBoundingClientRect()
      const bottom = bottomSentinel.current?.getBoundingClientRect()
      const rootRect = scrollRootRect(root)
      if (top && top.bottom > rootRect.top - 500) void loadNewer()
      if (bottom && bottom.top < rootRect.bottom + 700) void loadOlder()
      rememberVisibleRun(rowRefs.current, root, setSearchParams, urlTimer)
    }
    root.addEventListener('scroll', onScroll, { passive: true })
    onScroll()
    return () => {
      root.removeEventListener('scroll', onScroll)
      if (urlTimer.current !== null) window.clearTimeout(urlTimer.current)
    }
  }, [runs, hasOlder, hasNewer, loadingOlder, loadingNewer, loading, error, statusFilter, api, setSearchParams])

  return (
    <Layout title="История запусков" subtitle={`загружено ${runs.length}`} onLogout={logout}>
      {loading && <div style={MUTED}>Загрузка…</div>}
      {error && <div style={ERRSTYLE} role="alert">{error}</div>}
      {!loading && !error && (
        <div style={{ padding: '20px 26px' }} className="dc-fade">
          <div style={{ display: 'flex', alignItems: 'center', gap: 8, marginBottom: 16, flexWrap: 'wrap' }}>
            <span style={{ fontSize: 11, color: 'var(--ink-3)', fontFamily: MONO, marginRight: 4 }}>СТАТУС:</span>
            {STATUS_CHIPS.map((c) => (
              <Chip key={c.key} label={c.label} active={statusFilter === c.key} onClick={() => setStatusFilter(c.key)} />
            ))}
            {engines.length > 0 && (
              <>
                <span style={{ fontSize: 11, color: 'var(--ink-3)', fontFamily: MONO, margin: '0 4px 0 16px' }}>СУБД:</span>
                {engineChips.map((c) => (
                  <Chip key={c.key} label={c.label} active={engineFilter === c.key} onClick={() => setEngineFilter(c.key)} />
                ))}
              </>
            )}
          </div>

          {shown.length === 0 ? (
            <div style={MUTED}>Нет запусков под фильтр.</div>
          ) : (
            <div style={{ border: '1px solid var(--line)', borderRadius: 10, overflow: 'hidden', background: 'var(--panel)' }}>
              <div ref={topSentinel} style={SENTINEL}>{loadingNewer ? 'Загрузка…' : ''}</div>
              <div style={{ display: 'grid', gridTemplateColumns: COLS, padding: '10px 20px', fontSize: 10, fontWeight: 600, letterSpacing: '0.05em', color: 'var(--ink-3)', borderBottom: '1px solid var(--line)', fontFamily: MONO }}>
                <div>#</div><div>ЗАДАЧА</div><div>СУБД</div><div>НАЧАЛО</div><div>ДЛИТ.</div><div style={{ textAlign: 'right' }}>РАЗМЕР</div><div style={{ textAlign: 'right' }}>СТАТУС</div>
              </div>
              {shown.map((r) => {
                const task = taskById.get(r.task_id)
                const engine = engineOf(r)
                return (
                  <div
                    key={r.id}
                    ref={(node) => {
                      if (node) rowRefs.current.set(r.id, node)
                      else rowRefs.current.delete(r.id)
                    }}
                    data-run-id={r.id}
                    onClick={() => setSelected(r.id)}
                    className="dc-h-panel2"
                    style={{ display: 'grid', gridTemplateColumns: COLS, padding: '11px 20px', fontSize: 12.5, borderBottom: '1px solid var(--line)', cursor: 'pointer', alignItems: 'center' }}
                  >
                    <div style={{ fontFamily: MONO, fontSize: 11, color: 'var(--ink-3)' }}>{r.id}</div>
                    <div style={{ display: 'flex', alignItems: 'center', gap: 8, minWidth: 0 }}>
                      {r.attempt > 1 && <span style={{ fontFamily: MONO, fontSize: 9.5, fontWeight: 600, color: 'var(--err)', border: '1px solid var(--err)', padding: '0 4px', borderRadius: 3 }}>#{r.attempt}</span>}
                      <span style={{ fontWeight: 500, color: 'var(--ink)', whiteSpace: 'nowrap', overflow: 'hidden', textOverflow: 'ellipsis' }}>{task?.name ?? `задача #${r.task_id}`}</span>
                    </div>
                    <div style={{ display: 'flex', alignItems: 'center', gap: 7 }}>
                      {engine ? <EngineDot engine={engine} /> : null}
                      <span style={{ fontFamily: MONO, color: 'var(--ink-2)', fontSize: 11.5 }}>{engine || '—'}</span>
                    </div>
                    <div style={{ fontFamily: MONO, fontSize: 11.5, color: 'var(--ink-2)' }}>{fmtDateTime(r.started_at ?? r.created_at)}</div>
                    <div style={{ fontFamily: MONO, fontSize: 11.5, color: 'var(--ink-2)' }}>{fmtDuration(r)}</div>
                    <div style={{ fontFamily: MONO, fontSize: 11.5, color: 'var(--ink-2)', textAlign: 'right' }}>{fmtBytes(r.artifact?.size)}</div>
                    <div style={{ textAlign: 'right' }}><StatusBadge status={r.status} /></div>
                  </div>
                )
              })}
              <div ref={bottomSentinel} style={SENTINEL}>{loadingOlder ? 'Загрузка…' : ''}</div>
            </div>
          )}
        </div>
      )}

      {selected !== null && <RunDrawer runId={selected} onClose={() => setSelected(null)} />}
    </Layout>
  )
}

function statusParam(status: string): string | undefined {
  return status === 'all' ? undefined : status
}

function parseRunID(v: string | null): number | null {
  if (!v) return null
  const n = Number(v)
  return Number.isInteger(n) && n > 0 ? n : null
}

function sortRuns(runs: Run[]): Run[] {
  return [...runs].sort((a, b) => b.id - a.id)
}

function mergeRuns(existing: Run[], page: Run[]): Run[] {
  const byRunID = new Map<number, Run>()
  for (const r of existing) byRunID.set(r.id, r)
  for (const r of page) byRunID.set(r.id, r)
  return sortRuns([...byRunID.values()])
}

function rememberVisibleRun(
  rows: Map<number, HTMLDivElement>,
  root: HTMLElement | Window,
  setSearchParams: ReturnType<typeof useSearchParams>[1],
  timer: MutableRefObject<number | null>,
) {
  let best: number | null = null
  let bestTop = Number.POSITIVE_INFINITY
  const rootTop = scrollRootRect(root).top
  rows.forEach((node, id) => {
    const rect = node.getBoundingClientRect()
    const top = Math.abs(rect.top - rootTop)
    if (rect.bottom >= rootTop && top < bestTop) {
      best = id
      bestTop = top
    }
  })
  if (best === null) return
  if (timer.current !== null) window.clearTimeout(timer.current)
  timer.current = window.setTimeout(() => {
    setSearchParams((prev) => {
      const next = new URLSearchParams(prev)
      next.set('at', String(best))
      return next
    }, { replace: true })
  }, 150)
}

function getScrollParent(node: HTMLElement | null): HTMLElement | Window {
  let p = node?.parentElement ?? null
  while (p) {
    const style = window.getComputedStyle(p)
    if (/(auto|scroll)/.test(style.overflowY) && p.scrollHeight > p.clientHeight) return p
    p = p.parentElement
  }
  return window
}

function scrollRootRect(root: HTMLElement | Window): { top: number; bottom: number } {
  if (isWindow(root)) return { top: 0, bottom: window.innerHeight }
  const rect = root.getBoundingClientRect()
  return { top: rect.top, bottom: rect.bottom }
}

function scrollHeight(root: HTMLElement | Window | null): number {
  if (!root || isWindow(root)) return document.documentElement.scrollHeight
  return root.scrollHeight
}

function scrollByRoot(root: HTMLElement | Window | null, dy: number) {
  if (!root || isWindow(root)) window.scrollBy(0, dy)
  else root.scrollTop += dy
}

function isWindow(root: HTMLElement | Window): root is Window {
  return root === window
}

function Chip({ label, active, onClick }: { label: string; active: boolean; onClick: () => void }) {
  return (
    <button
      type="button"
      onClick={onClick}
      className="dc-reset"
      style={{
        padding: '5px 12px',
        borderRadius: 6,
        fontSize: 12,
        fontWeight: 500,
        cursor: 'pointer',
        border: `1px solid ${active ? 'var(--accent)' : 'var(--line-2)'}`,
        background: active ? 'var(--run-bg)' : 'var(--panel)',
        color: active ? 'var(--ink)' : 'var(--ink-2)',
      }}
    >
      {label}
    </button>
  )
}

const PAGE_SIZE = 50
const COLS = '0.5fr 1.7fr 0.9fr 1.1fr 0.8fr 0.7fr 1fr'
const SENTINEL = { minHeight: 1, padding: '4px 20px', color: 'var(--ink-3)', fontFamily: MONO, fontSize: 11, textAlign: 'center' } as const
const MUTED = { padding: '40px 26px', color: 'var(--ink-3)', fontSize: 13 } as const
const ERRSTYLE = { padding: '40px 26px', color: 'var(--err)', fontSize: 13 } as const
