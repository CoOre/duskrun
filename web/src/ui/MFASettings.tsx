import { CSSProperties, FormEvent, ReactNode, useState } from 'react'
import { ApiError } from '../api/client'
import { useApiData } from '../api/useApiData'
import { fmtDateTime } from '../api/view'
import type { MFAStatus, Passkey, TOTPSetup } from '../api/types'
import { useAuth } from '../auth'
import { createPasskey, webauthnErrorMessage, webauthnSupported } from '../webauthn'
import { ModalShell } from './ModalShell'
import { MONO, btnDanger, btnPrimary, btnSecondary, inputStyle, labelStyle } from './dc'

const PANEL: CSSProperties = { border: '1px solid var(--line)', borderRadius: 10, background: 'var(--panel)', marginBottom: 26 }
const ROW: CSSProperties = { display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: 16, padding: '14px 20px', borderBottom: '1px solid var(--line)' }
const SMALL_BTN: CSSProperties = { ...btnSecondary, padding: '5px 10px', fontSize: 11.5 }
const HINT: CSSProperties = { fontSize: 11.5, color: 'var(--ink-3)', lineHeight: 1.5 }

type Modal =
  | { kind: 'totp' }
  | { kind: 'totp-disable' }
  | { kind: 'passkey-add' }
  | { kind: 'passkey-delete'; passkey: Passkey }
  | { kind: 'codes-regen' }
  | { kind: 'codes'; codes: string[] }

function message(e: unknown): string {
  if (e instanceof ApiError && e.status === 403 && /password/i.test(e.message)) return 'Неверный пароль'
  return e instanceof Error ? e.message : String(e)
}

