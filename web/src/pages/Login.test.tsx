import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter } from 'react-router-dom'
import { beforeEach, expect, test, vi } from 'vitest'
import { AuthProvider } from '../auth'
import Login from './Login'

function renderLogin(fetchImpl: typeof fetch) {
  return render(
    <MemoryRouter>
      <AuthProvider fetchImpl={fetchImpl}>
        <Login />
      </AuthProvider>
    </MemoryRouter>,
  )
}

function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })
}

beforeEach(() => {
  localStorage.clear()
})

test('TestLoginStoresSessionToken: email + password stores the token the server returned', async () => {
  const user = userEvent.setup()
  const fetchImpl = vi.fn(async (input: RequestInfo | URL) => {
    const url = String(input)
    if (url.endsWith('/api/login')) {
      return json({
        token: 'session-token',
        expires_at: '2026-09-11T00:00:00Z',
        user: { id: 1, email: 'a@corp.io', name: '', role: 'admin', disabled: false, created_at: '' },
      })
    }
    return json({ static: false, role: 'admin', email: 'a@corp.io' })
  }) as unknown as typeof fetch

  renderLogin(fetchImpl)
  await user.type(screen.getByLabelText('Email'), 'a@corp.io')
  await user.type(screen.getByLabelText('Пароль'), 'correct horse battery staple')
  await user.click(screen.getByRole('button', { name: 'Войти' }))

  expect(localStorage.getItem('duskrun.token')).toBe('session-token')
})

test('TestLoginShowsErrorOn401: a rejected password shows the server-neutral message', async () => {
  const user = userEvent.setup()
  const fetchImpl = vi
    .fn()
    .mockResolvedValue(json({ error: 'invalid email or password' }, 401)) as unknown as typeof fetch

  renderLogin(fetchImpl)
  await user.type(screen.getByLabelText('Email'), 'a@corp.io')
  await user.type(screen.getByLabelText('Пароль'), 'wrong password here')
  await user.click(screen.getByRole('button', { name: 'Войти' }))

  // The wording must not distinguish a wrong password from an unknown address,
  // mirroring the server's refusal to confirm which accounts exist.
  expect(await screen.findByRole('alert')).toHaveTextContent(/неверный email или пароль/i)
  expect(localStorage.getItem('duskrun.token')).toBeNull()
})

test('TestLoginThrottleMessage: 429 explains the lockout instead of "wrong password"', async () => {
  const user = userEvent.setup()
  const fetchImpl = vi
    .fn()
    .mockResolvedValue(json({ error: 'too many attempts' }, 429)) as unknown as typeof fetch

  renderLogin(fetchImpl)
  await user.type(screen.getByLabelText('Email'), 'a@corp.io')
  await user.type(screen.getByLabelText('Пароль'), 'correct horse battery staple')
  await user.click(screen.getByRole('button', { name: 'Войти' }))

  expect(await screen.findByRole('alert')).toHaveTextContent(/слишком много попыток/i)
})

test('TestTokenTabStillWorks: the static API token remains a way in', async () => {
  const user = userEvent.setup()
  const fetchImpl = vi.fn(async (input: RequestInfo | URL) => {
    const url = String(input)
    if (url.endsWith('/api/ping')) return json({ status: 'ok' })
    return json({ static: true, role: 'admin' })
  }) as unknown as typeof fetch

  renderLogin(fetchImpl)
  await user.click(screen.getByRole('tab', { name: 'API-токен' }))
  await user.type(screen.getByLabelText('API-токен'), 'good-token')
  await user.click(screen.getByRole('button', { name: 'Войти' }))

  expect(localStorage.getItem('duskrun.token')).toBe('good-token')
})

test('TestTokenTabShowsErrorOn401: a bad token is not kept', async () => {
  const user = userEvent.setup()
  const fetchImpl = vi
    .fn()
    .mockResolvedValue(new Response('', { status: 401 })) as unknown as typeof fetch

  renderLogin(fetchImpl)
  await user.click(screen.getByRole('tab', { name: 'API-токен' }))
  await user.type(screen.getByLabelText('API-токен'), 'wrong')
  await user.click(screen.getByRole('button', { name: 'Войти' }))

  expect(await screen.findByRole('alert')).toHaveTextContent(/неверный токен/i)
  expect(localStorage.getItem('duskrun.token')).toBeNull()
})
