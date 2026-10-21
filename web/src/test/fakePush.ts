import { vi } from 'vitest'

// installFakePush gives jsdom the browser half of Web Push: a secure context,
// Notification with a scripted permission answer, and a service worker
// registration whose PushManager hands out deterministic subscriptions.
// restore() puts the window back as it was.

export interface FakeSub {
  endpoint: string
  options: { applicationServerKey: ArrayBuffer | null }
  toJSON: () => { endpoint: string; keys: { p256dh: string; auth: string } }
  unsubscribe: ReturnType<typeof vi.fn>
}

export function fakeSub(endpoint: string, key: Uint8Array | null = null): FakeSub {
  const sub: FakeSub = {
    endpoint,
    options: { applicationServerKey: key ? (key.slice().buffer as ArrayBuffer) : null },
    toJSON: () => ({ endpoint, keys: { p256dh: 'P256DH', auth: 'AUTH' } }),
    unsubscribe: vi.fn(async () => {
      if (state.current === sub) state.current = null
      return true
    }),
  }
  return sub
}

const state: { current: FakeSub | null } = { current: null }

export function installFakePush(opts: {
  permission?: NotificationPermission
  answer?: NotificationPermission
  existing?: FakeSub | null
  registered?: boolean
} = {}) {
  const saved = {
    secure: Object.getOwnPropertyDescriptor(window, 'isSecureContext'),
    notification: (window as unknown as Record<string, unknown>).Notification,
    pushManager: (window as unknown as Record<string, unknown>).PushManager,
    sw: Object.getOwnPropertyDescriptor(navigator, 'serviceWorker'),
  }
  state.current = opts.existing ?? null
  let permission: NotificationPermission = opts.permission ?? 'default'

  const subscribe = vi.fn(async ({ applicationServerKey }: { userVisibleOnly?: boolean; applicationServerKey: Uint8Array }) => {
    state.current = fakeSub('https://fcm.googleapis.com/fcm/send/new', applicationServerKey)
    return state.current
  })
  const pushManager = { getSubscription: vi.fn(async () => state.current), subscribe }
  const reg = { pushManager }
  let registered = opts.registered ?? !!opts.existing

  const requestPermission = vi.fn(async () => {
    permission = opts.answer ?? 'granted'
    return permission
  })
  const register = vi.fn(async () => {
    registered = true
    return reg
  })

  Object.defineProperty(window, 'isSecureContext', { value: true, configurable: true })
  ;(window as unknown as Record<string, unknown>).Notification = {
    get permission() {
      return permission
    },
    requestPermission,
  }
  ;(window as unknown as Record<string, unknown>).PushManager = function PushManager() {}
  Object.defineProperty(navigator, 'serviceWorker', {
    configurable: true,
    value: {
      register,
      ready: Promise.resolve(reg),
      getRegistration: vi.fn(async () => (registered ? reg : undefined)),
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
    },
  })

  return {
    subscribe,
    register,
    requestPermission,
    current: () => state.current,
    restore() {
      if (saved.secure) Object.defineProperty(window, 'isSecureContext', saved.secure)
      else delete (window as unknown as Record<string, unknown>).isSecureContext
      const w = window as unknown as Record<string, unknown>
      if (saved.notification === undefined) delete w.Notification
      else w.Notification = saved.notification
      if (saved.pushManager === undefined) delete w.PushManager
      else w.PushManager = saved.pushManager
      if (saved.sw) Object.defineProperty(navigator, 'serviceWorker', saved.sw)
      else delete (navigator as unknown as Record<string, unknown>).serviceWorker
      state.current = null
    },
  }
}