// MFASettings is the caller's own second-factor panel: authenticator app,
// passkeys and recovery codes. It is used on the settings page and, alone, on
// the forced-enrolment screen — onChange fires after a flow has fully finished
// (recovery codes acknowledged included), so the enrolment screen can re-read
// /me and step aside without cutting the codes off mid-display.
export function MFASettings({ onChange }: { onChange?: () => void }) {
  const { api } = useAuth()
  const [reload, setReload] = useState(0)
  const [modal, setModal] = useState<Modal | null>(null)
  const [banner, setBanner] = useState<string | null>(null)
  const { data, error, loading } = useApiData<MFAStatus>(() => api.getMFA(), [api, reload])

  // done closes a flow: refresh our own status, then tell the parent.
  function done(msg: string | null, codes?: string[]) {
    setBanner(msg)
    setReload((n) => n + 1)
    if (codes && codes.length > 0) {
      setModal({ kind: 'codes', codes })
      return
    }
    setModal(null)
    onChange?.()
  }

  if (loading) return <div style={{ ...PANEL, padding: 20, fontSize: 12.5, color: 'var(--ink-3)' }}>Загрузка…</div>
  if (error || !data) {
    return (
      <div style={{ ...PANEL, padding: 20, fontSize: 12.5, color: 'var(--err)' }}>
        Не удалось загрузить настройки 2FA: {error}
      </div>
    )
  }

  const browserPasskeys = webauthnSupported()
  const canAddPasskey = data.webauthn_available && browserPasskeys
  const factors = (data.totp ? 1 : 0) + data.passkeys.length
  // The last factor cannot go while the policy covers this account — the server
  // refuses with 409 anyway; the UI just says so up front.
  const lastLocked = data.required && factors <= 1

  return (
    <div>
      {banner && <div role="status" style={{ color: 'var(--ok)', fontSize: 12.5, marginBottom: 12 }}>{banner}</div>}
      {data.required && factors === 0 && (
        <div style={{ fontSize: 12.5, color: 'var(--warn)', marginBottom: 12 }}>
          Политика инстанса требует второй фактор для этой учётной записи.
        </div>
      )}

      <div style={PANEL}>
        <div style={ROW}>
          <div style={{ minWidth: 0 }}>
            <div style={{ fontSize: 12.5, color: 'var(--ink)', fontWeight: 500 }}>
              Приложение-аутентификатор
              <StateTag on={data.totp} />
            </div>
            <div style={HINT}>Одноразовые 6-значные коды: Google Authenticator, Aegis, 1Password и т. п.</div>
          </div>
          <div style={{ display: 'flex', gap: 6, flexShrink: 0 }}>
            <button type="button" style={data.totp ? SMALL_BTN : { ...btnPrimary, padding: '5px 12px', fontSize: 11.5 }} onClick={() => setModal({ kind: 'totp' })}>
              {data.totp ? 'Заменить устройство' : 'Настроить'}
            </button>
            {data.totp && (
              <button
                type="button"
                style={{ ...SMALL_BTN, opacity: lastLocked ? 0.5 : 1 }}
                disabled={lastLocked}
                title={lastLocked ? 'Политика требует 2FA — это последний фактор' : undefined}
                onClick={() => setModal({ kind: 'totp-disable' })}
              >
                Отключить
              </button>
            )}
          </div>
        </div>

        <div style={{ ...ROW, alignItems: 'flex-start', flexDirection: 'column', gap: 10 }}>
          <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', width: '100%', gap: 16 }}>
            <div style={{ minWidth: 0 }}>
              <div style={{ fontSize: 12.5, color: 'var(--ink)', fontWeight: 500 }}>
                Passkeys и ключи безопасности
                <StateTag on={data.passkeys.length > 0} label={data.passkeys.length > 0 ? String(data.passkeys.length) : undefined} />
              </div>
              <div style={HINT}>
                {!data.webauthn_available
                  ? 'Задайте DUSKRUN_PUBLIC_URL (HTTPS) для passkeys.'
                  : !browserPasskeys
                    ? 'Браузер не поддерживает passkeys на этой странице (нужен HTTPS).'
                    : 'Touch ID, Windows Hello, YubiKey или passkey в менеджере паролей.'}
              </div>
            </div>
            <button
              type="button"
              style={{ ...SMALL_BTN, flexShrink: 0, opacity: canAddPasskey ? 1 : 0.5 }}
              disabled={!canAddPasskey}
              onClick={() => setModal({ kind: 'passkey-add' })}
            >
              Добавить passkey
            </button>
          </div>
          {data.passkeys.length > 0 && (
            <div style={{ width: '100%' }}>
              {data.passkeys.map((p) => (
                <div key={p.id} style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: 12, padding: '8px 0', borderTop: '1px dashed var(--line)' }}>
                  <div style={{ minWidth: 0 }}>
                    <div style={{ fontSize: 12.5, color: 'var(--ink)' }}>{p.name || 'без имени'}</div>
                    <div style={{ fontSize: 11, color: 'var(--ink-3)', fontFamily: MONO }}>
                      добавлен {fmtDateTime(p.created_at)} · {p.last_used_at ? `использован ${fmtDateTime(p.last_used_at)}` : 'ещё не использовался'}
                    </div>
                  </div>
                  <button
                    type="button"
                    aria-label={`Удалить passkey ${p.name}`}
                    style={{ ...btnDanger, padding: '4px 10px', fontSize: 11.5, opacity: lastLocked ? 0.5 : 1 }}
                    disabled={lastLocked}
                    title={lastLocked ? 'Политика требует 2FA — это последний фактор' : undefined}
                    onClick={() => setModal({ kind: 'passkey-delete', passkey: p })}
                  >
                    Удалить
                  </button>
                </div>
              ))}
            </div>
          )}
        </div>

        {factors > 0 && (
          <div style={{ ...ROW, borderBottom: 'none' }}>
            <div style={{ minWidth: 0 }}>
              <div style={{ fontSize: 12.5, color: 'var(--ink)', fontWeight: 500 }}>Коды восстановления</div>
              <div style={{ ...HINT, color: data.recovery_codes_left <= 3 ? 'var(--warn)' : HINT.color }}>
                Осталось неиспользованных: {data.recovery_codes_left}. Каждый код входит один раз вместо второго фактора.
              </div>
            </div>
            <button type="button" style={{ ...SMALL_BTN, flexShrink: 0 }} onClick={() => setModal({ kind: 'codes-regen' })}>
              Сгенерировать новые
            </button>
          </div>
        )}
      </div>

      {modal?.kind === 'totp' && (
        <TOTPModal
          replacing={data.totp}
          onClose={() => setModal(null)}
          onDone={(codes) => done(data.totp ? 'Устройство заменено' : 'Приложение-аутентификатор подключено', codes)}
        />
      )}
      {modal?.kind === 'totp-disable' && (
        <PasswordConfirmModal
          title="Отключить приложение-аутентификатор"
          text={factors > 1 ? 'Вход останется защищён passkey.' : 'Это последний второй фактор: вход снова будет только по паролю, коды восстановления будут удалены.'}
          confirm="Отключить"
          danger
          onClose={() => setModal(null)}
          onConfirm={async (pw) => {
            await api.disableTOTP(pw)
            done('Приложение-аутентификатор отключено')
          }}
        />
      )}
      {modal?.kind === 'passkey-add' && (
        <PasskeyAddModal onClose={() => setModal(null)} onDone={(codes) => done('Passkey добавлен', codes)} />
      )}
      {modal?.kind === 'passkey-delete' && (
        <PasswordConfirmModal
          title={`Удалить passkey «${modal.passkey.name || 'без имени'}»`}
          text={factors > 1 ? 'Ключ больше не будет приниматься при входе.' : 'Это последний второй фактор: вход снова будет только по паролю, коды восстановления будут удалены.'}
          confirm="Удалить"
          danger
          onClose={() => setModal(null)}
          onConfirm={async (pw) => {
            await api.deletePasskey(modal.passkey.id, pw)
            done('Passkey удалён')
          }}
        />
      )}
      {modal?.kind === 'codes-regen' && (
        <PasswordConfirmModal
          title="Новые коды восстановления"
          text="Все прежние коды перестанут работать."
          confirm="Сгенерировать"
          onClose={() => setModal(null)}
          onConfirm={async (pw) => {
            const r = await api.regenerateRecoveryCodes(pw)
            done('Коды восстановления обновлены', r.recovery_codes)
          }}
        />
      )}
      {modal?.kind === 'codes' && (
        <ModalShell title="Коды восстановления" onClose={() => undefined}>
          <div style={{ padding: '20px 22px' }}>
            <RecoveryCodes codes={modal.codes} onDone={() => { setModal(null); onChange?.() }} />
          </div>
        </ModalShell>
      )}
    </div>
  )
}

