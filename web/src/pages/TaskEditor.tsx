import { CSSProperties, ReactNode, useMemo, useState } from 'react'
import { useNavigate, useParams } from 'react-router-dom'
import { ApiError } from '../api/client'
import { useApiData } from '../api/useApiData'
import type { Connection, NotifierChannel, Storage, Task } from '../api/types'
import { useAuth } from '../auth'
import { Layout } from '../ui/Layout'
import { DeleteButton, EngineDot, MONO, OptionCard, btnPrimary, btnSecondary, inputStyle, labelStyle } from '../ui/dc'
import { COMPRESSION_PRESETS, buildCodecChain, compressionFromCodecs, dumpExtFor, extFor, type CompressionKey } from '../ui/codecs'
import { bothRules, buildRetention, clampCount, formatPolicy, gfsFrom, keepLastFrom, modeOf } from '../ui/retention'

type Data = { tasks: Task[]; connections: Connection[]; storages: Storage[]; channels: NotifierChannel[] }
const STEPS = ['Тип БД', 'Соединение', 'Расписание', 'Хранилище', 'Хранение', 'Обзор']
// Watchdog thresholds, in seconds. 0 = derive from cron, -1 = off.
const WATCHDOG_OPTIONS: [string, number][] = [
  ['авто (по расписанию)', 0],
  ['6 часов', 6 * 3600],
  ['12 часов', 12 * 3600],
  ['24 часа', 24 * 3600],
  ['48 часов', 48 * 3600],
  ['7 дней', 7 * 24 * 3600],
  ['выключен', -1],
]

// GFS bucket inputs, in the order the policy notation prints them (7/4/12).
const GFS_FIELDS: ['daily' | 'weekly' | 'monthly', string][] = [
  ['daily', 'дн'],
  ['weekly', 'нед'],
  ['monthly', 'мес'],
]

// watchdogOptions keeps a stored threshold selectable even when it is not one
// of the presets (set via the API, or a preset list that changed later).
// Without it the select renders blank while still re-submitting that value.
export function watchdogOptions(current: number): [string, number][] {
  if (WATCHDOG_OPTIONS.some(([, value]) => value === current)) return WATCHDOG_OPTIONS
  return [...WATCHDOG_OPTIONS, [`${current} с`, current]]
}

const CRON_PRESETS = [
  ['ежедневно 02:00', '0 2 * * *'],
  ['каждый час', '0 * * * *'],
  ['каждые 30 мин', '*/30 * * * *'],
  ['еженедельно', '0 3 * * 0'],
] as const
const ENGINES = [
  ['postgres', 'PostgreSQL', 'pg_dump', true],
  ['mysql', 'MySQL / MariaDB', 'mysqldump / mydumper', true],
  ['mssql', 'MSSQL', 'sqlcmd · staged', false],
  ['mongodb', 'MongoDB', 'mongodump', false],
  ['redis', 'Redis', 'RDB snapshot', false],
  ['sqlite', 'SQLite', 'file copy', false],
] as const
const CONNECTORS = [
  ['direct', 'direct', 'TCP host:port'],
  ['socket', 'socket', 'unix socket'],
  ['ssh-tunnel', 'ssh-tunnel', 'проброс по SSH'],
  ['docker-proxy', 'docker-proxy', 'Docker network'],
  ['docker-ssh-proxy', 'docker-ssh-proxy', 'SSH + Docker'],
] as const
const ENCRYPTION = [
  ['age', 'age', 'X25519', true],
  ['aes', 'AES-256-GCM', 'key', false],
  ['none', 'без шифрования', '—', true],
] as const

export default function TaskEditor() {
  const { id } = useParams()
  const taskId = Number(id)
  const { api, logout } = useAuth()
  const navigate = useNavigate()
  const { data, error, loading } = useApiData<Data>(async () => {
    const [tasks, connections, storages, channels] = await Promise.all([
      api.listTasks(),
      api.listConnections(),
      api.listStorages(),
      // Channels are configured rows, so the list has to come from the server:
      // a hardcoded one offers names the backend rejects.
      api.listNotifiers(),
    ])
    return { tasks, connections, storages, channels }
  }, [taskId])

  const task = data?.tasks.find((t) => t.id === taskId)

  if (loading) return <Layout title="Редактирование задачи" subtitle="загрузка" onLogout={logout}><div style={MUTED}>Загрузка…</div></Layout>
  if (error) return <Layout title="Редактирование задачи" subtitle="ошибка" onLogout={logout}><div style={ERRSTYLE} role="alert">{error}</div></Layout>
  if (!task || !data) return <Layout title="Редактирование задачи" subtitle="не найдено" onLogout={logout}><div style={MUTED}>Задача не найдена.</div></Layout>

  return <TaskEditorLoaded task={task} connections={data.connections} storages={data.storages} channels={data.channels} onLogout={logout} onDone={() => navigate('/tasks')} />
}

