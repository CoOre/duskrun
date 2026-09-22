import { FormEvent, useEffect, useRef, useState } from 'react'
import { ApiError } from '../api/client'
import { useApiData } from '../api/useApiData'
import { fmtDateTime } from '../api/view'
import type { CreateNotifierReq, EventKind, Notification, NotifierChannel } from '../api/types'
import { useAuth, useCan } from '../auth'
import { Layout } from '../ui/Layout'
import { DeleteButton, MONO, OptionCard, Toggle, btnPrimary, btnSecondary, inputStyle, labelStyle } from '../ui/dc'
import { ModalShell } from '../ui/ModalShell'
import { SecretPicker } from '../ui/SecretPicker'

// Channel plugins registered in the backend (internal/notifier/*).
const CHANNEL_TYPES: { k: string; label: string; sub: string }[] = [
  { k: 'log', label: 'Log', sub: 'в журнал демона' },
  { k: 'telegram', label: 'Telegram', sub: 'Bot API' },
  { k: 'webhook', label: 'Webhook', sub: 'POST JSON' },
  { k: 'smtp', label: 'Email', sub: 'SMTP' },
]

// TLS modes the smtp plugin accepts. The mode is explicit rather than derived
// from the port: guessing wrong means either a failed send or a password on
// the wire.
const TLS_MODES: { k: string; label: string }[] = [
  { k: 'starttls', label: 'STARTTLS (обычно порт 587)' },
  { k: 'implicit', label: 'TLS сразу (обычно порт 465)' },
  { k: 'none', label: 'Без шифрования (порт 25)' },
]

// The event kinds a channel can subscribe to, in the order the matrix shows.
const EVENTS: { k: EventKind; label: string; dot: string }[] = [
  { k: 'success', label: 'Успешный бэкап', dot: 'var(--ok)' },
  { k: 'failure', label: 'Ошибка бэкапа', dot: 'var(--err)' },
  { k: 'retention_error', label: 'Ошибка retention', dot: 'var(--warn)' },
  { k: 'watchdog', label: 'Watchdog: нет свежего бэкапа', dot: 'var(--warn)' },
]

type Data = { channels: NotifierChannel[]; log: Notification[] }

