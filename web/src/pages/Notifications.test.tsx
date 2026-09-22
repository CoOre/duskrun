import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { beforeEach, expect, test } from 'vitest'
import { makeFetch, renderWithAuth } from '../test/utils'
import Notifications, { parseRecipients, toggleEvent } from './Notifications'

beforeEach(() => localStorage.clear())

const CHANNELS = [
  { id: 1, name: 'log', type: 'log', config: {}, events: ['success', 'failure', 'retention_error', 'watchdog'], enabled: true, created_at: '2026-08-01T00:00:00Z', used_by: ['orders-db nightly'] },
  { id: 2, name: 'ops-telegram', type: 'telegram', config: { chat_id: '-100', token_ref: 'secret://tg/bot' }, events: ['failure'], enabled: false, created_at: '2026-08-02T00:00:00Z', used_by: [] },
]

const LOG = [
  { id: 9, kind: 'failure', task: 'analytics-mysql', run_id: 5230, channel: 'ops-telegram', status: 'failed', error: 'chat not found', created_at: '2026-08-12T01:30:00Z' },
  { id: 8, kind: 'success', task: 'orders-db nightly', run_id: 5231, channel: 'log', status: 'sent', created_at: '2026-08-12T02:04:00Z' },
]

const SECRETS = [
  { id: 4, name: 'smtp/ops', type: 'smtp-password', created_at: '2026-08-01T00:00:00Z', used_by: [] },
]

function routes(over: Record<string, unknown> = {}) {
  return {
    'GET /api/notifiers': CHANNELS,
    'GET /api/notifications': LOG,
    'GET /api/secrets': SECRETS,
    ...over,
  }
}

// openSmtpModal opens the create dialog and switches it to the Email channel.
async function openSmtpModal(user: ReturnType<typeof userEvent.setup>) {
  await user.click(await screen.findByRole('button', { name: /Канал/ }))
  await user.click(screen.getByText('Email'))
}

test('TestToggleEvent: subscribing and unsubscribing a kind', () => {
  expect(toggleEvent(['failure'], 'success')).toEqual(['failure', 'success'])
  expect(toggleEvent(['failure', 'success'], 'failure')).toEqual(['success'])
})

test('TestNotificationsRendersChannelsAndLog: cards, matrix and delivery log come from the API', async () => {
  renderWithAuth(<Notifications />, { fetchImpl: makeFetch(routes()) })

  expect(await screen.findByText('ops-telegram')).toBeInTheDocument()
  expect(screen.getByText('включён')).toBeInTheDocument()
  expect(screen.getByText('выключен')).toBeInTheDocument()
  // The delivery log shows both outcomes, including why one failed.
  expect(screen.getByText('chat not found')).toBeInTheDocument()
  expect(screen.getByText('→ ops-telegram')).toBeInTheDocument()
})

test('TestNotificationsMatrixPatchesSubscription: toggling a cell PATCHes that channel events', async () => {
  const user = userEvent.setup()
  const fetchImpl = makeFetch(routes({ 'PATCH /api/notifiers/2': { id: 2 } }))
  renderWithAuth(<Notifications />, { fetchImpl })

  // Each matrix cell names the event and the channel it switches.
  await user.click(await screen.findByRole('switch', { name: 'Успешный бэкап → ops-telegram' }))

  const patch = fetchImpl.calls().find(([url, init]) => url === '/api/notifiers/2' && init?.method === 'PATCH')
  expect(patch).toBeDefined()
  expect(JSON.parse(patch![1].body as string).events).toEqual(['failure', 'success'])
})

