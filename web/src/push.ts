import type { ApiClient } from './api/client'
import type { PushSubscriptionJSONBody, PushUnavailableReason } from './api/types'

// Browser side of Web Push: registering the service worker, asking for
// permission, and keeping the server's copy of this browser's subscription in
// step with the browser's own.

const SW_URL = '/sw.js'

/**
 * What this browser can do, independent of the server:
 * - ok: push works here;
 * - insecure: the page is not a secure context (plain HTTP off localhost);
 * - ios-install: iPhone/iPad, where push needs the site added to the Home Screen;
 * - unsupported: no Push API at all.
 */
export type PushSupport = 'ok' | 'insecure' | 'ios-install' | 'unsupported'

function isIOS(): boolean {
  const ua = navigator.userAgent
  // iPadOS reports itself as a Mac; touch support gives it away.
  return /iPad|iPhone|iPod/.test(ua) || (/Macintosh/.test(ua) && navigator.maxTouchPoints > 1)
}

function isStandalone(): boolean {
  return (
    window.matchMedia?.('(display-mode: standalone)').matches === true ||
    (navigator as Navigator & { standalone?: boolean }).standalone === true
  )
}

export function pushSupport(): PushSupport {
  if (!window.isSecureContext) return 'insecure'
  const api = 'serviceWorker' in navigator && 'PushManager' in window && 'Notification' in window
  // Safari on iOS only exposes PushManager to a Home Screen web app.
  if (isIOS() && !isStandalone()) return 'ios-install'
  return api ? 'ok' : 'unsupported'
}

/** The browser-level permission, or 'default' where there is no Notification API. */
export function pushPermission(): NotificationPermission {
  return 'Notification' in window ? Notification.permission : 'default'
}

export class PushPermissionError extends Error {
  constructor() {
    super('Браузер не разрешил уведомления')
    this.name = 'PushPermissionError'
  }
}

/** base64url → bytes, as PushManager.subscribe wants the server key. */
export function b64urlToBytes(s: string): Uint8Array<ArrayBuffer> {
  const b64 = s.replace(/-/g, '+').replace(/_/g, '/').padEnd(Math.ceil(s.length / 4) * 4, '=')
  const raw = atob(b64)
  const out = new Uint8Array(raw.length)
  for (let i = 0; i < raw.length; i++) out[i] = raw.charCodeAt(i)
  return out
}

function bytesEqual(a: ArrayBuffer | null | undefined, b: Uint8Array): boolean {
  if (!a || a.byteLength !== b.length) return false
  const v = new Uint8Array(a)
  return v.every((x, i) => x === b[i])
}

function toBody(sub: PushSubscription): PushSubscriptionJSONBody {
  const j = sub.toJSON()
  return { endpoint: j.endpoint ?? sub.endpoint, keys: { p256dh: j.keys?.p256dh ?? '', auth: j.keys?.auth ?? '' } }
}

async function registration(): Promise<ServiceWorkerRegistration | undefined> {
  if (!('serviceWorker' in navigator)) return undefined
  return navigator.serviceWorker.getRegistration('/')
}

/** This browser's current push subscription, if it has one. */
export async function currentSubscription(): Promise<PushSubscription | null> {
  const reg = await registration()
  return reg ? reg.pushManager.getSubscription() : null
}

/**
 * subscribeWithKey returns a subscription bound to publicKey. One bound to a
 * different key — the instance's key changed, or the browser was last used
 * with another Duskrun on this origin — is replaced, since the push service
 * would refuse every message signed with the new key.
 */
async function subscribeWithKey(reg: ServiceWorkerRegistration, publicKey: string): Promise<PushSubscription> {
  const key = b64urlToBytes(publicKey)
  const existing = await reg.pushManager.getSubscription()
  if (existing && bytesEqual(existing.options.applicationServerKey, key)) return existing
  if (existing) await existing.unsubscribe()
  return reg.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: key })
}