function TaskEditorLoaded({
  task,
  connections,
  storages,
  channels,
  onLogout,
  onDone,
}: {
  task: Task
  connections: Connection[]
  storages: Storage[]
  channels: NotifierChannel[]
  onLogout: () => void
  onDone: () => void
}) {
  const { api } = useAuth()
  const [step, setStep] = useState(0)
  const [name, setName] = useState(task.name)
  const [connectionId, setConnectionId] = useState(task.connection_id)
  const [storageId, setStorageId] = useState(task.storage_id)
  const [cron, setCron] = useState(task.cron)
  const [codecs, setCodecs] = useState<string[]>(task.codec_chain ?? [])
  // The two rules are a CHOICE in the form, though the backend unions them: mode
  // decides which one is sent, and the other keeps its numbers so switching back
  // does not lose them.
  const [mode, setMode] = useState(modeOf(task.retention))
  const [keepLast, setKeepLast] = useState(keepLastFrom(task.retention))
  const [gfs, setGfs] = useState(gfsFrom(task.retention))
  const [notifiers, setNotifiers] = useState<string[]>(task.notifiers ?? [])
  // Names the task still carries that no longer exist as channels — e.g. a task
  // created when notifiers named plugin types. The backend rejects them on save,
  // so they are shown rather than dropped silently: removing one is the
  // operator's decision, not a side effect of opening the editor.
  const staleNotifiers = notifiers.filter((n) => !channels.some((c) => c.name === n))
  const [enabled, setEnabled] = useState(task.enabled)
  const [retries, setRetries] = useState(task.retries ?? 0)
  const [timeoutSec, setTimeoutSec] = useState(task.timeout_sec ?? 1800)
  // Empty input = 0 = derive the watchdog window from cron; -1 = off.
  const [watchdogSec, setWatchdogSec] = useState(task.watchdog_sec ?? 0)
  // `database` is a required dumper option, so it gets a dedicated field; the rest
  // of dumper_opts (exclude_table, format, …) stays in the advanced JSON box. We
  // split them on load and re-merge on save.
  const initOpts = task.dumper_opts && typeof task.dumper_opts === 'object' && !Array.isArray(task.dumper_opts)
    ? { ...(task.dumper_opts as Record<string, unknown>) }
    : {}
  const initDatabase = typeof initOpts.database === 'string' ? initOpts.database : ''
  const initAll = initOpts.all_databases === true
  const initExcludeSystem = initOpts.exclude_system === true
  delete initOpts.database
  delete initOpts.all_databases
  delete initOpts.exclude_system
  const [database, setDatabase] = useState(initDatabase)
  const [allDatabases, setAllDatabases] = useState(initAll)
  const [excludeSystem, setExcludeSystem] = useState(initExcludeSystem)
  const [dumperOpts, setDumperOpts] = useState(Object.keys(initOpts).length ? jsonText(initOpts) : '')
  const [err, setErr] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)

  const connection = connections.find((c) => c.id === connectionId)
  const storage = storages.find((s) => s.id === storageId)
  const engine = connection?.engine ?? ''
  // Same engine rules as the wizard: redis has no database at all, mongodb takes
  // one optionally (empty = every database).
  const hasDatabase = engine !== 'redis'
  const databaseRequired = engine === 'postgres' || engine === 'mysql' || engine === ''
  const connector = connection?.connector_type ?? 'direct'
  const isDockerProxy = connector === 'docker-proxy' || connector === 'docker-ssh-proxy'
  const isSSH = connector === 'ssh-tunnel' || connector === 'docker-ssh-proxy'
  const compression = compressionFromCodecs(codecs)
  const encryption = codecs.includes('age') ? 'age' : 'none'
  const artifactPreview = useMemo(() => {
    const safe = (name.trim() || 'task').replace(/\s+/g, '_')
    return `{task}/{db}/{ts}_${safe}${dumpExtFor(engine)}${extFor(compression)}${encryption === 'age' ? '.age' : ''}`
  }, [name, compression, encryption, engine])
  const retention = buildRetention(mode, keepLast, gfs)
  const summary = [
    ['задача', name || '—', 'var(--ink)'],
    ['connector', connector, 'var(--ink)'],
    ['движок', engine || '—', 'var(--ink)'],
    ['cron', cron || '—', 'var(--accent)'],
    ['хранилище', storage?.name ?? `#${storageId}`, 'var(--ink)'],
    ['обработка', [compression === 'none' ? null : compression, encryption === 'none' ? null : encryption].filter(Boolean).join(' → ') || '—', 'var(--ink)'],
    ['retention', formatPolicy(retention) || '—', 'var(--ink)'],
    ['уведомления', notifiers.join(', ') || 'нет', 'var(--ink)'],
  ] as const

  function toggleCodec(kind: 'compression' | 'encryption', value: string) {
    // Rebuild the whole chain from the two axes so ordering stays
    // "compress/archive → encrypt" regardless of click order.
    const nextComp = (kind === 'compression' ? value : compression) as CompressionKey
    const nextEnc = kind === 'encryption' ? value === 'age' : encryption === 'age'
    setCodecs(buildCodecChain(nextComp, nextEnc))
  }

  function toggleNotifier(value: string) {
    setNotifiers((cur) => cur.includes(value) ? cur.filter((x) => x !== value) : [...cur, value])
  }

  async function save() {
    setErr(null)
    let opts: Record<string, unknown> = {}
    if (dumperOpts.trim()) {
      try {
        const parsed = JSON.parse(dumperOpts)
        if (parsed && typeof parsed === 'object' && !Array.isArray(parsed)) {
          opts = parsed as Record<string, unknown>
        } else {
          setErr('Доп. параметры дампа должны быть JSON-объектом')
          setStep(0)
          return
        }
      } catch {
        setErr('Доп. параметры дампа: некорректный JSON')
        setStep(0)
        return
      }
    }
    if (!hasDatabase) {
      delete opts.database
      delete opts.all_databases
      delete opts.exclude_system
    } else if (allDatabases) {
      opts.all_databases = true
      if (excludeSystem) opts.exclude_system = true
      else delete opts.exclude_system
      delete opts.database
    } else if (database.trim()) {
      opts.database = database.trim()
    } else if (databaseRequired) {
      setErr('Укажите базу данных')
      setStep(0)
      return
    } else {
      delete opts.database
    }
    setBusy(true)
    try {
      await api.updateTask(task.id, {
        name: name.trim(),
        connection_id: connectionId,
        storage_id: storageId,
        cron: cron.trim(),
        dumper_opts: opts,
        codec_chain: codecs,
        retention,
        notifiers,
        enabled,
        retries,
        timeout_sec: timeoutSec,
        watchdog_sec: watchdogSec,
      })
      onDone()
    } catch (e) {
      setErr(e instanceof ApiError ? e.message : String(e))
    } finally {
      setBusy(false)
    }
  }

  async function remove() {
    setErr(null)
    try {
      await api.deleteTask(task.id)
      onDone()
    } catch (e) {
      setErr(e instanceof ApiError ? e.message : String(e))
      throw e
    }
  }

  return (
    <Layout
      title="Редактирование задачи"
      subtitle={`${name || task.name} · шаг ${step + 1} из ${STEPS.length}`}
      onLogout={onLogout}
      actions={<DeleteButton onDelete={remove} label="Удалить задачу" />}
    >
      <div className="dc-fade">
        <Stepper step={step} setStep={setStep} />
        <div style={{ display: 'grid', gridTemplateColumns: '1fr 320px', alignItems: 'start' }}>
          <div style={{ padding: '26px 30px', borderRight: '1px solid var(--line)' }}>
            <div style={{ maxWidth: 620 }}>
              {err && <div role="alert" style={ERR_BANNER}>{err}</div>}
              {step === 0 && (
                <>
                  <StepHead title="Тип БД и дамп" sub="Выберите драйвер СУБД (плагин) и параметры дампа." />
                  <Field label="Название задачи" htmlFor="te-name">
                    <input id="te-name" className="dc-input" style={{ ...inputStyle, padding: '10px 12px', fontSize: 13.5 }} value={name} onChange={(e) => setName(e.target.value)} />
                  </Field>
                  <div style={ROW_HEAD}><span>Драйвер СУБД (плагин)</span><span>{ENGINES.length} плагинов доступно</span></div>
                  <div style={CARD_GRID}>
                    {ENGINES.map(([key, label, sub, available]) => (
                      <PluginCard key={key} active={engine === key} disabled={!available || engine !== key} onClick={() => {}}>
                        <div style={{ display: 'flex', alignItems: 'center', gap: 7 }}><EngineDot engine={key} /><b>{label}</b></div>
                        <div style={SUB}>{sub}</div>
                      </PluginCard>
                    ))}
                    <PluginCard disabled><b>＋ Плагин</b><div style={SUB}>свой драйвер</div></PluginCard>
                  </div>
                  <div style={HINT}>Драйвер выбирается через соединение. Неподключенные плагины показаны, но отключены.</div>
                  {!hasDatabase && (
                    <div style={{ fontSize: 12, color: 'var(--ink-3)', margin: '4px 0 14px' }}>
                      Redis выгружается целиком (RDB-снимок) — базу указывать не нужно.
                    </div>
                  )}
                  {hasDatabase && (
                  <Field label="База данных" htmlFor="te-database">
                    <input id="te-database" className="dc-input" style={{ ...inputStyle, opacity: allDatabases ? 0.5 : 1 }} value={database} onChange={(e) => setDatabase(e.target.value)} placeholder={engine === 'mysql' ? 'app_production' : 'postgres'} disabled={allDatabases} />
                    {!databaseRequired && (
                      <div style={{ fontSize: 11.5, color: 'var(--ink-3)', marginTop: 8 }}>Пусто — выгрузить все базы инстанса.</div>
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
                  <div style={TWO_COL}>
                    <Field label="Формат дампа" htmlFor="te-format"><select id="te-format" className="dc-select" style={inputStyle} disabled><option>custom (-Fc)</option><option>plain (-Fp)</option><option>directory (-Fd)</option></select></Field>
                    <Field label="Параллельность (jobs)" htmlFor="te-jobs"><input id="te-jobs" className="dc-input" style={inputStyle} disabled value="4" readOnly /></Field>
                  </div>
                  <Field label="Исключить таблицы (--exclude-table)" htmlFor="te-opts">
                    <textarea id="te-opts" className="dc-input" style={{ ...inputStyle, minHeight: 74, resize: 'vertical' } as CSSProperties} value={dumperOpts} onChange={(e) => setDumperOpts(e.target.value)} placeholder='{"exclude_table":["audit_log","sessions"]}' />
                  </Field>
                </>
              )}
              {step === 1 && (
                <>
                  <StepHead title="Соединение" sub="Как воркер получит доступ к серверу БД." />
                  <Field label="Соединение" htmlFor="te-conn">
                    <select id="te-conn" className="dc-select" style={inputStyle} value={connectionId} onChange={(e) => setConnectionId(Number(e.target.value))}>
                      {connections.map((c) => <option key={c.id} value={c.id}>{c.name} ({c.engine} · {c.connector_type})</option>)}
                    </select>
                  </Field>
                  <div style={SECTION_LABEL}>Тип connector'а</div>
                  <div style={CARD_GRID_3}>
                    {CONNECTORS.map(([key, label, sub]) => <PluginCard key={key} active={connector === key} disabled={connector !== key}><b>{label}</b><div style={SUB}>{sub}</div></PluginCard>)}
                  </div>
                  {isDockerProxy && (
                    <Field label="Docker network" htmlFor="te-docker-network"><input id="te-docker-network" className="dc-input" style={inputStyle} disabled value={configString(connection?.connector_config, 'network')} readOnly /></Field>
                  )}
                  <div style={TWO_COL}>
                    <Field label={isDockerProxy ? 'Хост в Docker network' : 'Хост БД'} htmlFor="te-host"><input id="te-host" className="dc-input" style={inputStyle} disabled value={configString(connection?.connector_config, 'host') || configString(connection?.connector_config, 'remote_host') || configString(connection?.connector_config, 'target_host')} readOnly /></Field>
                    <Field label="Порт" htmlFor="te-port"><input id="te-port" className="dc-input" style={inputStyle} disabled value={configString(connection?.connector_config, 'port') || configString(connection?.connector_config, 'remote_port') || configString(connection?.connector_config, 'target_port')} readOnly /></Field>
                  </div>
                  <div style={SSH_BOX}>
                    <div style={{ fontSize: 11, fontWeight: 600, color: isSSH ? 'var(--accent)' : 'var(--ink-3)', marginBottom: 12, fontFamily: MONO }}>SSH-ТУННЕЛЬ</div>
                    <div style={{ ...THREE_COL, opacity: isSSH ? 1 : 0.45 }}>
                      <Field label="SSH хост" htmlFor="te-ssh-host"><input id="te-ssh-host" className="dc-input" style={inputStyle} disabled value={configString(connection?.connector_config, 'ssh_host')} readOnly /></Field>
                      <Field label="Порт" htmlFor="te-ssh-port"><input id="te-ssh-port" className="dc-input" style={inputStyle} disabled value={configString(connection?.connector_config, 'ssh_port') || '22'} readOnly /></Field>
                      <Field label="Пользователь" htmlFor="te-ssh-user"><input id="te-ssh-user" className="dc-input" style={inputStyle} disabled value={configString(connection?.connector_config, 'ssh_user')} readOnly /></Field>
                    </div>
                    <Field label="SSH-ключ (из секретов)" htmlFor="te-ssh-key"><input id="te-ssh-key" className="dc-input" style={inputStyle} disabled value={configString(connection?.connector_config, 'private_key_ref')} readOnly /></Field>
                  </div>
                </>
              )}
              {step === 2 && (
                <>
                  <StepHead title="Расписание и исполнение" sub="Cron-выражение и политики запуска." />
                  <Field label="Cron-выражение" htmlFor="te-cron"><input id="te-cron" className="dc-input" style={{ ...inputStyle, padding: '11px 13px', fontSize: 15, borderColor: 'var(--accent)', letterSpacing: '0.05em' }} value={cron} onChange={(e) => setCron(e.target.value)} /></Field>
                  <div style={{ display: 'flex', gap: 8, margin: '-4px 0 22px', flexWrap: 'wrap' }}>{CRON_PRESETS.map(([label, value]) => <button key={value} type="button" onClick={() => setCron(value)} className="dc-reset dc-h-accent-ink" style={CHIP}>{label}</button>)}</div>
                  <div style={TWO_COL}>
                    <Field label="Misfire policy" htmlFor="te-misfire"><div style={{ display: 'flex', gap: 8 }}><DisabledPill active>run_once_now</DisabledPill><DisabledPill>skip</DisabledPill></div></Field>
                    <Field label="Таймаут run'а" htmlFor="te-timeout"><input id="te-timeout" className="dc-input" style={inputStyle} value={timeoutSec} onChange={(e) => setTimeoutSec(Number(e.target.value))} /></Field>
                  </div>
                  <div style={TWO_COL}>
                    <Field label="Ретраи (попыток)" htmlFor="te-retries"><input id="te-retries" className="dc-input" style={inputStyle} value={retries} onChange={(e) => setRetries(Number(e.target.value))} /></Field>
                    <Field label="Backoff" htmlFor="te-backoff"><input id="te-backoff" className="dc-input" style={inputStyle} disabled value="exp, 30s → 8m" readOnly /></Field>
                  </div>
                  <div style={TWO_COL}>
                    <Field label="Watchdog: порог без успешного бэкапа" htmlFor="te-watchdog">
                      <select id="te-watchdog" className="dc-select" style={inputStyle} value={String(watchdogSec)} onChange={(e) => setWatchdogSec(Number(e.target.value))}>
                        {watchdogOptions(watchdogSec).map(([label, value]) => <option key={value} value={value}>{label}</option>)}
                      </select>
                    </Field>
                    <div style={{ alignSelf: 'end', paddingBottom: 9, fontSize: 11.5, color: 'var(--ink-3)' }}>
                      Авто — два пропущенных окна расписания, минимум 1 ч.
                    </div>
                  </div>
                </>
              )}
              {step === 3 && (
                <>
                  <StepHead title="Хранилище и обработка" sub="Куда положить артефакт и как его сжать/зашифровать." />
                  <div style={SECTION_LABEL}>Хранилище</div>
                  <div style={{ display: 'flex', flexDirection: 'column', gap: 8, marginBottom: 22 }}>
                    {storages.map((s) => <OptionRow key={s.id} active={storageId === s.id} onClick={() => setStorageId(s.id)} label={s.name} sub={s.type} />)}
                    <OptionRow disabled label="sftp://offsite-nas" sub="SFTP · плагин не подключен" />
                  </div>
                  <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 22 }}>
                    <div><div style={SECTION_LABEL}>Сжатие / формат</div>{COMPRESSION_PRESETS.map((p) => <OptionRow key={p.key} active={compression === p.key} onClick={() => toggleCodec('compression', p.key)} label={p.label} sub={p.sub} />)}</div>
                    <div><div style={SECTION_LABEL}>Шифрование</div>{ENCRYPTION.map(([key, label, sub, available]) => <OptionRow key={key} active={encryption === key} disabled={!available} onClick={() => toggleCodec('encryption', key)} label={label} sub={sub} />)}</div>
                  </div>
                  <div style={ARTIFACT_BOX}><span style={{ color: 'var(--ink-3)' }}>имя артефакта →</span> {artifactPreview}</div>
                </>
              )}
              {step === 4 && (
                <>
                  <StepHead title="Хранение и уведомления" sub="Политики retention и каналы оповещений." />
                  {/* Only the API can set both rules at once. Saving through the
                      form drops one, so the loss is announced before it happens
                      rather than discovered in the next sweep. */}
                  {bothRules(task.retention) && (
                    <div style={{ fontSize: 11.5, color: 'var(--warn)', marginBottom: 12 }}>
                      У задачи заданы оба правила ({formatPolicy(task.retention)}). Здесь можно выбрать только одно — при сохранении второе будет снято.
                    </div>
                  )}
                  <PolicyRow title="keep_last" sub="хранить последние N артефактов" enabled={mode === 'keep_last'} onToggle={() => setMode(mode === 'keep_last' ? 'none' : 'keep_last')}>
                    <input aria-label="keep_last" disabled={mode !== 'keep_last'} className="dc-input" style={{ ...inputStyle, width: 60, textAlign: 'center', padding: 7 }} value={keepLast} onChange={(e) => setKeepLast((cur) => clampCount(e.target.value, cur))} />
                  </PolicyRow>
                  <PolicyRow title="GFS (grandfather-father-son)" sub="дневные / недельные / месячные" enabled={mode === 'gfs'} onToggle={() => setMode(mode === 'gfs' ? 'none' : 'gfs')}>
                    {GFS_FIELDS.map(([key, label]) => (
                      <GfsInput key={key} label={label} value={gfs[key]} disabled={mode !== 'gfs'} onChange={(v) => setGfs((cur) => ({ ...cur, [key]: v }))} />
                    ))}
                  </PolicyRow>
                  {/* Neither rule is a legitimate choice — it just has to be a
                      stated one, since nothing will ever be pruned. */}
                  {formatPolicy(retention) === '' && (
                    <div style={{ fontSize: 11.5, color: 'var(--warn)', marginTop: -6, marginBottom: 14 }}>
                      Ни одно правило не включено — артефакты этой задачи не будут удаляться.
                    </div>
                  )}
                  <div style={SECTION_LABEL}>Уведомления</div>
                  <div style={{ display: 'flex', flexDirection: 'column', gap: 8 }}>
                    {channels.length === 0 && (
                      <div style={{ fontSize: 12.5, color: 'var(--ink-3)' }}>
                        Каналов пока нет — создайте их на странице «Уведомления».
                      </div>
                    )}
                    {channels.map((c) => (
                      <NotifyRow key={c.id} label={c.name} detail={c.enabled ? c.type : `${c.type} · выключен`} enabled={notifiers.includes(c.name)} onClick={() => toggleNotifier(c.name)} />
                    ))}
                    {staleNotifiers.map((n) => (
                      <NotifyRow key={n} label={n} detail="канала больше нет — снимите, чтобы сохранить" enabled stale onClick={() => toggleNotifier(n)} />
                    ))}
                  </div>
                  <label style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: 10, color: 'var(--ink-2)', fontSize: 12, marginTop: 16 }}>
                    <span>Задача включена</span><input type="checkbox" checked={enabled} onChange={(e) => setEnabled(e.target.checked)} />
                  </label>
                </>
              )}
              {step === 5 && (
                <>
                  <StepHead title="Обзор" sub="Проверьте конфигурацию задачи перед сохранением." />
                  <div style={{ border: '1px solid var(--line)', borderRadius: 10, overflow: 'hidden' }}>
                    {summary.map(([k, v]) => <div key={k} style={REVIEW_ROW}><span>{k}</span><b>{v}</b></div>)}
                  </div>
                  <div style={OK_BANNER}><span style={DOT_OK} />Внешние утилиты проверяются сервером при сохранении задачи.</div>
                </>
              )}
              <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginTop: 30 }}>
                <button type="button" onClick={() => setStep((s) => Math.max(0, s - 1))} style={{ ...btnSecondary, visibility: step === 0 ? 'hidden' : 'visible' }}>← Назад</button>
                {step < STEPS.length - 1 ? <button type="button" onClick={() => setStep((s) => Math.min(STEPS.length - 1, s + 1))} style={btnPrimary}>Далее →</button> : <button type="button" onClick={save} disabled={busy} style={{ ...btnPrimary, opacity: busy ? 0.6 : 1 }}>{busy ? 'Сохранение…' : 'Сохранить изменения'}</button>}
              </div>
            </div>
          </div>
          <div style={{ padding: '22px', position: 'sticky', top: 0 }}>
            <div style={{ fontSize: 11, fontWeight: 600, letterSpacing: '0.06em', color: 'var(--ink-3)', marginBottom: 14, fontFamily: MONO }}>КОНФИГУРАЦИЯ</div>
            {summary.map(([k, v, color]) => <div key={k} style={SUMMARY_ROW}><span>{k}</span><b style={{ color }}>{v}</b></div>)}
          </div>
        </div>
      </div>
    </Layout>
  )
}

