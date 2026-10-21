import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { beforeEach, expect, test } from 'vitest'
import { makeFetch, renderWithAuth } from '../test/utils'
import Settings from './Settings'

beforeEach(() => localStorage.clear())

const SETTINGS = {
  instance_name: 'prod-eu',
  ssh_host_key_mode_default: 'tofu',
  instance: {
    version: '1.2.3',
    db_path: '/data/duskrun.db',
    workers: 4,
    retention_cron: '30 3 * * *',
    listen_addr: ':8080',
    timezone: 'UTC',
    timezone_offset: '+00:00',
    now: '2026-08-12T10:00:00Z',
  },
}

const USERS = [
  { id: 1, email: 'admin@corp.io', name: 'Админ', role: 'admin', disabled: false, created_at: '2026-08-01T00:00:00Z', last_login_at: '2026-08-12T09:00:00Z' },
  { id: 2, email: 'viewer@corp.io', name: '', role: 'viewer', disabled: true, created_at: '2026-08-02T00:00:00Z', mfa_totp: true, mfa_passkeys: 1 },
]

const MFA_OFF = { totp: false, passkeys: [], recovery_codes_left: 0, webauthn_available: true, policy: 'off', required: false }

const SESSIONS = [
  { id: 11, user_id: 1, email: 'admin@corp.io', user_agent: 'Firefox', ip: '10.0.0.2', created_at: '2026-08-12T08:00:00Z', last_seen_at: '2026-08-12T09:59:00Z', expires_at: '2026-09-11T08:00:00Z', current: true },
  { id: 12, user_id: 2, email: 'viewer@corp.io', user_agent: 'curl/8', ip: '10.0.0.3', created_at: '2026-08-10T08:00:00Z', last_seen_at: '2026-08-11T09:00:00Z', expires_at: '2026-09-09T08:00:00Z', current: false },
]

const ADMIN_ME = { static: false, role: 'admin', email: 'admin@corp.io', name: 'Админ', user_id: 1, session_id: 11 }
const VIEWER_ME = { static: false, role: 'viewer', email: 'viewer@corp.io', user_id: 2, session_id: 12 }

function routes(over: Record<string, unknown> = {}) {
  return {
    'GET /api/me': ADMIN_ME,
    'GET /api/settings': SETTINGS,
    'GET /api/users': USERS,
    'GET /api/sessions': SESSIONS,
    'GET /api/me/mfa': MFA_OFF,
    ...over,
  }
}

test('TestSettingsRendersRealValues: general, users, sessions and the read-only instance block', async () => {
  renderWithAuth(<Settings />, { fetchImpl: makeFetch(routes()) })

  expect(await screen.findByDisplayValue('prod-eu')).toBeInTheDocument()
  // The user table arrives on a second pass: /me has to resolve before the page
  // knows it is allowed to ask for /users at all.
  expect(await screen.findByText('viewer@corp.io')).toBeInTheDocument()
  // Twice over: once in the user table, once as the owner of a live session.
  expect(screen.getAllByText('admin@corp.io')).toHaveLength(2)

  // The instance block reports where each value comes from, so the page does
  // not imply it can change them.
  expect(screen.getByText('DUSKRUN_WORKERS')).toBeInTheDocument()
  expect(screen.getByText('/data/duskrun.db')).toBeInTheDocument()
  expect(screen.getByText('UTC (UTC+00:00)')).toBeInTheDocument()
})

test('TestSettingsSavesGeneral: PATCH carries both editable keys', async () => {
  const user = userEvent.setup()
  const fetch = makeFetch(routes({ 'PATCH /api/settings': { ...SETTINGS, instance_name: 'staging' } }))
  renderWithAuth(<Settings />, { fetchImpl: fetch })

  const name = await screen.findByLabelText('Имя инстанса')
  await user.clear(name)
  await user.type(name, 'staging')
  await user.click(screen.getByRole('button', { name: 'Сохранить' }))

  await waitFor(() => {
    const patch = fetch.calls().find(([, init]) => init?.method === 'PATCH')
    expect(patch).toBeTruthy()
    expect(JSON.parse(patch![1].body)).toEqual({
      instance_name: 'staging',
      ssh_host_key_mode_default: 'tofu',
    })
  })
})

