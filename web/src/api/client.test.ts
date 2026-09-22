import { describe, expect, test, vi } from 'vitest'
import { ApiClient, ApiError } from './client'
import type { RunProgressEvent } from './types'

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })
}

// sseResponse builds a Response whose body streams the given SSE text in chunks,
// exercising the client's frame reassembly across chunk boundaries.
function sseResponse(chunks: string[]): Response {
  const enc = new TextEncoder()
  const stream = new ReadableStream<Uint8Array>({
    start(controller) {
      for (const c of chunks) controller.enqueue(enc.encode(c))
      controller.close()
    },
  })
  return new Response(stream, { status: 200, headers: { 'Content-Type': 'text/event-stream' } })
}

describe('ApiClient', () => {
  test('TestClientAddsBearer: attaches the bearer token', async () => {
    const fetchImpl = vi.fn().mockResolvedValue(jsonResponse([]))
    const client = new ApiClient({ getToken: () => 'tok-123', fetchImpl })

    await client.listTasks()

    expect(fetchImpl).toHaveBeenCalledTimes(1)
    const [url, init] = fetchImpl.mock.calls[0]
    expect(url).toBe('/api/tasks')
    expect(init.method).toBe('GET')
    expect(init.headers['Authorization']).toBe('Bearer tok-123')
  })

  test('TestStreamRunParsesFrames: reassembles SSE frames across chunks', async () => {
    // A frame is split across two chunks to prove buffering; a heartbeat comment
    // and a trailing partial frame (no terminator) must be ignored.
    const fetchImpl = vi.fn().mockResolvedValue(
      sseResponse([
        'data: {"run_id":5,"seq":1,"kind":"phase","phase":"stream","at":"t"}\n\n',
        ': keep-alive\n\ndata: {"run_id":5,"seq":2,"kind":"by',
        'tes","bytes":2048,"throughput_bps":1024,"at":"t"}\n\n',
        'data: {"run_id":5,"seq":3,"kind":"status","status":"success","at":"t"}\n\n',
      ]),
    )
    const client = new ApiClient({ getToken: () => 't', fetchImpl })

    const got: RunProgressEvent[] = []
    await client.streamRun(5, (ev) => got.push(ev))

    expect(got.map((e) => e.kind)).toEqual(['phase', 'bytes', 'status'])
    expect(got[1].bytes).toBe(2048)
    expect(got[1].throughput_bps).toBe(1024)
    expect(got[2].status).toBe('success')
    const [url, init] = fetchImpl.mock.calls[0]
    expect(url).toBe('/api/runs/5/events')
    expect(init.headers['Authorization']).toBe('Bearer t')
  })

  test('TestClientLogoutOn401: a 401 triggers onUnauthorized and throws', async () => {
    const onUnauthorized = vi.fn()
    const fetchImpl = vi.fn().mockResolvedValue(new Response('', { status: 401 }))
    const client = new ApiClient({ getToken: () => 'stale', onUnauthorized, fetchImpl })

    await expect(client.listTasks()).rejects.toBeInstanceOf(ApiError)
    expect(onUnauthorized).toHaveBeenCalledTimes(1)
  })

  test('TestListTasksParsed: parses the JSON array', async () => {
    const fetchImpl = vi.fn().mockResolvedValue(
      jsonResponse([
        {
          id: 1,
          name: 'nightly',
          connection_id: 1,
          storage_id: 1,
          cron: '0 2 * * *',
          codec_chain: ['zstd'],
          notifiers: [],
          enabled: true,
        },
      ]),
    )
    const client = new ApiClient({ getToken: () => 't', fetchImpl })

    const tasks = await client.listTasks()
    expect(tasks).toHaveLength(1)
    expect(tasks[0].name).toBe('nightly')
    expect(tasks[0].codec_chain).toEqual(['zstd'])
    expect(tasks[0].enabled).toBe(true)
  })

  test('TestListRunsPageParams: serializes keyset pagination params', async () => {
    const fetchImpl = vi.fn().mockResolvedValue(jsonResponse([]))
    const client = new ApiClient({ getToken: () => 't', fetchImpl })

    await client.listRuns({ status: 'success', limit: 50, before: 42 })

    const [url] = fetchImpl.mock.calls[0]
    expect(url).toBe('/api/runs?status=success&limit=50&before=42')
  })

  test('TestListConnectionDatabases: fetches database catalog for a connection', async () => {
    const fetchImpl = vi.fn().mockResolvedValue(jsonResponse({ databases: ['app', 'orders'] }))
    const client = new ApiClient({ getToken: () => 't', fetchImpl })

    const res = await client.listConnectionDatabases(7)

    expect(res.databases).toEqual(['app', 'orders'])
    const [url, init] = fetchImpl.mock.calls[0]
    expect(url).toBe('/api/connections/7/databases')
    expect(init.method).toBe('GET')
  })

  test('TestConnectionTestFailedBody: returns failed check body on HTTP 200', async () => {
    const fetchImpl = vi.fn().mockResolvedValue(jsonResponse({
      status: 'failed',
      latency_ms: 120,
      checks: [
        { key: 'config', status: 'ok', label: 'Конфигурация' },
        { key: 'connect', status: 'failed', label: 'Подключение к БД' },
      ],
      error: {
        code: 'connect_failed',
        message: 'Не удалось проверить соединение',
        hint: 'Проверьте host, port, пользователя, секрет и параметры туннеля',
      },
    }))
    const client = new ApiClient({ getToken: () => 't', fetchImpl })

    const res = await client.testConnection({
      name: 'pg',
      engine: 'postgres',
      connector_type: 'direct',
      connector_config: { host: '127.0.0.1', port: 5432 },
    })

    expect(res.status).toBe('failed')
    expect(res.error?.code).toBe('connect_failed')
    const [url, init] = fetchImpl.mock.calls[0]
    expect(url).toBe('/api/connections/test')
    expect(init.method).toBe('POST')
  })

  test('surfaces the server {error} on non-2xx', async () => {
    const fetchImpl = vi.fn().mockResolvedValue(jsonResponse({ error: 'bad cron' }, 400))
    const client = new ApiClient({ getToken: () => 't', fetchImpl })

    await expect(client.createTask({ name: 'x', connection_id: 1, storage_id: 1, cron: 'bad' })).rejects.toMatchObject(
      { status: 400, message: 'bad cron' },
    )
  })

  test('PATCH helpers send update payloads', async () => {
    const fetchImpl = vi.fn().mockResolvedValue(jsonResponse({ id: 3 }))
    const client = new ApiClient({ getToken: () => 't', fetchImpl })

    await client.updateTask(3, { name: 'renamed', connection_id: 1, storage_id: 2, cron: '0 3 * * *' })

    const [url, init] = fetchImpl.mock.calls[0]
    expect(url).toBe('/api/tasks/3')
    expect(init.method).toBe('PATCH')
    expect(JSON.parse(init.body)).toMatchObject({ name: 'renamed', cron: '0 3 * * *' })
  })

  test('no token → no Authorization header', async () => {
    const fetchImpl = vi.fn().mockResolvedValue(jsonResponse({ status: 'ok' }))
    const client = new ApiClient({ getToken: () => null, fetchImpl })

    await client.ping()
    const [, init] = fetchImpl.mock.calls[0]
    expect(init.headers['Authorization']).toBeUndefined()
  })
})
