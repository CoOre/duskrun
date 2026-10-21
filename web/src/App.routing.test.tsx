import { render, screen } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'
import { beforeEach, expect, test } from 'vitest'
import { AppRoutes } from './App'
import { AuthProvider } from './auth'
import { makeFetch } from './test/utils'

function renderAt(route: string, token: string | null, fetchImpl?: typeof fetch) {
  localStorage.clear()
  if (token) localStorage.setItem('duskrun.token', token)
  return render(
    <AuthProvider fetchImpl={fetchImpl}>
      <MemoryRouter initialEntries={[route]}>
        <AppRoutes />
      </MemoryRouter>
    </AuthProvider>,
  )
}

const EMPTY = () => makeFetch({ '/api/tasks': [], '/api/runs': [], '/api/connections': [], '/api/storages': [] })

beforeEach(() => localStorage.clear())

test('TestUnauthedRedirectsToLogin: a protected route bounces to login', () => {
  renderAt('/', null)
  expect(screen.getByLabelText('Email')).toBeInTheDocument()
})

test('TestSecretsRouteRenders: secrets section is implemented', () => {
  renderAt('/secrets', 'a-token')
  expect(screen.getByText('Мастер-ключ')).toBeInTheDocument()
})

test('unknown authed path falls back to the dashboard', async () => {
  renderAt('/nope', 'a-token', EMPTY())
  expect(await screen.findByText('Активность запусков')).toBeInTheDocument()
})

test('TestEnrollGate: a required-but-missing 2FA replaces every page with enrolment', async () => {
  const fetch = makeFetch({
    'GET /api/me': { static: false, role: 'operator', email: 'op@corp.io', user_id: 3, session_id: 9, mfa_enabled: false, mfa_enroll_required: true },
    'GET /api/me/mfa': { totp: false, passkeys: [], recovery_codes_left: 0, webauthn_available: false, policy: 'all', required: true },
  })
  renderAt('/tasks', 'a-token', fetch)
  expect(await screen.findByText('Администратор требует 2FA')).toBeInTheDocument()
  expect(await screen.findByRole('button', { name: 'Настроить' })).toBeInTheDocument()
  expect(screen.getByRole('button', { name: 'Выйти' })).toBeInTheDocument()
  expect(screen.queryByRole('heading', { name: 'Задачи' }) ?? screen.queryByText(/^\d+ задач$/)).toBeNull()
})
