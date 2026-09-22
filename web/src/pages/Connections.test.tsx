import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { beforeEach, expect, test } from 'vitest'
import { makeFetch, renderWithAuth } from '../test/utils'
import Connections from './Connections'

beforeEach(() => localStorage.clear())

const CONNS = [
  { id: 1, name: 'pg-primary', engine: 'postgres', connector_type: 'direct', username: 'backup', secret_ref: 'secret://db/pg' },
  { id: 2, name: 'my-replica', engine: 'mysql', connector_type: 'ssh-tunnel', username: '', secret_ref: '' },
]

test('TestConnectionsListRenders: shows connection cards', async () => {
  const user = userEvent.setup()
  const fetchImpl = makeFetch({ '/api/connections': CONNS })
  renderWithAuth(<Connections />, { fetchImpl })

  expect(await screen.findByText('pg-primary')).toBeInTheDocument()
  expect(screen.getByText('my-replica')).toBeInTheDocument()
  expect(screen.getByText('backup')).toBeInTheDocument()
  expect(screen.getByRole('button', { name: /Соединение/ })).toBeInTheDocument()

  await user.click(screen.getByText('pg-primary'))
  expect(await screen.findByText('Редактировать соединение')).toBeInTheDocument()
  // The editor pre-fills the connection's login user.
  expect(screen.getByDisplayValue('backup')).toBeInTheDocument()
})

test('TestEngineSwitchUpdatesDefaultPort: postgres→mysql retargets the default port', async () => {
  const user = userEvent.setup()
  const fetchImpl = makeFetch({ 'GET /api/connections': [], 'POST /api/connections': { id: 9 } })
  renderWithAuth(<Connections />, { fetchImpl })

  await user.click(await screen.findByRole('button', { name: /Соединение/ }))
  // Default engine is postgres → port 5432.
  expect(screen.getByLabelText('Порт')).toHaveValue('5432')

  await user.click(screen.getByText('MySQL'))
  // The untouched default follows the engine.
  expect(screen.getByLabelText('Порт')).toHaveValue('3306')
})

test('TestEngineSwitchKeepsCustomPort: a hand-typed port survives an engine switch', async () => {
  const user = userEvent.setup()
  const fetchImpl = makeFetch({ 'GET /api/connections': [], 'POST /api/connections': { id: 9 } })
  renderWithAuth(<Connections />, { fetchImpl })

  await user.click(await screen.findByRole('button', { name: /Соединение/ }))
  const portInput = screen.getByLabelText('Порт')
  await user.clear(portInput)
  await user.type(portInput, '6543')
  await user.click(screen.getByText('MySQL'))
  expect(screen.getByLabelText('Порт')).toHaveValue('6543')
})

test('TestCreateConnectionPostsUsername: the modal posts the DB username', async () => {
  const user = userEvent.setup()
  const fetchImpl = makeFetch({
    'GET /api/connections': [],
    'POST /api/connections': { id: 5 },
  })
  renderWithAuth(<Connections />, { fetchImpl })

  await user.click(await screen.findByRole('button', { name: /Соединение/ }))
  await user.type(screen.getByLabelText('Название'), 'pg-primary')
  await user.type(screen.getByLabelText('Хост БД'), '10.0.0.1')
  await user.type(screen.getByLabelText('Пользователь БД'), 'backup')
  await user.click(screen.getByRole('button', { name: 'Создать' }))

  const post = fetchImpl.calls().find(([url, init]) => url === '/api/connections' && init?.method === 'POST')
  const body = JSON.parse(post![1].body as string)
  expect(body).toMatchObject({ name: 'pg-primary', username: 'backup' })
})