export default function Notifications() {
  const { api, logout } = useAuth()
  const canAdmin = useCan('admin')
  const [reload, setReload] = useState(0)
  const [editing, setEditing] = useState<NotifierChannel | null>(null)
  const [creating, setCreating] = useState(false)
  const [tested, setTested] = useState<Record<number, string>>({})
  const [error, setError] = useState<string | null>(null)

  const { data, error: loadErr, loading } = useApiData<Data>(async () => {
    const [channels, log] = await Promise.all([api.listNotifiers(), api.listNotifications(20)])
    return { channels, log }
  }, [api, reload])

  // rows mirrors the loaded channels and carries the toggles the operator has
  // flipped since. Reading the toggles off the last completed fetch instead
  // would make a second click compute from a row that no longer reflects the
  // first one, quietly dropping the subscription it just turned on.
  const [rows, setRows] = useState<NotifierChannel[] | null>(null)
  useEffect(() => { setRows(data?.channels ?? null) }, [data])

  const channels = rows ?? []
  const log = data?.log ?? []

  // pending serialises channel writes and inflight defers the refetch until the
  // last of them lands: overlapping PATCHes would otherwise let the server apply
  // them in the wrong order, and an early refetch would flash a stale row back.
  const pending = useRef<Promise<unknown>>(Promise.resolve())
  const inflight = useRef(0)

  function patch(ch: NotifierChannel, body: { events?: EventKind[]; enabled?: boolean }) {
    setError(null)
    const next = { ...ch, ...body }
    setRows((prev) => (prev ?? []).map((r) => (r.id === next.id ? next : r)))

    inflight.current += 1
    pending.current = pending.current
      // config is deliberately omitted: the listing masks credentials, so
      // echoing it back would persist the mask over the real value. The server
      // keeps the stored config when the field is absent.
      .then(() => api.updateNotifier(next.id, {
        name: next.name, type: next.type, events: next.events, enabled: next.enabled,
      }))
      .catch((e) => setError(e instanceof Error ? e.message : String(e)))
      .finally(() => {
        inflight.current -= 1
        // Resync either way: on failure the optimistic row never landed.
        if (inflight.current === 0) setReload((n) => n + 1)
      })
  }

  async function runTest(ch: NotifierChannel) {
    setTested((t) => ({ ...t, [ch.id]: 'проверка…' }))
    try {
      const res = await api.testNotifier(ch.id)
      setTested((t) => ({ ...t, [ch.id]: res.status === 'ok' ? '✓ доставлено' : `✗ ${res.error ?? 'ошибка'}` }))
    } catch (e) {
      setTested((t) => ({ ...t, [ch.id]: `✗ ${e instanceof Error ? e.message : String(e)}` }))
    }
  }

  async function remove(ch: NotifierChannel) {
    setError(null)
    try {
      await api.deleteNotifier(ch.id)
      setReload((n) => n + 1)
    } catch (e) {
      // 409 means a task still points at it — say who, not just "conflict".
      setError(e instanceof ApiError && e.status === 409
        ? `Канал «${ch.name}» используется задачами: ${ch.used_by.join(', ')}`
        : e instanceof Error ? e.message : String(e))
    }
  }

  const createBtn = (
    canAdmin ? <button style={btnPrimary} onClick={() => setCreating(true)}>
      <span style={{ fontSize: 14, lineHeight: 1 }}>＋</span>Канал
    </button> : null
  )

  return (
    <Layout title="Уведомления" subtitle={`${channels.length} каналов`} onLogout={logout} actions={createBtn}>
      {loading && <div style={MUTED}>Загрузка…</div>}
      {loadErr && <div style={ERRSTYLE} role="alert">{loadErr}</div>}
      {!loading && !loadErr && (
        <div style={{ padding: '24px 26px' }} className="dc-fade">
          {error && <div style={{ ...ERRSTYLE, padding: '0 0 16px 0' }} role="alert">{error}</div>}

          {channels.length === 0 ? (
            <div style={{ ...MUTED, padding: '0 0 22px 0' }}>
              Каналов пока нет — события никуда не доставляются.
            </div>
          ) : (
            <div style={{ display: 'grid', gridTemplateColumns: 'repeat(3, 1fr)', gap: 14, marginBottom: 26 }}>
              {channels.map((c) => (
                <div key={c.id} style={{ border: '1px solid var(--line)', borderRadius: 11, padding: 18, background: 'var(--panel)' }}>
                  <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: 12, gap: 10 }}>
                    <div style={{ minWidth: 0 }}>
                      <div style={{ fontSize: 14, fontWeight: 600, color: 'var(--ink)', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{c.name}</div>
                      <div style={{ fontFamily: MONO, fontSize: 11, color: 'var(--ink-3)', marginTop: 2 }}>{c.type}</div>
                    </div>
                    <Toggle on={c.enabled} label={`Канал ${c.name}`} onClick={() => patch(c, { enabled: !c.enabled })} />
                  </div>
                  <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: 8 }}>
                    <span style={{ fontSize: 11.5, color: c.enabled ? 'var(--ok)' : 'var(--ink-3)', fontWeight: 600 }}>
                      {c.enabled ? 'включён' : 'выключен'}
                    </span>
                    <div style={{ display: 'flex', gap: 8 }}>
                      <button type="button" style={{ ...btnSecondary, padding: '6px 12px', color: 'var(--ink)' }} onClick={() => runTest(c)}>Тест</button>
                      <button type="button" style={{ ...btnSecondary, padding: '6px 12px' }} onClick={() => setEditing(c)}>Изменить</button>
                    </div>
                  </div>
                  {tested[c.id] && (
                    <div style={{ fontFamily: MONO, fontSize: 11, marginTop: 10, color: tested[c.id].startsWith('✓') ? 'var(--ok)' : 'var(--ink-2)', wordBreak: 'break-word' }}>
                      {tested[c.id]}
                    </div>
                  )}
                </div>
              ))}
            </div>
          )}

          <div style={{ fontSize: 13, fontWeight: 600, color: 'var(--ink)', marginBottom: 12 }}>События и каналы</div>
          <div style={{ border: '1px solid var(--line)', borderRadius: 10, overflow: 'hidden', background: 'var(--panel)', marginBottom: 26 }}>
            <div style={{ display: 'grid', gridTemplateColumns: matrixCols(channels.length), padding: '10px 20px', fontSize: 10, fontWeight: 600, letterSpacing: '0.05em', color: 'var(--ink-3)', borderBottom: '1px solid var(--line)', fontFamily: MONO }}>
              <div>СОБЫТИЕ</div>
              {channels.map((c) => <div key={c.id} style={{ textAlign: 'center', overflow: 'hidden', textOverflow: 'ellipsis' }}>{c.name.toUpperCase()}</div>)}
            </div>
            {EVENTS.map((ev) => (
              <div key={ev.k} style={{ display: 'grid', gridTemplateColumns: matrixCols(channels.length), padding: '13px 20px', borderBottom: '1px solid var(--line)', alignItems: 'center' }}>
                <div style={{ display: 'flex', alignItems: 'center', gap: 9 }}>
                  <div style={{ width: 7, height: 7, borderRadius: '50%', background: ev.dot }} />
                  <span style={{ fontSize: 12.5, color: 'var(--ink)', fontWeight: 500 }}>{ev.label}</span>
                </div>
                {channels.map((c) => (
                  <div key={c.id} style={{ display: 'flex', justifyContent: 'center' }}>
                    <Toggle
                      on={c.events.includes(ev.k)}
                      label={`${ev.label} → ${c.name}`}
                      onClick={() => patch(c, { events: toggleEvent(c.events, ev.k) })}
                    />
                  </div>
                ))}
              </div>
            ))}
          </div>

          <div style={{ fontSize: 13, fontWeight: 600, color: 'var(--ink)', marginBottom: 12 }}>Недавние уведомления</div>
          <div style={{ border: '1px solid var(--line)', borderRadius: 10, overflow: 'hidden', background: 'var(--panel)' }}>
            {log.length === 0 ? (
              <div style={{ padding: 20, fontSize: 12.5, color: 'var(--ink-3)' }}>Отправок пока не было.</div>
            ) : log.map((n) => (
              <div key={n.id} style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: 14, padding: '11px 20px', borderBottom: '1px solid var(--line)' }}>
                <div style={{ display: 'flex', alignItems: 'center', gap: 14, minWidth: 0 }}>
                  <span style={{ fontFamily: MONO, fontSize: 11.5, color: 'var(--ink-3)', minWidth: 80 }}>{fmtDateTime(n.created_at)}</span>
                  <div style={{ width: 6, height: 6, borderRadius: '50%', background: n.status === 'sent' ? 'var(--ok)' : 'var(--err)', flexShrink: 0 }} />
                  <span style={{ fontSize: 12.5, color: 'var(--ink)' }}>{eventLabel(n.kind)}</span>
                  {n.task && <span style={{ fontSize: 12, color: 'var(--ink-2)', fontWeight: 500 }}>{n.task}</span>}
                  {n.error && <span title={n.error} style={{ fontSize: 11.5, color: 'var(--err)', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{n.error}</span>}
                </div>
                <span style={{ fontFamily: MONO, fontSize: 11, color: 'var(--ink-3)', flexShrink: 0 }}>→ {n.channel}</span>
              </div>
            ))}
          </div>
        </div>
      )}

      {creating && (
        <ChannelModal onClose={() => setCreating(false)} onSaved={() => { setCreating(false); setReload((n) => n + 1) }} />
      )}
      {editing && (
        <ChannelModal
          initial={editing}
          onClose={() => setEditing(null)}
          onSaved={() => { setEditing(null); setReload((n) => n + 1) }}
          onDelete={() => { const ch = editing; setEditing(null); void remove(ch) }}
        />
      )}
    </Layout>
  )
}