function Stepper({ step, setStep }: { step: number; setStep: (n: number) => void }) {
  return <div style={{ display: 'flex', borderBottom: '1px solid var(--line)', background: 'var(--panel)' }}>{STEPS.map((label, i) => {
    const active = i === step
    const done = i < step
    return <div key={label} onClick={() => setStep(i)} style={{ flex: 1, padding: '14px 16px', borderLeft: i ? '1px solid var(--line)' : 'none', cursor: 'pointer', display: 'flex', alignItems: 'center', gap: 10, minWidth: 0 }}>
      <div style={{ width: 22, height: 22, borderRadius: '50%', flexShrink: 0, display: 'flex', alignItems: 'center', justifyContent: 'center', fontFamily: MONO, fontSize: 11, fontWeight: 600, background: active ? 'var(--accent)' : done ? 'var(--ok-bg)' : 'var(--panel-2)', color: active ? 'var(--accent-ink)' : done ? 'var(--ok)' : 'var(--ink-3)', border: `1px solid ${active ? 'var(--accent)' : done ? 'var(--ok)' : 'var(--line-2)'}` }}>{done ? '✓' : i + 1}</div>
      <div style={{ fontSize: 12.5, fontWeight: 600, color: active ? 'var(--ink)' : done ? 'var(--ink-2)' : 'var(--ink-3)', whiteSpace: 'nowrap', overflow: 'hidden', textOverflow: 'ellipsis' }}>{label}</div>
    </div>
  })}</div>
}