function StateTag({ on, label }: { on: boolean; label?: string }) {
  return (
    <span style={{ marginLeft: 8, fontSize: 10.5, fontFamily: MONO, color: on ? 'var(--ok)' : 'var(--ink-3)', border: `1px solid ${on ? 'var(--ok)' : 'var(--line-2)'}`, borderRadius: 5, padding: '1px 6px' }}>
      {on ? label ?? 'включено' : 'не настроено'}
    </span>
  )
}

function ModalError({ err }: { err: string | null }) {
  if (!err) return null
  return <div role="alert" style={{ color: 'var(--err)', fontSize: 12.5, marginBottom: 16 }}>{err}</div>
}

function Footer({ onClose, busy, label, danger, disabled }: { onClose: () => void; busy: boolean; label: string; danger?: boolean; disabled?: boolean }) {
  const off = busy || disabled
  return (
    <div style={{ display: 'flex', gap: 8, justifyContent: 'flex-end' }}>
      <button type="button" style={btnSecondary} onClick={onClose}>Отмена</button>
      <button type="submit" disabled={off} style={{ ...(danger ? btnDanger : btnPrimary), opacity: off ? 0.6 : 1 }}>
        {busy ? 'Подождите…' : label}
      </button>
    </div>
  )
}

function PasswordField({ value, onChange, id = 'mfa-pw' }: { value: string; onChange: (v: string) => void; id?: string }) {
  return (
    <>
      <label style={labelStyle} htmlFor={id}>Текущий пароль</label>
      <input id={id} type="password" autoFocus autoComplete="current-password" value={value} onChange={(e) => onChange(e.target.value)} className="dc-input" style={{ ...inputStyle, marginBottom: 16 }} />
    </>
  )
}