export function toggleEvent(events: EventKind[], kind: EventKind): EventKind[] {
  return events.includes(kind) ? events.filter((e) => e !== kind) : [...events, kind]
}

function matrixCols(channels: number): string {
  return `2fr ${'1fr '.repeat(Math.max(1, channels)).trim()}`
}

function eventLabel(kind: string): string {
  return EVENTS.find((e) => e.k === kind)?.label ?? kind
}

function cfgObj(v: unknown): Record<string, unknown> {
  return v && typeof v === 'object' && !Array.isArray(v) ? (v as Record<string, unknown>) : {}
}

function str(v: unknown, fallback = ''): string {
  return typeof v === 'string' ? v : fallback
}

// parseRecipients turns the free-text field into the array the plugin expects.
// Operators paste addresses separated by commas, semicolons or newlines, and
// any of those should work.
export function parseRecipients(raw: string): string[] {
  return raw.split(/[,;\s]+/).map((s) => s.trim()).filter(Boolean)
}

function recipientsText(v: unknown): string {
  return Array.isArray(v) ? v.filter((x) => typeof x === 'string').join(', ') : ''
}

function numText(v: unknown): string {
  return typeof v === 'number' ? String(v) : str(v)
}

function ChannelModal({ initial, onClose, onSaved, onDelete }: {
  initial?: NotifierChannel
  onClose: () => void
  onSaved: () => void
  onDelete?: () => void
}) {
  const { api } = useAuth()
  const cfg = cfgObj(initial?.config)
  const isEdit = Boolean(initial)

  const [name, setName] = useState(initial?.name ?? '')
  const [type, setType] = useState(initial?.type ?? CHANNEL_TYPES[0].k)
  const [url, setUrl] = useState(str(cfg.url))
  const [chatID, setChatID] = useState(str(cfg.chat_id))
  const [tokenRef, setTokenRef] = useState(str(cfg.token_ref))
  const [host, setHost] = useState(str(cfg.host))
  const [port, setPort] = useState(numText(cfg.port))
  const [from, setFrom] = useState(str(cfg.from))
  const [to, setTo] = useState(recipientsText(cfg.to))
  const [username, setUsername] = useState(str(cfg.username))
  const [passwordRef, setPasswordRef] = useState(str(cfg.password_ref))
  const [tlsMode, setTlsMode] = useState(str(cfg.tls, TLS_MODES[0].k))
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState<string | null>(null)

  function config(): unknown {
    if (type === 'webhook') return { url: url.trim() }
    // The bot token stays a reference: it is decrypted at delivery time and
    // never travels back to the browser.
    if (type === 'telegram') return { chat_id: chatID.trim(), token_ref: tokenRef.trim() }
    if (type === 'smtp') {
      const c: Record<string, unknown> = {
        host: host.trim(), from: from.trim(), to: parseRecipients(to), tls: tlsMode,
      }
      // An omitted port lets the plugin pick the default for the TLS mode.
      // Sending "" instead would be the one value it cannot make sense of.
      if (port.trim()) c.port = Number(port.trim())
      if (username.trim()) c.username = username.trim()
      if (passwordRef.trim()) c.password_ref = passwordRef.trim()
      return c
    }
    return {}
  }

  function validate(): string | null {
    if (!name.trim()) return 'Укажите название'
    if (type === 'webhook' && !url.trim()) return 'Укажите URL'
    if (type === 'telegram') {
      if (!chatID.trim()) return 'Укажите chat_id'
      if (!tokenRef.trim()) return 'Выберите секрет с токеном бота'
    }
    if (type === 'smtp') {
      if (!host.trim()) return 'Укажите SMTP-хост'
      if (!from.trim()) return 'Укажите адрес отправителя'
      if (parseRecipients(to).length === 0) return 'Укажите хотя бы одного получателя'
      const p = Number(port.trim())
      if (port.trim() && (!Number.isInteger(p) || p < 1 || p > 65535)) return 'Порт должен быть числом от 1 до 65535'
      // Mirrors the plugin: a login without a password almost always means the
      // secret reference went missing.
      if (username.trim() && !passwordRef.trim()) return 'Выберите секрет с паролем SMTP'
    }
    return null
  }

  async function submit(e: FormEvent) {
    e.preventDefault()
    const problem = validate()
    if (problem) {
      setErr(problem)
      return
    }
    setBusy(true)
    setErr(null)
    try {
      // Merge over the stored config so keys this form does not model — a webhook
      // auth header, an inline password, whatever a plugin gains next — survive an
      // edit made for something else entirely. Masked values travel back as
      // "••••" and the server restores them from the stored row. A type change
      // starts clean: those keys belong to the previous plugin.
      const base = initial && type === initial.type ? cfgObj(initial.config) : {}
      const body: CreateNotifierReq = {
        name: name.trim(), type, config: { ...base, ...cfgObj(config()) },
        events: initial?.events, enabled: initial?.enabled ?? true,
      }
      if (initial) await api.updateNotifier(initial.id, body)
      else await api.createNotifier(body)
      onSaved()
    } catch (e2) {
      setErr(e2 instanceof Error ? e2.message : String(e2))
    } finally {
      setBusy(false)
    }
  }

  return (
    <ModalShell title={isEdit ? 'Редактировать канал' : 'Новый канал'} onClose={onClose} width={480}>
      <form onSubmit={submit}>
        <div style={{ padding: 22 }}>
          <div style={{ ...labelStyle, marginBottom: 10 }}>Тип (плагин)</div>
          <div style={{ display: 'grid', gridTemplateColumns: 'repeat(2, 1fr)', gap: 10, marginBottom: 18 }}>
            {CHANNEL_TYPES.map((o) => (
              <OptionCard key={o.k} active={type === o.k} onClick={() => setType(o.k)}>
                <div style={{ fontFamily: MONO, fontSize: 12.5, fontWeight: 600, color: type === o.k ? 'var(--ink)' : 'var(--ink-2)' }}>{o.label}</div>
                <div style={{ fontSize: 10.5, color: 'var(--ink-3)', marginTop: 4 }}>{o.sub}</div>
              </OptionCard>
            ))}
          </div>

          <label htmlFor="nc-name" style={labelStyle}>Название</label>
          <input id="nc-name" className="dc-input" style={{ ...inputStyle, marginBottom: 16 }} value={name} onChange={(e) => setName(e.target.value)} placeholder="ops-telegram" />

          {type === 'webhook' && (
            <>
              <label htmlFor="nc-url" style={labelStyle}>URL</label>
              <input id="nc-url" className="dc-input" style={inputStyle} value={url} onChange={(e) => setUrl(e.target.value)} placeholder="https://hooks.example.com/backup" />
            </>
          )}
          {type === 'telegram' && (
            <>
              <label htmlFor="nc-chat" style={labelStyle}>chat_id</label>
              <input id="nc-chat" className="dc-input" style={{ ...inputStyle, marginBottom: 16 }} value={chatID} onChange={(e) => setChatID(e.target.value)} placeholder="-1001234567890" />
              <SecretPicker
                id="nc-token"
                label="Токен бота (из секретов)"
                value={tokenRef}
                onChange={setTokenRef}
                newSecretType="telegram-token"
              />
            </>
          )}
          {type === 'smtp' && (
            <>
              <div style={{ display: 'grid', gridTemplateColumns: '2fr 1fr', gap: 12, marginBottom: 16 }}>
                <div>
                  <label htmlFor="nc-host" style={labelStyle}>SMTP-хост</label>
                  <input id="nc-host" className="dc-input" style={inputStyle} value={host} onChange={(e) => setHost(e.target.value)} placeholder="smtp.corp.io" />
                </div>
                <div>
                  <label htmlFor="nc-port" style={labelStyle}>Порт</label>
                  <input id="nc-port" className="dc-input" style={inputStyle} value={port} onChange={(e) => setPort(e.target.value)} placeholder="по режиму" inputMode="numeric" />
                </div>
              </div>

              <label htmlFor="nc-tls" style={labelStyle}>Шифрование</label>
              <select id="nc-tls" className="dc-select" style={{ ...inputStyle, marginBottom: 16 }} value={tlsMode} onChange={(e) => setTlsMode(e.target.value)}>
                {TLS_MODES.map((m) => <option key={m.k} value={m.k}>{m.label}</option>)}
              </select>

              <label htmlFor="nc-from" style={labelStyle}>От кого</label>
              <input id="nc-from" className="dc-input" style={{ ...inputStyle, marginBottom: 16 }} value={from} onChange={(e) => setFrom(e.target.value)} placeholder="duskrun@corp.io" />

              <label htmlFor="nc-to" style={labelStyle}>Кому (через запятую)</label>
              <input id="nc-to" className="dc-input" style={{ ...inputStyle, marginBottom: 16 }} value={to} onChange={(e) => setTo(e.target.value)} placeholder="ops@corp.io, oncall@corp.io" />

              <label htmlFor="nc-user" style={labelStyle}>Логин (пусто — без аутентификации)</label>
              <input id="nc-user" className="dc-input" style={{ ...inputStyle, marginBottom: 16 }} value={username} onChange={(e) => setUsername(e.target.value)} placeholder="duskrun@corp.io" autoComplete="off" />

              {username.trim() !== '' && (
                <SecretPicker
                  id="nc-smtp-password"
                  label="Пароль SMTP (из секретов)"
                  value={passwordRef}
                  onChange={setPasswordRef}
                  newSecretType="smtp-password"
                />
              )}
              {username.trim() !== '' && tlsMode === 'none' && (
                <div style={{ fontSize: 11.5, color: 'var(--warn)', marginTop: 10 }}>
                  Пароль по нешифрованному каналу отправляется только на localhost — выберите STARTTLS или TLS.
                </div>
              )}
            </>
          )}
          {type === 'log' && (
            <div style={{ fontSize: 12, color: 'var(--ink-3)' }}>Пишет события в журнал демона. Настраивать нечего.</div>
          )}

          {err && <div style={{ color: 'var(--err)', fontSize: 12.5, marginTop: 14 }} role="alert">{err}</div>}
        </div>
        <div style={{ padding: '16px 22px', borderTop: '1px solid var(--line)', display: 'flex', alignItems: 'center', justifyContent: 'space-between' }}>
          <div>{isEdit && onDelete && <DeleteButton onDelete={async () => onDelete()} />}</div>
          <div style={{ display: 'flex', gap: 10 }}>
            <button type="button" style={btnSecondary} onClick={onClose}>Отмена</button>
            <button type="submit" style={{ ...btnPrimary, opacity: busy ? 0.6 : 1 }} disabled={busy}>
              {busy ? 'Сохранение…' : isEdit ? 'Сохранить' : 'Создать'}
            </button>
          </div>
        </div>
      </form>
    </ModalShell>
  )
}

const MUTED = { padding: '40px 26px', color: 'var(--ink-3)', fontSize: 13 } as const
const ERRSTYLE = { padding: '40px 26px', color: 'var(--err)', fontSize: 13 } as const