/**
 * enablePush turns notifications on for this browser. It must run from a click
 * handler: the permission prompt is only allowed in response to the user, and
 * Safari loses that after the first await — so it is asked for first.
 */
export async function enablePush(api: ApiClient, publicKey: string): Promise<PushSubscription> {
  const permission = await Notification.requestPermission()
  if (permission !== 'granted') throw new PushPermissionError()
  await navigator.serviceWorker.register(SW_URL, { scope: '/' })
  const reg = await navigator.serviceWorker.ready
  const sub = await subscribeWithKey(reg, publicKey)
  await api.subscribePush(toBody(sub))
  return sub
}

/**
 * disablePush stops notifications to this browser: the server forgets the
 * subscription first (it needs the session), then the browser drops it. A
 * failure on the server side still unsubscribes locally — the push service
 * then answers 410 and the server cleans up on its next delivery.
 */
export async function disablePush(api: ApiClient): Promise<void> {
  const sub = await currentSubscription()
  if (!sub) return
  try {
    await api.unsubscribePush(sub.endpoint)
  } finally {
    await sub.unsubscribe()
  }
}

/**
 * syncPush re-reports this browser's subscription to the server. Browsers
 * rotate subscriptions on their own, and the service worker cannot report the
 * new one (the session token lives in the page), so the page does it whenever
 * it opens. It also hands the browser over when a different user signs in to
 * it. Never prompts; does nothing for a browser that never enabled push.
 */
export async function syncPush(api: ApiClient): Promise<void> {
  if (pushSupport() !== 'ok' || pushPermission() !== 'granted') return
  const reg = await registration()
  if (!reg || !(await reg.pushManager.getSubscription())) return
  const status = await api.pushStatus()
  if (!status.available || !status.public_key) return
  const sub = await subscribeWithKey(reg, status.public_key)
  await api.subscribePush(toBody(sub))
}

/** Human wording for a server that cannot offer push. */
export function unavailableText(reason?: PushUnavailableReason): string {
  switch (reason) {
    case 'insecure_origin':
      return 'DUSKRUN_PUBLIC_URL должен быть https:// — браузеры дают push только защищённым сайтам.'
    case 'no_master_key':
      return 'Нет мастер-ключа для хранения ключа подписи.'
    case 'key_unreadable':
      return 'Сохранённый ключ подписи не открывается текущим мастер-ключом.'
    default:
      return 'Задайте DUSKRUN_PUBLIC_URL (HTTPS), чтобы включить push-уведомления.'
  }
}

/** Human wording for a browser that cannot receive push. */
export function supportText(s: PushSupport): string {
  switch (s) {
    case 'insecure':
      return 'Страница открыта не по HTTPS — браузер не даст подписаться.'
    case 'ios-install':
      return 'На iPhone и iPad уведомления работают после «Поделиться → На экран „Домой“»: откройте Duskrun оттуда.'
    case 'unsupported':
      return 'Этот браузер не поддерживает push-уведомления.'
    default:
      return ''
  }
}

/** "Chrome · macOS" from a User-Agent string; good enough to tell devices apart. */
export function describeUserAgent(ua: string): string {
  if (!ua) return 'неизвестный браузер'
  const browser = /Edg\//.test(ua)
    ? 'Edge'
    : /OPR\//.test(ua)
      ? 'Opera'
      : /YaBrowser\//.test(ua)
        ? 'Яндекс Браузер'
        : /Firefox\//.test(ua)
          ? 'Firefox'
          : /Chrome\//.test(ua)
            ? 'Chrome'
            : /Safari\//.test(ua)
              ? 'Safari'
              : 'браузер'
  const os = /iPhone/.test(ua)
    ? 'iPhone'
    : /iPad/.test(ua)
      ? 'iPad'
      : /Android/.test(ua)
        ? 'Android'
        : /Mac OS X|Macintosh/.test(ua)
          ? 'macOS'
          : /Windows/.test(ua)
            ? 'Windows'
            : /Linux/.test(ua)
              ? 'Linux'
              : ''
  return os ? `${browser} · ${os}` : browser
}