test('TestSessionsShowCurrent: the caller\'s own session is labelled', async () => {
  renderWithAuth(<Settings />, { fetchImpl: makeFetch(routes()) })
  expect(await screen.findByText('текущая')).toBeInTheDocument()
  expect(screen.getByText(/curl\/8/)).toBeInTheDocument()
})

test('TestEndSession: DELETE goes to the chosen session', async () => {
  const user = userEvent.setup()
  const fetch = makeFetch(routes({ 'DELETE /api/sessions/12': { status: 204, body: null } }))
  renderWithAuth(<Settings />, { fetchImpl: fetch })

  await screen.findByText(/curl\/8/)
  const rows = screen.getAllByRole('button', { name: 'Завершить' })
  // Second row is the other user's session; ending the current one logs out.
  await user.click(rows[1])

  await waitFor(() => {
    expect(fetch.calls().some(([url, init]) => String(url).endsWith('/api/sessions/12') && init?.method === 'DELETE')).toBe(true)
  })
})

test('TestCreateUserModal: POST /users carries the form', async () => {
  const user = userEvent.setup()
  const created = { id: 3, email: 'new@corp.io', name: 'Новый', role: 'operator', disabled: false, created_at: '2026-08-12T00:00:00Z' }
  const fetch = makeFetch(routes({ 'POST /api/users': created }))
  renderWithAuth(<Settings />, { fetchImpl: fetch })

  await user.click(await screen.findByRole('button', { name: 'Новый пользователь' }))
  const dialog = screen.getByRole('dialog')
  await user.type(within(dialog).getByLabelText('Email'), 'new@corp.io')
  await user.type(within(dialog).getByLabelText('Имя'), 'Новый')
  await user.selectOptions(within(dialog).getByLabelText('Роль'), 'operator')
  await user.type(within(dialog).getByLabelText('Пароль'), 'correct horse battery')
  await user.click(within(dialog).getByRole('button', { name: 'Сохранить' }))

  await waitFor(() => {
    const post = fetch.calls().find(([url, init]) => String(url).endsWith('/api/users') && init?.method === 'POST')
    expect(post).toBeTruthy()
    expect(JSON.parse(post![1].body)).toEqual({
      email: 'new@corp.io', name: 'Новый', role: 'operator', password: 'correct horse battery',
    })
  })
})

test('TestToggleDisable: the button flips the stored flag', async () => {
  const user = userEvent.setup()
  const fetch = makeFetch(routes({ 'PATCH /api/users/2': { ...USERS[1], disabled: false } }))
  renderWithAuth(<Settings />, { fetchImpl: fetch })

  await screen.findByText('viewer@corp.io')
  await user.click(screen.getByRole('button', { name: 'Включить' }))

  await waitFor(() => {
    const patch = fetch.calls().find(([url, init]) => String(url).endsWith('/api/users/2') && init?.method === 'PATCH')
    expect(JSON.parse(patch![1].body)).toEqual({ disabled: false })
  })
})

test('TestLastAdminConflictSurfaces: a 409 from the server is shown, not swallowed', async () => {
  const user = userEvent.setup()
  const fetch = makeFetch(routes({
    'DELETE /api/users/1': { status: 409, body: { error: 'refusing to leave the instance without an enabled admin' } },
  }))
  renderWithAuth(<Settings />, { fetchImpl: fetch })

  await screen.findByText('admin@corp.io')
  await user.click(screen.getAllByRole('button', { name: 'Удалить' })[0])

  expect(await screen.findByRole('alert')).toHaveTextContent(/without an enabled admin/i)
})

