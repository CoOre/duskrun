import { screen } from '@testing-library/react'
import { beforeEach, expect, test } from 'vitest'
import { makeFetch, renderWithAuth } from '../test/utils'
import Tasks from './Tasks'

beforeEach(() => localStorage.clear())

const TASKS = [
  { id: 1, name: 'nightly-pg', connection_id: 1, storage_id: 1, cron: '0 2 * * *', codec_chain: ['zstd'], notifiers: [], enabled: true },
  { id: 2, name: 'weekly-my', connection_id: 2, storage_id: 1, cron: '0 3 * * 0', codec_chain: [], notifiers: [], enabled: false },
]
const RUNS = [{ id: 42, task_id: 1, status: 'success', attempt: 1, created_at: '2026-07-21T02:00:00Z' }]
const CONNS = [
  { id: 1, name: 'pg-primary', engine: 'postgres', connector_type: 'direct', secret_ref: '' },
  { id: 2, name: 'my-replica', engine: 'mysql', connector_type: 'ssh-tunnel', secret_ref: '' },
]
const STORAGES = [{ id: 1, name: 'local', type: 'localfs', secret_ref: '' }]

test('TestTasksListRenders: renders task rows and the create button', async () => {
  const fetchImpl = makeFetch({ '/api/tasks': TASKS, '/api/runs': RUNS, '/api/connections': CONNS, '/api/storages': STORAGES })
  renderWithAuth(<Tasks />, { fetchImpl })

  expect(await screen.findByText('nightly-pg')).toBeInTheDocument()
  expect(screen.getByText('weekly-my')).toBeInTheDocument()
  expect(screen.getByText('0 2 * * *')).toBeInTheDocument()
  expect(screen.getByText('на паузе')).toBeInTheDocument()
  expect(screen.getByRole('button', { name: /Создать задачу/ })).toBeInTheDocument()
})

// The permission that matters is the server's 403; this asserts the page does
// not dangle actions a viewer is guaranteed to be refused.
test('TestViewerSeesNoWriteActions: no create button, no run-now arrow', async () => {
  const fetchImpl = makeFetch({
    'GET /api/me': { static: false, role: 'viewer', email: 'v@corp.io', user_id: 9, session_id: 9 },
    '/api/tasks': TASKS, '/api/runs': RUNS, '/api/connections': CONNS, '/api/storages': STORAGES,
  })
  renderWithAuth(<Tasks />, { fetchImpl })

  expect(await screen.findByText('nightly-pg')).toBeInTheDocument()
  expect(screen.queryByRole('button', { name: /Создать задачу/ })).not.toBeInTheDocument()
  expect(screen.queryByTitle('Запустить сейчас')).not.toBeInTheDocument()
})
