import { screen } from '@testing-library/react'
import { beforeEach, expect, test, vi } from 'vitest'
import { renderWithAuth } from '../test/utils'
import { RunDrawer } from './RunDrawer'

beforeEach(() => localStorage.clear())

function json(body: unknown, status = 200): Response {
  return new Response(status === 204 ? null : JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })
}

// sse builds a streaming Response that emits the given frames then closes.
function sse(frames: string[]): Response {
  const enc = new TextEncoder()
  const stream = new ReadableStream<Uint8Array>({
    start(c) {
      for (const f of frames) c.enqueue(enc.encode(f))
      c.close()
    },
  })
  return new Response(stream, { status: 200, headers: { 'Content-Type': 'text/event-stream' } })
}

// TestRunDrawerLiveStream: a running run subscribes to the SSE stream and renders
// the live log line and the transfer meter.
test('TestRunDrawerLiveStream: streams live log + meter for a running run', async () => {
  const fetchImpl = vi.fn(async (url: string) => {
    const path = url.split('?')[0]
    if (path === '/api/runs/77/events') {
      return sse([
        'data: {"run_id":77,"seq":1,"kind":"phase","phase":"stream","at":"t"}\n\n',
        'data: {"run_id":77,"seq":2,"kind":"log","phase":"stream","message":"качаю дамп","at":"t"}\n\n',
        'data: {"run_id":77,"seq":3,"kind":"bytes","bytes":4096,"throughput_bps":2048,"at":"t"}\n\n',
      ])
    }
    if (path === '/api/runs/77') {
      return json({ id: 77, task_id: 1, status: 'running', attempt: 1, started_at: '2026-07-21T02:00:00Z', created_at: '2026-07-21T02:00:00Z' })
    }
    if (path === '/api/tasks') {
      return json([{ id: 1, name: 'nightly-pg', connection_id: 1, storage_id: 1, cron: '0 2 * * *', codec_chain: ['zstd'], notifiers: [], enabled: true }])
    }
    if (path === '/api/connections') {
      return json([{ id: 1, name: 'pg-primary', engine: 'postgres', connector_type: 'direct', secret_ref: '' }])
    }
    return json({ error: `no mock for ${path}` }, 404)
  }) as unknown as typeof fetch

  renderWithAuth(<RunDrawer runId={77} onClose={() => {}} />, { fetchImpl })

  // Live log line arrives over the stream (not from run.log).
  expect(await screen.findByText(/качаю дамп/)).toBeInTheDocument()
  // The transfer meter renders while the run is active. Without a total estimate
  // it is the indeterminate sweep.
  expect(screen.getByText('ПЕРЕДАНО В STORAGE')).toBeInTheDocument()
  expect(screen.getByRole('progressbar', { name: 'передача данных' })).toBeInTheDocument()

  // In the `stream` phase the whole Dumper→Codec→Storage stretch is in-flight
  // (pulsing), while the Connector is already done. Regression guard: Codec must
  // not read as skipped while Storage lights up.
  const box = (name: string) => screen.getByText(name).parentElement
  expect(box('Codec')?.className).toContain('dc-pulse')
  expect(box('Storage')?.className).toContain('dc-pulse')
  expect(box('Connector')?.className ?? '').not.toContain('dc-pulse')
})

// TestRunDrawerLivePercent: when a total estimate arrives (previous run's size),
// the meter switches to a determinate bar with an approximate percentage.
test('TestRunDrawerLivePercent: shows a percentage when a total estimate is known', async () => {
  const fetchImpl = vi.fn(async (url: string) => {
    const path = url.split('?')[0]
    if (path === '/api/runs/88/events') {
      return sse([
        'data: {"run_id":88,"seq":1,"kind":"phase","phase":"stream","at":"t"}\n\n',
        'data: {"run_id":88,"seq":2,"kind":"total","total":1000,"at":"t"}\n\n',
        'data: {"run_id":88,"seq":3,"kind":"bytes","bytes":500,"total":1000,"throughput_bps":250,"at":"t"}\n\n',
      ])
    }
    if (path === '/api/runs/88') {
      return json({ id: 88, task_id: 1, status: 'running', attempt: 1, started_at: '2026-07-21T02:00:00Z', created_at: '2026-07-21T02:00:00Z' })
    }
    if (path === '/api/tasks') {
      return json([{ id: 1, name: 'nightly-pg', connection_id: 1, storage_id: 1, cron: '0 2 * * *', codec_chain: ['zstd'], notifiers: [], enabled: true }])
    }
    if (path === '/api/connections') {
      return json([{ id: 1, name: 'pg-primary', engine: 'postgres', connector_type: 'direct', secret_ref: '' }])
    }
    return json({ error: `no mock for ${path}` }, 404)
  }) as unknown as typeof fetch

  renderWithAuth(<RunDrawer runId={88} onClose={() => {}} />, { fetchImpl })

  // 500 / 1000 → ≈50%.
  expect(await screen.findByText('≈50%')).toBeInTheDocument()
  const bar = screen.getByRole('progressbar', { name: 'прогресс передачи' })
  expect(bar.getAttribute('aria-valuenow')).toBe('50')
})