// The role check that matters is the server's; this only asserts the page does
// not offer admin-only controls to someone who would get a 403 for using them.
test('TestViewerSeesNoUserManagement: no user table, no create button', async () => {
  const fetch = makeFetch({
    'GET /api/me': VIEWER_ME,
    'GET /api/settings': SETTINGS,
    'GET /api/sessions': [SESSIONS[1]],
  })
  renderWithAuth(<Settings />, { fetchImpl: fetch })

  await screen.findByDisplayValue('prod-eu')
  expect(screen.queryByRole('button', { name: 'Новый пользователь' })).not.toBeInTheDocument()
  expect(screen.queryByText('Пользователи и доступ')).not.toBeInTheDocument()
  // …and it never asked for the list it may not have.
  expect(fetch.calls().some(([url]) => String(url).endsWith('/api/users'))).toBe(false)
})

test('TestStaticTokenHidesPasswordBlock: an API token has no password to change', async () => {
  const fetch = makeFetch({
    'GET /api/me': { static: true, role: 'admin' },
    'GET /api/settings': SETTINGS,
    'GET /api/users': USERS,
    'GET /api/sessions': [],
  })
  renderWithAuth(<Settings />, { fetchImpl: fetch })

  await screen.findByDisplayValue('prod-eu')
  expect(screen.queryByRole('button', { name: 'Сменить пароль' })).not.toBeInTheDocument()
  expect(screen.getByText(/вход выполнен по API-токену/i)).toBeInTheDocument()
})

test('TestChangeOwnPassword: mismatched repeat is caught before the request', async () => {
  const user = userEvent.setup()
  const fetch = makeFetch(routes({ 'POST /api/me/password': { status: 'ok' } }))
  renderWithAuth(<Settings />, { fetchImpl: fetch })

  await user.click(await screen.findByRole('button', { name: 'Сменить пароль' }))
  const dialog = screen.getByRole('dialog')
  await user.type(within(dialog).getByLabelText('Текущий пароль'), 'old password here')
  await user.type(within(dialog).getByLabelText('Новый пароль'), 'new password here')
  await user.type(within(dialog).getByLabelText('Повторите новый пароль'), 'different')
  await user.click(within(dialog).getByRole('button', { name: 'Сменить пароль' }))

  expect(await within(dialog).findByRole('alert')).toHaveTextContent(/не совпадают/i)
  expect(fetch.calls().some(([url]) => String(url).endsWith('/api/me/password'))).toBe(false)

  // Matching values do send it.
  await user.clear(within(dialog).getByLabelText('Повторите новый пароль'))
  await user.type(within(dialog).getByLabelText('Повторите новый пароль'), 'new password here')
  await user.click(within(dialog).getByRole('button', { name: 'Сменить пароль' }))

  await waitFor(() => {
    const post = fetch.calls().find(([url]) => String(url).endsWith('/api/me/password'))
    expect(JSON.parse(post![1].body)).toEqual({
      old_password: 'old password here',
      new_password: 'new password here',
    })
  })
})

// --- 2FA -----------------------------------------------------------------

test('TestUserTableShowsMFA: factors are summarised and only others can be reset', async () => {
  renderWithAuth(<Settings />, { fetchImpl: makeFetch(routes()) })
  expect(await screen.findByText('TOTP + 1 passkey')).toBeInTheDocument()
  // One reset button: viewer has 2FA; the admin row is the caller and has none.
  expect(screen.getAllByRole('button', { name: 'Сбросить 2FA' })).toHaveLength(1)
})

test('TestAdminResetsMFA: confirmation dialog, then POST /users/{id}/mfa/reset', async () => {
  const user = userEvent.setup()
  const fetch = makeFetch(routes({ 'POST /api/users/2/mfa/reset': { ...USERS[1], mfa_totp: false, mfa_passkeys: 0 } }))
  renderWithAuth(<Settings />, { fetchImpl: fetch })

  await user.click(await screen.findByRole('button', { name: 'Сбросить 2FA' }))
  const dialog = screen.getByRole('dialog')
  expect(dialog).toHaveTextContent('viewer@corp.io')
  // Nothing is sent until the admin confirms in the dialog.
  expect(fetch.calls().some(([url]) => String(url).endsWith('/mfa/reset'))).toBe(false)
  await user.click(within(dialog).getByRole('button', { name: 'Сбросить 2FA' }))

  await waitFor(() => {
    expect(fetch.calls().some(([url, init]) => String(url).endsWith('/api/users/2/mfa/reset') && init?.method === 'POST')).toBe(true)
  })
  expect(await screen.findByRole('status')).toHaveTextContent(/2FA пользователя viewer@corp.io сброшена/)
})

