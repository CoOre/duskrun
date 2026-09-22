import { CSSProperties, ReactNode, useMemo, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { ApiError } from '../api/client'
import { useApiData } from '../api/useApiData'
import type { Connection, NotifierChannel, Storage } from '../api/types'
import { useAuth } from '../auth'
import { Layout } from '../ui/Layout'
import { MONO, OptionCard, btnPrimary, btnSecondary, inputStyle, labelStyle } from '../ui/dc'
import { COMPRESSION_PRESETS, buildCodecChain, dumpExtFor, extFor, type CompressionKey } from '../ui/codecs'
import { GFS_DEFAULT, KEEP_LAST_DEFAULT, buildRetention, clampCount, formatPolicy, type RetentionMode } from '../ui/retention'

const STEPS = ['Основное', 'Расписание', 'Хранилище', 'Хранение', 'Обзор']
// The retention rules are exclusive in the forms: pick one, or neither.
const RETENTION_MODES: [RetentionMode, string, string][] = [
  ['keep_last', 'keep_last', 'последние N артефактов'],
  ['gfs', 'GFS', 'дневные / недельные / месячные'],
  ['none', 'без очистки', 'хранить всё'],
]

// GFS bucket inputs, in the order the policy notation prints them (7/4/12).
const GFS_FIELDS: ['daily' | 'weekly' | 'monthly', string][] = [
  ['daily', 'дневных'],
  ['weekly', 'недельных'],
  ['monthly', 'месячных'],
]
const CRON_PRESETS: { label: string; cron: string }[] = [
  { label: 'ежедневно 02:00', cron: '0 2 * * *' },
  { label: 'каждый час', cron: '0 * * * *' },
  { label: 'еженедельно', cron: '0 3 * * 0' },
]

export default function TaskWizard() {
  const { api, logout } = useAuth()
  const navigate = useNavigate()
  const { data } = useApiData<{ connections: Connection[]; storages: Storage[]; channels: NotifierChannel[] }>(async () => {
    const [connections, storages, channels] = await Promise.all([
      api.listConnections(),
      api.listStorages(),
      // Notifiers name CONFIGURED channels, not plugin types: a hardcoded list
      // would offer names the backend rejects and hide the ones that exist.
      api.listNotifiers(),
    ])
    return { connections, storages, channels }
  })
  const connections = data?.connections ?? []
  const storages = data?.storages ?? []
  const channels = data?.channels ?? []

  const [step, setStep] = useState(0)
  const [name, setName] = useState('')
  const [connectionId, setConnectionId] = useState<number | ''>('')
  const [database, setDatabase] = useState('')
  const [allDatabases, setAllDatabases] = useState(false)
  const [excludeSystem, setExcludeSystem] = useState(false)
  const [databaseList, setDatabaseList] = useState<string[]>([])
  const [databaseListError, setDatabaseListError] = useState<string | null>(null)
  const [databaseListBusy, setDatabaseListBusy] = useState(false)
  const [cron, setCron] = useState('0 2 * * *')
  const [storageId, setStorageId] = useState<number | ''>('')
  const [compression, setCompression] = useState<CompressionKey>('zstd')
  const [encrypt, setEncrypt] = useState(false)
  const codecs = useMemo(() => buildCodecChain(compression, encrypt), [compression, encrypt])
  // One rule or none — the two are a choice in the form, though the backend
  // unions them. Both keep their numbers across a switch.
  const [mode, setMode] = useState<RetentionMode>('keep_last')
  const [keepLast, setKeepLast] = useState(KEEP_LAST_DEFAULT)
  const [gfs, setGfs] = useState({ ...GFS_DEFAULT })
  const [notifiers, setNotifiers] = useState<string[]>([])
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)

  // Redis dumps the whole instance and has no database to name; mongodump takes
  // one optionally (empty = every database). Only postgres/mysql demand one, and
  // only they can list what is on the server.
  const engine = connections.find((c) => c.id === connectionId)?.engine ?? ''
  const hasDatabase = engine !== 'redis'
  const databaseRequired = engine === 'postgres' || engine === 'mysql' || engine === ''
  const canListDatabases = engine === 'postgres' || engine === 'mysql'

  const artifactPreview = useMemo(() => {
    const base = name.trim() ? name.trim().replace(/\s+/g, '-') : 'backup'
    const suffix = `${extFor(compression)}${encrypt ? '.age' : ''}`
    return `${base}-20260722-0200${dumpExtFor(engine)}${suffix}`
  }, [name, compression, encrypt, engine])

  const connName = connections.find((c) => c.id === connectionId)?.name ?? (connectionId ? `#${connectionId}` : '—')
  const storName = storages.find((s) => s.id === storageId)?.name ?? (storageId ? `#${storageId}` : '—')

  function toggle(list: string[], value: string, set: (v: string[]) => void) {
    set(list.includes(value) ? list.filter((x) => x !== value) : [...list, value])
  }

  function changeConnection(value: string) {
    setConnectionId(value ? Number(value) : '')
    setDatabaseList([])
    setDatabaseListError(null)
  }

  async function loadDatabases() {
    if (!connectionId) return
    setDatabaseListBusy(true)
    setDatabaseListError(null)
    try {
      const res = await api.listConnectionDatabases(Number(connectionId))
      setDatabaseList(res.databases)
      if (res.databases.length === 0) setDatabaseListError('Список БД пуст — введите имя вручную')
    } catch (e) {
      setDatabaseList([])
      setDatabaseListError(e instanceof ApiError ? e.message : 'Не удалось получить список БД')
    } finally {
      setDatabaseListBusy(false)
    }
  }

  function validate(s: number): string | null {
    if (s === 0) {
      if (!name.trim()) return 'Укажите имя задачи'
      if (!connectionId) return 'Выберите соединение'
      if (databaseRequired && !allDatabases && !database.trim()) return 'Укажите базу данных'
    }
    if (s === 1 && cron.trim().split(/\s+/).length !== 5) return 'Cron должен содержать 5 полей'
    if (s === 2 && !storageId) return 'Выберите хранилище'
    return null
  }

  function next() {
    const v = validate(step)
    if (v) return setError(v)
    setError(null)
    setStep((s) => Math.min(s + 1, STEPS.length - 1))
  }
  function back() {
    setError(null)
    setStep((s) => Math.max(s - 1, 0))
  }

  async function submit() {
    for (let s = 0; s <= 2; s++) {
      const v = validate(s)
      if (v) {
        setError(v)
        setStep(s)
        return
      }
    }
    setBusy(true)
    setError(null)
    try {
      await api.createTask({
        name: name.trim(),
        connection_id: Number(connectionId),
        storage_id: Number(storageId),
        cron: cron.trim(),
        dumper_opts: !hasDatabase
          ? {}
          : allDatabases
            ? excludeSystem
              ? { all_databases: true, exclude_system: true }
              : { all_databases: true }
            : database.trim()
              ? { database: database.trim() }
              : {},
        codec_chain: codecs,
        retention,
        notifiers,
      })
      navigate('/tasks')
    } catch (e) {
      setError(e instanceof ApiError ? e.message : String(e))
    } finally {
      setBusy(false)
    }
  }

  const retention = buildRetention(mode, keepLast, gfs)
  const summary: { k: string; v: string; accent?: boolean }[] = [
    { k: 'задача', v: name.trim() || '—' },
    { k: 'соединение', v: connName },
    { k: 'база данных', v: allDatabases ? (excludeSystem ? 'все (без системных)' : 'все базы') : database.trim() || '—' },
    { k: 'cron', v: cron, accent: true },
    { k: 'хранилище', v: storName },
    { k: 'кодеки', v: codecs.join(' → ') || 'нет' },
    { k: 'retention', v: formatPolicy(retention) || '—' },
    { k: 'уведомления', v: notifiers.join(', ') || 'нет' },
  ]

  return (
    <Layout title="Новая задача" subtitle={`шаг ${step + 1} из ${STEPS.length}`} onLogout={logout}>
      <div className="dc-fade">
        {/* Stepper */}
        <div style={{ display: 'flex', borderBottom: '1px solid var(--line)', background: 'var(--panel)' }}>
          {STEPS.map((label, i) => {
            const active = i === step
            const done = i < step
            return (
              <div key={label} onClick={() => setStep(i)} style={{ flex: 1, padding: '14px 16px', borderLeft: i ? '1px solid var(--line)' : 'none', cursor: 'pointer', display: 'flex', alignItems: 'center', gap: 10 }}>
                <div style={{ width: 22, height: 22, borderRadius: '50%', flexShrink: 0, display: 'flex', alignItems: 'center', justifyContent: 'center', fontFamily: MONO, fontSize: 11, fontWeight: 600, background: active ? 'var(--accent)' : done ? 'var(--ok-bg)' : 'var(--panel-2)', color: active ? 'var(--accent-ink)' : done ? 'var(--ok)' : 'var(--ink-3)', border: `1px solid ${active ? 'var(--accent)' : done ? 'var(--ok)' : 'var(--line-2)'}` }}>
                  {done ? '✓' : i + 1}
                </div>
                <div style={{ fontSize: 12.5, fontWeight: 600, color: active ? 'var(--ink)' : done ? 'var(--ink-2)' : 'var(--ink-3)', whiteSpace: 'nowrap' }}>{label}</div>
              </div>
            )
          })}
        </div>

        <div style={{ display: 'grid', gridTemplateColumns: '1fr 320px', alignItems: 'start' }}>
          <div style={{ padding: '26px 30px', borderRight: '1px solid var(--line)' }}>
            <div style={{ maxWidth: 620 }}>
              {error && <div role="alert" style={{ color: 'var(--err)', fontSize: 12.5, marginBottom: 16, padding: '10px 12px', background: 'var(--err-bg)', borderRadius: 7 }}>{error}</div>}

              {step === 0 && (
                <>
                  <StepHead title="Основное" sub="Имя задачи и соединение с сервером БД." />
                  <Field label="Имя задачи" htmlFor="w-name">
                    <input id="w-name" className="dc-input" style={inputStyle} value={name} onChange={(e) => setName(e.target.value)} placeholder="orders-db nightly" />
                  </Field>
                  <Field label="Соединение" htmlFor="w-conn">
                    <select id="w-conn" className="dc-select" style={inputStyle} value={connectionId} onChange={(e) => changeConnection(e.target.value)}>
                      <option value="">— выберите —</option>
                      {connections.map((c) => (
                        <option key={c.id} value={c.id}>{c.name} ({c.engine})</option>
                      ))}
                    </select>
                  </Field>
                  {!hasDatabase && (
                    <div style={{ fontSize: 12, color: 'var(--ink-3)', margin: '4px 0 14px' }}>
                      Redis выгружается целиком (RDB-снимок) — базу указывать не нужно.
                    </div>
                  )}
                  {hasDatabase && (
                  <Field label="База данных" htmlFor="w-database">
                    <div style={{ display: 'grid', gridTemplateColumns: '1fr auto', gap: 8 }}>
                      <input id="w-database" className="dc-input" style={{ ...inputStyle, opacity: allDatabases ? 0.5 : 1 }} value={database} onChange={(e) => setDatabase(e.target.value)} placeholder="orders_production" disabled={allDatabases} />
                      <button
                        type="button"
                        onClick={loadDatabases}
                        disabled={!connectionId || databaseListBusy || allDatabases || !canListDatabases}
                        style={{ ...btnSecondary, height: 40, opacity: !connectionId || databaseListBusy || allDatabases || !canListDatabases ? 0.55 : 1 }}
                      >
                        {databaseListBusy ? 'Загрузка…' : 'Список БД'}
                      </button>
                    </div>
                    {databaseListError && !allDatabases && <div style={{ fontSize: 11.5, color: 'var(--ink-3)', marginTop: 8 }}>{databaseListError}</div>}
                    {databaseList.length > 0 && !allDatabases && (
                      <div style={{ display: 'flex', gap: 8, flexWrap: 'wrap', marginTop: 10 }}>
                        {databaseList.map((db) => (
                          <button
                            key={db}
                            type="button"
                            onClick={() => setDatabase(db)}
                            className="dc-reset dc-h-accent-ink"
                            style={{
                              padding: '5px 9px',
                              borderRadius: 999,
                              border: `1px solid ${database === db ? 'var(--accent)' : 'var(--line-2)'}`,
                              background: database === db ? 'var(--panel-2)' : 'var(--panel)',
                              color: database === db ? 'var(--accent)' : 'var(--ink-2)',
                              cursor: 'pointer',
                              fontFamily: MONO,
                              fontSize: 11.5,
                            }}
                          >
                            {db}
                          </button>
                        ))}
                      </div>
                    )}
                    {!databaseRequired && (
                      <div style={{ fontSize: 11.5, color: 'var(--ink-3)', marginTop: 8 }}>
                        Пусто — выгрузить все базы инстанса.
                      </div>
                    )}
                    {databaseRequired && (
                    <label style={{ display: 'flex', alignItems: 'center', gap: 8, marginTop: 12, fontSize: 12.5, color: 'var(--ink-2)', cursor: 'pointer' }}>
                      <input type="checkbox" checked={allDatabases} onChange={(e) => setAllDatabases(e.target.checked)} />
                      Бэкапить все базы данных на сервере
                    </label>
                    )}
                    {allDatabases && (
                      <label style={{ display: 'flex', alignItems: 'center', gap: 8, marginTop: 10, marginLeft: 22, fontSize: 12.5, color: 'var(--ink-2)', cursor: 'pointer' }}>
                        <input type="checkbox" checked={excludeSystem} onChange={(e) => setExcludeSystem(e.target.checked)} />
                        без системных баз данных
                      </label>
                    )}
                  </Field>
                  )}
                </>
              )}

              {step === 1 && (
                <>
                  <StepHead title="Расписание" sub="Cron-выражение (5 полей)." />
                  <Field label="Cron" htmlFor="w-cron">
                    <input id="w-cron" className="dc-input" style={{ ...inputStyle, fontSize: 15, letterSpacing: '0.05em', borderColor: 'var(--accent)' }} value={cron} onChange={(e) => setCron(e.target.value)} />
                  </Field>
                  <div style={{ display: 'flex', gap: 8, marginTop: 12, flexWrap: 'wrap' }}>
                    {CRON_PRESETS.map((p) => (
                      <button key={p.cron} type="button" onClick={() => setCron(p.cron)} className="dc-reset dc-h-accent-ink" style={{ padding: '6px 12px', borderRadius: 6, fontSize: 11.5, cursor: 'pointer', border: '1px solid var(--line-2)', background: 'var(--panel)', color: 'var(--ink-2)' }}>{p.label}</button>
                    ))}
                  </div>
                </>
              )}

              {step === 2 && (
                <>
                  <StepHead title="Хранилище и обработка" sub="Куда положить артефакт и как его обработать." />
                  <Field label="Хранилище" htmlFor="w-storage">
                    <select id="w-storage" className="dc-select" style={inputStyle} value={storageId} onChange={(e) => setStorageId(e.target.value ? Number(e.target.value) : '')}>
                      <option value="">— выберите —</option>
                      {storages.map((s) => (
                        <option key={s.id} value={s.id}>{s.name} ({s.type})</option>
                      ))}
                    </select>
                  </Field>
                  <div style={{ fontSize: 11, color: 'var(--ink-2)', fontWeight: 600, margin: '4px 0 10px' }}>Сжатие / формат</div>
                  <div style={{ display: 'flex', gap: 10, flexWrap: 'wrap' }}>
                    {COMPRESSION_PRESETS.map((p) => (
                      <OptionCard key={p.key} active={compression === p.key} onClick={() => setCompression(p.key)} style={{ flex: 1, minWidth: 96 }}>
                        <div style={{ fontFamily: MONO, fontSize: 13, fontWeight: 600, color: compression === p.key ? 'var(--ink)' : 'var(--ink-2)' }}>{p.label}</div>
                        <div style={{ fontSize: 11, color: 'var(--ink-3)', marginTop: 4 }}>{p.sub}</div>
                      </OptionCard>
                    ))}
                  </div>
                  <div style={{ fontSize: 11, color: 'var(--ink-2)', fontWeight: 600, margin: '16px 0 10px' }}>Шифрование</div>
                  <div style={{ display: 'flex', gap: 10 }}>
                    <OptionCard active={encrypt} onClick={() => setEncrypt((v) => !v)} style={{ flex: 1 }}>
                      <div style={{ fontFamily: MONO, fontSize: 13, fontWeight: 600, color: encrypt ? 'var(--ink)' : 'var(--ink-2)' }}>age</div>
                      <div style={{ fontSize: 11, color: 'var(--ink-3)', marginTop: 4 }}>{encrypt ? '.age' : 'выкл'}</div>
                    </OptionCard>
                  </div>
                </>
              )}

              {step === 3 && (
                <>
                  <StepHead title="Хранение и уведомления" sub="Политика retention и каналы оповещений." />
                  {/* One rule or none: the modes are exclusive here, and each
                      keeps its numbers so switching back does not retype them. */}
                  <div style={{ fontSize: 11, color: 'var(--ink-2)', fontWeight: 600, margin: '0 0 10px' }}>Политика хранения</div>
                  <div style={{ display: 'flex', gap: 10, marginBottom: 14 }}>
                    {RETENTION_MODES.map(([key, label, sub]) => (
                      <OptionCard key={key} active={mode === key} onClick={() => setMode(key)} style={{ flex: 1, minWidth: 120 }}>
                        <div style={{ fontFamily: MONO, fontSize: 13, fontWeight: 600, color: mode === key ? 'var(--ink)' : 'var(--ink-2)' }}>{label}</div>
                        <div style={{ fontSize: 10.5, color: 'var(--ink-3)', marginTop: 4 }}>{sub}</div>
                      </OptionCard>
                    ))}
                  </div>
                  {mode === 'keep_last' && (
                    <Field label="Хранить последних (keep_last)" htmlFor="w-keep">
                      <input id="w-keep" type="number" min={0} className="dc-input" style={inputStyle} value={keepLast} onChange={(e) => setKeepLast((cur) => clampCount(e.target.value, cur))} />
                    </Field>
                  )}
                  {mode === 'gfs' && (
                    <>
                      <div style={{ fontSize: 11.5, color: 'var(--ink-3)', marginBottom: 10 }}>
                        Самый свежий артефакт в каждом из последних N периодов. 0 — период не хранится.
                      </div>
                      <div style={{ display: 'flex', gap: 10 }}>
                        {GFS_FIELDS.map(([key, label]) => (
                          <Field key={key} label={label} htmlFor={`w-gfs-${key}`}>
                            <input
                              id={`w-gfs-${key}`}
                              type="number"
                              min={0}
                              className="dc-input"
                              style={inputStyle}
                              value={gfs[key]}
                              onChange={(e) => setGfs((cur) => ({ ...cur, [key]: clampCount(e.target.value, cur[key]) }))}
                            />
                          </Field>
                        ))}
                      </div>
                    </>
                  )}
                  {/* "No policy" is a legitimate choice, but the consequence is
                      not obvious from a card labelled «без очистки». */}
                  {formatPolicy(retention) === '' && (
                    <div style={{ fontSize: 11.5, color: 'var(--warn)', margin: '2px 0 10px' }}>
                      Артефакты этой задачи не будут удаляться.
                    </div>
                  )}
                  <div style={{ fontSize: 11, color: 'var(--ink-2)', fontWeight: 600, margin: '4px 0 10px' }}>Уведомления</div>
                  <div style={{ display: 'flex', gap: 10, flexWrap: 'wrap' }}>
                    {channels.length === 0 && (
                      <div style={{ fontSize: 12.5, color: 'var(--ink-3)' }}>
                        Каналов пока нет — создайте их на странице «Уведомления».
                      </div>
                    )}
                    {channels.map((c) => (
                      <OptionCard key={c.id} active={notifiers.includes(c.name)} onClick={() => toggle(notifiers, c.name, setNotifiers)} style={{ flex: 1, minWidth: 120 }}>
                        <div style={{ fontFamily: MONO, fontSize: 13, fontWeight: 600, color: notifiers.includes(c.name) ? 'var(--ink)' : 'var(--ink-2)' }}>{c.name}</div>
                        <div style={{ fontSize: 10.5, color: 'var(--ink-3)', marginTop: 4 }}>{c.type}{c.enabled ? '' : ' · выключен'}</div>
                      </OptionCard>
                    ))}
                  </div>
                </>
              )}

              {step === 4 && (
                <>
                  <StepHead title="Обзор" sub="Проверьте конфигурацию перед созданием." />
                  <div style={{ border: '1px solid var(--line)', borderRadius: 10, overflow: 'hidden' }}>
                    {[...summary, { k: 'артефакт', v: artifactPreview }].map((r) => (
                      <div key={r.k} style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', padding: '13px 18px', borderBottom: '1px solid var(--line)', background: 'var(--panel)' }}>
                        <span style={{ fontSize: 12, color: 'var(--ink-3)', fontFamily: MONO }}>{r.k}</span>
                        <span style={{ fontSize: 12.5, color: 'var(--ink)', fontFamily: MONO, textAlign: 'right' }}>{r.v}</span>
                      </div>
                    ))}
                  </div>
                </>
              )}

              <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginTop: 30 }}>
                <button type="button" onClick={back} style={{ ...btnSecondary, visibility: step === 0 ? 'hidden' : 'visible' }}>← Назад</button>
                {step < STEPS.length - 1 ? (
                  <button type="button" onClick={next} style={btnPrimary}>Далее →</button>
                ) : (
                  <button type="button" onClick={submit} disabled={busy} style={{ ...btnPrimary, opacity: busy ? 0.6 : 1 }}>{busy ? 'Создание…' : 'Создать задачу'}</button>
                )}
              </div>
            </div>
          </div>

          {/* Config summary */}
          <div style={{ padding: '22px', position: 'sticky', top: 0 }}>
            <div style={{ fontSize: 11, fontWeight: 600, letterSpacing: '0.06em', color: 'var(--ink-3)', marginBottom: 14, fontFamily: MONO }}>КОНФИГУРАЦИЯ</div>
            <div style={{ display: 'flex', flexDirection: 'column', gap: 2 }}>
              {summary.map((s) => (
                <div key={s.k} style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', padding: '9px 0', borderBottom: '1px solid var(--line)' }}>
                  <span style={{ fontSize: 11.5, color: 'var(--ink-3)' }}>{s.k}</span>
                  <span style={{ fontSize: 11.5, color: s.accent ? 'var(--accent)' : 'var(--ink)', fontFamily: MONO, textAlign: 'right', maxWidth: 170, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{s.v}</span>
                </div>
              ))}
            </div>
          </div>
        </div>
      </div>
    </Layout>
  )
}

function StepHead({ title, sub }: { title: string; sub: string }) {
  return (
    <>
      <div style={{ fontSize: 16, fontWeight: 700, color: 'var(--ink)', marginBottom: 4 }}>{title}</div>
      <div style={{ fontSize: 12.5, color: 'var(--ink-2)', marginBottom: 22 }}>{sub}</div>
    </>
  )
}

function Field({ label, htmlFor, children }: { label: string; htmlFor: string; children: ReactNode }) {
  const st: CSSProperties = { marginBottom: 16 }
  return (
    <div style={st}>
      <label style={labelStyle} htmlFor={htmlFor}>{label}</label>
      {children}
    </div>
  )
}
