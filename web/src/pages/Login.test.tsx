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

// --- second factor -------------------------------------------------------

const SESSION = {
  token: 'mfa-session',
  expires_at: '2026-09-11T00:00:00Z',
  user: { id: 1, email: 'a@corp.io', name: '', role: 'admin', disabled: false, created_at: '' },
}

const CHALLENGE = { mfa_required: true, mfa_token: 'chal-1', mfa_methods: ['totp', 'recovery'] }

// mfaFetch answers /login with a challenge and routes /login/mfa to `second`.
function mfaFetch(second: (body: any) => Response, challenge: unknown = CHALLENGE) {
  return vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input)
    if (url.endsWith('/api/login')) return json(challenge)
    if (url.endsWith('/api/login/mfa/webauthn/options')) {
      return json({ publicKey: { challenge: 'AQID', allowCredentials: [{ type: 'public-key', id: 'Bw' }] } })
    }
    if (url.endsWith('/api/login/mfa')) return second(JSON.parse(String(init?.body)))
    return json({ static: false, role: 'admin', email: 'a@corp.io' })
  })
}

async function passPassword(user: ReturnType<typeof userEvent.setup>) {
  await user.type(screen.getByLabelText('Email'), 'a@corp.io')
  await user.type(screen.getByLabelText('Пароль'), 'correct horse battery staple')
  await user.click(screen.getByRole('button', { name: 'Войти' }))
}

test('TestLoginTOTPStep: a challenge asks for the code and only then stores the session', async () => {
  const user = userEvent.setup()
  const fetchImpl = mfaFetch(() => json(SESSION))
  renderLogin(fetchImpl as unknown as typeof fetch)
  await passPassword(user)

  // No token yet: the password alone is not a login on this account.
  const code = await screen.findByLabelText('Код из приложения')
  expect(localStorage.getItem('duskrun.token')).toBeNull()
  expect(code).toHaveAttribute('autocomplete', 'one-time-code')

  await user.type(code, '123456')
  await user.click(screen.getByRole('button', { name: 'Подтвердить' }))

  expect(localStorage.getItem('duskrun.token')).toBe('mfa-session')
  const call = fetchImpl.mock.calls.find(([u]) => String(u).endsWith('/api/login/mfa'))!
  expect(JSON.parse(String(call[1]!.body))).toEqual({ mfa_token: 'chal-1', method: 'totp', code: '123456' })
})

test('TestLoginWrongCodeStaysOnStep: 401 lets the user try again', async () => {
  const user = userEvent.setup()
  renderLogin(mfaFetch(() => json({ error: 'invalid code' }, 401)) as unknown as typeof fetch)
  await passPassword(user)

  await user.type(await screen.findByLabelText('Код из приложения'), '000000')
  await user.click(screen.getByRole('button', { name: 'Подтвердить' }))

  expect(await screen.findByRole('alert')).toHaveTextContent('Неверный код')
  expect(screen.getByLabelText('Код из приложения')).toBeInTheDocument()
  expect(localStorage.getItem('duskrun.token')).toBeNull()
})

test('TestLoginExpiredChallenge: 410 sends the user back to the password', async () => {
  const user = userEvent.setup()
  renderLogin(mfaFetch(() => json({ error: 'challenge expired' }, 410)) as unknown as typeof fetch)
  await passPassword(user)

  await user.type(await screen.findByLabelText('Код из приложения'), '123456')
  await user.click(screen.getByRole('button', { name: 'Подтвердить' }))

  expect(await screen.findByRole('alert')).toHaveTextContent(/сессия входа истекла/i)
  expect(screen.getByLabelText('Пароль')).toBeInTheDocument()
})

test('TestLoginRecoveryCodeWarnsWhenLow: the session waits until the warning is read', async () => {
  const user = userEvent.setup()
  const fetchImpl = mfaFetch(() => json({ ...SESSION, recovery_codes_left: 2 }))
  renderLogin(fetchImpl as unknown as typeof fetch)
  await passPassword(user)

  await user.click(await screen.findByRole('button', { name: 'Использовать код восстановления' }))
  await user.type(screen.getByLabelText('Код восстановления'), 'abcd-efgh-ijkl')
  await user.click(screen.getByRole('button', { name: 'Подтвердить' }))

  expect(await screen.findByRole('status')).toHaveTextContent('Осталось кодов восстановления: 2')
  expect(localStorage.getItem('duskrun.token')).toBeNull()
  const call = fetchImpl.mock.calls.find(([u]) => String(u).endsWith('/api/login/mfa'))!
  expect(JSON.parse(String(call[1]!.body))).toMatchObject({ method: 'recovery', code: 'abcd-efgh-ijkl' })

  await user.click(screen.getByRole('button', { name: 'Перейти в настройки' }))
  expect(localStorage.getItem('duskrun.token')).toBe('mfa-session')
})

test('TestLoginPasskey: the assertion is serialised and sent as method webauthn', async () => {
  const user = userEvent.setup()
  const buf = (...b: number[]) => new Uint8Array(b).buffer
  const get = vi.fn().mockResolvedValue({
    id: 'Bw',
    rawId: buf(7),
    type: 'public-key',
    getClientExtensionResults: () => ({}),
    response: { clientDataJSON: buf(1), authenticatorData: buf(2), signature: buf(3), userHandle: null },
  })
  vi.stubGlobal('PublicKeyCredential', function PublicKeyCredential() {})
  Object.defineProperty(navigator, 'credentials', { value: { get }, configurable: true })
  try {
    const fetchImpl = mfaFetch(() => json(SESSION), { ...CHALLENGE, mfa_methods: ['webauthn', 'recovery'] })
    renderLogin(fetchImpl as unknown as typeof fetch)
    await passPassword(user)

    // With no TOTP enrolled the passkey is the default second step.
    await user.click(await screen.findByRole('button', { name: 'Войти с passkey' }))

    expect(localStorage.getItem('duskrun.token')).toBe('mfa-session')
    const opts = get.mock.calls[0][0].publicKey
    expect(Array.from(new Uint8Array(opts.challenge))).toEqual([1, 2, 3])
    const call = fetchImpl.mock.calls.find(([u]) => String(u).endsWith('/api/login/mfa'))!
    expect(JSON.parse(String(call[1]!.body))).toMatchObject({
      mfa_token: 'chal-1',
      method: 'webauthn',
      credential: { id: 'Bw', rawId: 'Bw', response: { clientDataJSON: 'AQ', authenticatorData: 'Ag', signature: 'Aw' } },
    })
  } finally {
    vi.unstubAllGlobals()
    Reflect.deleteProperty(navigator, 'credentials')
  }
})
