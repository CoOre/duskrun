import { afterEach, expect, test, vi } from 'vitest'
import { ApiClient } from './api/client'
import {
  b64urlToBytes,
  describeUserAgent,
  disablePush,
  enablePush,
  PushPermissionError,
  pushSupport,
  syncPush,
} from './push'
import { fakeSub, installFakePush } from './test/fakePush'
import { makeFetch } from './test/utils'

// A 65-byte uncompressed point, base64url: what GET /api/push hands out.
const KEY = 'BAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8gISIjJCUmJygpKissLS4vMDEyMzQ1Njc4OTo7PD0-P0A'

let env: ReturnType<typeof installFakePush> | null = null
afterEach(() => {
  env?.restore()
  env = null
})

function client(fetchImpl: typeof fetch) {
  return new ApiClient({ getToken: () => 'tok', fetchImpl })
}

function bodyOf(fetchImpl: ReturnType<typeof makeFetch>, method: string, path: string) {
  const call = fetchImpl.calls().find(([url, init]) => url === path && (init?.method ?? 'GET') === method)
  return call ? JSON.parse(call[1].body as string) : undefined
}

test('TestB64urlToBytes: decodes the unpadded url-safe alphabet', () => {
  const b = b64urlToBytes(KEY)
  expect(b.length).toBe(65)
  expect(b[0]).toBe(4)
  expect(b[62]).toBe(62) // '-' and '_' map to + and /
  expect(Array.from(b64urlToBytes('_-8'))).toEqual([255, 239])
})

test('TestDescribeUserAgent: names the browser and the platform', () => {
  const mac = 'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36'
  expect(describeUserAgent(mac)).toBe('Chrome · macOS')
  expect(describeUserAgent('Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:133.0) Gecko/20100101 Firefox/133.0')).toBe('Firefox · Windows')
  expect(describeUserAgent('Mozilla/5.0 (iPhone; CPU iPhone OS 18_1 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.1 Mobile/15E148 Safari/604.1')).toBe('Safari · iPhone')
  expect(describeUserAgent(mac.replace('Safari/537.36', 'Safari/537.36 Edg/131.0.0.0'))).toBe('Edge · macOS')
  expect(describeUserAgent('')).toBe('неизвестный браузер')
})

test('TestPushSupport: secure context, API presence and the iOS Home Screen rule', () => {
  expect(pushSupport()).toBe('insecure') // jsdom is not a secure context
  env = installFakePush()
  expect(pushSupport()).toBe('ok')

  const ua = vi.spyOn(navigator, 'userAgent', 'get').mockReturnValue(
    'Mozilla/5.0 (iPhone; CPU iPhone OS 18_1 like Mac OS X) AppleWebKit/605.1.15 Version/18.1 Mobile/15E148 Safari/604.1',
  )
  try {
    expect(pushSupport()).toBe('ios-install')
  } finally {
    ua.mockRestore()
  }
})

test('TestEnablePush: asks permission, subscribes with the server key, reports the subscription', async () => {
  env = installFakePush()
  const fetchImpl = makeFetch({ 'POST /api/push/subscriptions': { id: 3 } })
  await enablePush(client(fetchImpl), KEY)

  expect(env.requestPermission).toHaveBeenCalled()
  expect(env.register).toHaveBeenCalledWith('/sw.js', { scope: '/' })
  const opts = env.subscribe.mock.calls[0][0]
  expect(opts.userVisibleOnly).toBe(true)
  expect(Array.from(opts.applicationServerKey)).toEqual(Array.from(b64urlToBytes(KEY)))
  expect(bodyOf(fetchImpl, 'POST', '/api/push/subscriptions')).toEqual({
    endpoint: 'https://fcm.googleapis.com/fcm/send/new',
    keys: { p256dh: 'P256DH', auth: 'AUTH' },
  })
})

test('TestEnablePushDenied: no subscription and nothing sent when permission is refused', async () => {
  env = installFakePush({ answer: 'denied' })
  const fetchImpl = makeFetch({})
  await expect(enablePush(client(fetchImpl), KEY)).rejects.toBeInstanceOf(PushPermissionError)
  expect(env.subscribe).not.toHaveBeenCalled()
  expect(fetchImpl.calls()).toHaveLength(0)
})

test('TestEnablePushReplacesForeignKey: a subscription bound to another key is dropped first', async () => {
  const old = fakeSub('https://fcm.googleapis.com/fcm/send/old', new Uint8Array([4, 9, 9]))
  env = installFakePush({ existing: old, permission: 'granted' })
  await enablePush(client(makeFetch({ 'POST /api/push/subscriptions': { id: 1 } })), KEY)
  expect(old.unsubscribe).toHaveBeenCalled()
  expect(env.subscribe).toHaveBeenCalled()
})

test('TestDisablePush: the server forgets the endpoint, and the browser drops it even if that fails', async () => {
  const sub = fakeSub('https://fcm.googleapis.com/fcm/send/abc')
  env = installFakePush({ existing: sub, permission: 'granted' })
  const fetchImpl = makeFetch({ 'POST /api/push/unsubscribe': { status: 500, body: { error: 'boom' } } })
  await expect(disablePush(client(fetchImpl))).rejects.toThrow('boom')
  expect(bodyOf(fetchImpl, 'POST', '/api/push/unsubscribe')).toEqual({ endpoint: 'https://fcm.googleapis.com/fcm/send/abc' })
  expect(sub.unsubscribe).toHaveBeenCalled()
})

test('TestSyncPush: re-reports an existing subscription, and never prompts or subscribes from scratch', async () => {
  env = installFakePush({ permission: 'granted' })
  const fetchImpl = makeFetch({
    'GET /api/push': { available: true, public_key: KEY },
    'POST /api/push/subscriptions': { id: 1 },
  })
  await syncPush(client(fetchImpl))
  expect(fetchImpl.calls()).toHaveLength(0) // no worker registered: never enabled here
  env.restore()

  const sub = fakeSub('https://fcm.googleapis.com/fcm/send/abc', b64urlToBytes(KEY))
  env = installFakePush({ existing: sub, permission: 'granted' })
  await syncPush(client(fetchImpl))
  expect(env.requestPermission).not.toHaveBeenCalled()
  expect(env.subscribe).not.toHaveBeenCalled()
  expect(bodyOf(fetchImpl, 'POST', '/api/push/subscriptions').endpoint).toBe('https://fcm.googleapis.com/fcm/send/abc')
})
