import { useEffect, useMemo, useState } from 'react'
import type { ApiClient } from './client'
import type { Run } from './types'

export const isActiveRun = (r?: Run): boolean => r?.status === 'running' || r?.status === 'queued'

export interface RunsPollOptions {
  // Cadence while some run is queued/running — fast enough to feel live.
  activeMs?: number
  // Cadence when everything is terminal. Polling never stops: a run started by
  // the dispatcher (or from another browser tab) has to become visible without
  // a page reload, and nothing on this page would otherwise notice it.
  idleMs?: number
  // Page size of each poll. Only the newest runs can have changed, so a poll
  // fetches a bounded page instead of the whole history; `mergeById` keeps the
  // older runs that came from `seed`.
  limit?: number
}

// useRunsPoll keeps a live copy of a run list. It seeds from `seed` (typically
// an initial useApiData load of the full history) and polls the newest page,
// merging by id — so status badges flip from "выполняется" to a terminal state,
// and brand-new runs appear, without a full-page reload. With no `seed` the
// hook fetches its own first page immediately.
//
// `nowMs` advances each second while a run is active, so callers can render
// live elapsed timers.
export function useRunsPoll(
  api: ApiClient,
  seed?: Run[],
  opts: RunsPollOptions = {},
): { runs: Run[]; hasActive: boolean; nowMs: number } {
  const { activeMs = 3000, idleMs = 15000, limit = 50 } = opts
  const [runs, setRuns] = useState<Run[]>([])
  useEffect(() => {
    if (!seed) return
    // A seed is a full, authoritative history, so it wins on every id it covers;
    // runs the poll found while the seed request was in flight are kept in front.
    const ids = new Set(seed.map((r) => r.id))
    setRuns((prev) => [...prev.filter((r) => !ids.has(r.id)), ...seed])
  }, [seed])

  const hasActive = useMemo(() => runs.some(isActiveRun), [runs])
  const [nowMs, setNowMs] = useState(() => Date.now())

  useEffect(() => {
    if (!hasActive) return
    const tick = setInterval(() => setNowMs(Date.now()), 1000)
    return () => clearInterval(tick)
  }, [hasActive])

  useEffect(() => {
    let alive = true
    const fetchPage = () => {
      api
        .listRuns({ limit })
        .then((page) => alive && setRuns((prev) => mergeById(prev, page)))
        .catch(() => {})
    }
    // Without a seed this hook owns the first load; with one, the seed already
    // covers the initial paint and the interval takes over from there.
    if (!seed) fetchPage()
    const poll = setInterval(fetchPage, hasActive ? activeMs : idleMs)
    return () => {
      alive = false
      clearInterval(poll)
    }
  }, [hasActive, api, activeMs, idleMs, limit, seed])

  return { runs, hasActive, nowMs }
}

// mergeById overlays `page` onto `prev` by run id (page wins), preserving prev's
// order and appending any genuinely new runs at the front (newest ids first).
function mergeById(prev: Run[], page: Run[]): Run[] {
  const updated = new Map(page.map((r) => [r.id, r]))
  const merged = prev.map((r) => updated.get(r.id) ?? r)
  const seen = new Set(prev.map((r) => r.id))
  const fresh = page.filter((r) => !seen.has(r.id)).sort((a, b) => b.id - a.id)
  return [...fresh, ...merged]
}