test('TestConnectionCheckSuccessShowsPanel: posts form payload and renders databases', async () => {
  const user = userEvent.setup()
  const fetchImpl = makeFetch({
    'GET /api/connections': [],
    'POST /api/connections/test': {
      status: 200,
      body: {
        status: 'ok',
        latency_ms: 42,
        checks: [
          { key: 'config', status: 'ok', label: 'Конфигурация' },
          { key: 'connector', status: 'ok', label: 'Туннель / endpoint' },
          { key: 'auth', status: 'ok', label: 'Авторизация в БД' },
          { key: 'catalog', status: 'ok', label: 'Список баз' },
        ],
        databases: ['analytics', 'orders'],
      },
    },
  })
  renderWithAuth(<Connections />, { fetchImpl })

  await user.click(await screen.findByRole('button', { name: /Соединение/ }))
  await user.type(screen.getByLabelText('Название'), 'pg-primary')
  await user.type(screen.getByLabelText('Хост БД'), '10.0.0.1')
  await user.type(screen.getByLabelText('Пользователь БД'), 'backup')
  await user.click(screen.getByRole('button', { name: 'Проверить соединение' }))

  expect(await screen.findByText('Соединение работает')).toBeInTheDocument()
  expect(screen.getByText('analytics, orders')).toBeInTheDocument()

  const post = fetchImpl.calls().find(([url, init]) => url === '/api/connections/test' && init?.method === 'POST')
  expect(post).toBeTruthy()
  expect(JSON.parse(post![1].body as string)).toMatchObject({
    name: 'pg-primary',
    engine: 'postgres',
    connector_type: 'direct',
    username: 'backup',
    connector_config: { host: '10.0.0.1', port: 5432 },
  })
})

test('TestConnectionCheckRedisSkipsStages: no DB user is demanded and untried stages read as skipped', async () => {
  // Redis has no user by default and no database list, so the old check refused
  // to even POST, and the catalog stage would have failed a healthy connection.
  const user = userEvent.setup()
  const fetchImpl = makeFetch({
    'GET /api/connections': [],
    'POST /api/connections/test': {
      status: 200,
      body: {
        status: 'ok',
        latency_ms: 12,
        checks: [
          { key: 'config', status: 'ok', label: 'Конфигурация' },
          { key: 'connector', status: 'ok', label: 'Туннель / endpoint' },
          { key: 'auth', status: 'skipped', label: 'Авторизация в БД' },
          { key: 'catalog', status: 'skipped', label: 'Список баз' },
        ],
      },
    },
  })
  renderWithAuth(<Connections />, { fetchImpl })

  await user.click(await screen.findByRole('button', { name: /Соединение/ }))
  await user.click(screen.getByText('Redis'))
  await user.type(screen.getByLabelText('Название'), 'cache')
  await user.type(screen.getByLabelText('Хост БД'), '10.0.0.9')
  await user.click(screen.getByRole('button', { name: 'Проверить соединение' }))

  expect(await screen.findByText('Соединение работает')).toBeInTheDocument()
  expect(screen.getAllByText('— не проверяется для этого движка')).toHaveLength(2)

  const post = fetchImpl.calls().find(([url, init]) => url === '/api/connections/test' && init?.method === 'POST')
  expect(post).toBeTruthy()
  expect(JSON.parse(post![1].body as string)).toMatchObject({ engine: 'redis', connector_config: { host: '10.0.0.9', port: 6379 } })
})

test('TestConnectionCheckRequiresUsername: empty DB user fails before calling API', async () => {
  const user = userEvent.setup()
  const fetchImpl = makeFetch({ 'GET /api/connections': [] })
  renderWithAuth(<Connections />, { fetchImpl })

  await user.click(await screen.findByRole('button', { name: /Соединение/ }))
  await user.type(screen.getByLabelText('Название'), 'pg-primary')
  await user.type(screen.getByLabelText('Хост БД'), '10.0.0.1')
  await user.click(screen.getByRole('button', { name: 'Проверить соединение' }))

  const alert = await screen.findByRole('alert')
  expect(alert).toHaveTextContent('Не указан пользователь БД')
  expect(fetchImpl.calls().some(([url, init]) => url === '/api/connections/test' && init?.method === 'POST')).toBe(false)
})