// PasswordConfirmModal asks for the account password before a change to the
// second factors. The server demands it on every such route.
function PasswordConfirmModal({
  title,
  text,
  confirm,
  danger,
  onClose,
  onConfirm,
}: {
  title: string
  text: ReactNode
  confirm: string
  danger?: boolean
  onClose: () => void
  onConfirm: (password: string) => Promise<void>
}) {
  const [pw, setPw] = useState('')
  const [err, setErr] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)

  async function submit(e: FormEvent) {
    e.preventDefault()
    if (!pw) {
      setErr('Введите пароль')
      return
    }
    setBusy(true)
    setErr(null)
    try {
      await onConfirm(pw)
    } catch (e2) {
      setErr(message(e2))
      setBusy(false)
    }
  }

  return (
    <ModalShell title={title} onClose={onClose}>
      <form onSubmit={submit} style={{ padding: '20px 22px' }}>
        <div style={{ fontSize: 12.5, color: 'var(--ink-2)', marginBottom: 16, lineHeight: 1.5 }}>{text}</div>
        <PasswordField value={pw} onChange={setPw} />
        <ModalError err={err} />
        <Footer onClose={onClose} busy={busy} label={confirm} danger={danger} />
      </form>
    </ModalShell>
  )
}

// TOTPModal walks through enrolment: password → scan the QR → prove it with a
// code. The secret is only bound to the account once the code checks out, so
// abandoning the dialog half-way changes nothing.
function TOTPModal({ replacing, onClose, onDone }: { replacing: boolean; onClose: () => void; onDone: (codes?: string[]) => void }) {
  const { api } = useAuth()
  const [pw, setPw] = useState('')
  const [setup, setSetup] = useState<TOTPSetup | null>(null)
  const [code, setCode] = useState('')
  const [err, setErr] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)

  async function submit(e: FormEvent) {
    e.preventDefault()
    setErr(null)
    if (!setup) {
      if (!pw) {
        setErr('Введите пароль')
        return
      }
      setBusy(true)
      try {
        setSetup(await api.setupTOTP(pw))
      } catch (e2) {
        setErr(message(e2))
      } finally {
        setBusy(false)
      }
      return
    }
    const c = code.replace(/\s+/g, '')
    if (!/^\d{6}$/.test(c)) {
      setErr('Код — 6 цифр из приложения')
      return
    }
    setBusy(true)
    try {
      const r = await api.confirmTOTP(c)
      onDone(r.recovery_codes)
    } catch (e2) {
      setErr(e2 instanceof ApiError && e2.status === 400 ? 'Код не подошёл — проверьте время на телефоне и попробуйте следующий' : message(e2))
      setBusy(false)
    }
  }

  return (
    <ModalShell title={replacing ? 'Замена устройства' : 'Приложение-аутентификатор'} onClose={onClose}>
      <form onSubmit={submit} style={{ padding: '20px 22px' }}>
        {!setup ? (
          <>
            <div style={{ fontSize: 12.5, color: 'var(--ink-2)', marginBottom: 16, lineHeight: 1.5 }}>
              {replacing
                ? 'Прежнее устройство перестанет работать, как только новое подтвердит код.'
                : 'Понадобится приложение-аутентификатор на телефоне или в менеджере паролей.'}
            </div>
            <PasswordField value={pw} onChange={setPw} />
          </>
        ) : (
          <>
            <div style={{ fontSize: 12.5, color: 'var(--ink-2)', marginBottom: 14, lineHeight: 1.5 }}>
              Отсканируйте QR-код в приложении или введите ключ вручную.
            </div>
            <div style={{ display: 'flex', justifyContent: 'center', marginBottom: 14 }}>
              <img src={setup.qr} alt="QR-код для приложения-аутентификатора" width={180} height={180} style={{ background: '#fff', padding: 8, borderRadius: 8 }} />
            </div>
            <div style={{ ...labelStyle }}>Ключ</div>
            <div data-testid="totp-secret" style={{ fontFamily: MONO, fontSize: 12.5, color: 'var(--ink)', background: 'var(--panel-2)', border: '1px solid var(--line)', borderRadius: 6, padding: '8px 10px', marginBottom: 16, overflowWrap: 'anywhere', userSelect: 'all' }}>
              {setup.secret.replace(/(.{4})/g, '$1 ').trim()}
            </div>
            <label style={labelStyle} htmlFor="totp-code">Код из приложения</label>
            <input id="totp-code" value={code} autoFocus autoComplete="one-time-code" inputMode="numeric" maxLength={7} placeholder="123456" onChange={(e) => setCode(e.target.value)} className="dc-input" style={{ ...inputStyle, fontFamily: MONO, letterSpacing: '0.2em', marginBottom: 16 }} />
          </>
        )}
        <ModalError err={err} />
        <Footer onClose={onClose} busy={busy} label={setup ? 'Подтвердить' : 'Далее'} />
      </form>
    </ModalShell>
  )
}

