import { FormEvent, useState } from 'react'
import { ApiError } from '../api/client'
import { useApiData } from '../api/useApiData'
import { fmtDateTime } from '../api/view'
import type { Role, Session, Settings as SettingsData, User } from '../api/types'
import { useAuth, useCan } from '../auth'
import { Layout } from '../ui/Layout'
import { ModalShell } from '../ui/ModalShell'
import { MONO, btnDanger, btnPrimary, btnSecondary, inputStyle, labelStyle } from '../ui/dc'

type Data = { settings: SettingsData; users: User[]; sessions: Session[] }

const ROLE_LABEL: Record<Role, string> = {
  admin: 'Администратор',
  operator: 'Оператор',
  viewer: 'Наблюдатель',
}

const ROLE_HINT: Record<Role, string> = {
  admin: 'всё, включая пользователей, секреты, каналы и настройки',
  operator: 'задачи, соединения, хранилища, запуск и скачивание артефактов',
  viewer: 'только чтение: дашборд, история и логи прогонов',
}

const SECTION: React.CSSProperties = { fontSize: 13, fontWeight: 600, color: 'var(--ink)', marginBottom: 12 }
const PANEL: React.CSSProperties = { border: '1px solid var(--line)', borderRadius: 10, background: 'var(--panel)', marginBottom: 26 }
const MUTED: React.CSSProperties = { padding: 26, fontSize: 13, color: 'var(--ink-3)' }
const ERRSTYLE: React.CSSProperties = { padding: 26, fontSize: 13, color: 'var(--err)' }
const USER_COLS = '1.6fr 1fr 0.8fr 1fr auto'

