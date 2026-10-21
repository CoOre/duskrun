import { CSSProperties, FormEvent, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { ApiError } from '../api/client'
import { isMFAChallenge, type LoginMFAChallenge, type LoginResp, type MFAMethod } from '../api/types'
import { useAuth } from '../auth'
import { getPasskeyAssertion, webauthnErrorMessage, webauthnSupported } from '../webauthn'
import { BrandMark, useTheme } from '../ui/Layout'
import { MONO, btnPrimary, inputStyle, labelStyle } from '../ui/dc'

type Mode = 'password' | 'token'

// Below this many unused recovery codes the user is told to mint new ones
// before they find themselves locked out with none left.
const LOW_RECOVERY_CODES = 3

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
  // Second step: set once the password checked out on an account with 2FA.
  const [challenge, setChallenge] = useState<LoginMFAChallenge | null>(null)
  const [mfaMethod, setMfaMethod] = useState<MFAMethod>('totp')
  const [code, setCode] = useState('')
  // A recovery-code login that left few codes: the session is held back until
  // the user has read the warning, because storing the token flips the route
  // away from this page at once.
  const [lowCodes, setLowCodes] = useState<LoginResp | null>(null)

  function finish(resp: LoginResp) {
    if (resp.recovery_codes_left !== undefined && resp.recovery_codes_left <= LOW_RECOVERY_CODES) {
      setLowCodes(resp)
      return
    }
    login(resp.token)
    navigate('/', { replace: true })
  }

  function backToPassword(msg: string | null) {
    setChallenge(null)
    setCode('')
    setPassword('')
    setError(msg)
  }

  function mfaError(err: unknown, wrong: string) {
    if (err instanceof ApiError && err.status === 401) {
      setError(wrong)
    } else if (err instanceof ApiError && err.status === 410) {
      // The challenge is single-use and short-lived; the only way forward is
      // to start again from the password.
      backToPassword('Сессия входа истекла, войдите снова')
    } else if (err instanceof ApiError && err.status === 429) {
      setError('Слишком много попыток — повторите через несколько минут')
    } else if (err instanceof ApiError && err.status === 503) {
      setError('Сервер занят, попробуйте ещё раз')
    } else {
      setError('Не удалось подключиться к серверу')
    }
  }

  async function submitCode() {
    if (!challenge) return
    const c = code.trim()
    if (!c) {
      setError(mfaMethod === 'recovery' ? 'Введите код восстановления' : 'Введите код из приложения')
      return
    }
    setBusy(true)
    setError(null)
    try {
      finish(await api.loginMFA({ mfa_token: challenge.mfa_token, method: mfaMethod, code: c }))
    } catch (err) {
      mfaError(err, mfaMethod === 'recovery' ? 'Неверный или уже использованный код восстановления' : 'Неверный код')
    } finally {
      setBusy(false)
    }
  }

  async function submitPasskey() {
    if (!challenge) return
    setBusy(true)
    setError(null)
    try {
      const options = await api.loginWebAuthnOptions(challenge.mfa_token)
      let credential: Record<string, unknown>
      try {
        credential = await getPasskeyAssertion(options)
      } catch (err) {
        setError(webauthnErrorMessage(err))
        return
      }
      finish(await api.loginMFA({ mfa_token: challenge.mfa_token, method: 'webauthn', credential }))
    } catch (err) {
      mfaError(err, 'Passkey не принят')
    } finally {
      setBusy(false)
    }
  }

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
      if (isMFAChallenge(resp)) {
        setChallenge(resp)
        const m = resp.mfa_methods
        setMfaMethod(m.includes('totp') ? 'totp' : m.includes('webauthn') && webauthnSupported() ? 'webauthn' : 'recovery')
        setCode('')
        return
      }
      finish(resp)
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
    if (lowCodes) {
      login(lowCodes.token)
      navigate('/settings', { replace: true })
      return
    }
    if (challenge) {
      await (mfaMethod === 'webauthn' ? submitPasskey() : submitCode())
      return
    }
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
          {lowCodes ? (
            <LowCodesNotice left={lowCodes.recovery_codes_left ?? 0} />
          ) : challenge ? (
            <MFAStep
              challenge={challenge}
              method={mfaMethod}
              code={code}
              busy={busy}
              error={error}
              field={field}
              onCode={setCode}
              onMethod={(m) => { setMfaMethod(m); setCode(''); setError(null) }}
              onBack={() => backToPassword(null)}
            />
          ) : (
            <>
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
            </>
          )}
        </form>

        <div style={{ textAlign: 'center', fontSize: 11, color: 'var(--ink-3)', marginTop: 18, fontFamily: MONO }}>
          локальные пользователи / API-токен · задел на OIDC
        </div>
      </div>
    </div>
  )
}

