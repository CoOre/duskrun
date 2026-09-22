import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter } from 'react-router-dom'
import { makeFetch, renderWithAuth } from '../test/utils'
import { Layout, identityOf } from './Layout'

function renderLayout() {
  return render(
    <MemoryRouter>
      <Layout title="Дашборд">
        <div>content</div>
      </Layout>
    </MemoryRouter>,
  )
}

beforeEach(() => {
  localStorage.clear()
  document.documentElement.removeAttribute('data-theme')
})

test('TestLayoutRendersNav: sidebar shows the primary nav items', () => {
  renderLayout()
  expect(screen.getByText('Задачи')).toBeInTheDocument()
  expect(screen.getByText(/История/)).toBeInTheDocument()
  expect(screen.getByText('Соединения')).toBeInTheDocument()
})

test('TestThemeToggle: clicking the light option sets data-theme', async () => {
  const user = userEvent.setup()
  renderLayout()
  await user.click(screen.getByLabelText('Светлая тема'))
  expect(document.documentElement.getAttribute('data-theme')).toBe('light')

  await user.click(screen.getByLabelText('Тёмная тема'))
  expect(document.documentElement.getAttribute('data-theme')).toBe('dark')
})

test('TestIdentityOf: the sidebar describes the real credential, not a placeholder', () => {
  // A named user: initials come from the name, the row shows the email.
  expect(identityOf({ static: false, role: 'operator', email: 'ivan.petrov@corp.io', name: 'Иван Петров' }))
    .toEqual({ initials: 'ИП', title: 'ivan.petrov@corp.io', subtitle: 'operator' })

  // No name: fall back to the local part of the address.
  expect(identityOf({ static: false, role: 'viewer', email: 'ops.duty@corp.io' }))
    .toEqual({ initials: 'OD', title: 'ops.duty@corp.io', subtitle: 'viewer' })

  // The static token is a shared machine credential and is labelled as one,
  // rather than being dressed up as a person.
  expect(identityOf({ static: true, role: 'admin' }))
    .toEqual({ initials: 'API', title: 'API-токен', subtitle: 'admin' })

  // Identity not resolved yet: say so instead of guessing.
  expect(identityOf(null).title).toBe('Не определён')
})

test('TestLayoutShowsUserFromMe: the sidebar renders /me, not a hardcoded account', async () => {
  const fetchImpl = makeFetch({ 'GET /api/me': { static: false, role: 'admin', email: 'real@corp.io', name: 'Real Person' } })
  renderWithAuth(
    <Layout title="Дашборд">
      <div>content</div>
    </Layout>,
    { fetchImpl },
  )
  expect(await screen.findByText('real@corp.io')).toBeInTheDocument()
  expect(screen.queryByText('admin@corp.io')).not.toBeInTheDocument()
})
