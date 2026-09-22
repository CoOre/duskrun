import { screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { beforeEach, expect, test } from 'vitest'
import { makeFetch, renderWithAuth } from '../test/utils'
import Retention, { formatPolicy } from './Retention'

beforeEach(() => localStorage.clear())

const TASKS = [
  { id: 1, name: 'orders-db nightly', connection_id: 1, storage_id: 1, cron: '0 2 * * *', codec_chain: ['zstd'], retention: { keep_last: 14 }, notifiers: [], enabled: true, retries: 0, timeout_sec: 1800 },
  { id: 2, name: 'wiki-mysql', connection_id: 2, storage_id: 1, cron: '0 3 * * 0', codec_chain: [], retention: {}, notifiers: [], enabled: true, retries: 0, timeout_sec: 1800 },
]

const CONNECTIONS = [
  { id: 1, name: 'pg-primary', engine: 'postgres', connector_type: 'direct', username: '', secret_ref: '' },
  { id: 2, name: 'mysql-wiki', engine: 'mysql', connector_type: 'direct', username: '', secret_ref: '' },
]

const SUMMARY = {
  cron: '30 3 * * *',
  next_sweep: '2026-08-13T03:30:00Z',
  last_sweep: {
    id: 9, started_at: '2026-08-12T03:30:00Z', finished_at: '2026-08-12T03:30:12Z',
    status: 'success', source: 'schedule', deleted_count: 42, freed_bytes: 65498251264, orphan_count: 3,
  },
  tasks: [
    { task_id: 1, name: 'orders-db nightly', enabled: true, retention: { keep_last: 14 }, artifacts: 14, bytes: 26843545600 },
    { task_id: 2, name: 'wiki-mysql', enabled: true, retention: {}, artifacts: 31, bytes: 6442450944 },
  ],
}

function routes(over: Record<string, unknown> = {}) {
  return {
    'GET /api/retention': SUMMARY,
    'GET /api/retention/sweeps': [SUMMARY.last_sweep],
    'GET /api/tasks': TASKS,
    'GET /api/connections': CONNECTIONS,
    ...over,
  }
}

test('TestFormatPolicy: renders keep_last, GFS, and their union', () => {
  expect(formatPolicy({ keep_last: 14 })).toBe('keep_last 14')
  expect(formatPolicy({ gfs: { daily: 7, weekly: 4, monthly: 12 } })).toBe('GFS 7/4/12')
  expect(formatPolicy({ keep_last: 3, gfs: { daily: 7, weekly: 0, monthly: 0 } })).toBe('keep_last 3 + GFS 7/0/0')
  // An unset policy formats to empty — the caller renders it as a warning.
  expect(formatPolicy({})).toBe('')
  expect(formatPolicy({ gfs: { daily: 0, weekly: 0, monthly: 0 } })).toBe('')
})

// card returns the summary tile carrying the given label, so assertions on bare
// numbers ("3") cannot accidentally match elsewhere on the page.
function card(label: string): HTMLElement {
  return screen.getByText(label).parentElement as HTMLElement
}

test('TestRetentionSummaryRendersSchedule: cron, last sweep and orphans come from the API', async () => {
  renderWithAuth(<Retention />, { fetchImpl: makeFetch(routes()) })

  expect(await screen.findByText('30 3 * * *')).toBeInTheDocument()
  expect(within(card('ПОСЛЕДНИЙ ПРОГОН')).getByText('42')).toBeInTheDocument()
  expect(within(card('ПОСЛЕДНИЙ ПРОГОН')).getByText(/освобождено 61 ГБ/)).toBeInTheDocument()
  expect(within(card('ORPHAN-ОБЪЕКТЫ')).getByText('3')).toBeInTheDocument()
})

test('TestRetentionFlagsUnconfiguredPolicy: a task with no policy is called out', async () => {
  renderWithAuth(<Retention />, { fetchImpl: makeFetch(routes()) })

  expect(await screen.findByText('keep_last 14')).toBeInTheDocument()
  // The second task keeps everything forever — that must be visible, not blank.
  expect(screen.getByText('не настроена')).toBeInTheDocument()
})

test('TestRetentionSweepDisabled: no cron reports manual-only operation', async () => {
  const fetchImpl = makeFetch(routes({
    'GET /api/retention': { cron: '', tasks: [] },
    'GET /api/retention/sweeps': [],
  }))
  renderWithAuth(<Retention />, { fetchImpl })

  expect(await screen.findByText('выключено')).toBeInTheDocument()
  expect(screen.getByText('запуск только вручную')).toBeInTheDocument()
  expect(screen.getByText('Очисток пока не было.')).toBeInTheDocument()
})

test('TestRetentionSweepNowPosts: the action calls the sweep endpoint and reloads', async () => {
  const user = userEvent.setup()
  const fetchImpl = makeFetch(routes({
    'POST /api/retention/sweep': { ...SUMMARY.last_sweep, id: 10, source: 'manual' },
  }))
  renderWithAuth(<Retention />, { fetchImpl })

  await user.click(await screen.findByRole('button', { name: /Очистить сейчас/ }))

  const post = fetchImpl.calls().find(([url, init]) => url === '/api/retention/sweep' && init?.method === 'POST')
  expect(post).toBeDefined()
  // The summary is re-read afterwards, so the page shows the new state.
  const reads = fetchImpl.calls().filter(([url]) => url === '/api/retention')
  expect(reads.length).toBeGreaterThan(1)
})

test('TestRetentionShowsFailedSweepError: a failed sweep surfaces its cause', async () => {
  const failed = { ...SUMMARY.last_sweep, status: 'failed', error: 'task "wiki": storage 7: no such storage' }
  const fetchImpl = makeFetch(routes({
    'GET /api/retention': { ...SUMMARY, last_sweep: failed },
    'GET /api/retention/sweeps': [failed],
  }))
  renderWithAuth(<Retention />, { fetchImpl })

  const alert = await screen.findByText('Последняя очистка завершилась ошибкой')
  expect(alert).toBeInTheDocument()
  const banner = alert.parentElement as HTMLElement
  expect(within(banner).getByText(/no such storage/)).toBeInTheDocument()
})

test('TestRetentionSweepConflictIsNotAnError: a 409 explains that a sweep is already running', async () => {
  const user = userEvent.setup()
  const fetchImpl = makeFetch(routes({
    'POST /api/retention/sweep': { status: 409, body: { error: 'a retention sweep is already running' } },
  }))
  renderWithAuth(<Retention />, { fetchImpl })

  await user.click(await screen.findByRole('button', { name: /Очистить сейчас/ }))

  // The raw server string would leak English at the operator; the page explains
  // it in its own terms instead.
  expect(await screen.findByText(/Очистка уже выполняется/)).toBeInTheDocument()
})
