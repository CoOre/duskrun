// Duskrun service worker. It exists for push notifications only: it shows the
// ones the server sends and opens the page they are about. It caches nothing —
// the SPA is served by the daemon itself, and pinning an old build in the
// browser would outlive every upgrade.
//
// Plain script, no build step: Vite copies public/ into the bundle as-is, and
// the server sends it with Cache-Control: no-cache so an update is picked up on
// the next visit.

const TITLES = {
  failure: (task) => `✗ ${task}: бэкап не выполнен`,
  success: (task) => `✓ ${task}: бэкап выполнен`,
  retention_error: (task) => `⚠ ${task}: ошибка ротации`,
  watchdog: (task) => `⚠ ${task}: watchdog`,
}

// Anything other than a routine success is worth keeping on screen until the
// operator has seen it.
const QUIET = new Set(['success', 'test'])

self.addEventListener('install', () => self.skipWaiting())
self.addEventListener('activate', (event) => event.waitUntil(self.clients.claim()))

function parse(data) {
  if (!data) return {}
  try {
    return data.json()
  } catch {
    return { message: data.text() }
  }
}

self.addEventListener('push', (event) => {
  const p = parse(event.data)
  const title =
    p.kind === 'test'
      ? 'Duskrun: тестовое уведомление'
      : TITLES[p.kind]
        ? TITLES[p.kind](p.task || 'Duskrun')
        : 'Duskrun'
  const urgent = !QUIET.has(p.kind)
  const options = {
    body: p.message || '',
    icon: '/icon-192.png',
    badge: '/badge-96.png',
    requireInteraction: urgent,
    timestamp: p.at ? Date.parse(p.at) : Date.now(),
    data: { url: p.url || '/' },
  }
  if (p.task) {
    // One notification per task and kind: a retried failure replaces the
    // previous one instead of stacking, and still alerts again.
    options.tag = `${p.kind}:${p.task}`
    options.renotify = urgent
  }
  event.waitUntil(self.registration.showNotification(title, options))
})

self.addEventListener('notificationclick', (event) => {
  event.notification.close()
  const url = new URL(event.notification.data?.url || '/', self.location.origin)
  event.waitUntil(
    (async () => {
      const windows = await self.clients.matchAll({ type: 'window', includeUncontrolled: true })
      const open = windows.find((w) => new URL(w.url).origin === url.origin)
      if (open) {
        await open.focus()
        // navigate() needs a controlled client; a tab opened before the worker
        // took control gets the address through a message instead.
        if ('navigate' in open && open.url !== url.href) {
          try {
            await open.navigate(url.href)
            return
          } catch {
            /* fall through to postMessage */
          }
        }
        open.postMessage({ type: 'duskrun:navigate', url: url.pathname + url.search })
        return
      }
      await self.clients.openWindow(url.href)
    })(),
  )
})

// The browser replaced the subscription (key rotation, expiry). The worker
// cannot reach the API — the session token lives in the page — so it only
// resubscribes; the page reports the new subscription the next time it opens.
self.addEventListener('pushsubscriptionchange', (event) => {
  const key = event.oldSubscription?.options?.applicationServerKey
  if (!key) return
  event.waitUntil(self.registration.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: key }))
})