function StepHead({ title, sub }: { title: string; sub: string }) {
  return <><div style={{ fontSize: 16, fontWeight: 700, color: 'var(--ink)', marginBottom: 4 }}>{title}</div><div style={{ fontSize: 12.5, color: 'var(--ink-2)', marginBottom: 22 }}>{sub}</div></>
}

function Field({ label, htmlFor, children }: { label: string; htmlFor: string; children: ReactNode }) {
  return <div style={{ marginBottom: 16 }}><label style={labelStyle} htmlFor={htmlFor}>{label}</label>{children}</div>
}

function PluginCard({ active, disabled, onClick, children }: { active?: boolean; disabled?: boolean; onClick?: () => void; children: ReactNode }) {
  return <OptionCard active={Boolean(active)} onClick={disabled ? undefined : onClick} style={{ position: 'relative', padding: 14, borderRadius: 9, opacity: disabled && !active ? 0.45 : 1, cursor: disabled ? 'default' : 'pointer' }}>{children}</OptionCard>
}

function OptionRow({ active, disabled, onClick, label, sub }: { active?: boolean; disabled?: boolean; onClick?: () => void; label: string; sub: string }) {
  return <div onClick={disabled ? undefined : onClick} style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', padding: '10px 12px', borderRadius: 7, cursor: disabled ? 'default' : 'pointer', border: `1px solid ${active ? 'var(--accent)' : 'var(--line-2)'}`, background: active ? 'var(--run-bg)' : 'var(--panel)', opacity: disabled && !active ? 0.45 : 1, marginBottom: 8 }}><span style={{ fontSize: 12.5, color: active ? 'var(--ink)' : 'var(--ink-2)' }}>{label}</span><span style={{ fontFamily: MONO, fontSize: 11, color: 'var(--ink-3)' }}>{sub}</span></div>
}

