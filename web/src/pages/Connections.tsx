import { CSSProperties, FormEvent, useEffect, useState } from 'react'
import { useApiData } from '../api/useApiData'
import { ApiError } from '../api/client'
import type { Connection, ConnectionCheckStatus, ConnectionTestResult, CreateConnectionReq } from '../api/types'
import { useAuth, useCan } from '../auth'
import { Layout } from '../ui/Layout'
import { DeleteButton, EngineDot, MONO, OptionCard, btnPrimary, btnSecondary, inputStyle, labelStyle } from '../ui/dc'
import { ModalShell } from '../ui/ModalShell'
import { SecretPicker } from '../ui/SecretPicker'

// Engine catalog (the client-side plugin table): label, whether the dumper is
// wired up yet, and the engine's default TCP port. defaultPort drives the port
// field's initial value, placeholder, and the reset-on-switch behaviour.
const ENGINES: { k: string; label: string; enabled: boolean; defaultPort: number }[] = [
  { k: 'postgres', label: 'PostgreSQL', enabled: true, defaultPort: 5432 },
  { k: 'mysql', label: 'MySQL', enabled: true, defaultPort: 3306 },
  { k: 'mssql', label: 'MSSQL', enabled: false, defaultPort: 1433 },
  { k: 'mongodb', label: 'MongoDB', enabled: true, defaultPort: 27017 },
  { k: 'redis', label: 'Redis', enabled: true, defaultPort: 6379 },
  { k: 'sqlite', label: 'SQLite', enabled: false, defaultPort: 0 },
]

function defaultPortFor(engine: string): number {
  return ENGINES.find((e) => e.k === engine)?.defaultPort ?? 0
}

// needsDbUser mirrors the backend's supportsDatabaseList: only the engines whose
// live check runs a catalog query need a DB user up front. Redis has no user by
// default, so demanding one would block the check for a healthy connection.
function needsDbUser(engine: string): boolean {
  return engine === 'postgres' || engine === 'mysql'
}
const CONNECTORS: { k: string; label: string; sub: string }[] = [
  { k: 'direct', label: 'direct', sub: 'TCP host:port' },
  { k: 'socket', label: 'socket', sub: 'unix socket' },
  { k: 'ssh-tunnel', label: 'ssh-tunnel', sub: 'по SSH' },
  { k: 'docker-proxy', label: 'docker-proxy', sub: 'Docker network' },
  { k: 'docker-ssh-proxy', label: 'docker-ssh-proxy', sub: 'SSH + Docker' },
]

