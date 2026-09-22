import { screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { beforeEach, expect, test } from 'vitest'
import { makeFetch, renderWithAuth } from '../test/utils'
import TaskWizard from './TaskWizard'

beforeEach(() => localStorage.clear())

const CONNS = [{ id: 3, name: 'pg-primary', engine: 'postgres', connector_type: 'direct', secret_ref: '' }]
const STORS = [{ id: 5, name: 'local-fs', type: 'localfs', secret_ref: '' }]

function baseRoutes(extra: Record<string, unknown> = {}) {
  // Both task pages read the configured notifier channels; an unmocked route
  // would 404 and sink the page's initial load.
  return makeFetch({ '/api/connections': CONNS, '/api/storages': STORS, '/api/notifiers': [], ...extra })
}

test('TestWizardValidatesRequired: cannot advance without name/connection', async () => {
  const user = userEvent.setup()
  renderWithAuth(<TaskWizard />, { fetchImpl: baseRoutes(), route: '/tasks/new' })

  await user.click(await screen.findByRole('button', { name: /Далее/ }))
  expect(screen.getByRole('alert')).toHaveTextContent(/имя задачи/i)
})

test('TestWizardSubmitsTaskPayload: walking the steps posts the task', async () => {
  const user = userEvent.setup()
  const fetchImpl = baseRoutes({ 'POST /api/tasks': { id: 99 } })
  renderWithAuth(<TaskWizard />, { fetchImpl, route: '/tasks/new' })

  // Step 0: name + connection + database.
  await user.type(await screen.findByLabelText('Имя задачи'), 'nightly-pg')
  await user.selectOptions(screen.getByLabelText('Соединение'), '3')
  await user.type(screen.getByLabelText('База данных'), 'orders')
  await user.click(screen.getByRole('button', { name: /Далее/ }))

  // Step 1: schedule (default cron is valid).
  await user.click(screen.getByRole('button', { name: /Далее/ }))

  // Step 2: storage + codec (zstd default).
  await user.selectOptions(screen.getByLabelText('Хранилище'), '5')
  await user.click(screen.getByRole('button', { name: /Далее/ }))

  // Step 3: retention/notifiers → review.
  await user.click(screen.getByRole('button', { name: /Далее/ }))

  // Step 4: submit.
  await user.click(screen.getByRole('button', { name: 'Создать задачу' }))

  const post = fetchImpl.calls().find(([url, init]) => url === '/api/tasks' && init?.method === 'POST')
  expect(post).toBeDefined()
  const body = JSON.parse(post![1].body as string)
  expect(body).toMatchObject({
    name: 'nightly-pg',
    connection_id: 3,
    storage_id: 5,
    cron: '0 2 * * *',
    codec_chain: ['zstd'],
    dumper_opts: { database: 'orders' },
  })
})

test('TestWizardDatabaseChips: loads database list and inserts selected name', async () => {
  const user = userEvent.setup()
  const fetchImpl = baseRoutes({ '/api/connections/3/databases': { databases: ['analytics', 'orders'] } })
  renderWithAuth(<TaskWizard />, { fetchImpl, route: '/tasks/new' })

  await user.selectOptions(await screen.findByLabelText('Соединение'), '3')
  await user.click(screen.getByRole('button', { name: 'Список БД' }))
  await user.click(await screen.findByRole('button', { name: 'orders' }))

  expect(screen.getByLabelText('База данных')).toHaveValue('orders')
})

test('TestTaskWizardListsConfiguredChannels: the notifier step offers channels from /notifiers', async () => {
  const user = userEvent.setup()
  const fetchImpl = baseRoutes({
    '/api/notifiers': [
      { id: 2, name: 'ops-telegram', type: 'telegram', config: {}, events: ['failure'], enabled: true, created_at: '2026-08-01T00:00:00Z', used_by: [] },
    ],
  })
  renderWithAuth(<TaskWizard />, { fetchImpl, route: '/tasks/new' })

  // Step 0 gates on the required fields before the wizard will advance.
  await user.type(await screen.findByLabelText('Имя задачи'), 'nightly-pg')
  await user.selectOptions(screen.getByLabelText('Соединение'), '3')
  await user.type(screen.getByLabelText('База данных'), 'orders')
  await user.click(screen.getByRole('button', { name: /Далее/ })) // → schedule
  await user.click(screen.getByRole('button', { name: /Далее/ })) // → storage
  await user.selectOptions(screen.getByLabelText('Хранилище'), '5') // gated
  await user.click(screen.getByRole('button', { name: /Далее/ })) // → retention/notifiers

  // The operator's own channel is offered; a plugin type nobody configured is
  // not — the backend rejects those names.
  expect(await screen.findByText('ops-telegram')).toBeInTheDocument()
  expect(screen.queryByText('webhook')).not.toBeInTheDocument()
})

test('TestWizardSubmitsGFS: choosing GFS posts it alone, with usable defaults', async () => {
  const user = userEvent.setup()
  const fetchImpl = baseRoutes({ 'POST /api/tasks': { id: 42 } })
  renderWithAuth(<TaskWizard />, { fetchImpl, route: '/tasks/new' })

  await user.type(await screen.findByLabelText('Имя задачи'), 'nightly-pg')
  await user.selectOptions(screen.getByLabelText('Соединение'), '3')
  await user.type(screen.getByLabelText('База данных'), 'orders')
  await user.click(screen.getByRole('button', { name: /Далее/ })) // → schedule
  await user.click(screen.getByRole('button', { name: /Далее/ })) // → storage
  await user.selectOptions(screen.getByLabelText('Хранилище'), '5')
  await user.click(screen.getByRole('button', { name: /Далее/ })) // → retention

  // keep_last is the default rule; picking GFS replaces it rather than adding.
  await user.click(screen.getByText('GFS'))
  expect(screen.queryByLabelText('Хранить последних (keep_last)')).toBeNull()
  await user.clear(screen.getByLabelText('недельных'))
  await user.type(screen.getByLabelText('недельных'), '6')
  await user.click(screen.getByRole('button', { name: /Далее/ })) // → review

  expect(screen.getAllByText('GFS 7/6/12').length).toBeGreaterThan(0)
  await user.click(screen.getByRole('button', { name: 'Создать задачу' }))

  const post = fetchImpl.calls().find(([url, init]) => url === '/api/tasks' && init?.method === 'POST')
  expect(JSON.parse(post![1].body as string).retention).toEqual({
    gfs: { daily: 7, weekly: 6, monthly: 12 },
  })
})

test('TestWizardNoRetention: «без очистки» posts an empty policy and says what that means', async () => {
  const user = userEvent.setup()
  const fetchImpl = baseRoutes({ 'POST /api/tasks': { id: 43 } })
  renderWithAuth(<TaskWizard />, { fetchImpl, route: '/tasks/new' })

  await user.type(await screen.findByLabelText('Имя задачи'), 'nightly-pg')
  await user.selectOptions(screen.getByLabelText('Соединение'), '3')
  await user.type(screen.getByLabelText('База данных'), 'orders')
  await user.click(screen.getByRole('button', { name: /Далее/ }))
  await user.click(screen.getByRole('button', { name: /Далее/ }))
  await user.selectOptions(screen.getByLabelText('Хранилище'), '5')
  await user.click(screen.getByRole('button', { name: /Далее/ }))

  await user.click(screen.getByText('без очистки'))
  expect(screen.getByText(/не будут удаляться/)).toBeInTheDocument()
  await user.click(screen.getByRole('button', { name: /Далее/ }))
  await user.click(screen.getByRole('button', { name: 'Создать задачу' }))

  const post = fetchImpl.calls().find(([url, init]) => url === '/api/tasks' && init?.method === 'POST')
  expect(JSON.parse(post![1].body as string).retention).toEqual({})
})

const REDIS_CONN = { id: 7, name: 'cache', engine: 'redis', connector_type: 'direct', secret_ref: '' }
const MONGO_CONN = { id: 8, name: 'mongo', engine: 'mongodb', connector_type: 'direct', secret_ref: '' }

test('TestWizardRedisNeedsNoDatabase: the field disappears and the task posts empty dumper_opts', async () => {
  const user = userEvent.setup()
  const fetchImpl = makeFetch({
    '/api/connections': [...CONNS, REDIS_CONN],
    '/api/storages': STORS,
    '/api/notifiers': [],
    'POST /api/tasks': { id: 11 },
  })
  renderWithAuth(<TaskWizard />, { fetchImpl, route: '/tasks/new' })

  await user.type(await screen.findByLabelText('Имя задачи'), 'cache-nightly')
  await user.selectOptions(screen.getByLabelText('Соединение'), '7')
  // An RDB snapshot covers the whole instance: naming a database would be a lie.
  expect(screen.queryByLabelText('База данных')).toBeNull()

  for (let i = 0; i < 4; i++) {
    if (i === 2) await user.selectOptions(screen.getByLabelText('Хранилище'), '5')
    await user.click(screen.getByRole('button', { name: /Далее/ }))
  }
  await user.click(screen.getByRole('button', { name: 'Создать задачу' }))

  const post = fetchImpl.calls().find(([url, init]) => url === '/api/tasks' && init?.method === 'POST')
  expect(post).toBeDefined()
  expect(JSON.parse(post![1].body as string).dumper_opts).toEqual({})
})

test('TestWizardMongoDatabaseOptional: an empty database is accepted and means every database', async () => {
  const user = userEvent.setup()
  const fetchImpl = makeFetch({
    '/api/connections': [...CONNS, MONGO_CONN],
    '/api/storages': STORS,
    '/api/notifiers': [],
    'POST /api/tasks': { id: 12 },
  })
  renderWithAuth(<TaskWizard />, { fetchImpl, route: '/tasks/new' })

  await user.type(await screen.findByLabelText('Имя задачи'), 'mongo-nightly')
  await user.selectOptions(screen.getByLabelText('Соединение'), '8')
  expect(screen.getByLabelText('База данных')).toBeInTheDocument()

  for (let i = 0; i < 4; i++) {
    if (i === 2) await user.selectOptions(screen.getByLabelText('Хранилище'), '5')
    await user.click(screen.getByRole('button', { name: /Далее/ }))
  }
  await user.click(screen.getByRole('button', { name: 'Создать задачу' }))

  const post = fetchImpl.calls().find(([url, init]) => url === '/api/tasks' && init?.method === 'POST')
  expect(post).toBeDefined()
  expect(JSON.parse(post![1].body as string).dumper_opts).toEqual({})
})
