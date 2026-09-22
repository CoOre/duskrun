import { screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { beforeEach, expect, test } from 'vitest'
import { makeFetch, renderWithAuth } from '../test/utils'
import History from './History'

beforeEach(() => localStorage.clear())

const TASKS = [{ id: 1, name: 'nightly-pg', connection_id: 1, storage_id: 1, cron: '0 2 * * *', codec_chain: [], notifiers: [], enabled: true }]
const RUNS = [
  { id: 42, task_id: 1, status: 'success', attempt: 1, created_at: '2026-07-21T02:00:00Z', artifact: { id: 7, run_id: 42, storage_id: 1, key: 'nightly-pg.zst', size: 1536, checksum: 'sha256', created_at: '2026-07-21T02:01:00Z' } },
  { id: 43, task_id: 1, status: 'failed', attempt: 2, created_at: '2026-07-21T03:00:00Z' },
]
const CONNS = [{ id: 1, name: 'pg-primary', engine: 'postgres', connector_type: 'direct', secret_ref: '' }]

test('TestHistoryFiltersByStatus: status chip narrows the rows', async () => {
  const user = userEvent.setup()
  const fetchImpl = makeFetch({ '/api/tasks': TASKS, '/api/runs': RUNS, '/api/connections': CONNS })
  renderWithAuth(<History />, { fetchImpl })

  expect(await screen.findByText('42')).toBeInTheDocument()
  expect(screen.getByText('43')).toBeInTheDocument()
  expect(screen.getByText('РАЗМЕР')).toBeInTheDocument()
  expect(screen.getByText('1.5 КБ')).toBeInTheDocument()

  await user.click(screen.getByRole('button', { name: 'ошибка' }))
  expect(screen.queryByText('42')).not.toBeInTheDocument()
  expect(screen.getByText('43')).toBeInTheDocument()
})
