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
