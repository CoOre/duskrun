import { CSSProperties, FormEvent, useState } from 'react'
import { createPortal } from 'react-dom'
import { ApiError } from '../api/client'
import type { Secret } from '../api/types'
import { useAuth } from '../auth'
import { DeleteButton, OptionCard, btnPrimary, btnSecondary, inputStyle, labelStyle } from './dc'
import { ModalShell } from './ModalShell'

const SECRET_TYPES: { k: string; label: string; sub: string }[] = [
  { k: 'db-password', label: 'db-password', sub: 'пароль БД' },
  { k: 'ssh-key', label: 'ssh-key', sub: 'приватный SSH-ключ' },
  { k: 'age-key', label: 'age-key', sub: 'ключ age / x25519' },
  { k: 'smtp-password', label: 'smtp-password', sub: 'пароль SMTP' },
]

const TYPE_COLOR: Record<string, string> = {
  'db-password': 'var(--eng-postgres)',
  'ssh-key': 'var(--eng-mysql)',
  'age-key': 'var(--ok)',
  'smtp-password': 'var(--eng-mssql)',
}

function typeColor(t: string): string {
  return TYPE_COLOR[t] ?? 'var(--ink-3)'
}

export function SecretModal({
  initial,
  initialType,
  onClose,
  onSaved,
}: {
  initial?: Secret
  initialType?: string
  onClose: () => void
  onSaved: (name: string) => void
}) {
  const { api } = useAuth()
  const isEdit = Boolean(initial)
  const [name, setName] = useState(initial?.name ?? '')
  const [type, setType] = useState(initial?.type ?? initialType ?? SECRET_TYPES[0].k)
  const [value, setValue] = useState('')
  const [err, setErr] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)

  function validate(): string | null {
    if (!name.trim()) return 'Укажите имя секрета'
    if (!value.trim()) return isEdit ? 'Введите новое значение' : 'Укажите значение'
    return null
  }

  async function submit(e: FormEvent) {
    e.preventDefault()
    setErr(null)
    const validationError = validate()
    if (validationError) return setErr(validationError)
    setBusy(true)
    try {
      await api.createSecret({ name: name.trim(), type, value })
      onSaved(name.trim())
    } catch (e2) {
      setErr(e2 instanceof ApiError ? e2.message : String(e2))
    } finally {
      setBusy(false)
    }
  }

  async function remove() {
    if (!initial) return
    setErr(null)
    try {
      await api.deleteSecret(initial.id)
      onSaved(initial.name)
    } catch (e2) {
      setErr(e2 instanceof ApiError ? e2.message : String(e2))
      throw e2
    }
  }

  const multiline = type === 'ssh-key' || type === 'age-key'

  return createPortal(
    <ModalShell title={isEdit ? 'Редактировать секрет' : 'Новый секрет'} onClose={onClose} width={560} zIndex={50}>
      <form onSubmit={submit}>
        <div style={{ padding: 22 }}>
          {err && <div role="alert" style={ERR_BANNER}>{err}</div>}

          <div style={{ marginBottom: 18 }}>
            <label style={labelStyle} htmlFor="s-name">Имя</label>
            <input id="s-name" className="dc-input" style={{ ...inputStyle, ...(isEdit ? { opacity: 0.6, cursor: 'not-allowed' } : {}) }} value={name} onChange={(e) => setName(e.target.value)} placeholder="db/pg-primary" disabled={isEdit} />
            <div style={{ fontSize: 11, color: 'var(--ink-3)', marginTop: 6, fontFamily: 'var(--mono)' }}>ссылка: secret://{name.trim() || 'db/pg-primary'}</div>
          </div>

          <div style={{ fontSize: 11, color: 'var(--ink-2)', fontWeight: 600, marginBottom: 10 }}>Тип</div>
          <div style={{ display: 'grid', gridTemplateColumns: 'repeat(2, 1fr)', gap: 10, marginBottom: 20 }}>
            {SECRET_TYPES.map((o) => (
              <OptionCard key={o.k} active={type === o.k} onClick={() => setType(o.k)} style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
                <div style={{ width: 7, height: 7, borderRadius: 2, background: typeColor(o.k), flexShrink: 0 }} />
                <div style={{ minWidth: 0 }}>
                  <div style={{ fontFamily: 'var(--mono)', fontSize: 12, fontWeight: 600, color: type === o.k ? 'var(--ink)' : 'var(--ink-2)' }}>{o.label}</div>
                  <div style={{ fontSize: 10.5, color: 'var(--ink-3)', marginTop: 3 }}>{o.sub}</div>
                </div>
              </OptionCard>
            ))}
          </div>

          <div>
            <label style={labelStyle} htmlFor="s-value">{isEdit ? 'Новое значение' : 'Значение'}</label>
            {multiline ? (
              <textarea id="s-value" className="dc-input" style={{ ...inputStyle, minHeight: 120, resize: 'vertical' }} value={value} onChange={(e) => setValue(e.target.value)} placeholder="-----BEGIN OPENSSH PRIVATE KEY-----&#10;…" />
            ) : (
              <input id="s-value" type="password" className="dc-input" style={inputStyle} value={value} onChange={(e) => setValue(e.target.value)} placeholder="••••••••" />
            )}
            <div style={{ fontSize: 11, color: 'var(--ink-3)', marginTop: 6 }}>
              {isEdit ? 'старое значение нельзя показать — введите новое, оно перезапишет секрет' : 'значение шифруется мастер-ключом и больше не отображается'}
            </div>
          </div>
        </div>
        <div style={{ ...FOOTER, justifyContent: isEdit ? 'space-between' : 'flex-end' }}>
          {isEdit && <DeleteButton onDelete={remove} />}
          <div style={{ display: 'flex', gap: 10 }}>
            <button type="button" style={btnSecondary} onClick={onClose}>Отмена</button>
            <button type="submit" style={{ ...btnPrimary, opacity: busy ? 0.6 : 1 }} disabled={busy}>{busy ? 'Сохранение…' : isEdit ? 'Сохранить' : 'Создать'}</button>
          </div>
        </div>
      </form>
    </ModalShell>,
    document.body,
  )
}

const ERR_BANNER: CSSProperties = { color: 'var(--err)', fontSize: 12.5, marginBottom: 14 }
const FOOTER: CSSProperties = { padding: '16px 22px', borderTop: '1px solid var(--line)', display: 'flex', justifyContent: 'flex-end', gap: 10, position: 'sticky', bottom: 0, background: 'var(--app)' }