export default function Settings() {
  const { api, logout, me, refreshMe } = useAuth()
  const isAdmin = useCan('admin')
  const [reload, setReload] = useState(0)
  const [banner, setBanner] = useState<string | null>(null)
  const [err, setErr] = useState<string | null>(null)
  const [userModal, setUserModal] = useState<'new' | User | null>(null)
  const [pwModal, setPwModal] = useState(false)

  const { data, error, loading } = useApiData<Data>(async () => {
    // Only an admin may list users; a viewer asking would get a 403 and blank
    // the whole page, so the request is simply not made.
    const [settings, sessions, users] = await Promise.all([
      api.getSettings(),
      api.listSessions(),
      isAdmin ? api.listUsers() : Promise.resolve([] as User[]),
    ])
    return { settings, users, sessions }
  }, [api, reload, isAdmin])

  const settings = data?.settings
  const bump = () => setReload((n) => n + 1)

  function report(e: unknown) {
    setErr(e instanceof ApiError ? e.message : e instanceof Error ? e.message : String(e))
    setBanner(null)
  }

  async function saveSettings(patch: Partial<Pick<SettingsData, 'instance_name' | 'ssh_host_key_mode_default'>>) {
    setErr(null)
    try {
      await api.updateSettings(patch)
      setBanner('Настройки сохранены')
      bump()
    } catch (e) {
      report(e)
    }
  }

  async function endSession(id: number, current: boolean) {
    setErr(null)
    try {
      await api.deleteSession(id)
      // Ending your own session is a deliberate "log out everywhere"; the next
      // request would 401 anyway, so leave cleanly instead.
      if (current) logout()
      else bump()
    } catch (e) {
      report(e)
    }
  }

  async function patchUser(u: User, patch: Parameters<typeof api.updateUser>[1]) {
    setErr(null)
    try {
      await api.updateUser(u.id, patch)
      setBanner(`Пользователь ${u.email} обновлён`)
      if (u.id === me?.user_id) refreshMe()
      bump()
    } catch (e) {
      report(e)
    }
  }

  async function removeUser(u: User) {
    setErr(null)
    try {
      await api.deleteUser(u.id)
      setBanner(`Пользователь ${u.email} удалён`)
      bump()
    } catch (e) {
      report(e)
    }
  }

  return (
    <Layout
      title="Настройки"
      subtitle={settings ? `${settings.instance_name} · ${settings.instance.version}` : ''}
      onLogout={logout}
      actions={
        isAdmin ? (
          <button type="button" style={btnPrimary} onClick={() => setUserModal('new')}>
            Новый пользователь
          </button>
        ) : null
      }
    >
      {loading && <div style={MUTED}>Загрузка…</div>}
      {error && <div style={ERRSTYLE} role="alert">{error}</div>}

      {!loading && !error && settings && (
        <div style={{ padding: '24px 26px' }} className="dc-fade">
          {err && <div role="alert" style={{ color: 'var(--err)', fontSize: 12.5, marginBottom: 16 }}>{err}</div>}
          {banner && <div role="status" style={{ color: 'var(--ok)', fontSize: 12.5, marginBottom: 16 }}>{banner}</div>}

          <GeneralBlock settings={settings} canEdit={isAdmin} onSave={saveSettings} />

          {isAdmin && (
            <>
              <div style={SECTION}>Пользователи и доступ</div>
              <div style={PANEL}>
                <div style={{ display: 'grid', gridTemplateColumns: USER_COLS, padding: '10px 20px', fontSize: 10, fontWeight: 600, letterSpacing: '0.05em', color: 'var(--ink-3)', borderBottom: '1px solid var(--line)', fontFamily: MONO }}>
                  <div>ПОЛЬЗОВАТЕЛЬ</div><div>РОЛЬ</div><div>СОСТОЯНИЕ</div><div>ПОСЛЕДНИЙ ВХОД</div><div />
                </div>
                {(data?.users ?? []).map((u) => (
                  <div key={u.id} style={{ display: 'grid', gridTemplateColumns: USER_COLS, padding: '12px 20px', fontSize: 12.5, borderBottom: '1px solid var(--line)', alignItems: 'center', gap: 10 }}>
                    <div style={{ minWidth: 0 }}>
                      <div style={{ color: u.disabled ? 'var(--ink-3)' : 'var(--ink)', fontWeight: 500, overflow: 'hidden', textOverflow: 'ellipsis' }}>{u.email}</div>
                      {u.name && <div style={{ fontSize: 11, color: 'var(--ink-3)' }}>{u.name}</div>}
                    </div>
                    <div style={{ fontSize: 11.5, color: 'var(--ink-2)' }} title={ROLE_HINT[u.role]}>{ROLE_LABEL[u.role] ?? u.role}</div>
                    <div style={{ fontSize: 11.5, color: u.disabled ? 'var(--warn)' : 'var(--ink-3)', fontFamily: MONO }}>
                      {u.disabled ? 'отключён' : 'активен'}
                    </div>
                    <div style={{ fontSize: 11, color: 'var(--ink-3)', fontFamily: MONO }}>
                      {u.last_login_at ? fmtDateTime(u.last_login_at) : 'ни разу'}
                    </div>
                    <div style={{ display: 'flex', gap: 6, justifyContent: 'flex-end' }}>
                      <button type="button" style={{ ...btnSecondary, padding: '5px 10px', fontSize: 11.5 }} onClick={() => setUserModal(u)}>
                        Изменить
                      </button>
                      <button
                        type="button"
                        style={{ ...btnSecondary, padding: '5px 10px', fontSize: 11.5 }}
                        onClick={() => patchUser(u, { disabled: !u.disabled })}
                      >
                        {u.disabled ? 'Включить' : 'Отключить'}
                      </button>
                      <button type="button" style={{ ...btnDanger, padding: '5px 10px', fontSize: 11.5 }} onClick={() => removeUser(u)}>
                        Удалить
                      </button>
                    </div>
                  </div>
                ))}
              </div>
            </>
          )}

          <div style={SECTION}>Активные сессии</div>
          <div style={PANEL}>
            {(data?.sessions ?? []).length === 0 ? (
              <div style={{ padding: 20, fontSize: 12.5, color: 'var(--ink-3)' }}>
                Сессий нет — вход выполнен по API-токену, который сессию не создаёт.
              </div>
            ) : (
              (data?.sessions ?? []).map((s) => (
                <div key={s.id} style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: 16, padding: '12px 20px', borderBottom: '1px solid var(--line)' }}>
                  <div style={{ minWidth: 0 }}>
                    <div style={{ fontSize: 12.5, color: 'var(--ink)' }}>
                      {s.email ? `${s.email} · ` : ''}{s.ip || 'адрес неизвестен'}
                      {s.current && (
                        <span style={{ marginLeft: 8, fontSize: 10.5, fontFamily: MONO, color: 'var(--accent)', border: '1px solid var(--accent)', borderRadius: 5, padding: '1px 6px' }}>текущая</span>
                      )}
                    </div>
                    <div style={{ fontSize: 11, color: 'var(--ink-3)', fontFamily: MONO, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
                      {s.user_agent || 'без User-Agent'}
                    </div>
                  </div>
                  <div style={{ display: 'flex', alignItems: 'center', gap: 14, flexShrink: 0 }}>
                    <div style={{ textAlign: 'right', fontSize: 11, color: 'var(--ink-3)', fontFamily: MONO }}>
                      <div>активна {fmtDateTime(s.last_seen_at)}</div>
                      <div>истекает {fmtDateTime(s.expires_at)}</div>
                    </div>
                    <button type="button" style={{ ...btnSecondary, padding: '5px 10px', fontSize: 11.5 }} onClick={() => endSession(s.id, s.current)}>
                      Завершить
                    </button>
                  </div>
                </div>
              ))
            )}
          </div>

          {me && !me.static && (
            <>
              <div style={SECTION}>Мой пароль</div>
              <div style={{ ...PANEL, padding: '16px 20px', display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: 16 }}>
                <div style={{ fontSize: 12.5, color: 'var(--ink-2)' }}>
                  Смена пароля завершает все остальные сессии этой учётной записи.
                </div>
                <button type="button" style={btnSecondary} onClick={() => setPwModal(true)}>Сменить пароль</button>
              </div>
            </>
          )}

          <div style={SECTION}>Инстанс</div>
          <div style={{ ...PANEL, padding: '4px 20px 12px' }}>
            <InstanceRow label="Версия" value={settings.instance.version} source="сборка" />
            <InstanceRow label="Файл метаданных" value={settings.instance.db_path} source="DUSKRUN_DB_PATH" />
            <InstanceRow label="Лимит воркеров" value={String(settings.instance.workers)} source="DUSKRUN_WORKERS" />
            <InstanceRow label="Адрес" value={settings.instance.listen_addr} source="DUSKRUN_LISTEN" />
            <InstanceRow
              label="Расписание очистки"
              value={settings.instance.retention_cron || 'выключено'}
              source="DUSKRUN_RETENTION_CRON"
            />
            <InstanceRow
              label="Часовой пояс"
              value={`${settings.instance.timezone} (UTC${settings.instance.timezone_offset})`}
              // Read-only on purpose: cron is interpreted in this zone, so
              // changing it here would move every existing schedule.
              source="часовой пояс процесса; расписания считаются в нём"
            />
          </div>
        </div>
      )}

      {userModal && (
        <UserModal
          user={userModal === 'new' ? null : userModal}
          onClose={() => setUserModal(null)}
          onSaved={(msg) => { setUserModal(null); setBanner(msg); setErr(null); bump() }}
          onError={report}
        />
      )}
      {pwModal && (
        <PasswordModal
          onClose={() => setPwModal(false)}
          onSaved={() => { setPwModal(false); setBanner('Пароль изменён, остальные сессии завершены'); setErr(null); bump() }}
        />
      )}
    </Layout>
  )
}

