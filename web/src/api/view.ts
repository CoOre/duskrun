// View helpers that derive display fields from the real API DTOs (the backend
// does not return engine-per-run, duration, or a task-name join, so we compute
// them here). Kept framework-free and unit-testable.
import type { Connection, Run, Task } from './types'

// Map helpers -------------------------------------------------------------

export function byId<T extends { id: number }>(items: T[]): Map<number, T> {
  return new Map(items.map((i) => [i.id, i]))
}

// The DB engine backing a task, resolved through its connection.
export function taskEngine(task: Task | undefined, conns: Map<number, Connection>): string {
  if (!task) return ''
  return conns.get(task.connection_id)?.engine ?? ''
}

// Runs come back newest-first; the first run seen per task is its latest.
export function latestRunByTask(runs: Run[]): Map<number, Run> {
  const m = new Map<number, Run>()
  for (const r of runs) if (!m.has(r.task_id)) m.set(r.task_id, r)
  return m
}

// Schedule ----------------------------------------------------------------

// Enabled tasks that have a next_run, soonest first. Paused tasks and tasks
// whose cron the backend could not parse are dropped: neither will ever fire.
export function scheduledTasks(tasks: Task[]): Task[] {
  return tasks
    .filter((t) => t.enabled && parse(t.next_run) !== null)
    .sort((a, b) => (parse(a.next_run) as number) - (parse(b.next_run) as number))
}

// "14 ч" until a task's next activation; null when it has no schedule.
export function nextRunIn(task: Task, nowMs: number): Countdown | null {
  const at = parse(task.next_run)
  return at === null ? null : fmtCountdown(at - nowMs)
}

export interface Countdown {
  value: string
  unit: string
}

// A coarse "time from now", split into value and unit so a KPI tile can size
// them differently. Rounds to the largest unit that keeps the number readable.
export function fmtCountdown(ms: number): Countdown {
  const secs = Math.max(0, Math.round(ms / 1000))
  if (secs < 60) return { value: '<1', unit: 'мин' }
  const mins = Math.round(secs / 60)
  if (mins < 60) return { value: String(mins), unit: 'мин' }
  const hours = Math.round(mins / 60)
  if (hours < 48) return { value: String(hours), unit: 'ч' }
  return { value: String(Math.round(hours / 24)), unit: 'д' }
}

// Formatting --------------------------------------------------------------

function parse(iso?: string): number | null {
  if (!iso) return null
  const t = Date.parse(iso)
  return Number.isNaN(t) ? null : t
}

// "4м 12с" from a run's started/finished timestamps; "—" when unknown.
export function fmtDuration(run: Run): string {
  const start = parse(run.started_at)
  const end = parse(run.finished_at)
  if (start === null) return '—'
  if (run.status === 'running' || end === null) return '…'
  const secs = Math.max(0, Math.round((end - start) / 1000))
  const m = Math.floor(secs / 60)
  const s = secs % 60
  return `${m}м ${String(s).padStart(2, '0')}с`
}

const pad = (n: number) => String(n).padStart(2, '0')

export function fmtBytes(bytes?: number): string {
  if (!bytes || bytes <= 0) return '—'
  const units = ['Б', 'КБ', 'МБ', 'ГБ', 'ТБ']
  let n = bytes
  let u = 0
  while (n >= 1024 && u < units.length - 1) {
    n /= 1024
    u++
  }
  const digits = n >= 10 || u === 0 ? 0 : 1
  return `${n.toFixed(digits)} ${units[u]}`
}

// "12.4 МБ/с" — throughput from bytes-per-second; "—" when unknown/zero.
export function fmtThroughput(bps?: number): string {
  if (!bps || bps <= 0) return '—'
  return `${fmtBytes(bps)}/с`
}

// "4м 12с" from a running run's start to now — a live elapsed timer. Callers
// re-render on a ticker to advance it.
export function fmtElapsed(startIso: string | undefined, nowMs: number): string {
  const start = parse(startIso)
  if (start === null) return '—'
  const secs = Math.max(0, Math.round((nowMs - start) / 1000))
  const m = Math.floor(secs / 60)
  const s = secs % 60
  return `${m}м ${String(s).padStart(2, '0')}с`
}

// "21.07 02:00" — short local date+time; falls back to created_at.
export function fmtDateTime(iso?: string): string {
  const t = parse(iso)
  if (t === null) return '—'
  const d = new Date(t)
  return `${pad(d.getDate())}.${pad(d.getMonth() + 1)} ${pad(d.getHours())}:${pad(d.getMinutes())}`
}

// "02:00" — time only.
export function fmtTime(iso?: string): string {
  const t = parse(iso)
  if (t === null) return '—'
  const d = new Date(t)
  return `${pad(d.getHours())}:${pad(d.getMinutes())}`
}

// The moment a run started (or was created), for sorting/bucketing.
export function runMoment(run: Run): string | undefined {
  return run.started_at ?? run.created_at
}

// Activity buckets: one per calendar day over the trailing `days`-day window
// ending today, oldest→newest. Days without runs are kept as empty buckets —
// collapsing them would draw a dense bar chart over a sparse schedule. Returns
// [] when the whole window is empty, so callers can show an empty state.
export interface DayBucket {
  key: string
  label: string
  ok: number
  fail: number
  total: number
  bytes: number
}

const dayKey = (d: Date) => `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`

export function activityBuckets(runs: Run[], days = 14, nowMs = Date.now()): DayBucket[] {
  const counts = new Map<string, DayBucket>()
  const today = new Date(nowMs)
  today.setHours(0, 0, 0, 0)

  const out: DayBucket[] = []
  for (let i = days - 1; i >= 0; i--) {
    const d = new Date(today)
    d.setDate(today.getDate() - i)
    const bucket: DayBucket = { key: dayKey(d), label: String(d.getDate()), ok: 0, fail: 0, total: 0, bytes: 0 }
    counts.set(bucket.key, bucket)
    out.push(bucket)
  }

  for (const r of runs) {
    const t = parse(runMoment(r))
    if (t === null) continue
    const b = counts.get(dayKey(new Date(t)))
    if (!b) continue // outside the window
    b.total++
    b.bytes += r.artifact?.size ?? 0
    if (r.status === 'success') b.ok++
    else if (r.status === 'failed') b.fail++
  }
  return out.some((b) => b.total > 0) ? out : []
}
