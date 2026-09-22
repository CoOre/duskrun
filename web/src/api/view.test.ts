import { describe, expect, test } from 'vitest'
import { activityBuckets, fmtCountdown, nextRunIn, scheduledTasks } from './view'
import type { Run, Task } from './types'

const task = (over: Partial<Task>): Task => ({
  id: 1, name: 't', connection_id: 1, storage_id: 1, cron: '0 2 * * *',
  codec_chain: [], notifiers: [], enabled: true, ...over,
})

const run = (over: Partial<Run>): Run => ({
  id: 1, task_id: 1, status: 'success', attempt: 1, created_at: '2026-08-12T02:00:00Z', ...over,
})

describe('scheduledTasks', () => {
  test('TestScheduledTasksOrdersBySoonest: earliest next_run wins, not lowest id', () => {
    const tasks = [
      task({ id: 1, name: 'late', next_run: '2026-08-13T02:00:00Z' }),
      task({ id: 2, name: 'soon', next_run: '2026-08-12T09:00:00Z' }),
    ]
    expect(scheduledTasks(tasks).map((t) => t.name)).toEqual(['soon', 'late'])
  })

  test('TestScheduledTasksDropsPaused: a paused task is never "next", even with a next_run', () => {
    const tasks = [
      task({ id: 1, name: 'paused', enabled: false, next_run: '2026-08-12T09:00:00Z' }),
      task({ id: 2, name: 'live', next_run: '2026-08-13T02:00:00Z' }),
    ]
    expect(scheduledTasks(tasks).map((t) => t.name)).toEqual(['live'])
  })

  test('TestScheduledTasksDropsUnschedulable: no next_run means the backend could not schedule it', () => {
    expect(scheduledTasks([task({ next_run: undefined })])).toEqual([])
  })
})

describe('fmtCountdown', () => {
  test('TestFmtCountdownUnits: picks the largest readable unit', () => {
    expect(fmtCountdown(30_000)).toEqual({ value: '<1', unit: 'мин' })
    expect(fmtCountdown(13 * 60_000)).toEqual({ value: '13', unit: 'мин' })
    expect(fmtCountdown(14 * 3_600_000)).toEqual({ value: '14', unit: 'ч' })
    expect(fmtCountdown(5 * 86_400_000)).toEqual({ value: '5', unit: 'д' })
    expect(fmtCountdown(-1000)).toEqual({ value: '<1', unit: 'мин' })
  })

  test('TestNextRunInUsesBackendSchedule: a daily 02:00 cron reads as hours away, not minutes', () => {
    const now = Date.parse('2026-08-12T06:51:00Z')
    expect(nextRunIn(task({ next_run: '2026-08-13T02:00:00Z' }), now)).toEqual({ value: '19', unit: 'ч' })
    expect(nextRunIn(task({ next_run: undefined }), now)).toBeNull()
  })
})

describe('activityBuckets', () => {
  const now = Date.parse('2026-08-12T12:00:00Z')

  test('TestActivityBucketsKeepsEmptyDays: a sparse schedule is not drawn as a dense one', () => {
    const buckets = activityBuckets(
      [run({ id: 1, started_at: '2026-08-12T02:00:00Z' }), run({ id: 2, started_at: '2026-08-07T02:00:00Z' })],
      14,
      now,
    )
    expect(buckets).toHaveLength(14)
    expect(buckets.filter((b) => b.total > 0)).toHaveLength(2)
    // Oldest → newest, ending on today.
    expect(buckets[13].total).toBe(1)
    expect(buckets[12].total).toBe(0)
  })

  test('TestActivityBucketsCountsAndBytes: statuses and artifact sizes land in the right day', () => {
    const buckets = activityBuckets(
      [
        run({ id: 1, status: 'success', started_at: '2026-08-12T02:00:00Z', artifact: { id: 1, run_id: 1, storage_id: 1, key: 'k', size: 2048, checksum: '', created_at: '' } }),
        run({ id: 2, status: 'failed', started_at: '2026-08-12T03:00:00Z' }),
      ],
      14,
      now,
    )
    const today = buckets[13]
    expect(today).toMatchObject({ ok: 1, fail: 1, total: 2, bytes: 2048 })
  })

  test('TestActivityBucketsEmptyWindow: nothing in range returns [] so callers show an empty state', () => {
    expect(activityBuckets([run({ started_at: '2025-01-01T02:00:00Z' })], 14, now)).toEqual([])
    expect(activityBuckets([], 14, now)).toEqual([])
  })
})