export default function Connections() {
  const { api, logout } = useAuth()
  const canWrite = useCan('operator')
  const [reload, setReload] = useState(0)
  const [open, setOpen] = useState(false)
  const [editing, setEditing] = useState<Connection | null>(null)
  const { data, error, loading } = useApiData<Connection[]>(() => api.listConnections(), [reload])
  const conns = data ?? []

  const createBtn = (
    canWrite ? <button style={btnPrimary} onClick={() => setOpen(true)}>
      <span style={{ fontSize: 14, lineHeight: 1 }}>＋</span>Соединение
    </button> : null
  )

  return (
    <Layout title="Соединения" subtitle={`${conns.length} соединений`} onLogout={logout} actions={createBtn}>
      {loading && <div style={MUTED}>Загрузка…</div>}
      {error && <div style={ERRSTYLE} role="alert">{error}</div>}
      {!loading && !error && (
        conns.length === 0 ? (
          <div style={MUTED}>Соединений пока нет.</div>
        ) : (
          <div style={{ padding: '24px 26px', display: 'grid', gridTemplateColumns: 'repeat(2, 1fr)', gap: 14 }} className="dc-fade">
            {conns.map((c) => (
              <div
                key={c.id}
                role="button"
                tabIndex={0}
                onClick={() => setEditing(c)}
                onKeyDown={(e) => {
                  if (e.key === 'Enter' || e.key === ' ') setEditing(c)
                }}
                className="dc-h-panel2"
                style={{ border: '1px solid var(--line)', borderRadius: 10, padding: '18px 20px', background: 'var(--panel)', cursor: 'pointer', transition: 'background 0.15s ease, border-color 0.15s ease' }}
              >
                <div style={{ display: 'flex', alignItems: 'center', gap: 10, minWidth: 0, marginBottom: 14 }}>
                  <EngineDot engine={c.engine} size={10} />
                  <div style={{ minWidth: 0 }}>
                    <div style={{ fontSize: 14, fontWeight: 600, color: 'var(--ink)', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{c.name}</div>
                    <div style={{ fontFamily: MONO, fontSize: 11, color: 'var(--ink-3)', marginTop: 2 }}>{c.engine}</div>
                  </div>
                </div>
                <div style={{ display: 'flex', flexDirection: 'column', gap: 8, fontFamily: MONO, fontSize: 11.5 }}>
                  <Row k="connector" v={c.connector_type} />
                  <Row k="пользователь" v={c.username || '—'} />
                  <Row k="пароль" v={c.secret_ref || '—'} />
                </div>
              </div>
            ))}
          </div>
        )
      )}

      {open && (
        <ConnectionModal
          onClose={() => setOpen(false)}
          onSaved={() => {
            setOpen(false)
            setReload((n) => n + 1)
          }}
        />
      )}
      {editing && (
        <ConnectionModal
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

function Row({ k, v }: { k: string; v: string }) {
  return (
    <div style={{ display: 'flex', justifyContent: 'space-between', gap: 10 }}>
      <span style={{ color: 'var(--ink-3)' }}>{k}</span>
      <span style={{ color: 'var(--ink)', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{v}</span>
    </div>
  )
}

// checkColor paints one checklist dot: green passed, red failed, muted for a
// stage the engine has no probe for — a skipped stage is not a failure.
function checkColor(status: ConnectionCheckStatus): string {
  if (status === 'ok') return 'var(--ok)'
  if (status === 'skipped') return 'var(--line-2)'
  return 'var(--err)'
}

function ConnectionTestPanel({ result }: { result: ConnectionTestResult }) {
  const ok = result.status === 'ok'
  const databases = result.databases ?? []
  return (
    <div role={ok ? undefined : 'alert'} style={{ ...TEST_PANEL, borderColor: ok ? 'color-mix(in srgb, var(--ok) 42%, var(--line))' : 'color-mix(in srgb, var(--err) 42%, var(--line))' }}>
      <div style={{ display: 'flex', alignItems: 'flex-start', justifyContent: 'space-between', gap: 14, marginBottom: 12 }}>
        <div>
          <div style={{ fontSize: 13, fontWeight: 700, color: ok ? 'var(--ok)' : 'var(--err)' }}>
            {ok ? 'Соединение работает' : result.error?.message ?? 'Проверка не прошла'}
          </div>
          <div style={{ fontSize: 11.5, color: 'var(--ink-3)', marginTop: 4 }}>
            {result.latency_ms > 0 ? `${result.latency_ms} мс` : 'проверка завершена'}
          </div>
        </div>
        <div style={{ fontFamily: MONO, fontSize: 10.5, color: ok ? 'var(--ok)' : 'var(--err)', fontWeight: 700 }}>
          {result.status.toUpperCase()}
        </div>
      </div>
      <div style={{ display: 'grid', gap: 7 }}>
        {result.checks.map((c) => (
          <div key={c.key} style={{ display: 'flex', alignItems: 'center', gap: 8, minWidth: 0 }}>
            <span aria-hidden="true" style={{ ...CHECK_DOT, background: checkColor(c.status) }} />
            <span style={{ fontSize: 12, color: c.status === 'skipped' ? 'var(--ink-3)' : 'var(--ink)', fontWeight: 600 }}>
              {c.label}
              {c.status === 'skipped' && <span style={{ fontWeight: 400 }}> — не проверяется для этого движка</span>}
            </span>
          </div>
        ))}
      </div>
      {!ok && result.error?.hint && (
        <div style={{ marginTop: 12, color: 'var(--ink-2)', fontSize: 12.5, lineHeight: 1.5 }}>{result.error.hint}</div>
      )}
      {ok && databases.length > 0 && (
        <div style={{ marginTop: 12, display: 'flex', gap: 8, alignItems: 'flex-start', minWidth: 0 }}>
          <span style={{ fontSize: 11.5, color: 'var(--ink-3)', flex: '0 0 auto' }}>Базы</span>
          <span style={{ fontFamily: MONO, fontSize: 11.5, color: 'var(--ink)', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
            {databases.join(', ')}
          </span>
        </div>
      )}
    </div>
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

function num(v: unknown, fallback: number): number {
  return typeof v === 'number' && Number.isFinite(v) ? v : fallback
}

function ConnectionModal({ initial, onClose, onSaved }: { initial?: Connection; onClose: () => void; onSaved: () => void }) {
  const { api } = useAuth()
  const cfg = cfgObj(initial?.connector_config)
  const [name, setName] = useState(initial?.name ?? '')
  const [engine, setEngine] = useState(initial?.engine ?? ENGINES[0].k)
  const [connectorType, setConnectorType] = useState(initial?.connector_type ?? CONNECTORS[0].k)
  const [host, setHost] = useState(str(cfg.host ?? cfg.remote_host ?? cfg.target_host, ''))
  const [port, setPort] = useState(String(num(cfg.port ?? cfg.remote_port ?? cfg.target_port, defaultPortFor(engine))))
  const [socketPath, setSocketPath] = useState(str(cfg.path, ''))
  const [dockerNetwork, setDockerNetwork] = useState(str(cfg.network, ''))
  const [sshHost, setSshHost] = useState(str(cfg.ssh_host, ''))
  const [sshPort, setSshPort] = useState(String(num(cfg.ssh_port, 22)))
  const [sshUser, setSshUser] = useState(str(cfg.ssh_user, 'backup'))
  const [sshKeyRef, setSshKeyRef] = useState(str(cfg.private_key_ref, ''))
  const [username, setUsername] = useState(initial?.username ?? '')
  const [secretRef, setSecretRef] = useState(initial?.secret_ref ?? '')
  const [err, setErr] = useState<string | null>(null)
  const [testResult, setTestResult] = useState<ConnectionTestResult | null>(null)
  const [testBusy, setTestBusy] = useState(false)
  const [busy, setBusy] = useState(false)
  const isEdit = Boolean(initial)
  const isDockerProxy = connectorType === 'docker-proxy' || connectorType === 'docker-ssh-proxy'
  const isSSH = connectorType === 'ssh-tunnel' || connectorType === 'docker-ssh-proxy'

  // Switching engine updates the port to the new engine's default, but only when
  // the field still holds the previous engine's default (or is empty) — a port
  // the user typed by hand is preserved.
  function selectEngine(next: string) {
    if (port.trim() === '' || port.trim() === String(defaultPortFor(engine))) {
      setPort(String(defaultPortFor(next)))
    }
    setEngine(next)
  }

  useEffect(() => {
    setTestResult(null)
  }, [name, engine, connectorType, host, port, socketPath, dockerNetwork, sshHost, sshPort, sshUser, sshKeyRef, username, secretRef])

  function buildConfig(): Record<string, unknown> {
    if (connectorType === 'socket') return { path: socketPath.trim() }
    if (connectorType === 'docker-proxy') {
      return {
        network: dockerNetwork.trim(),
        target_host: host.trim(),
        target_port: Number(port) || 0,
      }
    }
    if (connectorType === 'docker-ssh-proxy') {
      return {
        ssh_host: sshHost.trim(),
        ssh_port: Number(sshPort) || 22,
        ssh_user: sshUser.trim(),
        private_key_ref: sshKeyRef.trim(),
        network: dockerNetwork.trim(),
        target_host: host.trim(),
        target_port: Number(port) || 0,
      }
    }
    if (connectorType === 'ssh-tunnel') {
      return {
        ssh_host: sshHost.trim(),
        ssh_port: Number(sshPort) || 22,
        ssh_user: sshUser.trim(),
        private_key_ref: sshKeyRef.trim(),
        remote_host: host.trim(),
        remote_port: Number(port) || 0,
      }
    }
    return { host: host.trim(), port: Number(port) || 0 }
  }

  function validate(): string | null {
    if (!name.trim()) return 'Укажите название'
    if (connectorType === 'socket' && !socketPath.trim()) return 'Укажите путь сокета'
    if (isDockerProxy && !dockerNetwork.trim()) return 'Укажите Docker network'
    if (connectorType !== 'socket' && (!host.trim() || !Number(port))) return 'Укажите хост и порт БД'
    if (isSSH && (!sshHost.trim() || !sshUser.trim())) return 'Укажите параметры SSH-туннеля'
    return null
  }

  function buildBody(): CreateConnectionReq {
    return {
      name: name.trim(),
      engine,
      connector_type: connectorType,
      connector_config: buildConfig(),
      username: username.trim() || undefined,
      secret_ref: secretRef.trim() || undefined,
    }
  }

  async function testConnection() {
    const v = validate()
    if (v) {
      setTestResult({
        status: 'failed',
        latency_ms: 0,
        checks: [{ key: 'config', status: 'failed', label: 'Конфигурация' }],
        error: {
          code: 'invalid_config',
          message: 'Конфигурация соединения неполная',
          hint: v,
        },
      })
      return
    }
    if (needsDbUser(engine) && !username.trim()) {
      setTestResult({
        status: 'failed',
        latency_ms: 0,
        checks: [{ key: 'config', status: 'failed', label: 'Конфигурация' }],
        error: {
          code: 'username_required',
          message: 'Не указан пользователь БД',
          hint: 'Укажите пользователя БД для live-проверки соединения',
        },
      })
      return
    }
    setErr(null)
    setTestBusy(true)
    try {
      setTestResult(await api.testConnection(buildBody()))
    } catch (e2) {
      setTestResult({
        status: 'failed',
        latency_ms: 0,
        checks: [
          { key: 'config', status: 'ok', label: 'Конфигурация' },
          { key: 'connect', status: 'failed', label: 'Подключение к БД' },
        ],
        error: {
          code: 'request_failed',
          message: e2 instanceof ApiError ? e2.message : 'Не удалось выполнить проверку',
          hint: 'Проверьте доступность API duskrun и повторите проверку',
        },
      })
    } finally {
      setTestBusy(false)
    }
  }

  async function submit(e: FormEvent) {
    e.preventDefault()
    setErr(null)
    const v = validate()
    if (v) return setErr(v)
    setBusy(true)
    try {
      const body = buildBody()
      if (initial) await api.updateConnection(initial.id, body)
      else await api.createConnection(body)
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
      await api.deleteConnection(initial.id)
      onSaved()
    } catch (e2) {
      setErr(e2 instanceof ApiError ? e2.message : String(e2))
      throw e2
    }
  }

  return (
    <ModalShell title={isEdit ? 'Редактировать соединение' : 'Новое соединение'} onClose={onClose} width={720}>
      <form onSubmit={submit}>
        <div style={{ padding: 22 }}>
          {err && <div role="alert" style={ERR_BANNER}>{err}</div>}
          <div style={{ marginBottom: 18 }}>
            <label style={labelStyle} htmlFor="c-name">Название</label>
            <input id="c-name" className="dc-input" style={inputStyle} value={name} onChange={(e) => setName(e.target.value)} placeholder="pg-primary" />
          </div>

          <div style={{ fontSize: 11, color: 'var(--ink-2)', fontWeight: 600, marginBottom: 10 }}>Тип СУБД (плагин)</div>
          <div style={{ display: 'grid', gridTemplateColumns: 'repeat(3, 1fr)', gap: 8, marginBottom: 20 }}>
            {ENGINES.map((o) => (
              <OptionCard key={o.k} active={engine === o.k} onClick={o.enabled ? () => selectEngine(o.k) : undefined} style={{ display: 'flex', alignItems: 'center', gap: 7, padding: '10px 11px', opacity: o.enabled ? 1 : 0.45, cursor: o.enabled ? 'pointer' : 'default' }}>
                <EngineDot engine={o.k} />
                <span style={{ fontSize: 12, fontWeight: 600, color: engine === o.k ? 'var(--ink)' : 'var(--ink-2)' }}>{o.label}</span>
              </OptionCard>
            ))}
          </div>

          <div style={{ fontSize: 11, color: 'var(--ink-2)', fontWeight: 600, marginBottom: 10 }}>Тип connector'а</div>
          <div style={{ display: 'grid', gridTemplateColumns: 'repeat(5, 1fr)', gap: 10, marginBottom: 20 }}>
            {CONNECTORS.map((o) => (
              <OptionCard key={o.k} active={connectorType === o.k} onClick={() => setConnectorType(o.k)}>
                <div style={{ fontFamily: MONO, fontSize: 12.5, fontWeight: 600, color: connectorType === o.k ? 'var(--ink)' : 'var(--ink-2)' }}>{o.label}</div>
                <div style={{ fontSize: 10.5, color: 'var(--ink-3)', marginTop: 4 }}>{o.sub}</div>
              </OptionCard>
            ))}
          </div>

          {connectorType === 'socket' ? (
            <div style={{ marginBottom: 14 }}>
              <label style={labelStyle} htmlFor="c-socket">Путь сокета</label>
              <input id="c-socket" className="dc-input" style={inputStyle} value={socketPath} onChange={(e) => setSocketPath(e.target.value)} placeholder="/var/run/postgresql/.s.PGSQL.5432" />
            </div>
          ) : (
            <>
              {isDockerProxy && (
                <div style={{ marginBottom: 14 }}>
                  <label style={labelStyle} htmlFor="c-docker-network">Docker network</label>
                  <input id="c-docker-network" className="dc-input" style={inputStyle} value={dockerNetwork} onChange={(e) => setDockerNetwork(e.target.value)} placeholder="myapp_default" />
                </div>
              )}
              <div style={{ display: 'grid', gridTemplateColumns: '2fr 1fr', gap: 14, marginBottom: 14 }}>
                <div>
                  <label style={labelStyle} htmlFor="c-host">{isDockerProxy ? 'Хост в Docker network' : 'Хост БД'}</label>
                  <input id="c-host" className="dc-input" style={inputStyle} value={host} onChange={(e) => setHost(e.target.value)} placeholder={isDockerProxy ? 'postgres' : '10.0.4.12'} />
                </div>
                <div>
                  <label style={labelStyle} htmlFor="c-port">Порт</label>
                  <input id="c-port" className="dc-input" style={inputStyle} value={port} onChange={(e) => setPort(e.target.value)} placeholder={String(defaultPortFor(engine))} />
                </div>
              </div>
            </>
          )}

          <div style={{ display: 'grid', gridTemplateColumns: '1fr 1.4fr', gap: 14, marginBottom: isSSH ? 14 : 0 }}>
            <div>
              <label style={labelStyle} htmlFor="c-user">Пользователь БД</label>
              <input id="c-user" className="dc-input" style={inputStyle} value={username} placeholder="backup" onChange={(e) => setUsername(e.target.value)} />
            </div>
            <div>
              <SecretPicker id="c-secret" label="Пароль (секрет)" value={secretRef} onChange={setSecretRef} types={['db-password']} newSecretType="db-password" />
            </div>
          </div>

          {isSSH && (
            <div style={{ borderTop: '1px solid var(--line)', paddingTop: 16, marginTop: 2 }}>
              <div style={{ fontSize: 11, fontWeight: 600, color: 'var(--accent)', marginBottom: 12, fontFamily: MONO }}>SSH-ТУННЕЛЬ</div>
              <div style={{ display: 'grid', gridTemplateColumns: '2fr 1fr 1fr', gap: 14, marginBottom: 14 }}>
                <div>
                  <label style={labelStyle} htmlFor="c-ssh-host">SSH хост</label>
                  <input id="c-ssh-host" className="dc-input" style={inputStyle} value={sshHost} onChange={(e) => setSshHost(e.target.value)} placeholder="bastion.corp.internal" />
                </div>
                <div>
                  <label style={labelStyle} htmlFor="c-ssh-port">Порт</label>
                  <input id="c-ssh-port" className="dc-input" style={inputStyle} value={sshPort} onChange={(e) => setSshPort(e.target.value)} />
                </div>
                <div>
                  <label style={labelStyle} htmlFor="c-ssh-user">Юзер</label>
                  <input id="c-ssh-user" className="dc-input" style={inputStyle} value={sshUser} onChange={(e) => setSshUser(e.target.value)} placeholder="backup" />
                </div>
              </div>
              <div>
                <SecretPicker id="c-ssh-key" label="SSH-ключ (из секретов)" value={sshKeyRef} onChange={setSshKeyRef} types={['ssh-key']} newSecretType="ssh-key" />
              </div>
            </div>
          )}
          {testResult && <ConnectionTestPanel result={testResult} />}
        </div>
        <div style={FOOTER_SPLIT}>
          <div style={{ display: 'flex', alignItems: 'center', gap: 12, flexWrap: 'wrap' }}>
            {isEdit && <DeleteButton onDelete={remove} />}
            <button type="button" style={{ ...btnSecondary, color: 'var(--ink)', opacity: testBusy ? 0.6 : 1 }} onClick={testConnection} disabled={testBusy}>
              {testBusy ? 'Проверяем…' : 'Проверить соединение'}
            </button>
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
const FOOTER_SPLIT: CSSProperties = { padding: '16px 22px', borderTop: '1px solid var(--line)', display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: 10, position: 'sticky', bottom: 0, background: 'var(--app)', flexWrap: 'wrap' }
const TEST_PANEL: CSSProperties = { marginTop: 18, border: '1px solid var(--line)', borderRadius: 8, padding: '14px 15px', background: 'var(--panel)' }
const CHECK_DOT: CSSProperties = { width: 7, height: 7, borderRadius: 999, flex: '0 0 auto' }
