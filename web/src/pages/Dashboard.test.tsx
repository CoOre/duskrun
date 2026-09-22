import { screen } from '@testing-library/react'
import { beforeEach, expect, test } from 'vitest'
import { makeFetch, renderWithAuth } from '../test/utils'
import Dashboard, { fmtAge } from './Dashboard'

beforeEach(() => localStorage.clear())

const inHours = (h: number) => new Date(Date.now() + h * 3_600_000).toISOString()

const TASKS = [
  { id: 1, name: 'nightly-pg', connection_id: 1, storage_id: 1, cron: '0 2 * * *', codec_chain: ['zstd'], notifiers: [], enabled: true, next_run: inHours(14) },
  { id: 2, name: 'weekly-my', connection_id: 2, storage_id: 1, cron: '0 3 * * 0', codec_chain: [], notifiers: [], enabled: false },
]
const RUNS = [
  { id: 42, task_id: 1, status: 'success', attempt: 1, created_at: '2026-07-21T02:00:00Z', started_at: '2026-07-21T02:00:00Z', finished_at: '2026-07-21T02:04:00Z' },
  { id: 41, task_id: 1, status: 'failed', attempt: 2, created_at: '2026-07-20T02:00:00Z', started_at: '2026-07-20T02:00:00Z', finished_at: '2026-07-20T02:00:30Z' },
]
const CONNS = [{ id: 1, name: 'pg-primary', engine: 'postgres', connector_type: 'direct', secret_ref: '' }]
const STORAGES = [{ id: 1, name: 'local', type: 'localfs', secret_ref: '' }]

test('TestDashboardRendersRealData: KPIs, activity and recent runs from the API', async () => {
  const fetchImpl = makeFetch({ '/api/tasks': TASKS, '/api/runs': RUNS, '/api/connections': CONNS, '/api/storages': STORAGES })
  renderWithAuth(<Dashboard />, { fetchImpl })

  expect(await screen.findByText('АКТИВНЫХ ЗАДАЧ')).toBeInTheDocument()
  expect(screen.getByText('Активность запусков')).toBeInTheDocument()
  expect(screen.getByText('Наблюдение (watchdog)')).toBeInTheDocument()
  expect(screen.getByText('Ближайшие запуски')).toBeInTheDocument()
  expect(screen.getByText('РАЗМЕР')).toBeInTheDocument()
  // Recent-runs rows resolve the task name via the join.
  expect(screen.getAllByText('nightly-pg').length).toBeGreaterThan(0)
  // Success + failure status pills (the activity legend also uses these words).
  expect(screen.getAllByText('успешно').length).toBeGreaterThan(0)
  expect(screen.getAllByText('ошибка').length).toBeGreaterThan(0)
})

test('TestDashboardNextRunFromBackend: countdown comes from next_run, not a hardcoded guess', async () => {
  const fetchImpl = makeFetch({ '/api/tasks': TASKS, '/api/runs': RUNS, '/api/connections': CONNS, '/api/storages': STORAGES })
  renderWithAuth(<Dashboard />, { fetchImpl })

  expect(await screen.findByText('СЛЕД. ЗАПУСК')).toBeInTheDocument()
  // 14 h away, in hours — the old stub rendered "13 мин" for every such cron.
  expect(screen.getAllByText('14').length).toBeGreaterThan(0)
  expect(screen.getAllByText('ч').length).toBeGreaterThan(0)
  expect(screen.queryByText('мин')).not.toBeInTheDocument()
  // Named next task is the scheduled one.
  expect(screen.getAllByText('nightly-pg').length).toBeGreaterThan(0)
})

test('TestDashboardIgnoresPausedTasks: a paused task is never announced as upcoming', async () => {
  // The only enabled task is paused-out; nothing is left to schedule.
  const paused = TASKS.map((t) => ({ ...t, enabled: false, next_run: undefined }))
  const fetchImpl = makeFetch({ '/api/tasks': paused, '/api/runs': RUNS, '/api/connections': CONNS, '/api/storages': STORAGES })
  renderWithAuth(<Dashboard />, { fetchImpl })

  expect(await screen.findByText('нет запланированных задач')).toBeInTheDocument()
  expect(screen.getByText('Нет запланированных задач.')).toBeInTheDocument()
  // Neither paused task name shows up in the schedule panels.
  expect(screen.queryByText('weekly-my')).not.toBeInTheDocument()
})

test('TestDashboardAlertsComeFromServer: the watchdog panel renders /api/watchdog, not a local guess', async () => {
  const fetchImpl = makeFetch({
    '/api/tasks': TASKS, '/api/runs': RUNS, '/api/connections': CONNS, '/api/storages': STORAGES,
    '/api/watchdog': [
      { task_id: 2, task: 'weekly-my', threshold_sec: 1209600, since_sec: 1900800, reason: 'no successful backup for 528h (threshold 336h)', last_success: '2026-07-01T03:00:00Z' },
      { task_id: 1, task: 'nightly-pg', threshold_sec: 172800, since_sec: 259200, reason: 'no successful backup yet (threshold 48h)' },
    ],
  })
  renderWithAuth(<Dashboard />, { fetchImpl })

  expect(await screen.findByText('2 алерта')).toBeInTheDocument()
  // Gaps beyond two days read as days, and the reason is stated in Russian
  // rather than echoing the server's wire text.
  expect(screen.getByText('22 д')).toBeInTheDocument()
  expect(screen.getByText('превышен порог watchdog (14 д)')).toBeInTheDocument()
  expect(screen.getByText('успешных бэкапов ещё не было')).toBeInTheDocument()
})

test('TestDashboardSurvivesWatchdogOutage: an unavailable watchdog is reported, not read as all-clear', async () => {
  const fetchImpl = makeFetch({
    '/api/tasks': TASKS, '/api/runs': RUNS, '/api/connections': CONNS, '/api/storages': STORAGES,
    '/api/watchdog': { status: 503, body: { error: 'watchdog not configured' } },
  })
  renderWithAuth(<Dashboard />, { fetchImpl })

  // The rest of the page still works...
  expect(await screen.findByText('АКТИВНЫХ ЗАДАЧ')).toBeInTheDocument()
  // ...but "we could not check" must not look like "nothing is wrong".
  expect(screen.getByText('Не удалось проверить свежесть бэкапов')).toBeInTheDocument()
  expect(screen.getByText('watchdog not configured')).toBeInTheDocument()
  expect(screen.queryByText('Нет активных алертов.')).not.toBeInTheDocument()
})

test('TestFmtAge: sub-hour gaps read as minutes, not a rounded-up hour', () => {
  // watchdog_sec accepts any non-negative value, so a 5-minute threshold is
  // real and must not render as "1 ч".
  expect(fmtAge(300)).toBe('5 мин')
  expect(fmtAge(45)).toBe('45 с')
  expect(fmtAge(3600)).toBe('1 ч')
  expect(fmtAge(31 * 3600)).toBe('31 ч')
  expect(fmtAge(48 * 3600)).toBe('2 д')
})
