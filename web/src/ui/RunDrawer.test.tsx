import { screen } from '@testing-library/react'
import { beforeEach, expect, test } from 'vitest'
import { makeFetch, renderWithAuth } from '../test/utils'
import { RunDrawer } from './RunDrawer'

beforeEach(() => localStorage.clear())

test('TestRunDrawerShowsLog: fetches the run and renders its log', async () => {
  const fetchImpl = makeFetch({
    '/api/runs/42': {
      id: 42,
      task_id: 1,
      status: 'success',
      attempt: 1,
      log: 'pg_dump ok\n1024 bytes written',
      created_at: '2026-07-21T02:00:00Z',
    },
    '/api/tasks': [{ id: 1, name: 'nightly-pg', connection_id: 1, storage_id: 1, cron: '0 2 * * *', codec_chain: ['zstd'], notifiers: [], enabled: true }],
    '/api/connections': [{ id: 1, name: 'pg-primary', engine: 'postgres', connector_type: 'direct', secret_ref: '' }],
  })
  renderWithAuth(<RunDrawer runId={42} onClose={() => {}} />, { fetchImpl })

  expect(await screen.findByText('nightly-pg')).toBeInTheDocument()
  expect(screen.getByText('ПАЙПЛАЙН')).toBeInTheDocument()
  expect(await screen.findByText(/pg_dump ok/)).toBeInTheDocument()
  expect(screen.getByText(/1024 bytes written/)).toBeInTheDocument()
  expect(screen.getByRole('button', { name: 'Команда restore' })).toBeInTheDocument()
})