function InstanceRow({ label, value, source }: { label: string; value: string; source: string }) {
  return (
    <div style={{ display: 'flex', alignItems: 'baseline', justifyContent: 'space-between', gap: 16, padding: '10px 0', borderBottom: '1px solid var(--line)' }}>
      <div style={{ fontSize: 12.5, color: 'var(--ink-2)' }}>{label}</div>
      <div style={{ textAlign: 'right', minWidth: 0 }}>
        <div style={{ fontFamily: MONO, fontSize: 12, color: 'var(--ink)', overflowWrap: 'anywhere' }}>{value}</div>
        <div style={{ fontSize: 10.5, color: 'var(--ink-3)', fontFamily: MONO }}>{source}</div>
      </div>
    </div>
  )
}

function GeneralBlock({
  settings,
  canEdit,
  onSave,
}: {
  settings: SettingsData
  canEdit: boolean
  onSave: (patch: Partial<Pick<SettingsData, 'instance_name' | 'ssh_host_key_mode_default'>>) => void
}) {
  const [name, setName] = useState(settings.instance_name)
  const [mode, setMode] = useState(settings.ssh_host_key_mode_default)
  const dirty = name !== settings.instance_name || mode !== settings.ssh_host_key_mode_default

  return (
    <>
      <div style={SECTION}>Общие</div>
      <div style={{ ...PANEL, padding: '18px 20px' }}>
        <label style={labelStyle} htmlFor="instance-name">Имя инстанса</label>
        <input
          id="instance-name"
          value={name}
          disabled={!canEdit}
          onChange={(e) => setName(e.target.value)}
          className="dc-input"
          style={{ ...inputStyle, marginBottom: 18, maxWidth: 360 }}
        />

        <label style={labelStyle} htmlFor="hostkey">Проверка SSH host key по умолчанию</label>
        <select
          id="hostkey"
          value={mode}
          disabled={!canEdit}
          onChange={(e) => setMode(e.target.value as SettingsData['ssh_host_key_mode_default'])}
          className="dc-input"
          style={{ ...inputStyle, maxWidth: 360 }}
        >
          <option value="tofu">tofu — принять ключ при первом подключении</option>
          <option value="strict">strict — только известные ключи</option>
        </select>
        <div style={{ fontSize: 11.5, color: 'var(--ink-3)', marginTop: 8, lineHeight: 1.5, maxWidth: 520 }}>
          Значение подставляется только в <b>новые</b> соединения. Уже настроенные
          хранят режим в своём конфиге и не меняются — иначе проверка у работающих
          подключений ослабла бы молча.
        </div>

        {canEdit && (
          <div style={{ marginTop: 18 }}>
            <button
              type="button"
              disabled={!dirty}
              style={{ ...btnPrimary, opacity: dirty ? 1 : 0.5, cursor: dirty ? 'pointer' : 'default' }}
              onClick={() => onSave({ instance_name: name, ssh_host_key_mode_default: mode })}
            >
              Сохранить
            </button>
          </div>
        )}
      </div>
    </>
  )
}