function DisabledPill({ active, children }: { active?: boolean; children: ReactNode }) {
  return <div style={{ flex: 1, textAlign: 'center', padding: 9, borderRadius: 7, fontFamily: MONO, fontSize: 12, border: `1px solid ${active ? 'var(--accent)' : 'var(--line-2)'}`, background: active ? 'var(--run-bg)' : 'var(--panel)', color: active ? 'var(--ink)' : 'var(--ink-2)', opacity: active ? 1 : 0.5 }}>{children}</div>
}

// GfsInput is one GFS bucket count. The label sits above a narrow box so the
// three read as "7 / 4 / 12" — the notation the retention page prints back.
function GfsInput({ label, value, disabled, onChange }: { label: string; value: number; disabled?: boolean; onChange: (v: number) => void }) {
  return (
    <label style={{ display: 'flex', flexDirection: 'column', alignItems: 'center', gap: 3 }}>
      <span style={{ fontSize: 9.5, fontFamily: MONO, letterSpacing: '0.04em', color: 'var(--ink-3)' }}>{label}</span>
      <input
        aria-label={label}
        disabled={disabled}
        className="dc-input"
        style={{ ...inputStyle, width: 52, textAlign: 'center', padding: 7 }}
        value={value}
        onChange={(e) => onChange(clampCount(e.target.value, value))}
      />
    </label>
  )
}

