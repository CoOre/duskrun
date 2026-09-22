import { expect, test } from 'vitest'
import { buildRetention, clampCount, formatPolicy, keepLastFrom, modeOf } from './retention'

test('clampCount: a blank box is 0 — clearing a field turns its bucket off', () => {
  expect(clampCount('', 7)).toBe(0)
  expect(clampCount('   ', 7)).toBe(0)
})

test('clampCount: a negative or non-numeric entry keeps the current value', () => {
  // Folding these to 0 would silently disarm the rule being edited — exactly
  // what core.Retention.Validate refuses to store.
  expect(clampCount('-1', 14)).toBe(14)
  expect(clampCount('abc', 14)).toBe(14)
  expect(clampCount('-1', 0)).toBe(0)
})

test('clampCount: a valid count is floored', () => {
  expect(clampCount('7', 1)).toBe(7)
  expect(clampCount('7.9', 1)).toBe(7)
  expect(clampCount('0', 5)).toBe(0)
})

test('buildRetention/modeOf: one rule round-trips', () => {
  expect(modeOf(buildRetention('keep_last', 14, { daily: 0, weekly: 0, monthly: 0 }))).toBe('keep_last')
  expect(modeOf(buildRetention('gfs', 0, { daily: 7, weekly: 4, monthly: 12 }))).toBe('gfs')
  expect(modeOf(buildRetention('none', 0, { daily: 0, weekly: 0, monthly: 0 }))).toBe('none')
})

test('formatPolicy/keepLastFrom: a rule-less policy edits from the default', () => {
  expect(formatPolicy({ keep_last: 14 })).toBe('keep_last 14')
  expect(formatPolicy({ gfs: { daily: 7, weekly: 4, monthly: 12 } })).toBe('GFS 7/4/12')
  expect(keepLastFrom(undefined)).toBeGreaterThan(0)
})