test('TestConnectionCheckFailedBodyShowsAlertPanel: HTTP 200 failed result does not fall into ApiError UI', async () => {
  const user = userEvent.setup()
  const fetchImpl = makeFetch({
    'GET /api/connections': [],
    'POST /api/connections/test': {
      status: 200,
      body: {
        status: 'failed',
        latency_ms: 120,
        checks: [
          { key: 'config', status: 'ok', label: 'Конфигурация' },
          { key: 'connector', status: 'ok', label: 'Туннель / endpoint' },
          { key: 'auth', status: 'failed', label: 'Авторизация в БД' },
        ],
        error: {
          code: 'connect_failed',
          message: 'Не удалось подключиться к базе данных',
          hint: 'Проверьте host, port, пользователя, секрет и параметры туннеля',
        },
      },
    },
  })
  renderWithAuth(<Connections />, { fetchImpl })

  await user.click(await screen.findByRole('button', { name: /Соединение/ }))
  await user.type(screen.getByLabelText('Название'), 'pg-primary')
  await user.type(screen.getByLabelText('Хост БД'), '10.0.0.1')
  await user.type(screen.getByLabelText('Пользователь БД'), 'backup')
  await user.click(screen.getByRole('button', { name: 'Проверить соединение' }))

  const alert = await screen.findByRole('alert')
  expect(alert).toHaveTextContent('Не удалось подключиться к базе данных')
  expect(alert).toHaveTextContent('Проверьте host, port, пользователя, секрет и параметры туннеля')
  expect(alert).toHaveTextContent('Авторизация в БД')
})

test('TestConnectionCheckResultResetsOnFieldChange: stale success disappears after editing', async () => {
  const user = userEvent.setup()
  const fetchImpl = makeFetch({
    'GET /api/connections': [],
    'POST /api/connections/test': {
      status: 200,
      body: {
        status: 'ok',
        latency_ms: 42,
        checks: [
          { key: 'config', status: 'ok', label: 'Конфигурация' },
          { key: 'connector', status: 'ok', label: 'Туннель / endpoint' },
          { key: 'auth', status: 'ok', label: 'Авторизация в БД' },
          { key: 'catalog', status: 'ok', label: 'Список баз' },
        ],
        databases: ['orders'],
      },
    },
  })
  renderWithAuth(<Connections />, { fetchImpl })

  await user.click(await screen.findByRole('button', { name: /Соединение/ }))
  await user.type(screen.getByLabelText('Название'), 'pg-primary')
  await user.type(screen.getByLabelText('Хост БД'), '10.0.0.1')
  await user.type(screen.getByLabelText('Пользователь БД'), 'backup')
  await user.click(screen.getByRole('button', { name: 'Проверить соединение' }))
  expect(await screen.findByText('Соединение работает')).toBeInTheDocument()

  await user.type(screen.getByLabelText('Хост БД'), '2')

  await waitFor(() => expect(screen.queryByText('Соединение работает')).not.toBeInTheDocument())
})

test('TestCreateDockerProxyConnectionPostsTargetConfig: docker-proxy uses compose network fields', async () => {
  const user = userEvent.setup()
  const fetchImpl = makeFetch({
    'GET /api/connections': [],
    'POST /api/connections': { id: 6 },
  })
  renderWithAuth(<Connections />, { fetchImpl })

  await user.click(await screen.findByRole('button', { name: /Соединение/ }))
  await user.click(screen.getByText('docker-proxy'))
  await user.type(screen.getByLabelText('Название'), 'pg-docker')
  await user.type(screen.getByLabelText('Docker network'), 'app_default')
  await user.type(screen.getByLabelText('Хост в Docker network'), 'postgres')
  await user.click(screen.getByRole('button', { name: 'Создать' }))

  const post = fetchImpl.calls().find(([url, init]) => url === '/api/connections' && init?.method === 'POST')
  const body = JSON.parse(post![1].body as string)
  expect(body).toMatchObject({
    name: 'pg-docker',
    connector_type: 'docker-proxy',
    connector_config: {
      network: 'app_default',
      target_host: 'postgres',
      target_port: 5432,
    },
  })
})

