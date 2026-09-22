// Compression/format presets shared by the task wizard and editor.
//
// A "compression" choice maps to an ordered codec chain. Most are a single
// codec; `tar.gz` is the two-codec chain [tar, gzip] — tar archives the dump,
// gzip compresses the archive, yielding the familiar `.tar.gz`. Encryption
// (age) is a separate axis appended after compression so the chain stays
// "compress/archive → encrypt".

export type CompressionKey = 'none' | 'gzip' | 'zstd' | 'tar' | 'tar.gz'

export interface CompressionPreset {
  key: CompressionKey
  label: string
  sub: string // suffix hint shown under the label
  chain: string[] // backend codec names, in application order
  ext: string // artifact-name suffix this preset contributes
}

export const COMPRESSION_PRESETS: CompressionPreset[] = [
  { key: 'none', label: 'без сжатия', sub: '—', chain: [], ext: '' },
  { key: 'gzip', label: 'gzip', sub: '.gz', chain: ['gzip'], ext: '.gz' },
  { key: 'zstd', label: 'zstd', sub: '.zst', chain: ['zstd'], ext: '.zst' },
  { key: 'tar', label: 'tar', sub: '.tar', chain: ['tar'], ext: '.tar' },
  { key: 'tar.gz', label: 'tar.gz', sub: '.tar.gz', chain: ['tar', 'gzip'], ext: '.tar.gz' },
]

// Codec names owned by the compression axis (everything reachable from a preset
// chain that isn't encryption).
export const COMPRESSION_CODECS: string[] = Array.from(
  new Set(COMPRESSION_PRESETS.flatMap((p) => p.chain)),
)

// compressionFromCodecs recovers which preset an existing codec_chain represents,
// matching the compression codecs present (ignoring encryption) as a set.
export function compressionFromCodecs(codecs: string[]): CompressionKey {
  const comp = codecs.filter((c) => COMPRESSION_CODECS.includes(c))
  const match = COMPRESSION_PRESETS.find(
    (p) => p.chain.length === comp.length && p.chain.every((c) => comp.includes(c)),
  )
  return match?.key ?? 'none'
}

// buildCodecChain composes the final ordered codec_chain from the two axes.
export function buildCodecChain(compression: CompressionKey, encrypt: boolean): string[] {
  const chain = COMPRESSION_PRESETS.find((p) => p.key === compression)?.chain ?? []
  return encrypt ? [...chain, 'age'] : [...chain]
}

// extFor is the artifact-name suffix contributed by a compression preset.
export function extFor(compression: CompressionKey): string {
  return COMPRESSION_PRESETS.find((p) => p.key === compression)?.ext ?? ''
}

// dumpExtFor is the extension the dump itself carries, before any codec suffix.
// It mirrors core.dumpExt (internal/core/pipeline.go) so the wizard's preview
// shows the name the backend will actually write.
export function dumpExtFor(engine: string): string {
  switch (engine) {
    case 'mongodb':
      return '.archive'
    case 'redis':
      return '.rdb'
    case 'mssql':
      return '.bak'
    case 'sqlite':
      return '.sqlite'
    default:
      return '.dump'
  }
}
