import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, expect, test } from 'vitest'
import { fakeSub, installFakePush } from '../test/fakePush'
import { makeFetch, renderWithAuth } from '../test/utils'
import { PushSettings } from './PushSettings'

const KEY = 'BAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8gISIjJCUmJygpKissLS4vMDEyMzQ1Njc4OTo7PD0-P0A'
const CHROME_MAC = 'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36'
const FIREFOX_WIN = 'Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:133.0) Gecko/20100101 Firefox/133.0'

const HERE = 'https://fcm.googleapis.com/fcm/send/here'
const DEVICES = [
  { id: 1, endpoint: HERE, service: 'fcm.googleapis.com', user_agent: CHROME_MAC, created_at: '2026-10-01T10:00:00Z', last_success_at: '2026-10-20T03:00:00Z', fail_count: 0 },
  { id: 2, endpoint: 'https://updates.push.services.mozilla.com/wpush/v2/x', service: 'updates.push.services.mozilla.com', user_agent: FIREFOX_WIN, created_at: '2026-10-02T10:00:00Z', last_error: 'push: service answered 403: {"reason":"BadJwtToken"}', fail_count: 3 },
]

let env: ReturnType<typeof installFakePush> | null = null
beforeEach(() => localStorage.clear())
afterEach(() => {
  env?.restore()
  env = null
})

test('TestPushSettingsUnavailable: the instance reason is explained and enabling is off', async () => {
  env = installFakePush()
  renderWithAuth(<PushSettings />, {
    fetchImpl: makeFetch({ 'GET /api/push': { available: false, reason: 'no_public_url' }, 'GET /api/push/subscriptions': [] }),
  })
  expect(await screen.findByText(/DUSKRUN_PUBLIC_URL/)).toBeInTheDocument()
  expect(screen.getByRole('button', { name: 'Включить в этом браузере' })).toBeDisabled()
})

test('TestPushSettingsInsecurePage: a browser off HTTPS is told why', async () => {
  renderWithAuth(<PushSettings />, {
    fetchImpl: makeFetch({ 'GET /api/push': { available: true, public_key: KEY }, 'GET /api/push/subscriptions': [] }),
  })
  expect(await screen.findByText(/не по HTTPS/)).toBeInTheDocument()
  expect(screen.getByRole('button', { name: 'Включить в этом браузере' })).toBeDisabled()
})

test('TestPushSettingsEnable: one click asks permission and registers this browser', async () => {
  const user = userEvent.setup()
  env = installFakePush()
  const fetchImpl = makeFetch({
    'GET /api/push': { available: true, public_key: KEY },
    'GET /api/push/subscriptions': [],
    'POST /api/push/subscriptions': { id: 5 },
  })
  renderWithAuth(<PushSettings />, { fetchImpl })

  await user.click(await screen.findByRole('button', { name: 'Включить в этом браузере' }))
  expect(await screen.findByText('Уведомления в этом браузере включены')).toBeInTheDocument()
  expect(env.requestPermission).toHaveBeenCalled()
  expect(fetchImpl.calls().some(([url, init]) => url === '/api/push/subscriptions' && init?.method === 'POST')).toBe(true)
})

test('TestPushSettingsDenied: a blocked site says where to unblock it', async () => {
  env = installFakePush({ permission: 'denied' })
  renderWithAuth(<PushSettings />, {
    fetchImpl: makeFetch({ 'GET /api/push': { available: true, public_key: KEY }, 'GET /api/push/subscriptions': [] }),
  })
  expect(await screen.findByText(/заблокированы в браузере/)).toBeInTheDocument()
  expect(screen.getByRole('button', { name: 'Включить в этом браузере' })).toBeDisabled()
})

test('TestPushSettingsDevices: lists browsers, marks this one, shows delivery errors', async () => {
  env = installFakePush({ existing: fakeSub(HERE), permission: 'granted' })
  renderWithAuth(<PushSettings />, {
    fetchImpl: makeFetch({ 'GET /api/push': { available: true, public_key: KEY }, 'GET /api/push/subscriptions': DEVICES }),
  })
  expect(await screen.findByText('(этот браузер)')).toBeInTheDocument()
  expect(screen.getByText(/Chrome · macOS/)).toBeInTheDocument()
  expect(screen.getByText('Firefox · Windows')).toBeInTheDocument()
  expect(screen.getByText(/ошибка ×3: .*BadJwtToken/)).toBeInTheDocument()
  // This browser is on: the header offers turning it off and a test.
  expect(screen.getByText('включено')).toBeInTheDocument()
  expect(screen.getByRole('button', { name: 'Выключить' })).toBeInTheDocument()
})

test('TestPushSettingsDeleteOther: removing another browser deletes it by id', async () => {
  const user = userEvent.setup()
  env = installFakePush({ existing: fakeSub(HERE), permission: 'granted' })
  const fetchImpl = makeFetch({
    'GET /api/push': { available: true, public_key: KEY },
    'GET /api/push/subscriptions': DEVICES,
    'DELETE /api/push/subscriptions/2': { status: 204, body: null },
  })
  renderWithAuth(<PushSettings />, { fetchImpl })

  await user.click(await screen.findByRole('button', { name: 'Удалить Firefox · Windows' }))
  await waitFor(() =>
    expect(fetchImpl.calls().some(([url, init]) => url === '/api/push/subscriptions/2' && init?.method === 'DELETE')).toBe(true),
  )
  expect(env.current()).not.toBeNull() // this browser's own subscription is untouched
})

test('TestPushSettingsDeleteThis: removing this browser also unsubscribes it locally', async () => {
  const user = userEvent.setup()
  const sub = fakeSub(HERE)
  env = installFakePush({ existing: sub, permission: 'granted' })
  const fetchImpl = makeFetch({
    'GET /api/push': { available: true, public_key: KEY },
    'GET /api/push/subscriptions': DEVICES,
    'POST /api/push/unsubscribe': { status: 204, body: null },
  })
  renderWithAuth(<PushSettings />, { fetchImpl })

  await user.click(await screen.findByRole('button', { name: /^Удалить Chrome · macOS/ }))
  await waitFor(() => expect(sub.unsubscribe).toHaveBeenCalled())
  expect(fetchImpl.calls().some(([url]) => url === '/api/push/unsubscribe')).toBe(true)
})

test('TestPushSettingsTestGone: a test to an expired browser reports it', async () => {
  const user = userEvent.setup()
  env = installFakePush({ existing: fakeSub(HERE), permission: 'granted' })
  renderWithAuth(<PushSettings />, {
    fetchImpl: makeFetch({
      'GET /api/push': { available: true, public_key: KEY },
      'GET /api/push/subscriptions': DEVICES,
      'POST /api/push/subscriptions/2/test': { status: 410, body: { error: 'push: the browser is no longer subscribed' } },
    }),
  })
  await user.click(await screen.findByRole('button', { name: 'Тест на Firefox · Windows' }))
  expect(await screen.findByRole('alert')).toHaveTextContent('больше не подписан')
})