test('TestNotificationsMatrixTogglesCompose: a second click keeps what the first one subscribed', async () => {
  const user = userEvent.setup()
  // A server that applies PATCHes, with the first write held open so the second
  // click happens while it is still in flight — the case where reading the
  // toggles off the last completed fetch loses a subscription.
  const state = CHANNELS.map((c) => ({ ...c }))
  const patches: { events: string[] }[] = []
  let release = () => {}
  const held = new Promise<void>((r) => { release = r })

  const fetchImpl = (async (url: string, init: RequestInit = {}) => {
    const path = (url as string).split('?')[0]
    const json = (body: unknown) =>
      new Response(JSON.stringify(body), { status: 200, headers: { 'Content-Type': 'application/json' } })
    if ((init.method ?? 'GET').toUpperCase() === 'PATCH') {
      const body = JSON.parse(init.body as string)
      patches.push(body)
      if (patches.length === 1) await held
      state[1] = { ...state[1], ...body }
      return json({ id: 2 })
    }
    if (path === '/api/notifiers') return json(state)
    if (path === '/api/notifications') return json(LOG)
    return json(SECRETS)
  }) as unknown as typeof fetch

  renderWithAuth(<Notifications />, { fetchImpl })

  await user.click(await screen.findByRole('switch', { name: 'Успешный бэкап → ops-telegram' }))
  await user.click(screen.getByRole('switch', { name: 'Watchdog: нет свежего бэкапа → ops-telegram' }))
  release()

  await waitFor(() => expect(patches).toHaveLength(2))
  // The second write is computed from the row as it stands after the first, not
  // from the one the last fetch returned, so nothing is silently unsubscribed.
  expect(patches[1].events).toEqual(['failure', 'success', 'watchdog'])
  await waitFor(() => expect(state[1].events).toEqual(['failure', 'success', 'watchdog']))
})

test('TestNotificationsEditKeepsUnmodelledConfig: saving a channel does not drop keys the form has no field for', async () => {
  const user = userEvent.setup()
  const channels = [
    { ...CHANNELS[0] },
    { ...CHANNELS[1], config: { chat_id: '-100', token_ref: 'secret://tg/bot', token: '••••', parse_mode: 'HTML' } },
  ]
  const fetchImpl = makeFetch(routes({ 'GET /api/notifiers': channels, 'PATCH /api/notifiers/2': { id: 2 } }))
  renderWithAuth(<Notifications />, { fetchImpl })

  const card = (await screen.findByRole('switch', { name: 'Канал ops-telegram' })).closest('div')
    ?.parentElement as HTMLElement
  await user.click(within(card).getByRole('button', { name: 'Изменить' }))
  // Rename it and nothing else — the edit must not touch the rest of the config.
  await user.clear(screen.getByLabelText('Название'))
  await user.type(screen.getByLabelText('Название'), 'ops-tg')
  await user.click(screen.getByRole('button', { name: 'Сохранить' }))

  const patch = fetchImpl.calls().find(([url, init]) => url === '/api/notifiers/2' && init?.method === 'PATCH')
  const body = JSON.parse(patch![1].body as string)
  expect(body.name).toBe('ops-tg')
  // parse_mode survives; the masked token travels back for the server to restore.
  expect(body.config).toEqual({
    chat_id: '-100', token_ref: 'secret://tg/bot', token: '••••', parse_mode: 'HTML',
  })
})

test('TestNotificationsTestButtonReportsResult: a failed test shows the reason', async () => {
  const user = userEvent.setup()
  const fetchImpl = makeFetch(routes({
    'POST /api/notifiers/2/test': { status: 'failed', error: 'telegram: token is required' },
  }))
  renderWithAuth(<Notifications />, { fetchImpl })

  const card = (await screen.findByRole('switch', { name: 'Канал ops-telegram' })).closest('div')
    ?.parentElement as HTMLElement
  await user.click(within(card).getByRole('button', { name: 'Тест' }))

  expect(await screen.findByText(/telegram: token is required/)).toBeInTheDocument()
})

test('TestParseRecipients: commas, semicolons and newlines all separate addresses', () => {
  expect(parseRecipients('ops@corp.io, oncall@corp.io')).toEqual(['ops@corp.io', 'oncall@corp.io'])
  expect(parseRecipients(' a@b.io ;c@d.io\ne@f.io ')).toEqual(['a@b.io', 'c@d.io', 'e@f.io'])
  expect(parseRecipients('  ')).toEqual([])
})

