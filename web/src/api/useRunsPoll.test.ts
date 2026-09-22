import { act, renderHook, waitFor } from '@testing-library/react'
import { afterEach, expect, test, vi } from 'vitest'
import { ApiClient } from './client'
import { useRunsPoll } from './useRunsPoll'
import type { Run } from './types'

const run = (over: Partial<Run>): Run => ({
  id: 1, task_id: 1, status: 'success', attempt: 1, created_at: '2026-08-12T02:00:00Z', ...over,
})

// clientReturning builds an ApiClient whose listRuns replays `pages`, sticking
// on the last one — so a test can script what successive polls observe.
function clientReturning(pages: Run[][]): { api: ApiClient; calls: () => number } {
  let n = 0
  const fetchImpl = vi.fn(async () => {
    const page = pages[Math.min(n, pages.length - 1)]
    n++
    return new Response(JSON.stringify(page), { headers: { 'Content-Type': 'application/json' } })
  })
  return {
    api: new ApiClient({ getToken: () => 'test-token', fetchImpl: fetchImpl as unknown as typeof fetch }),
    calls: () => n,
  }
}

afterEach(() => vi.useRealTimers())

test('TestRunsPollWithoutSeedLoadsImmediately: the hook owns its first page', async () => {
  const { api } = clientReturning([[run({ id: 7 })]])
  const { result } = renderHook(() => useRunsPoll(api, undefined, { idleMs: 10_000 }))
  await waitFor(() => expect(result.current.runs.map((r) => r.id)).toEqual([7]))
})

test('TestRunsPollKeepsPollingWhenIdle: a run started by the scheduler appears without a reload', async () => {
  // Everything terminal at first — the old hook stopped polling here entirely.
  const { api } = clientReturning([
    [run({ id: 1, status: 'success' })],
    [run({ id: 2, status: 'running', started_at: '2026-08-12T09:00:00Z' }), run({ id: 1, status: 'success' })],
  ])
  vi.useFakeTimers({ shouldAdvanceTime: true })
  const { result } = renderHook(() => useRunsPoll(api, undefined, { idleMs: 1000, activeMs: 500 }))

  await waitFor(() => expect(result.current.hasActive).toBe(false))
  await act(async () => {
    await vi.advanceTimersByTimeAsync(1200)
  })
  await waitFor(() => {
    expect(result.current.runs.map((r) => r.id)).toEqual([2, 1])
    expect(result.current.hasActive).toBe(true)
  })
})

test('TestRunsPollMergesOntoSeed: a bounded poll page never drops the seeded history', async () => {
  const seed = [run({ id: 3 }), run({ id: 2 }), run({ id: 1 })]
  // The poll only ever returns the newest page.
  const { api } = clientReturning([[run({ id: 4, status: 'running', started_at: '2026-08-12T09:00:00Z' }), run({ id: 3 })]])
  vi.useFakeTimers({ shouldAdvanceTime: true })
  const { result } = renderHook(() => useRunsPoll(api, seed, { idleMs: 1000, activeMs: 500 }))

  await waitFor(() => expect(result.current.runs.map((r) => r.id)).toEqual([3, 2, 1]))
  await act(async () => {
    await vi.advanceTimersByTimeAsync(1200)
  })
  await waitFor(() => expect(result.current.runs.map((r) => r.id)).toEqual([4, 3, 2, 1]))
})
