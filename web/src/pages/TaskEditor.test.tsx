import { screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { Route, Routes } from 'react-router-dom'
import { beforeEach, expect, test } from 'vitest'
import { makeFetch, renderWithAuth } from '../test/utils'
import TaskEditor, { watchdogOptions } from './TaskEditor'

beforeEach(() => localStorage.clear())

const TASKS = [{ id: 1, name: 'nightly-pg', connection_id: 1, storage_id: 1, cron: '0 2 * * *', dumper_opts: { database: 'orders' }, codec_chain: ['zstd'], notifiers: ['log'], enabled: true, retries: 2, timeout_sec: 1800 }]
const CONNS = [{ id: 1, name: 'pg-primary', engine: 'postgres', connector_type: 'direct', connector_config: { host: '10.0.4.12', port: 5432 }, secret_ref: '' }]
const STORAGES = [{ id: 1, name: 'local', type: 'localfs', config: { root: '/backups' }, secret_ref: '' }]

test('TestTaskEditorRendersWizardPage: edit task is a page with comp-like steps and disabled missing options', async () => {
  const user = userEvent.setup()
  const fetchImpl = makeFetch({ '/api/tasks': TASKS, '/api/connections': CONNS, '/api/storages': STORAGES, '/api/notifiers': [] })
  renderWithAuth(<Routes><Route path="/tasks/:id/edit" element={<TaskEditor />} /></Routes>, { fetchImpl, route: '/tasks/1/edit' })

  expect(await screen.findByText('Тип БД и дамп')).toBeInTheDocument()
  expect(screen.getByText('＋ Плагин')).toBeInTheDocument()
  // The database moved out of the raw JSON box into its own field.
  expect(screen.getByLabelText('База данных')).toHaveValue('orders')

  await user.click(screen.getByText('Хранилище'))
  expect(screen.getByText('sftp://offsite-nas')).toBeInTheDocument()

  await user.click(screen.getByText('Хранение'))
  expect(screen.getByText('GFS (grandfather-father-son)')).toBeInTheDocument()
})

test('TestTaskEditorSavesWatchdogThreshold: the watchdog select round-trips into the PATCH body', async () => {
  const user = userEvent.setup()
  const withWatchdog = [{ ...TASKS[0], watchdog_sec: 21600 }]
  const fetchImpl = makeFetch({
    '/api/tasks': withWatchdog, '/api/connections': CONNS, '/api/storages': STORAGES, '/api/notifiers': [],
    'PATCH /api/tasks/1': { id: 1 },
  })
  renderWithAuth(<Routes><Route path="/tasks/:id/edit" element={<TaskEditor />} /></Routes>, { fetchImpl, route: '/tasks/1/edit' })

  await user.click(await screen.findByText('Расписание'))
  const select = screen.getByLabelText('Watchdog: порог без успешного бэкапа')
  // The stored threshold is preselected, not silently reset to auto.
  expect(select).toHaveValue('21600')

  await user.selectOptions(select, '-1')

  // Saving lives on the last step.
  await user.click(screen.getByText('Обзор'))
  await user.click(screen.getByRole('button', { name: 'Сохранить изменения' }))

  const patch = fetchImpl.calls().find(([url, init]) => url === '/api/tasks/1' && init?.method === 'PATCH')
  expect(patch).toBeDefined()
  expect(JSON.parse(patch![1].body as string)).toMatchObject({ watchdog_sec: -1 })
})

test('TestWatchdogOptionsKeepCustomValue: a threshold outside the presets stays selectable', () => {
  // Set via the API (or by a preset list that changed later): without this the
  // select renders blank while still re-submitting the value on save.
  const opts = watchdogOptions(999)
  expect(opts.some(([, value]) => value === 999)).toBe(true)
  // A preset value adds nothing.
  expect(watchdogOptions(21600)).toHaveLength(opts.length - 1)
})

test('TestTaskEditorSwitchesToGFS: turning GFS on turns keep_last off and sends the defaults', async () => {
  const user = userEvent.setup()
  const fetchImpl = makeFetch({
    '/api/tasks': TASKS, '/api/connections': CONNS, '/api/storages': STORAGES, '/api/notifiers': [],
    'PATCH /api/tasks/1': { id: 1 },
  })
  renderWithAuth(<Routes><Route path="/tasks/:id/edit" element={<TaskEditor />} /></Routes>, { fetchImpl, route: '/tasks/1/edit' })

  await user.click(await screen.findByText('Хранение'))
  // The switched-off rule shows usable defaults, so one click arms a real
  // policy instead of a block of zeros that keeps nothing.
  expect(screen.getByLabelText('дн')).toHaveValue('7')
  expect(screen.getByLabelText('дн')).toBeDisabled()

  await user.click(screen.getByRole('switch', { name: 'Правило: GFS (grandfather-father-son)' }))
  expect(screen.getByLabelText('дн')).toBeEnabled()
  // The rules are exclusive here: arming one disarms the other.
  expect(screen.getByLabelText('keep_last')).toBeDisabled()

  await user.click(screen.getByText('Обзор'))
  await user.click(screen.getByRole('button', { name: 'Сохранить изменения' }))

  const patch = fetchImpl.calls().find(([url, init]) => url === '/api/tasks/1' && init?.method === 'PATCH')
  expect(JSON.parse(patch![1].body as string).retention).toEqual({
    gfs: { daily: 7, weekly: 4, monthly: 12 },
  })
})

test('TestTaskEditorOpensOnStoredGFS: a GFS task edits as GFS, not as keep_last', async () => {
  const user = userEvent.setup()
  // The editor used to submit keep_last only, so opening and saving a task with
  // a GFS policy wiped it.
  const withGfs = [{ ...TASKS[0], retention: { gfs: { daily: 3, weekly: 4, monthly: 12 } } }]
  const fetchImpl = makeFetch({
    '/api/tasks': withGfs, '/api/connections': CONNS, '/api/storages': STORAGES, '/api/notifiers': [],
    'PATCH /api/tasks/1': { id: 1 },
  })
  renderWithAuth(<Routes><Route path="/tasks/:id/edit" element={<TaskEditor />} /></Routes>, { fetchImpl, route: '/tasks/1/edit' })

  await user.click(await screen.findByText('Хранение'))
  expect(screen.getByLabelText('дн')).toHaveValue('3')
  expect(screen.getByRole('switch', { name: 'Правило: GFS (grandfather-father-son)' })).toHaveAttribute('aria-checked', 'true')

  await user.click(screen.getByText('Обзор'))
  expect(screen.getAllByText('GFS 3/4/12').length).toBeGreaterThan(0)
  await user.click(screen.getByRole('button', { name: 'Сохранить изменения' }))

  const patch = fetchImpl.calls().find(([url, init]) => url === '/api/tasks/1' && init?.method === 'PATCH')
  expect(JSON.parse(patch![1].body as string).retention).toEqual({ gfs: { daily: 3, weekly: 4, monthly: 12 } })
})

test('TestTaskEditorAnnouncesUnionedPolicy: a policy only the API can set says what a save will drop', async () => {
  const user = userEvent.setup()
  const both = [{ ...TASKS[0], retention: { keep_last: 5, gfs: { daily: 7, weekly: 4, monthly: 12 } } }]
  const fetchImpl = makeFetch({
    '/api/tasks': both, '/api/connections': CONNS, '/api/storages': STORAGES, '/api/notifiers': [],
  })
  renderWithAuth(<Routes><Route path="/tasks/:id/edit" element={<TaskEditor />} /></Routes>, { fetchImpl, route: '/tasks/1/edit' })

  await user.click(await screen.findByText('Хранение'))
  // Silently dropping keep_last on save is exactly the failure the editor used
  // to have with GFS; the form has to say it first.
  expect(screen.getByText(/заданы оба правила/)).toBeInTheDocument()
})

test('TestTaskEditorWarnsOnNoPolicy: switching the active rule off means nothing is ever pruned', async () => {
  const user = userEvent.setup()
  const kept = [{ ...TASKS[0], retention: { keep_last: 5 } }]
  const fetchImpl = makeFetch({
    '/api/tasks': kept, '/api/connections': CONNS, '/api/storages': STORAGES, '/api/notifiers': [],
  })
  renderWithAuth(<Routes><Route path="/tasks/:id/edit" element={<TaskEditor />} /></Routes>, { fetchImpl, route: '/tasks/1/edit' })

  await user.click(await screen.findByText('Хранение'))
  expect(screen.queryByText(/не будут удаляться/)).not.toBeInTheDocument()
  // Switching the only armed rule off is allowed — it just cannot be quiet.
  await user.click(screen.getByRole('switch', { name: 'Правило: keep_last' }))
  expect(screen.getByText(/не будут удаляться/)).toBeInTheDocument()
})

const CHANNELS = [
  { id: 1, name: 'log', type: 'log', config: {}, events: ['success'], enabled: true, created_at: '2026-08-01T00:00:00Z', used_by: [] },
  { id: 2, name: 'ops-telegram', type: 'telegram', config: {}, events: ['failure'], enabled: true, created_at: '2026-08-01T00:00:00Z', used_by: [] },
]

test('TestTaskEditorListsConfiguredChannels: notifier options come from /notifiers, not a hardcoded list', async () => {
  const user = userEvent.setup()
  const fetchImpl = makeFetch({
    '/api/tasks': TASKS, '/api/connections': CONNS, '/api/storages': STORAGES, '/api/notifiers': CHANNELS,
  })
  renderWithAuth(<Routes><Route path="/tasks/:id/edit" element={<TaskEditor />} /></Routes>, { fetchImpl, route: '/tasks/1/edit' })

  await user.click(await screen.findByText('Хранение'))
  // The operator's own channel is offered; plugin type names are not.
  expect(screen.getByText('ops-telegram')).toBeInTheDocument()
  expect(screen.queryByText('Email (SMTP)')).not.toBeInTheDocument()
})

test('TestTaskEditorSurfacesStaleChannel: a name that no longer exists is shown, not silently kept', async () => {
  const user = userEvent.setup()
  // A task from before channels were configured rows still names a plugin type.
  const stale = [{ ...TASKS[0], notifiers: ['telegram'] }]
  const fetchImpl = makeFetch({
    '/api/tasks': stale, '/api/connections': CONNS, '/api/storages': STORAGES, '/api/notifiers': CHANNELS,
  })
  renderWithAuth(<Routes><Route path="/tasks/:id/edit" element={<TaskEditor />} /></Routes>, { fetchImpl, route: '/tasks/1/edit' })

  await user.click(await screen.findByText('Хранение'))
  // The backend rejects it on save, so the editor has to say so rather than
  // letting the operator hit an opaque 400.
  expect(screen.getByText(/канала больше нет/)).toBeInTheDocument()
})

test('TestTaskEditorArtifactPreviewFollowsEngine: mongodb previews .archive, not .dump', async () => {
  // The wizard was fixed for this; the editor kept a hardcoded .dump and showed
  // a name the backend never writes.
  const user = userEvent.setup()
  const mongoConn = [{ id: 1, name: 'mongo-1', engine: 'mongodb', connector_type: 'direct', connector_config: { host: '10.0.4.12', port: 27017 }, secret_ref: '' }]
  const fetchImpl = makeFetch({ '/api/tasks': TASKS, '/api/connections': mongoConn, '/api/storages': STORAGES, '/api/notifiers': [] })
  renderWithAuth(<Routes><Route path="/tasks/:id/edit" element={<TaskEditor />} /></Routes>, { fetchImpl, route: '/tasks/1/edit' })

  await user.click(await screen.findByText('Хранилище'))
  expect(screen.getByText(/nightly-pg\.archive\.zst$/)).toBeInTheDocument()
})