test('TestNotificationsCreatesSmtpChannel: the modal posts the config the plugin expects', async () => {
  const user = userEvent.setup()
  const fetchImpl = makeFetch(routes({ 'POST /api/notifiers': { id: 3 } }))
  renderWithAuth(<Notifications />, { fetchImpl })

  await openSmtpModal(user)
  await user.type(screen.getByLabelText('Название'), 'ops-mail')
  await user.type(screen.getByLabelText('SMTP-хост'), 'smtp.corp.io')
  await user.type(screen.getByLabelText('Порт'), '2525')
  await user.selectOptions(screen.getByLabelText('Шифрование'), 'starttls')
  await user.type(screen.getByLabelText('От кого'), 'duskrun@corp.io')
  await user.type(screen.getByLabelText('Кому (через запятую)'), 'ops@corp.io, oncall@corp.io')
  await user.type(screen.getByLabelText(/Логин/), 'duskrun@corp.io')
  await user.selectOptions(await screen.findByLabelText(/Пароль SMTP/), 'secret://smtp/ops')
  await user.click(screen.getByRole('button', { name: 'Создать' }))

  const post = fetchImpl.calls().find(([url, init]) => url === '/api/notifiers' && init?.method === 'POST')
  expect(post).toBeDefined()
  const body = JSON.parse(post![1].body as string)
  expect(body.type).toBe('smtp')
  expect(body.config).toEqual({
    host: 'smtp.corp.io',
    port: 2525,
    tls: 'starttls',
    from: 'duskrun@corp.io',
    to: ['ops@corp.io', 'oncall@corp.io'],
    username: 'duskrun@corp.io',
    password_ref: 'secret://smtp/ops',
  })
})

test('TestNotificationsOmitsEmptySmtpPort: the plugin picks the default for the TLS mode', async () => {
  const user = userEvent.setup()
  const fetchImpl = makeFetch(routes({ 'POST /api/notifiers': { id: 3 } }))
  renderWithAuth(<Notifications />, { fetchImpl })

  await openSmtpModal(user)
  await user.type(screen.getByLabelText('Название'), 'ops-mail')
  await user.type(screen.getByLabelText('SMTP-хост'), 'smtp.corp.io')
  await user.type(screen.getByLabelText('От кого'), 'duskrun@corp.io')
  await user.type(screen.getByLabelText('Кому (через запятую)'), 'ops@corp.io')
  await user.click(screen.getByRole('button', { name: 'Создать' }))

  const post = fetchImpl.calls().find(([url, init]) => url === '/api/notifiers' && init?.method === 'POST')
  const body = JSON.parse(post![1].body as string)
  expect(body.config).not.toHaveProperty('port')
  expect(body.config).not.toHaveProperty('username')
})

test('TestNotificationsRejectsSmtpLoginWithoutSecret: a login with no password is a lost secret ref', async () => {
  const user = userEvent.setup()
  const fetchImpl = makeFetch(routes({ 'POST /api/notifiers': { id: 3 } }))
  renderWithAuth(<Notifications />, { fetchImpl })

  await openSmtpModal(user)
  await user.type(screen.getByLabelText('Название'), 'ops-mail')
  await user.type(screen.getByLabelText('SMTP-хост'), 'smtp.corp.io')
  await user.type(screen.getByLabelText('От кого'), 'duskrun@corp.io')
  await user.type(screen.getByLabelText('Кому (через запятую)'), 'ops@corp.io')
  await user.type(screen.getByLabelText(/Логин/), 'duskrun@corp.io')
  await user.click(screen.getByRole('button', { name: 'Создать' }))

  expect(await screen.findByRole('alert')).toHaveTextContent('Выберите секрет с паролем SMTP')
  expect(fetchImpl.calls().some(([url, init]) => url === '/api/notifiers' && init?.method === 'POST')).toBe(false)
})

test('TestNotificationsEmptyStateIsHonest: no channels means nothing is delivered', async () => {
  const fetchImpl = makeFetch(routes({ 'GET /api/notifiers': [], 'GET /api/notifications': [] }))
  renderWithAuth(<Notifications />, { fetchImpl })

  expect(await screen.findByText(/Каналов пока нет/)).toBeInTheDocument()
  expect(screen.getByText('Отправок пока не было.')).toBeInTheDocument()
})
