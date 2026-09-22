import { CSSProperties, FormEvent, useState } from 'react'
import { useApiData } from '../api/useApiData'
import { ApiError } from '../api/client'
import type { Storage } from '../api/types'
import { useAuth, useCan } from '../auth'
import { Layout } from '../ui/Layout'
import { DeleteButton, MONO, OptionCard, btnPrimary, btnSecondary, inputStyle, labelStyle } from '../ui/dc'
import { ModalShell } from '../ui/ModalShell'
import { SecretPicker } from '../ui/SecretPicker'

// Storage plugins registered in the backend (internal/storage/*).
const STORAGE_TYPES: { k: string; label: string; sub: string; enabled: boolean }[] = [
  { k: 'localfs', label: 'local FS', sub: 'локальный диск', enabled: true },
  { k: 's3', label: 'S3', sub: 'S3-совместимое', enabled: true },
  { k: 'sftp', label: 'SFTP', sub: 'по SSH', enabled: true },
]

function badge(type: string): string {
  if (type === 's3') return 'S3'
  if (type === 'localfs') return 'FS'
  return type.slice(0, 4).toUpperCase()
}

export default function Storages() {
  const { api, logout } = useAuth()
  const canWrite = useCan('operator')
  const [reload, setReload] = useState(0)
  const [open, setOpen] = useState(false)
  const [editing, setEditing] = useState<Storage | null>(null)
  const { data, error, loading } = useApiData<Storage[]>(() => api.listStorages(), [reload])
  const storages = data ?? []

  const createBtn = (
    canWrite ? <button style={btnPrimary} onClick={() => setOpen(true)}>
      <span style={{ fontSize: 14, lineHeight: 1 }}>＋</span>Хранилище
    </button> : null
  )

  return (
    <Layout title="Хранилища" subtitle={`${storages.length} хранилищ`} onLogout={logout} actions={createBtn}>
      {loading && <div style={MUTED}>Загрузка…</div>}
      {error && <div style={ERRSTYLE} role="alert">{error}</div>}
      {!loading && !error && (
        storages.length === 0 ? (
          <div style={MUTED}>Хранилищ пока нет.</div>
        ) : (
          <div style={{ padding: '24px 26px', display: 'flex', flexDirection: 'column', gap: 14 }} className="dc-fade">
            {storages.map((s) => (
              <div
                key={s.id}
                role="button"
                tabIndex={0}
                onClick={() => setEditing(s)}
                onKeyDown={(e) => {
                  if (e.key === 'Enter' || e.key === ' ') setEditing(s)
                }}
                className="dc-h-panel2"
                style={{ border: '1px solid var(--line)', borderRadius: 10, padding: '20px 22px', background: 'var(--panel)', display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: 12, cursor: 'pointer', transition: 'background 0.15s ease, border-color 0.15s ease' }}
              >
                <div style={{ display: 'flex', alignItems: 'center', gap: 12 }}>
                  <div style={{ width: 32, height: 32, borderRadius: 7, background: 'var(--panel-2)', border: '1px solid var(--line-2)', display: 'flex', alignItems: 'center', justifyContent: 'center', fontFamily: MONO, fontSize: 10, fontWeight: 700, color: 'var(--accent)' }}>{badge(s.type)}</div>
                  <div>
                    <div style={{ fontSize: 14, fontWeight: 600, color: 'var(--ink)', fontFamily: MONO }}>{s.name}</div>
                    <div style={{ fontSize: 11.5, color: 'var(--ink-3)', marginTop: 2 }}>{s.type}{s.secret_ref ? ` · ${s.secret_ref}` : ''}</div>
                  </div>
                </div>
              </div>
            ))}
          </div>
        )
      )}

      {open && (
        <StorageModal
          onClose={() => setOpen(false)}
          onSaved={() => {
            setOpen(false)
            setReload((n) => n + 1)
          }}
        />
      )}
      {editing && (
        <StorageModal
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

function jsonText(v: unknown): string {
  if (v === undefined || v === null) return ''
  return JSON.stringify(v, null, 2)
}

function cfgObj(v: unknown): Record<string, unknown> {
  return v && typeof v === 'object' && !Array.isArray(v) ? v as Record<string, unknown> : {}
}

function str(v: unknown, fallback = ''): string {
  return typeof v === 'string' ? v : fallback
}

function StorageModal({ initial, onClose, onSaved }: { initial?: Storage; onClose: () => void; onSaved: () => void }) {
  const { api } = useAuth()
  const cfg = cfgObj(initial?.config)
  const [name, setName] = useState(initial?.name ?? '')
  const [type, setType] = useState(initial?.type ?? STORAGE_TYPES[0].k)
  const [root, setRoot] = useState(str(cfg.root, ''))
  const [bucket, setBucket] = useState(str(cfg.bucket, ''))
  const [region, setRegion] = useState(str(cfg.region, 'eu-central-1'))
  const [endpoint, setEndpoint] = useState(str(cfg.endpoint, ''))
  const [sftpHost, setSftpHost] = useState(str(cfg.host, ''))
  const [sftpPort, setSftpPort] = useState(String(typeof cfg.port === 'number' ? cfg.port : 22))
  const [sftpUser, setSftpUser] = useState(str(cfg.user, ''))
  const [sftpPath, setSftpPath] = useState(str(cfg.path, '/backup'))
  const [sftpHostKeyMode, setSftpHostKeyMode] = useState(str(cfg.host_key_mode, 'tofu'))
  const [secretRef, setSecretRef] = useState(initial?.secret_ref ?? '')
  const [err, setErr] = useState<string | null>(null)
  const [testText, setTestText] = useState('')
  const [testColor, setTestColor] = useState('var(--run)')
  const [busy, setBusy] = useState(false)
  const isEdit = Boolean(initial)

  function buildConfig(): Record<string, unknown> {
    if (type === 'localfs') return { root: root.trim() }
    if (type === 's3') {
      return {
        bucket: bucket.trim(),
        region: region.trim() || 'us-east-1',
        endpoint: endpoint.trim() || undefined,
        access_key_ref: secretRef.trim() || undefined,
      }
    }
    // private_key_ref is what the plugin reads; the core substitutes the key
    // under private_key before the plugin is built.
    return {
      host: sftpHost.trim(),
      port: Number(sftpPort) || 22,
      user: sftpUser.trim(),
      path: sftpPath.trim(),
      host_key_mode: sftpHostKeyMode,
      private_key_ref: secretRef.trim() || undefined,
    }
  }

  function validate(): string | null {
    if (!name.trim()) return 'Укажите название'
    if (type === 'localfs' && !root.trim()) return 'Укажите путь'
    if (type === 's3' && !bucket.trim()) return 'Укажите bucket'
    if (type === 'sftp') {
      if (!sftpHost.trim()) return 'Укажите хост'
      if (!sftpUser.trim()) return 'Укажите пользователя'
      if (!sftpPath.trim()) return 'Укажите путь'
      if (!secretRef.trim()) return 'Выберите SSH-ключ'
    }
    return null
  }

  function testAccess() {
    const v = validate()
    setTestText(v ? v : '✓ конфигурация заполнена')
    setTestColor(v ? 'var(--err)' : 'var(--ok)')
  }

  async function submit(e: FormEvent) {
    e.preventDefault()
    setErr(null)
    const v = validate()
    if (v) return setErr(v)
    setBusy(true)
    try {
      const body = {
        name: name.trim(),
        type,
        config: buildConfig(),
        secret_ref: secretRef.trim() || undefined,
      }
      if (initial) await api.updateStorage(initial.id, body)
      else await api.createStorage(body)
      onSaved()
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
      await api.deleteStorage(initial.id)
      onSaved()
    } catch (e2) {
      setErr(e2 instanceof ApiError ? e2.message : String(e2))
      throw e2
    }
  }

  return (
    <ModalShell title={isEdit ? 'Редактировать хранилище' : 'Новое хранилище'} onClose={onClose} width={480}>
      <form onSubmit={submit}>
        <div style={{ padding: 22 }}>
          {err && <div role="alert" style={ERR_BANNER}>{err}</div>}
          <div style={{ fontSize: 11, color: 'var(--ink-2)', fontWeight: 600, marginBottom: 10 }}>Тип (плагин)</div>
          <div style={{ display: 'grid', gridTemplateColumns: 'repeat(3, 1fr)', gap: 10, marginBottom: 18 }}>
            {STORAGE_TYPES.map((o) => (
              <OptionCard key={o.k} active={type === o.k} onClick={o.enabled ? () => setType(o.k) : undefined} style={{ opacity: o.enabled ? 1 : 0.45, cursor: o.enabled ? 'pointer' : 'default' }}>
                <div style={{ fontFamily: MONO, fontSize: 12.5, fontWeight: 600, color: type === o.k ? 'var(--ink)' : 'var(--ink-2)' }}>{o.label}</div>
                <div style={{ fontSize: 10.5, color: 'var(--ink-3)', marginTop: 4 }}>{o.sub}</div>
              </OptionCard>
            ))}
          </div>
          <div style={{ marginBottom: 14 }}>
            <label style={labelStyle} htmlFor="s-name">Название</label>
            <input id="s-name" className="dc-input" style={inputStyle} value={name} onChange={(e) => setName(e.target.value)} placeholder="backups-primary" />
          </div>

          {type === 's3' && (
            <>
              <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 14, marginBottom: 14 }}>
                <div>
                  <label style={labelStyle} htmlFor="s-bucket">Bucket</label>
                  <input id="s-bucket" className="dc-input" style={inputStyle} value={bucket} onChange={(e) => setBucket(e.target.value)} placeholder="backups" />
                </div>
                <div>
                  <label style={labelStyle} htmlFor="s-region">Регион</label>
                  <input id="s-region" className="dc-input" style={inputStyle} value={region} onChange={(e) => setRegion(e.target.value)} placeholder="eu-central-1" />
                </div>
              </div>
              <div style={{ marginBottom: 14 }}>
                <label style={labelStyle} htmlFor="s-endpoint">Endpoint (опц.)</label>
                <input id="s-endpoint" className="dc-input" style={inputStyle} value={endpoint} onChange={(e) => setEndpoint(e.target.value)} placeholder="https://s3.amazonaws.com" />
              </div>
              <div>
                <SecretPicker id="s-secret" label="Ключи доступа (из секретов)" value={secretRef} onChange={setSecretRef} />
              </div>
            </>
          )}

          {type === 'localfs' && (
            <div>
              <label style={labelStyle} htmlFor="s-root">Путь</label>
              <input id="s-root" className="dc-input" style={inputStyle} value={root} onChange={(e) => setRoot(e.target.value)} placeholder="/srv/backup" />
            </div>
          )}

          {type === 'sftp' && (
            <>
              <div style={{ display: 'grid', gridTemplateColumns: '2fr 1fr', gap: 14, marginBottom: 14 }}>
                <div>
                  <label style={labelStyle} htmlFor="s-sftp-host">Хост</label>
                  <input id="s-sftp-host" className="dc-input" style={inputStyle} value={sftpHost} onChange={(e) => setSftpHost(e.target.value)} placeholder="offsite-nas.corp.io" />
                </div>
                <div>
                  <label style={labelStyle} htmlFor="s-sftp-port">Порт</label>
                  <input id="s-sftp-port" className="dc-input" style={inputStyle} value={sftpPort} onChange={(e) => setSftpPort(e.target.value)} />
                </div>
              </div>
              <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 14, marginBottom: 14 }}>
                <div>
                  <label style={labelStyle} htmlFor="s-sftp-user">Пользователь</label>
                  <input id="s-sftp-user" className="dc-input" style={inputStyle} value={sftpUser} onChange={(e) => setSftpUser(e.target.value)} placeholder="backup" />
                </div>
                <div>
                  <label style={labelStyle} htmlFor="s-sftp-path">Путь</label>
                  <input id="s-sftp-path" className="dc-input" style={inputStyle} value={sftpPath} onChange={(e) => setSftpPath(e.target.value)} placeholder="/backup" />
                </div>
              </div>
              <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 14 }}>
                <div>
                  <label style={labelStyle} htmlFor="s-sftp-hostkey">Host key</label>
                  <select id="s-sftp-hostkey" className="dc-select" style={inputStyle} value={sftpHostKeyMode} onChange={(e) => setSftpHostKeyMode(e.target.value)}>
                    <option value="tofu">tofu — запомнить при первом подключении</option>
                    <option value="strict">strict — только из known_hosts</option>
                  </select>
                </div>
                <div>
                  <SecretPicker id="s-secret" label="SSH-ключ" value={secretRef} onChange={setSecretRef} types={['ssh-key']} newSecretType="ssh-key" />
                </div>
              </div>
            </>
          )}
        </div>
        <div style={FOOTER_SPLIT}>
          <div style={{ display: 'flex', alignItems: 'center', gap: 12, flexWrap: 'wrap' }}>
            {isEdit && <DeleteButton onDelete={remove} />}
            <button type="button" style={{ ...btnSecondary, color: 'var(--ink)' }} onClick={testAccess}>Проверить доступ</button>
            <span style={{ fontFamily: MONO, fontSize: 11.5, fontWeight: 600, color: testColor }}>{testText}</span>
          </div>
          <div style={{ display: 'flex', gap: 10 }}>
            <button type="button" style={btnSecondary} onClick={onClose}>Отмена</button>
            <button type="submit" style={{ ...btnPrimary, opacity: busy ? 0.6 : 1 }} disabled={busy}>{busy ? 'Сохранение…' : isEdit ? 'Сохранить' : 'Создать'}</button>
          </div>
        </div>
      </form>
    </ModalShell>
  )
}

const MUTED = { padding: '40px 26px', color: 'var(--ink-3)', fontSize: 13 } as const
const ERRSTYLE = { padding: '40px 26px', color: 'var(--err)', fontSize: 13 } as const
const ERR_BANNER: CSSProperties = { color: 'var(--err)', fontSize: 12.5, marginBottom: 14 }
const FOOTER_SPLIT: CSSProperties = { padding: '16px 22px', borderTop: '1px solid var(--line)', display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: 10 }
