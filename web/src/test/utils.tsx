import { ReactNode } from 'react'
import { render } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'
import { vi } from 'vitest'
import { AuthProvider } from '../auth'

type RouteReply = unknown | ((init: RequestInit) => unknown)
type RouteMap = Record<string, RouteReply | { status: number; body: unknown }>

function jsonResp(body: unknown, status = 200): Response {
  // A 204 (and other null-body statuses) must be constructed with a null body —
  // `new Response('', {status: 204})` throws. This mirrors the real API, which
  // answers a successful DELETE with 204 No Content.
  return new Response(status === 204 ? null : JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })
}

// Every render goes through AuthProvider, which asks /me for the caller's role,
// and pages hide actions the role cannot use. Defaulting that answer to an admin
// keeps page tests about the page: a test that cares about roles overrides
// 'GET /api/me' explicitly.
const DEFAULT_ME = {
  static: false,
  role: 'admin',
  email: 'test-admin@corp.io',
  name: 'Test Admin',
  user_id: 1,
  session_id: 1,
}

// makeFetch builds a fetch mock that routes by pathname (query string ignored).
// A route value may be a plain body, a {status,body} object, or a function of
// the request init (to assert/return based on the POST body).
export function makeFetch(routes: RouteMap): typeof fetch & { calls: () => any[] } {
  const withMe: RouteMap = { 'GET /api/me': DEFAULT_ME, ...routes }
  const fn = vi.fn(async (url: string, init: RequestInit = {}) => {
    const path = url.split('?')[0]
    const method = (init.method ?? 'GET').toUpperCase()
    // Prefer a method-qualified route ("POST /api/storages"), then a bare path.
    const entry = withMe[`${method} ${path}`] ?? withMe[path]
    if (entry === undefined) {
      return jsonResp({ error: `no mock for ${method} ${path}` }, 404)
    }
    if (typeof entry === 'function') {
      return jsonResp(entry(init))
    }
    if (entry && typeof entry === 'object' && 'status' in entry && 'body' in entry) {
      const e = entry as { status: number; body: unknown }
      return jsonResp(e.body, e.status)
    }
    return jsonResp(entry)
  })
  const wrapped = fn as unknown as typeof fetch & { calls: () => any[] }
  wrapped.calls = () => fn.mock.calls
  return wrapped
}

export function renderWithAuth(
  ui: ReactNode,
  opts: { fetchImpl?: typeof fetch; token?: string | null; route?: string } = {},
) {
  const { fetchImpl, token = 'test-token', route = '/' } = opts
  if (token) localStorage.setItem('duskrun.token', token)
  return render(
    <MemoryRouter initialEntries={[route]}>
      <AuthProvider fetchImpl={fetchImpl}>{ui}</AuthProvider>
    </MemoryRouter>,
  )
}
