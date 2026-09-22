import { expect, test } from 'vitest'
import { buildCodecChain, compressionFromCodecs, extFor } from './codecs'

test('buildCodecChain: tar.gz composes [tar, gzip] in order', () => {
  expect(buildCodecChain('tar.gz', false)).toEqual(['tar', 'gzip'])
  expect(buildCodecChain('tar.gz', true)).toEqual(['tar', 'gzip', 'age'])
})

test('buildCodecChain: single-codec presets and none', () => {
  expect(buildCodecChain('zstd', false)).toEqual(['zstd'])
  expect(buildCodecChain('gzip', true)).toEqual(['gzip', 'age'])
  expect(buildCodecChain('tar', false)).toEqual(['tar'])
  expect(buildCodecChain('none', false)).toEqual([])
  expect(buildCodecChain('none', true)).toEqual(['age'])
})

test('compressionFromCodecs: recovers the preset, ignoring encryption', () => {
  expect(compressionFromCodecs(['tar', 'gzip', 'age'])).toBe('tar.gz')
  expect(compressionFromCodecs(['gzip'])).toBe('gzip')
  expect(compressionFromCodecs(['zstd', 'age'])).toBe('zstd')
  expect(compressionFromCodecs(['tar'])).toBe('tar')
  expect(compressionFromCodecs(['age'])).toBe('none')
  expect(compressionFromCodecs([])).toBe('none')
})

test('round-trips: chain → key → chain is stable', () => {
  for (const key of ['none', 'gzip', 'zstd', 'tar', 'tar.gz'] as const) {
    const chain = buildCodecChain(key, false)
    expect(compressionFromCodecs(chain)).toBe(key)
  }
})

test('extFor: suffix per preset', () => {
  expect(extFor('tar.gz')).toBe('.tar.gz')
  expect(extFor('gzip')).toBe('.gz')
  expect(extFor('none')).toBe('')
})