// PolicyRow is one retention rule. Its switch is a real control: the two rules
// are mutually exclusive here, so turning one on turns the other off, and
// turning the active one off leaves the task unpruned. `disabled` greys a rule
// that cannot be chosen at all; the inputs of an inactive-but-choosable rule
// stay visible with their numbers, so switching back does not retype them.
function PolicyRow({ title, sub, enabled, disabled, onToggle, children }: { title: string; sub: string; enabled?: boolean; disabled?: boolean; onToggle?: () => void; children: ReactNode }) {
  return <div style={{ border: `1px solid ${enabled ? 'var(--accent)' : 'var(--line)'}`, borderRadius: 9, padding: 16, marginBottom: 14, background: 'var(--panel)', opacity: disabled ? 0.55 : 1 }}><div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: 14 }}><div><div style={{ fontSize: 13, fontWeight: 600, color: 'var(--ink)' }}>{title}</div><div style={{ fontSize: 11.5, color: 'var(--ink-3)', marginTop: 2 }}>{sub}</div></div><div style={{ display: 'flex', alignItems: 'center', gap: 12 }}>{children}{onToggle && !disabled ? <button type="button" className="dc-reset" role="switch" aria-checked={Boolean(enabled)} aria-label={`Правило: ${title}`} onClick={onToggle}><ToggleView on={Boolean(enabled)} /></button> : <ToggleView on={Boolean(enabled)} />}</div></div></div>
}