function UserModal({
  user,
  onClose,
  onSaved,
  onError,
}: {
  user: User | null
  onClose: () => void
  onSaved: (msg: string) => void
  onError: (e: unknown) => void
}) {
  const { api } = useAuth()
  const [email, setEmail] = useState(user?.email ?? '')
  const [name, setName] = useState(user?.name ?? '')
  const [role, setRole] = useState<Role>(user?.role ?? 'viewer')
  const [password, setPassword] = useState('')
  const [busy, setBusy] = useState(false)

  async function submit(e: FormEvent) {
    e.preventDefault()
    setBusy(true)
    try {
      if (user) {
        // An empty password field means "leave it alone" — sending it would
        // reset the password and kick every one of that user's sessions.
        await api.updateUser(user.id, { name, role, ...(password ? { password } : {}) })
        onSaved(`Пользователь ${user.email} обновлён`)
      } else {
        await api.createUser({ email, name, role, password })
        onSaved(`Пользователь ${email} создан`)
      }
    } catch (err) {
      onError(err)
    } finally {
      setBusy(false)
    }
  }

  return (
    <ModalShell title={user ? `Пользователь ${user.email}` : 'Новый пользователь'} onClose={onClose}>
      <form onSubmit={submit} style={{ padding: '20px 22px' }}>
        {!user && (
          <>
            <label style={labelStyle} htmlFor="u-email">Email</label>
            <input id="u-email" type="email" value={email} onChange={(e) => setEmail(e.target.value)} className="dc-input" style={{ ...inputStyle, marginBottom: 16 }} />
          </>
        )}

        <label style={labelStyle} htmlFor="u-name">Имя</label>
        <input id="u-name" value={name} onChange={(e) => setName(e.target.value)} className="dc-input" style={{ ...inputStyle, marginBottom: 16 }} />

        <label style={labelStyle} htmlFor="u-role">Роль</label>
        <select id="u-role" value={role} onChange={(e) => setRole(e.target.value as Role)} className="dc-input" style={{ ...inputStyle, marginBottom: 6 }}>
          {(['viewer', 'operator', 'admin'] as Role[]).map((r) => (
            <option key={r} value={r}>{ROLE_LABEL[r]}</option>
          ))}
        </select>
        <div style={{ fontSize: 11.5, color: 'var(--ink-3)', marginBottom: 16 }}>{ROLE_HINT[role]}</div>

        <label style={labelStyle} htmlFor="u-pass">{user ? 'Новый пароль (необязательно)' : 'Пароль'}</label>
        <input id="u-pass" type="password" autoComplete="new-password" value={password} onChange={(e) => setPassword(e.target.value)} className="dc-input" style={{ ...inputStyle, marginBottom: 6 }} />
        <div style={{ fontSize: 11.5, color: 'var(--ink-3)', marginBottom: 20 }}>
          {user
            ? 'Смена пароля администратором завершает все сессии этого пользователя.'
            : 'Не короче 10 символов.'}
        </div>

        <div style={{ display: 'flex', gap: 8, justifyContent: 'flex-end' }}>
          <button type="button" style={btnSecondary} onClick={onClose}>Отмена</button>
          <button type="submit" disabled={busy} style={{ ...btnPrimary, opacity: busy ? 0.6 : 1 }}>
            {busy ? 'Сохранение…' : 'Сохранить'}
          </button>
        </div>
      </form>
    </ModalShell>
  )
}