function PasskeyAddModal({ onClose, onDone }: { onClose: () => void; onDone: (codes?: string[]) => void }) {
  const { api } = useAuth()
  const [name, setName] = useState('')
  const [pw, setPw] = useState('')
  const [err, setErr] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)

  async function submit(e: FormEvent) {
    e.preventDefault()
    if (!name.trim() || !pw) {
      setErr('Введите название и пароль')
      return
    }
    setBusy(true)
    setErr(null)
    try {
      const options = await api.passkeyOptions(pw, name.trim())
      let credential: Record<string, unknown>
      try {
        credential = await createPasskey(options)
      } catch (e2) {
        setErr(webauthnErrorMessage(e2))
        setBusy(false)
        return
      }
      const r = await api.registerPasskey(credential)
      onDone(r.recovery_codes)
    } catch (e2) {
      setErr(message(e2))
      setBusy(false)
    }
  }

  return (
    <ModalShell title="Новый passkey" onClose={onClose}>
      <form onSubmit={submit} style={{ padding: '20px 22px' }}>
        <label style={labelStyle} htmlFor="pk-name">Название</label>
        <input id="pk-name" value={name} autoFocus maxLength={64} placeholder="YubiKey 5, MacBook…" onChange={(e) => setName(e.target.value)} className="dc-input" style={{ ...inputStyle, marginBottom: 16 }} />
        <PasswordField id="pk-pw" value={pw} onChange={setPw} />
        <div style={{ ...HINT, marginBottom: 16 }}>После «Добавить» браузер попросит коснуться ключа или подтвердить биометрией.</div>
        <ModalError err={err} />
        <Footer onClose={onClose} busy={busy} label="Добавить" />
      </form>
    </ModalShell>
  )
}

// RecoveryCodes shows freshly minted codes exactly once. The server keeps only
// their hashes, so the user must take them now — hence the explicit
// acknowledgement before the dialog can be dismissed.
export function RecoveryCodes({ codes, onDone }: { codes: string[]; onDone: () => void }) {
  const [saved, setSaved] = useState(false)
  const [copied, setCopied] = useState(false)
  const text = codes.join('\n') + '\n'

  async function copy() {
    try {
      await navigator.clipboard.writeText(text)
      setCopied(true)
    } catch {
      setCopied(false)
    }
  }

  function download() {
    const url = URL.createObjectURL(new Blob([text], { type: 'text/plain' }))
    const a = document.createElement('a')
    a.href = url
    a.download = 'duskrun-recovery-codes.txt'
    document.body.appendChild(a)
    a.click()
    a.remove()
    URL.revokeObjectURL(url)
  }

  return (
    <div>
      <div style={{ fontSize: 12.5, color: 'var(--ink-2)', marginBottom: 14, lineHeight: 1.5 }}>
        Сохраните коды в надёжном месте. Каждый срабатывает один раз вместо второго
        фактора. Больше они показаны не будут.
      </div>
      <div data-testid="recovery-codes" style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: '6px 18px', fontFamily: MONO, fontSize: 13, color: 'var(--ink)', background: 'var(--panel-2)', border: '1px solid var(--line)', borderRadius: 8, padding: '12px 16px', marginBottom: 12 }}>
        {codes.map((c) => <div key={c}>{c}</div>)}
      </div>
      <div style={{ display: 'flex', gap: 8, marginBottom: 16 }}>
        <button type="button" style={SMALL_BTN} onClick={copy}>{copied ? 'Скопировано' : 'Скопировать'}</button>
        <button type="button" style={SMALL_BTN} onClick={download}>Скачать .txt</button>
      </div>
      <label style={{ display: 'flex', alignItems: 'center', gap: 8, fontSize: 12.5, color: 'var(--ink)', marginBottom: 18, cursor: 'pointer' }}>
        <input type="checkbox" checked={saved} onChange={(e) => setSaved(e.target.checked)} />
        Я сохранил коды
      </label>
      <div style={{ display: 'flex', justifyContent: 'flex-end' }}>
        <button type="button" disabled={!saved} style={{ ...btnPrimary, opacity: saved ? 1 : 0.5 }} onClick={onDone}>Готово</button>
      </div>
    </div>
  )
}