function NotifyRow({ label, detail, enabled, disabled, stale, onClick }: { label: string; detail: string; enabled: boolean; disabled?: boolean; stale?: boolean; onClick: () => void }) {
  return <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', padding: '11px 14px', borderRadius: 8, border: `1px solid ${stale ? 'var(--err)' : 'var(--line)'}`, background: 'var(--panel)', opacity: disabled ? 0.5 : 1 }}><div style={{ display: 'flex', alignItems: 'center', gap: 10, minWidth: 0 }}><div style={{ width: 7, height: 7, borderRadius: '50%', background: stale ? 'var(--err)' : enabled ? 'var(--ok)' : 'var(--ink-3)' }} /><span style={{ fontSize: 13, color: 'var(--ink)', fontWeight: 500 }}>{label}</span><span style={{ fontSize: 11, color: stale ? 'var(--err)' : 'var(--ink-3)', fontFamily: MONO, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{detail}</span></div><button type="button" className="dc-reset" role="switch" aria-checked={enabled} aria-label={label} onClick={disabled ? undefined : onClick}><ToggleView on={enabled} /></button></div>
}

function ToggleView({ on }: { on: boolean }) {
  return <div style={{ width: 34, height: 20, borderRadius: 20, padding: 2, cursor: 'pointer', background: on ? 'var(--accent)' : 'var(--line-2)' }}><div style={{ width: 16, height: 16, borderRadius: '50%', background: 'oklch(1 0 0)', transform: `translateX(${on ? 14 : 0}px)`, transition: 'transform 0.15s' }} /></div>
}

