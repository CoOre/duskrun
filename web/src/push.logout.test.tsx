import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, expect, test } from 'vitest'
import { useAuth } from './auth'
import { fakeSub, installFakePush } from './test/fakePush'
import { makeFetch, renderWithAuth } from './test/utils'

let env: ReturnType<typeof installFakePush> | null = null
beforeEach(() => localStorage.clear())
afterEach(() => {
  env?.restore()
  env = null
})

function LogoutButton() {
  const { logout } = useAuth()
  return <button onClick={logout}>Выйти</button>
}

// TestLogoutStopsPush: signing out means "not my browser" — alerts must stop
// arriving here. The unsubscribe needs the session, so it has to reach the
// server with the old token and before /logout ends that session.
test('TestLogoutStopsPush: unsubscribes this browser with the session, then logs out', async () => {
  const user = userEvent.setup()
  const sub = fakeSub('https://fcm.googleapis.com/fcm/send/mine')
  env = installFakePush({ existing: sub, permission: 'granted' })
  const fetchImpl = makeFetch({
    'POST /api/push/unsubscribe': { status: 204, body: null },
    'POST /api/logout': { status: 'ok' },
  })
  renderWithAuth(<LogoutButton />, { fetchImpl, token: 'sess-token' })

  await user.click(await screen.findByRole('button', { name: 'Выйти' }))
  expect(localStorage.getItem('duskrun.token')).toBeNull() // cleared at once

  await waitFor(() => expect(fetchImpl.calls().some(([url]) => url === '/api/logout')).toBe(true))
  const calls = fetchImpl.calls().map(([url, init]) => ({ url, auth: (init?.headers as Record<string, string>)?.Authorization }))
  const unsub = calls.findIndex((c) => c.url === '/api/push/unsubscribe')
  const out = calls.findIndex((c) => c.url === '/api/logout')
  expect(unsub).toBeGreaterThanOrEqual(0)
  expect(unsub).toBeLessThan(out)
  expect(calls[unsub].auth).toBe('Bearer sess-token')
  expect(calls[out].auth).toBe('Bearer sess-token')
  expect(sub.unsubscribe).toHaveBeenCalled()
})

test('TestLogoutWithoutPush: no subscription, plain logout', async () => {
  const user = userEvent.setup()
  const fetchImpl = makeFetch({ 'POST /api/logout': { status: 'ok' } })
  renderWithAuth(<LogoutButton />, { fetchImpl })
  await user.click(await screen.findByRole('button', { name: 'Выйти' }))
  await waitFor(() => expect(fetchImpl.calls().some(([url]) => url === '/api/logout')).toBe(true))
  expect(fetchImpl.calls().some(([url]) => url === '/api/push/unsubscribe')).toBe(false)
})
