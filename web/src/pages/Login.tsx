import { CSSProperties, FormEvent, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { ApiError } from '../api/client'
import { useAuth } from '../auth'
import { BrandMark, useTheme } from '../ui/Layout'
import { MONO, btnPrimary, inputStyle, labelStyle } from '../ui/dc'

type Mode = 'password' | 'token'

// Login offers both authentication paths from TZ §3.
//
// Email + password is the normal one. The static DUSKRUN_API_TOKEN stays as a
// second tab because it is the only way into an instance that has no users yet
// — dropping it would lock an operator out of their own first start.
export default function Login() {
  const { login, logout, api } = useAuth()
  const navigate = useNavigate()
  const [theme, setTheme] = useTheme()
  const [mode, setMode] = useState<Mode>('password')
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const [token, setToken] = useState('')
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)

  async function submitPassword() {
    const addr = email.trim()
    if (!addr || !password) {
      setError('Введите email и пароль')
      return
    }
    setBusy(true)
    setError(null)
    try {
      const resp = await api.login(addr, password)
      login(resp.token)
      navigate('/', { replace: true })
    } catch (err) {
      if (err instanceof ApiError && err.status === 401) {
        // Deliberately the server's own wording: it does not distinguish a
        // wrong password from an unknown address, and neither should this.
        setError('Неверный email или пароль')
      } else if (err instanceof ApiError && err.status === 429) {
        setError('Слишком много попыток — повторите через несколько минут')
      } else if (err instanceof ApiError && err.status === 503) {
        setError('Сервер занят, попробуйте ещё раз')
      } else {
        setError('Не удалось подключиться к серверу')
      }
    } finally {
      setBusy(false)
    }
  }

  async function submitToken() {
    const t = token.trim()
    if (!t) {
      setError('Введите токен')
      return
    }
    setBusy(true)
    setError(null)
    login(t)
    try {
      await api.ping()
      navigate('/', { replace: true })
    } catch (err) {
      logout()
      if (err instanceof ApiError && err.status === 401) {
        setError('Неверный токен — проверьте DUSKRUN_API_TOKEN')
      } else {
        setError('Не удалось подключиться к серверу')
      }
    } finally {
      setBusy(false)
    }
  }

  async function onSubmit(e: FormEvent) {
    e.preventDefault()
    await (mode === 'password' ? submitPassword() : submitToken())
  }

  const field: CSSProperties = { ...inputStyle, padding: '10px 12px', background: 'var(--panel-2)', fontSize: 13, marginBottom: 16 }

  return (
    <div style={{ flex: 1, display: 'flex', flexDirection: 'column', alignItems: 'center', justifyContent: 'center', background: 'var(--app)', position: 'relative', padding: 24, minHeight: '100vh' }}>
      <div style={{ position: 'absolute', top: 20, right: 24, display: 'flex', padding: 2, background: 'var(--panel)', border: '1px solid var(--line)', borderRadius: 7, gap: 2 }}>
        {([['light', 'Светлая тема', '☀'], ['dark', 'Тёмная тема', '☾']] as const).map(([k, label, glyph]) => {
          const active = theme === k
          return (
            <button key={k} type="button" title={label} aria-label={label} onClick={() => setTheme(k)} className="dc-reset" style={{ width: 28, height: 24, display: 'flex', alignItems: 'center', justifyContent: 'center', borderRadius: 5, cursor: 'pointer', background: active ? 'var(--accent)' : 'transparent', color: active ? 'var(--accent-ink)' : 'var(--ink-2)' }}>
              <span style={{ fontSize: 12 }}>{glyph}</span>
            </button>
          )
        })}
      </div>

      <div style={{ width: 364, maxWidth: '100%' }}>
        <div style={{ display: 'flex', alignItems: 'center', gap: 12, justifyContent: 'center', marginBottom: 26 }}>
          <BrandMark size={34} />
          <div style={{ fontFamily: MONO, fontSize: 22, fontWeight: 700, letterSpacing: '-0.03em', color: 'var(--ink)' }}>Duskrun</div>
        </div>

        <form onSubmit={onSubmit} style={{ border: '1px solid var(--line)', borderRadius: 13, background: 'var(--panel)', padding: 26 }}>
          <div style={{ fontSize: 16, fontWeight: 700, color: 'var(--ink)', marginBottom: 3 }}>Вход в систему</div>
          <div style={{ fontSize: 12.5, color: 'var(--ink-2)', marginBottom: 22, fontFamily: MONO }}>self-hosted · node-01</div>

          <div role="tablist" aria-label="Способ входа" style={{ display: 'flex', gap: 2, padding: 2, background: 'var(--panel-2)', border: '1px solid var(--line)', borderRadius: 8, marginBottom: 18 }}>
            {([['password', 'Email и пароль'], ['token', 'API-токен']] as const).map(([k, label]) => {
              const active = mode === k
              return (
                <button
                  key={k}
                  type="button"
                  role="tab"
                  aria-selected={active}
                  onClick={() => { setMode(k); setError(null) }}
                  className="dc-reset"
                  style={{ flex: 1, padding: '7px 0', borderRadius: 6, cursor: 'pointer', fontSize: 12.5, fontWeight: 600, background: active ? 'var(--accent)' : 'transparent', color: active ? 'var(--accent-ink)' : 'var(--ink-2)' }}
                >
                  {label}
                </button>
              )
            })}
          </div>

          {mode === 'password' ? (
            <>
              <label style={labelStyle} htmlFor="email">Email</label>
              <input id="email" type="email" value={email} autoComplete="username" placeholder="you@example.com" onChange={(e) => setEmail(e.target.value)} className="dc-input" style={field} />

              <label style={labelStyle} htmlFor="password">Пароль</label>
              <input id="password" type="password" value={password} autoComplete="current-password" onChange={(e) => setPassword(e.target.value)} className="dc-input" style={{ ...field, marginBottom: 22 }} />
            </>
          ) : (
            <>
              <label style={labelStyle} htmlFor="token">API-токен</label>
              <input id="token" type="password" value={token} autoComplete="off" placeholder="DUSKRUN_API_TOKEN" onChange={(e) => setToken(e.target.value)} className="dc-input" style={field} />
              <div style={{ fontSize: 11.5, color: 'var(--ink-3)', marginBottom: 22, lineHeight: 1.5 }}>
                Общий токен инстанса с правами администратора. Нужен на первом запуске,
                пока не заведён первый пользователь.
              </div>
            </>
          )}

          {error && (
            <div role="alert" style={{ color: 'var(--err)', fontSize: 12.5, marginBottom: 16 }}>{error}</div>
          )}

          <button type="submit" disabled={busy} style={{ ...btnPrimary, width: '100%', justifyContent: 'center', padding: 11, borderRadius: 8, fontSize: 13.5, opacity: busy ? 0.6 : 1 }}>
            {busy ? 'Проверка…' : 'Войти'}
          </button>

          <div style={{ display: 'flex', alignItems: 'center', gap: 12, margin: '18px 0' }}>
            <div style={{ flex: 1, height: 1, background: 'var(--line)' }} />
            <span style={{ fontSize: 11, color: 'var(--ink-3)' }}>или</span>
            <div style={{ flex: 1, height: 1, background: 'var(--line)' }} />
          </div>

          <button type="button" disabled title="Пока недоступно" style={{ width: '100%', padding: 11, background: 'var(--panel-2)', color: 'var(--ink)', border: '1px solid var(--line-2)', borderRadius: 8, fontSize: 13.5, fontWeight: 600, cursor: 'not-allowed', fontFamily: "'Inter', sans-serif", opacity: 0.7 }}>
            Войти через OIDC
          </button>
        </form>

        <div style={{ textAlign: 'center', fontSize: 11, color: 'var(--ink-3)', marginTop: 18, fontFamily: MONO }}>
          локальные пользователи / API-токен · задел на OIDC
        </div>
      </div>
    </div>
  )
}