test('TestCreateDockerSSHProxyConnectionPostsSSHAndDockerConfig: remote docker host fields are preserved', async () => {
  const user = userEvent.setup()
  const fetchImpl = makeFetch({
    'GET /api/connections': [],
    'GET /api/secrets': [{ id: 3, name: 'ssh/docker', type: 'ssh-key', key_id: 'env:v1', created_at: '2026-01-01T00:00:00Z' }],
    'POST /api/connections': { id: 7 },
  })
  renderWithAuth(<Connections />, { fetchImpl })

  await user.click(await screen.findByRole('button', { name: /Соединение/ }))
  await user.click(screen.getByText('docker-ssh-proxy'))
  await user.type(screen.getByLabelText('Название'), 'pg-remote-docker')
  await user.type(screen.getByLabelText('Docker network'), 'app_default')
  await user.type(screen.getByLabelText('Хост в Docker network'), 'postgres')
  await user.type(screen.getByLabelText('SSH хост'), 'docker.example.com')
  await user.selectOptions(screen.getByLabelText('SSH-ключ (из секретов)'), 'secret://ssh/docker')
  await user.click(screen.getByRole('button', { name: 'Создать' }))

  const post = fetchImpl.calls().find(([url, init]) => url === '/api/connections' && init?.method === 'POST')
  const body = JSON.parse(post![1].body as string)
  expect(body).toMatchObject({
    name: 'pg-remote-docker',
    connector_type: 'docker-ssh-proxy',
    connector_config: {
      ssh_host: 'docker.example.com',
      ssh_user: 'backup',
      private_key_ref: 'secret://ssh/docker',
      network: 'app_default',
      target_host: 'postgres',
      target_port: 5432,
    },
  })
})

test('TestSecretPickerCreatesAndSelectsSecret: a new DB secret is selected after creation', async () => {
  const user = userEvent.setup()
  const fetchImpl = makeFetch({
    'GET /api/connections': [],
    'GET /api/secrets': [],
    'POST /api/secrets': { id: 11 },
  })
  renderWithAuth(<Connections />, { fetchImpl })

  await user.click(await screen.findByRole('button', { name: /Соединение/ }))
  await user.selectOptions(screen.getByLabelText('Пароль (секрет)'), '__new_secret__')
  const dialog = await screen.findByRole('dialog', { name: 'Новый секрет' })
  await user.type(within(dialog).getByLabelText('Имя'), 'db/pg-primary')
  await user.type(within(dialog).getByLabelText('Значение'), 'password')
  await user.click(within(dialog).getByRole('button', { name: 'Создать' }))

  await waitFor(() => expect(screen.queryByText('Новый секрет')).not.toBeInTheDocument())
  expect(screen.getByLabelText('Пароль (секрет)')).toHaveValue('secret://db/pg-primary')
})

test('TestDeleteConnectionConfirmsThenDeletes: two-step confirm sends DELETE and closes', async () => {
  const user = userEvent.setup()
  const fetchImpl = makeFetch({
    'GET /api/connections': CONNS,
    'DELETE /api/connections/1': { status: 204, body: null },
  })
  renderWithAuth(<Connections />, { fetchImpl })

  await user.click(await screen.findByText('pg-primary'))
  await screen.findByText('Редактировать соединение')

  // First click arms the confirm; the DELETE only fires on the second.
  await user.click(screen.getByRole('button', { name: 'Удалить' }))
  expect(fetchImpl.calls().some(([, i]) => i?.method === 'DELETE')).toBe(false)
  await user.click(screen.getByRole('button', { name: 'Да, удалить' }))

  const del = fetchImpl.calls().find(([url, init]) => url === '/api/connections/1' && init?.method === 'DELETE')
  expect(del).toBeTruthy()
  // The modal closes on success.
  await waitFor(() => expect(screen.queryByText('Редактировать соединение')).not.toBeInTheDocument())
})

test('TestDeleteConnectionInUseShowsError: a 409 surfaces the server message and keeps the modal open', async () => {
  const user = userEvent.setup()
  const fetchImpl = makeFetch({
    'GET /api/connections': CONNS,
    'DELETE /api/connections/1': { status: 409, body: { error: 'соединение используется в задаче' } },
  })
  renderWithAuth(<Connections />, { fetchImpl })

  await user.click(await screen.findByText('pg-primary'))
  await screen.findByText('Редактировать соединение')
  await user.click(screen.getByRole('button', { name: 'Удалить' }))
  await user.click(screen.getByRole('button', { name: 'Да, удалить' }))

  expect(await screen.findByText('соединение используется в задаче')).toBeInTheDocument()
  // Still editing — the delete was refused, not applied.
  expect(screen.getByText('Редактировать соединение')).toBeInTheDocument()
})