test('TestMFAPolicySaves: the select PATCHes mfa_required alone', async () => {
  const user = userEvent.setup()
  const fetch = makeFetch(routes({ 'PATCH /api/settings': { ...SETTINGS, mfa_required: 'admins' } }))
  renderWithAuth(<Settings />, { fetchImpl: fetch })

  await user.selectOptions(await screen.findByLabelText('Требовать 2FA'), 'admins')

  await waitFor(() => {
    const patch = fetch.calls().find(([, init]) => init?.method === 'PATCH')
    expect(JSON.parse(patch![1].body)).toEqual({ mfa_required: 'admins' })
  })
})

test('TestMFAPolicyConflictSurfaces: an admin without 2FA sees the 409', async () => {
  const user = userEvent.setup()
  const fetch = makeFetch(routes({
    'PATCH /api/settings': { status: 409, body: { error: 'enable 2FA on your own account first' } },
  }))
  renderWithAuth(<Settings />, { fetchImpl: fetch })

  await user.selectOptions(await screen.findByLabelText('Требовать 2FA'), 'all')
  expect(await screen.findByRole('alert')).toHaveTextContent(/enable 2FA on your own account first/)
})

test('TestEnableTOTP: password → QR → code → recovery codes that must be acknowledged', async () => {
  const user = userEvent.setup()
  const codes = ['aaaa-bbbb-cccc', 'dddd-eeee-ffff']
  const fetch = makeFetch(routes({
    'POST /api/me/mfa/totp/setup': { secret: 'JBSWY3DPEHPK3PXP', otpauth_url: 'otpauth://totp/x', qr: 'data:image/png;base64,AAAA' },
    'POST /api/me/mfa/totp/confirm': { recovery_codes: codes },
  }))
  renderWithAuth(<Settings />, { fetchImpl: fetch })

  await user.click(await screen.findByRole('button', { name: 'Настроить' }))
  let dialog = screen.getByRole('dialog')
  await user.type(within(dialog).getByLabelText('Текущий пароль'), 'correct horse battery')
  await user.click(within(dialog).getByRole('button', { name: 'Далее' }))

  expect(await within(dialog).findByAltText(/QR-код/)).toHaveAttribute('src', 'data:image/png;base64,AAAA')
  expect(within(dialog).getByTestId('totp-secret')).toHaveTextContent('JBSW Y3DP EHPK 3PXP')
  const setup = fetch.calls().find(([url]) => String(url).endsWith('/totp/setup'))!
  expect(JSON.parse(setup[1].body)).toEqual({ password: 'correct horse battery' })

  // A malformed code is caught locally.
  await user.type(within(dialog).getByLabelText('Код из приложения'), '12')
  await user.click(within(dialog).getByRole('button', { name: 'Подтвердить' }))
  expect(within(dialog).getByRole('alert')).toHaveTextContent(/6 цифр/)
  expect(fetch.calls().some(([url]) => String(url).endsWith('/totp/confirm'))).toBe(false)

  await user.clear(within(dialog).getByLabelText('Код из приложения'))
  await user.type(within(dialog).getByLabelText('Код из приложения'), '123 456')
  await user.click(within(dialog).getByRole('button', { name: 'Подтвердить' }))

  dialog = await screen.findByRole('dialog', { name: 'Коды восстановления' })
  expect(within(dialog).getByTestId('recovery-codes')).toHaveTextContent('aaaa-bbbb-cccc')
  const confirm = fetch.calls().find(([url]) => String(url).endsWith('/totp/confirm'))!
  expect(JSON.parse(confirm[1].body)).toEqual({ code: '123456' })

  const doneBtn = within(dialog).getByRole('button', { name: 'Готово' })
  expect(doneBtn).toBeDisabled()
  await user.click(within(dialog).getByLabelText('Я сохранил коды'))
  await user.click(doneBtn)
  expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
})

