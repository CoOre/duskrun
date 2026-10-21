import { CSSProperties, useEffect, useState } from 'react'
import { ApiError } from '../api/client'
import { useApiData } from '../api/useApiData'
import { fmtDateTime } from '../api/view'
import type { PushDevice, PushStatus } from '../api/types'
import { useAuth } from '../auth'
import {
  currentSubscription,
  describeUserAgent,
  disablePush,
  enablePush,
  PushPermissionError,
  pushPermission,
  pushSupport,
  supportText,
  unavailableText,
} from '../push'
import { MONO, btnDanger, btnPrimary, btnSecondary } from './dc'

const PANEL: CSSProperties = { border: '1px solid var(--line)', borderRadius: 10, background: 'var(--panel)', marginBottom: 26 }
const ROW: CSSProperties = { display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: 16, padding: '14px 20px' }
const SMALL_BTN: CSSProperties = { ...btnSecondary, padding: '5px 10px', fontSize: 11.5 }
const HINT: CSSProperties = { fontSize: 11.5, color: 'var(--ink-3)', lineHeight: 1.5 }

type Data = { status: PushStatus; devices: PushDevice[] }

function message(e: unknown): string {
  if (e instanceof PushPermissionError) {
    return pushPermission() === 'denied'
      ? 'Уведомления для этого сайта заблокированы в браузере — разрешите их в настройках сайта и попробуйте снова.'
      : 'Браузер не разрешил уведомления.'
  }
  if (e instanceof ApiError && e.status === 410) return 'Браузер больше не подписан — подписка удалена.'
  return e instanceof Error ? e.message : String(e)
}