function jsonText(v: unknown): string {
  return v == null ? '' : JSON.stringify(v, null, 2)
}

function configString(config: unknown, key: string): string {
  if (!config || typeof config !== 'object') return ''
  const v = (config as Record<string, unknown>)[key]
  return v == null ? '' : String(v)
}

const MUTED = { padding: '40px 26px', color: 'var(--ink-3)', fontSize: 13 } as const
const ERRSTYLE = { padding: '40px 26px', color: 'var(--err)', fontSize: 13 } as const
const ERR_BANNER: CSSProperties = { color: 'var(--err)', fontSize: 12.5, marginBottom: 16, padding: '10px 12px', background: 'var(--err-bg)', borderRadius: 7 }
const ROW_HEAD: CSSProperties = { display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: 10, fontSize: 11, fontWeight: 600, color: 'var(--ink-2)' }
const CARD_GRID: CSSProperties = { display: 'grid', gridTemplateColumns: 'repeat(3, 1fr)', gap: 10, marginBottom: 10 }
const CARD_GRID_3: CSSProperties = { display: 'grid', gridTemplateColumns: 'repeat(3, 1fr)', gap: 10, marginBottom: 22 }
const TWO_COL: CSSProperties = { display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 14, marginBottom: 16 }
const THREE_COL: CSSProperties = { display: 'grid', gridTemplateColumns: '2fr 1fr 1fr', gap: 14, marginBottom: 14 }
const SSH_BOX: CSSProperties = { borderTop: '1px solid var(--line)', paddingTop: 16, marginTop: 4 }
const HINT: CSSProperties = { fontSize: 11, color: 'var(--ink-3)', marginBottom: 22 }
const SUB: CSSProperties = { fontFamily: MONO, fontSize: 11, color: 'var(--ink-3)', marginTop: 6 }
const SECTION_LABEL: CSSProperties = { fontSize: 11, fontWeight: 600, color: 'var(--ink-2)', marginBottom: 10 }
const CHIP: CSSProperties = { padding: '6px 12px', borderRadius: 6, fontSize: 11.5, cursor: 'pointer', border: '1px solid var(--line-2)', background: 'var(--panel)', color: 'var(--ink-2)' }
const ARTIFACT_BOX: CSSProperties = { marginTop: 22, padding: '12px 14px', borderRadius: 8, background: 'var(--sidebar)', border: '1px solid var(--line)', fontFamily: MONO, fontSize: 11.5, color: 'var(--ink-2)' }
const REVIEW_ROW: CSSProperties = { display: 'flex', alignItems: 'center', justifyContent: 'space-between', padding: '13px 18px', borderBottom: '1px solid var(--line)', background: 'var(--panel)', fontFamily: MONO, fontSize: 12.5, color: 'var(--ink)' }
const OK_BANNER: CSSProperties = { display: 'flex', alignItems: 'center', gap: 8, marginTop: 16, padding: '12px 14px', borderRadius: 8, background: 'var(--ok-bg)', fontSize: 12.5, color: 'var(--ok)' }
const DOT_OK: CSSProperties = { width: 7, height: 7, borderRadius: '50%', background: 'var(--ok)' }
const SUMMARY_ROW: CSSProperties = { display: 'flex', alignItems: 'center', justifyContent: 'space-between', padding: '9px 0', borderBottom: '1px solid var(--line)', gap: 10, fontSize: 11.5, color: 'var(--ink-3)' }