test('TestRegenerateCodesWrongPassword: the 403 reads as a wrong password', async () => {
  const user = userEvent.setup()
  const fetch = makeFetch(routes({
    'GET /api/me/mfa': { ...MFA_OFF, totp: true, recovery_codes_left: 2 },
    'POST /api/me/mfa/recovery-codes': { status: 403, body: { error: 'current password is incorrect' } },
  }))
  renderWithAuth(<Settings />, { fetchImpl: fetch })

  expect(await screen.findByText(/Осталось неиспользованных: 2/)).toBeInTheDocument()
  await user.click(screen.getByRole('button', { name: 'Сгенерировать новые' }))
  const dialog = screen.getByRole('dialog')
  await user.type(within(dialog).getByLabelText('Текущий пароль'), 'nope nope nope')
  await user.click(within(dialog).getByRole('button', { name: 'Сгенерировать' }))

  expect(await within(dialog).findByRole('alert')).toHaveTextContent('Неверный пароль')
})

test('TestLastFactorLockedByPolicy: the only factor cannot be removed when required', async () => {
  // As a viewer, so the admin user table's own "Отключить" is not on the page.
  const fetch = makeFetch(routes({
    'GET /api/me': VIEWER_ME,
    'GET /api/me/mfa': { ...MFA_OFF, totp: true, recovery_codes_left: 10, policy: 'all', required: true },
  }))
  renderWithAuth(<Settings />, { fetchImpl: fetch })

  expect(await screen.findByRole('button', { name: 'Отключить' })).toBeDisabled()
})

test('TestPasskeysNeedPublicURL: without DUSKRUN_PUBLIC_URL adding is disabled and explained', async () => {
  const fetch = makeFetch(routes({ 'GET /api/me/mfa': { ...MFA_OFF, webauthn_available: false } }))
  renderWithAuth(<Settings />, { fetchImpl: fetch })

  expect(await screen.findByText(/DUSKRUN_PUBLIC_URL/)).toBeInTheDocument()
  expect(screen.getByRole('button', { name: 'Добавить passkey' })).toBeDisabled()
})

test('TestDeletePasskey: password goes in the DELETE body', async () => {
  const user = userEvent.setup()
  const fetch = makeFetch(routes({
    'GET /api/me/mfa': { ...MFA_OFF, totp: true, recovery_codes_left: 10, passkeys: [{ id: 5, name: 'YubiKey', created_at: '2026-08-01T00:00:00Z' }] },
    'DELETE /api/me/mfa/passkeys/5': { status: 204, body: null },
  }))
  renderWithAuth(<Settings />, { fetchImpl: fetch })

  await user.click(await screen.findByRole('button', { name: 'Удалить passkey YubiKey' }))
  const dialog = screen.getByRole('dialog')
  await user.type(within(dialog).getByLabelText('Текущий пароль'), 'correct horse battery')
  await user.click(within(dialog).getByRole('button', { name: 'Удалить' }))

  await waitFor(() => {
    const del = fetch.calls().find(([url, init]) => String(url).endsWith('/api/me/mfa/passkeys/5') && init?.method === 'DELETE')
    expect(JSON.parse(del![1].body)).toEqual({ password: 'correct horse battery' })
  })
})

test('TestStaticTokenHidesMFA: the API token has no second factor to manage', async () => {
  const fetch = makeFetch({
    'GET /api/me': { static: true, role: 'admin' },
    'GET /api/settings': SETTINGS,
    'GET /api/users': USERS,
    'GET /api/sessions': [],
  })
  renderWithAuth(<Settings />, { fetchImpl: fetch })

  await screen.findByDisplayValue('prod-eu')
  expect(screen.queryByText('Двухфакторная аутентификация')).not.toBeInTheDocument()
  expect(fetch.calls().some(([url]) => String(url).endsWith('/api/me/mfa'))).toBe(false)
})
