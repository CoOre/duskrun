import { screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { beforeEach, expect, test } from 'vitest'
import { makeFetch, renderWithAuth } from '../test/utils'
import Secrets from './Secrets'

beforeEach(() => localStorage.clear())

const SECRETS = [
  { id: 1, name: 'db/pg-primary', type: 'db-password', key_id: 'env:v1', created_at: '2026-03-12T10:00:00Z' },
]
const CONNS = [
  { id: 1, name: 'pg-primary', engine: 'postgres', connector_type: 'direct', secret_ref: 'secret://db/pg-primary' },
]

function fetchImpl() {
  return makeFetch({
    'GET /api/secrets': SECRETS,
    'GET /api/connections': CONNS,
    'GET /api/storages': [],
    'POST /api/secrets': { id: 2 },
  })
}

test('TestSecretsListRenders: shows secret rows and usage', async () => {
  renderWithAuth(<Secrets />, { fetchImpl: fetchImpl() })

  expect(await screen.findByText('db/pg-primary')).toBeInTheDocument()
  // pg-primary connection references this secret → 1 usage.
  expect(screen.getByText('1 ссылка')).toBeInTheDocument()
})

test('TestSecretRowOpensEditor: clicking a row opens the editor with the name locked', async () => {
  const user = userEvent.setup()
  const f = makeFetch({
    'GET /api/secrets': SECRETS,
    'GET /api/connections': CONNS,
    'GET /api/storages': [],
    'POST /api/secrets': { id: 1 },
  })
  renderWithAuth(<Secrets />, { fetchImpl: f })

  await user.click(await screen.findByText('db/pg-primary'))
  expect(await screen.findByText('Редактировать секрет')).toBeInTheDocument()
  // Name is fixed (upsert is keyed by name); only the value is re-entered.
  expect(screen.getByLabelText('Имя')).toBeDisabled()

  await user.type(screen.getByLabelText('Новое значение'), 'rotated')
  await user.click(screen.getByRole('button', { name: 'Сохранить' }))

  const post = f.calls().find(([url, init]) => url === '/api/secrets' && init?.method === 'POST')
  const body = JSON.parse(post![1].body as string)
  expect(body).toMatchObject({ name: 'db/pg-primary', type: 'db-password', value: 'rotated' })
})

test('TestCreateSecretPostsPayload: submitting the modal posts the right JSON', async () => {
  const user = userEvent.setup()
  const f = fetchImpl()
  renderWithAuth(<Secrets />, { fetchImpl: f })

  await user.click(await screen.findByRole('button', { name: /Создать секрет/ }))
  await user.type(screen.getByLabelText('Имя'), 'smtp/ops')
  await user.click(screen.getByText('smtp-password'))
  await user.type(screen.getByLabelText('Значение'), 'hunter2')
  await user.click(screen.getByRole('button', { name: 'Создать' }))

  const post = f.calls().find(([url, init]) => url === '/api/secrets' && init?.method === 'POST')
  expect(post).toBeDefined()
  const body = JSON.parse(post![1].body as string)
  expect(body).toMatchObject({ name: 'smtp/ops', type: 'smtp-password', value: 'hunter2' })
})