// PushSettings is the caller's own browser-notification panel: switching push
// on for this browser and managing every browser that receives it. Which
// events arrive is decided by the push channels on the notifications page.
export function PushSettings() {
  const { api } = useAuth()
  const [reload, setReload] = useState(0)
  const [busy, setBusy] = useState(false)
  const [banner, setBanner] = useState<{ ok: boolean; text: string } | null>(null)
  // This browser's endpoint, read from the browser itself: it is what tells
  // "this browser" apart in the device list.
  const [endpoint, setEndpoint] = useState<string | null>(null)
  const { data, error, loading } = useApiData<Data>(
    async () => {
      const [status, devices] = await Promise.all([api.pushStatus(), api.listPushDevices()])
      return { status, devices }
    },
    [api, reload],
  )

  useEffect(() => {
    let alive = true
    currentSubscription()
      .then((s) => alive && setEndpoint(s?.endpoint ?? null))
      .catch(() => undefined)
    return () => {
      alive = false
    }
  }, [reload])

  async function act(fn: () => Promise<void>, ok: string) {
    setBusy(true)
    setBanner(null)
    try {
      await fn()
      setBanner({ ok: true, text: ok })
    } catch (e) {
      setBanner({ ok: false, text: message(e) })
    } finally {
      setBusy(false)
      setReload((n) => n + 1)
    }
  }

  if (loading && !data) return <div style={{ ...PANEL, padding: 20, fontSize: 12.5, color: 'var(--ink-3)' }}>Загрузка…</div>
  if (error || !data) {
    return (
      <div style={{ ...PANEL, padding: 20, fontSize: 12.5, color: 'var(--err)' }}>
        Не удалось загрузить настройки уведомлений: {error}
      </div>
    )
  }

  const support = pushSupport()
  const here = data.devices.find((d) => d.endpoint === endpoint)
  const on = !!here
  const blocked = pushPermission() === 'denied'
  const canEnable = data.status.available && support === 'ok' && !blocked

  const hint = !data.status.available
    ? unavailableText(data.status.reason)
    : support !== 'ok'
      ? supportText(support)
      : blocked
        ? 'Уведомления для этого сайта заблокированы в браузере. Разрешите их в настройках сайта (значок слева от адреса).'
        : on
          ? 'Сюда приходят события push-каналов, на которые подписана ваша роль, даже когда вкладка закрыта.'
          : 'Системные уведомления о сбоях бэкапов, даже когда вкладка Duskrun закрыта.'

  return (
    <div>
      {banner && (
        <div role={banner.ok ? 'status' : 'alert'} style={{ color: banner.ok ? 'var(--ok)' : 'var(--err)', fontSize: 12.5, marginBottom: 12 }}>
          {banner.text}
        </div>
      )}
      <div style={PANEL}>
        <div style={{ ...ROW, borderBottom: data.devices.length > 0 ? '1px solid var(--line)' : 'none' }}>
          <div style={{ minWidth: 0 }}>
            <div style={{ fontSize: 12.5, color: 'var(--ink)', fontWeight: 500 }}>
              Этот браузер
              <OnTag on={on} />
            </div>
            <div style={HINT}>{hint}</div>
          </div>
          <div style={{ display: 'flex', gap: 6, flexShrink: 0 }}>
            {on && here ? (
              <>
                <button type="button" style={SMALL_BTN} disabled={busy} onClick={() => act(() => api.testPushDevice(here.id), 'Тестовое уведомление отправлено')}>
                  Отправить тест
                </button>
                <button type="button" style={SMALL_BTN} disabled={busy} onClick={() => act(() => disablePush(api), 'Уведомления в этом браузере выключены')}>
                  Выключить
                </button>
              </>
            ) : (
              <button
                type="button"
                style={{ ...btnPrimary, padding: '5px 12px', fontSize: 11.5, opacity: canEnable && !busy ? 1 : 0.5 }}
                disabled={!canEnable || busy}
                onClick={() =>
                  act(async () => {
                    await enablePush(api, data.status.public_key ?? '')
                  }, 'Уведомления в этом браузере включены')
                }
              >
                Включить в этом браузере
              </button>
            )}
          </div>
        </div>

        {data.devices.length > 0 && (
          <div style={{ padding: '4px 20px 8px' }}>
            {data.devices.map((d, i) => {
              const current = d.endpoint === endpoint
              return (
                <div
                  key={d.id}
                  style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: 12, padding: '8px 0', borderTop: i === 0 ? 'none' : '1px dashed var(--line)' }}
                >
                  <div style={{ minWidth: 0 }}>
                    <div style={{ fontSize: 12.5, color: 'var(--ink)' }}>
                      {describeUserAgent(d.user_agent)}
                      {current && <span style={{ marginLeft: 6, fontSize: 11, color: 'var(--ink-3)' }}>(этот браузер)</span>}
                    </div>
                    <div style={{ fontSize: 11, color: 'var(--ink-3)', fontFamily: MONO }}>
                      {d.service} · добавлен {fmtDateTime(d.created_at)} ·{' '}
                      {d.last_success_at ? `доставлено ${fmtDateTime(d.last_success_at)}` : 'доставок ещё не было'}
                    </div>
                    {d.last_error && (
                      <div style={{ fontSize: 11, color: 'var(--err)', fontFamily: MONO, overflowWrap: 'anywhere' }}>
                        ошибка{d.fail_count > 1 ? ` ×${d.fail_count}` : ''}: {d.last_error}
                      </div>
                    )}
                  </div>
                  <div style={{ display: 'flex', gap: 6, flexShrink: 0 }}>
                    {!current && (
                      <button
                        type="button"
                        aria-label={`Тест на ${describeUserAgent(d.user_agent)}`}
                        style={{ ...SMALL_BTN, opacity: data.status.available ? 1 : 0.5 }}
                        disabled={busy || !data.status.available}
                        onClick={() => act(() => api.testPushDevice(d.id), 'Тестовое уведомление отправлено')}
                      >
                        Тест
                      </button>
                    )}
                    <button
                      type="button"
                      aria-label={`Удалить ${describeUserAgent(d.user_agent)}`}
                      style={{ ...btnDanger, padding: '4px 10px', fontSize: 11.5 }}
                      disabled={busy}
                      onClick={() =>
                        // Removing this browser also unsubscribes it locally,
                        // or the next page load would quietly re-register it.
                        act(() => (current ? disablePush(api) : api.deletePushDevice(d.id)), 'Устройство удалено')
                      }
                    >
                      Удалить
                    </button>
                  </div>
                </div>
              )
            })}
          </div>
        )}
      </div>
    </div>
  )
}

function OnTag({ on }: { on: boolean }) {
  return (
    <span style={{ marginLeft: 8, fontSize: 10.5, fontFamily: MONO, color: on ? 'var(--ok)' : 'var(--ink-3)', border: `1px solid ${on ? 'var(--ok)' : 'var(--line-2)'}`, borderRadius: 5, padding: '1px 6px' }}>
      {on ? 'включено' : 'выключено'}
    </span>
  )
}
