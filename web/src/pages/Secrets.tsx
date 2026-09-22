import { CSSProperties, useState } from 'react'
import type { Connection, Secret, Storage } from '../api/types'
import { useApiData } from '../api/useApiData'
import { useAuth, useCan } from '../auth'
import { Layout } from '../ui/Layout'
import { MONO, btnPrimary, btnSecondary } from '../ui/dc'
import { SecretModal } from '../ui/SecretModal'

const SECRET_COLS = '1.5fr 1fr 1.3fr 0.9fr 1fr'

// Secret types offered in the create modal (mirrors the comp + core.Secret.Type).
const TYPE_COLOR: Record<string, string> = {
  'db-password': 'var(--eng-postgres)',
  'ssh-key': 'var(--eng-mysql)',
  'age-key': 'var(--ok)',
  'smtp-password': 'var(--eng-mssql)',
}

function typeColor(t: string): string {
  return TYPE_COLOR[t] ?? 'var(--ink-3)'
}

// Normalise a "secret://name" (or bare "name") reference to its plain name.
function refName(ref: string | undefined): string {
  return (ref ?? '').replace(/^secret:\/\//, '').trim()
}

// Count how many connections/storages reference each secret name, so the list
// can show real usage instead of the comp's placeholder counts. Connections
// reference credentials via secret_ref and an SSH key via private_key_ref.
function usageMap(conns: Connection[], storages: Storage[]): Map<string, number> {
  const m = new Map<string, number>()
  const bump = (ref: string | undefined) => {
    const n = refName(ref)
    if (n) m.set(n, (m.get(n) ?? 0) + 1)
  }
  for (const c of conns) {
    bump(c.secret_ref)
    const cfg = c.connector_config as { private_key_ref?: string } | undefined
    bump(cfg?.private_key_ref)
  }
  for (const st of storages) bump(st.secret_ref)
  return m
}

function usageLabel(count: number): string {
  if (!count) return 'не используется'
  const mod100 = count % 100
  const mod10 = count % 10
  let word = 'ссылок'
  if (mod100 < 11 || mod100 > 14) {
    if (mod10 === 1) word = 'ссылка'
    else if (mod10 >= 2 && mod10 <= 4) word = 'ссылки'
  }
  return `${count} ${word}`
}

function fmtDate(iso?: string): string {
  if (!iso) return '—'
  const t = Date.parse(iso)
  if (Number.isNaN(t)) return '—'
  const d = new Date(t)
  const p = (n: number) => String(n).padStart(2, '0')
  return `${p(d.getDate())}.${p(d.getMonth() + 1)}.${d.getFullYear()}`
}

type Bundle = { secrets: Secret[]; conns: Connection[]; storages: Storage[] }

export default function Secrets() {
  const { api, logout } = useAuth()
  const canAdmin = useCan('admin')
  const [reload, setReload] = useState(0)
  const [open, setOpen] = useState(false)
  const [editing, setEditing] = useState<Secret | null>(null)
  const { data, error, loading } = useApiData<Bundle>(
    () =>
      Promise.all([api.listSecrets(), api.listConnections(), api.listStorages()]).then(
        ([secrets, conns, storages]) => ({ secrets, conns, storages }),
      ),
    [reload],
  )
  const secrets = data?.secrets ?? []
  const usage = usageMap(data?.conns ?? [], data?.storages ?? [])

  const createBtn = canAdmin ? (
    <button style={btnPrimary} onClick={() => setOpen(true)}>
      <span style={{ fontSize: 14, lineHeight: 1 }}>＋</span>Создать секрет
    </button>
  ) : null

  return (
    <Layout title="Секреты" subtitle="мастер-ключ · refs" onLogout={logout} actions={createBtn}>
      <div style={{ padding: '24px 26px' }} className="dc-fade">
        {/* Master-key panel */}
        <div style={{ ...PANEL, padding: '18px 20px', marginBottom: 20, display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: 16 }}>
          <div style={{ display: 'flex', alignItems: 'center', gap: 14, minWidth: 0 }}>
            <div style={{ width: 34, height: 34, borderRadius: 8, background: 'var(--run-bg)', border: '1px solid var(--run)', display: 'flex', alignItems: 'center', justifyContent: 'center', flexShrink: 0 }}>
              <div style={{ width: 12, height: 14, border: '1.5px solid var(--run)', borderRadius: '6px 6px 2px 2px' }} />
            </div>
            <div style={{ minWidth: 0 }}>
              <div style={{ fontSize: 14, fontWeight: 600, color: 'var(--ink)' }}>Мастер-ключ</div>
              <div style={{ fontFamily: MONO, fontSize: 11.5, color: 'var(--ink-3)', marginTop: 3, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>источник: env DUSKRUN_MASTER_KEY · AES-256-GCM</div>
            </div>
          </div>
          <div style={{ display: 'flex', alignItems: 'center', gap: 10, flexShrink: 0 }}>
            <span style={{ display: 'inline-flex', alignItems: 'center', gap: 6, padding: '4px 10px', borderRadius: 6, fontSize: 11.5, fontWeight: 600, background: 'var(--ok-bg)', color: 'var(--ok)' }}><span style={{ width: 6, height: 6, borderRadius: '50%', background: 'var(--ok)' }} />активен</span>
            <button style={{ ...btnSecondary, opacity: 0.5, cursor: 'default' }} disabled>Ротировать</button>
          </div>
        </div>

        {loading && <div style={MUTED}>Загрузка…</div>}
        {error && <div style={{ ...MUTED, color: 'var(--err)' }} role="alert">{error}</div>}

        {!loading && !error && (
          <div style={{ ...PANEL, overflow: 'hidden' }}>
            <div style={{ display: 'grid', gridTemplateColumns: SECRET_COLS, padding: '10px 20px', fontSize: 10, fontWeight: 600, letterSpacing: '0.05em', color: 'var(--ink-3)', borderBottom: '1px solid var(--line)', fontFamily: MONO }}>
              <div>ИМЯ</div><div>ТИП</div><div>ИСПОЛЬЗУЕТСЯ</div><div>СОЗДАН</div><div style={{ textAlign: 'right' }}>ПОСЛ. ДОСТУП</div>
            </div>
            {secrets.length === 0 ? (
              <div style={{ padding: '28px 20px', color: 'var(--ink-3)', fontSize: 13 }}>Секретов пока нет. Создайте первый — он будет зашифрован мастер-ключом.</div>
            ) : (
              secrets.map((s) => (
                <div
                  key={s.id}
                  role="button"
                  tabIndex={0}
                  onClick={() => setEditing(s)}
                  onKeyDown={(e) => {
                    if (e.key === 'Enter' || e.key === ' ') setEditing(s)
                  }}
                  className="dc-h-panel2"
                  style={{ display: 'grid', gridTemplateColumns: SECRET_COLS, padding: '13px 20px', fontSize: 12.5, borderBottom: '1px solid var(--line)', alignItems: 'center', cursor: 'pointer', transition: 'background 0.15s ease' }}
                >
                  <div style={{ display: 'flex', alignItems: 'center', gap: 9, minWidth: 0 }}><span style={{ fontFamily: MONO, color: 'var(--ink-3)', letterSpacing: 2 }}>••••</span><span style={{ fontFamily: MONO, fontSize: 12, color: 'var(--ink)', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{s.name}</span></div>
                  <div style={{ display: 'flex', alignItems: 'center', gap: 7 }}><div style={{ width: 6, height: 6, borderRadius: 2, background: typeColor(s.type) }} /><span style={{ fontFamily: MONO, fontSize: 11, color: 'var(--ink-2)' }}>{s.type}</span></div>
                  <div style={{ fontSize: 12, color: 'var(--ink-2)' }}>{usageLabel(usage.get(s.name) ?? 0)}</div>
                  <div style={{ fontFamily: MONO, fontSize: 11, color: 'var(--ink-3)' }}>{fmtDate(s.created_at)}</div>
                  <div style={{ fontFamily: MONO, fontSize: 11, color: 'var(--ink-3)', textAlign: 'right' }}>{s.last_used_at ? fmtDate(s.last_used_at) : 'никогда'}</div>
                </div>
              ))
            )}
          </div>
        )}
      </div>

      {open && (
        <SecretModal
          onClose={() => setOpen(false)}
          onSaved={() => {
            setOpen(false)
            setReload((n) => n + 1)
          }}
        />
      )}
      {editing && (
        <SecretModal
          initial={editing}
          onClose={() => setEditing(null)}
          onSaved={() => {
            setEditing(null)
            setReload((n) => n + 1)
          }}
        />
      )}
    </Layout>
  )
}

const PANEL: CSSProperties = { border: '1px solid var(--line)', borderRadius: 11, background: 'var(--panel)' }
const MUTED: CSSProperties = { padding: '28px 4px', color: 'var(--ink-3)', fontSize: 13 }