// MFAStep is the second screen of a login: a code from the authenticator app,
// a passkey, or one of the recovery codes. Which of them are offered comes from
// the server, which knows what the account has enrolled.
function MFAStep({
  challenge,
  method,
  code,
  busy,
  error,
  field,
  onCode,
  onMethod,
  onBack,
}: {
  challenge: LoginMFAChallenge
  method: MFAMethod
  code: string
  busy: boolean
  error: string | null
  field: CSSProperties
  onCode: (v: string) => void
  onMethod: (m: MFAMethod) => void
  onBack: () => void
}) {
  const methods = challenge.mfa_methods
  const hasTotp = methods.includes('totp')
  const hasPasskey = methods.includes('webauthn') && webauthnSupported()
  const hasRecovery = methods.includes('recovery')
  const link: CSSProperties = { background: 'none', border: 'none', padding: 0, color: 'var(--accent)', fontSize: 12.5, cursor: 'pointer', fontFamily: "'Inter', sans-serif" }
  const secondary: CSSProperties = { width: '100%', padding: 11, background: 'var(--panel-2)', color: 'var(--ink)', border: '1px solid var(--line-2)', borderRadius: 8, fontSize: 13.5, fontWeight: 600, cursor: 'pointer', fontFamily: "'Inter', sans-serif" }

  return (
    <>
      <div style={{ fontSize: 16, fontWeight: 700, color: 'var(--ink)', marginBottom: 3 }}>Подтверждение входа</div>
      <div style={{ fontSize: 12.5, color: 'var(--ink-2)', marginBottom: 22, lineHeight: 1.5 }}>
        {method === 'recovery'
          ? 'Введите один из сохранённых кодов восстановления. Каждый код срабатывает один раз.'
          : method === 'webauthn'
            ? 'Подтвердите вход ключом безопасности или passkey.'
            : 'Введите 6-значный код из приложения-аутентификатора.'}
      </div>

      {method === 'totp' && (
        <>
          <label style={labelStyle} htmlFor="mfa-code">Код из приложения</label>
          <input id="mfa-code" value={code} autoFocus autoComplete="one-time-code" inputMode="numeric" pattern="[0-9 ]*" maxLength={7} placeholder="123456" onChange={(e) => onCode(e.target.value)} className="dc-input" style={{ ...field, fontFamily: MONO, letterSpacing: '0.2em', marginBottom: 22 }} />
        </>
      )}
      {method === 'recovery' && (
        <>
          <label style={labelStyle} htmlFor="mfa-recovery">Код восстановления</label>
          <input id="mfa-recovery" value={code} autoFocus autoComplete="off" spellCheck={false} placeholder="xxxx-xxxx-xxxx" onChange={(e) => onCode(e.target.value)} className="dc-input" style={{ ...field, fontFamily: MONO, marginBottom: 22 }} />
        </>
      )}

      {error && (
        <div role="alert" style={{ color: 'var(--err)', fontSize: 12.5, marginBottom: 16 }}>{error}</div>
      )}

      <button type="submit" disabled={busy} style={{ ...btnPrimary, width: '100%', justifyContent: 'center', padding: 11, borderRadius: 8, fontSize: 13.5, opacity: busy ? 0.6 : 1 }}>
        {busy ? 'Проверка…' : method === 'webauthn' ? 'Войти с passkey' : 'Подтвердить'}
      </button>

      {method !== 'webauthn' && hasPasskey && (
        <button type="button" disabled={busy} onClick={() => onMethod('webauthn')} style={{ ...secondary, marginTop: 10 }}>
          Использовать passkey
        </button>
      )}

      <div style={{ display: 'flex', flexDirection: 'column', alignItems: 'center', gap: 10, marginTop: 18 }}>
        {method !== 'totp' && hasTotp && (
          <button type="button" onClick={() => onMethod('totp')} style={link}>Ввести код из приложения</button>
        )}
        {method !== 'recovery' && hasRecovery && (
          <button type="button" onClick={() => onMethod('recovery')} style={link}>Использовать код восстановления</button>
        )}
        <button type="button" onClick={onBack} style={{ ...link, color: 'var(--ink-3)' }}>← Назад</button>
      </div>
    </>
  )
}

function LowCodesNotice({ left }: { left: number }) {
  return (
    <>
      <div style={{ fontSize: 16, fontWeight: 700, color: 'var(--ink)', marginBottom: 10 }}>Вход выполнен</div>
      <div role="status" style={{ fontSize: 12.5, color: 'var(--warn)', lineHeight: 1.6, marginBottom: 22 }}>
        {left === 0
          ? 'Это был последний код восстановления.'
          : `Осталось кодов восстановления: ${left}.`}{' '}
        Сгенерируйте новые в настройках, пока есть доступ, — иначе при потере
        второго фактора вернуть вход сможет только администратор.
      </div>
      <button type="submit" style={{ ...btnPrimary, width: '100%', justifyContent: 'center', padding: 11, borderRadius: 8, fontSize: 13.5 }}>
        Перейти в настройки
      </button>
    </>
  )
}