function PasswordModal({ onClose, onSaved }: { onClose: () => void; onSaved: () => void }) {
  const { api } = useAuth()
  const [oldPassword, setOld] = useState('')
  const [newPassword, setNew] = useState('')
  const [repeat, setRepeat] = useState('')
  const [err, setErr] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)

  async function submit(e: FormEvent) {
    e.preventDefault()
    if (newPassword !== repeat) {
      setErr('Пароли не совпадают')
      return
    }
    setBusy(true)
    setErr(null)
    try {
      await api.changeOwnPassword(oldPassword, newPassword)
      onSaved()
    } catch (e2) {
      setErr(e2 instanceof Error ? e2.message : String(e2))
    } finally {
      setBusy(false)
    }
  }

  return (
    <ModalShell title="Смена пароля" onClose={onClose}>
      <form onSubmit={submit} style={{ padding: '20px 22px' }}>
        <label style={labelStyle} htmlFor="p-old">Текущий пароль</label>
        <input id="p-old" type="password" autoComplete="current-password" value={oldPassword} onChange={(e) => setOld(e.target.value)} className="dc-input" style={{ ...inputStyle, marginBottom: 16 }} />

        <label style={labelStyle} htmlFor="p-new">Новый пароль</label>
        <input id="p-new" type="password" autoComplete="new-password" value={newPassword} onChange={(e) => setNew(e.target.value)} className="dc-input" style={{ ...inputStyle, marginBottom: 16 }} />

        <label style={labelStyle} htmlFor="p-rep">Повторите новый пароль</label>
        <input id="p-rep" type="password" autoComplete="new-password" value={repeat} onChange={(e) => setRepeat(e.target.value)} className="dc-input" style={{ ...inputStyle, marginBottom: 16 }} />

        {err && <div role="alert" style={{ color: 'var(--err)', fontSize: 12.5, marginBottom: 16 }}>{err}</div>}

        <div style={{ display: 'flex', gap: 8, justifyContent: 'flex-end' }}>
          <button type="button" style={btnSecondary} onClick={onClose}>Отмена</button>
          <button type="submit" disabled={busy} style={{ ...btnPrimary, opacity: busy ? 0.6 : 1 }}>
            {busy ? 'Сохранение…' : 'Сменить пароль'}
          </button>
        </div>
      </form>
    </ModalShell>
  )
}
