// Downloading and decompressing replay chunks (spec/replay.md §5).
import { decodeChunk, type Chunk } from './lar1'

export type ReplayErrorCode = 'zstd_unsupported' | 'bad_chunk' | 'download'

export class ReplayLoadError extends Error {
  code: ReplayErrorCode
  seq: number
  constructor(code: ReplayErrorCode, seq: number, detail = '') {
    super(code + (detail ? ': ' + detail : ''))
    this.code = code
    this.seq = seq
  }
}

/** One `session_replay_chunk` index event, reduced to what the player needs. */
export interface ChunkRef {
  seq: number
  key: string
  ts: number
  durationMs: number
  bytes: number
  zstd: boolean
}

/** `props.encoding` is 1/2 in the WAL record; accept the obvious spellings and the key suffix too. */
export const isZstd = (encoding: unknown, key: string): boolean =>
  encoding === 1 || encoding === '1' || encoding === 'zstd' || encoding === 'zst' || (encoding == null && key.endsWith('.zst'))

type Format = 'raw' | 'gzip' | 'zstd' | 'unknown'

export const sniff = (b: Uint8Array): Format => {
  if (b.length >= 4 && b[0] == 0x4c && b[1] == 0x41 && b[2] == 0x52 && b[3] == 0x31) return 'raw'
  if (b.length >= 2 && b[0] == 0x1f && b[1] == 0x8b) return 'gzip'
  if (b.length >= 4 && b[0] == 0x28 && b[1] == 0xb5 && b[2] == 0x2f && b[3] == 0xfd) return 'zstd'
  return 'unknown'
}

const pipe = async (bytes: Uint8Array, format: string): Promise<Uint8Array> => {
  const stream = new Blob([bytes as BlobPart]).stream().pipeThrough(new DecompressionStream(format as CompressionFormat))
  return new Uint8Array(await new Response(stream).arrayBuffer())
}

/**
 * Returns the uncompressed chunk bytes. The proxy sets `Content-Encoding`, so
 * the browser may already have decoded the body; otherwise the magic bytes
 * say what we got. zstd only works where `DecompressionStream('zstd')` exists.
 */
export const inflate = async (bytes: Uint8Array, seq = 0): Promise<Uint8Array> => {
  const format = sniff(bytes)
  if (format == 'raw') return bytes
  if (format == 'gzip') {
    try {
      return await pipe(bytes, 'gzip')
    } catch (e) {
      throw new ReplayLoadError('bad_chunk', seq, String(e))
    }
  }
  if (format == 'zstd') {
    // constructing the stream is the feature test: unsupported formats throw a TypeError
    try {
      new DecompressionStream('zstd' as CompressionFormat)
    } catch {
      throw new ReplayLoadError('zstd_unsupported', seq)
    }
    try {
      return await pipe(bytes, 'zstd')
    } catch (e) {
      throw new ReplayLoadError('bad_chunk', seq, String(e))
    }
  }
  throw new ReplayLoadError('bad_chunk', seq, 'unknown container')
}

/** Downloads, inflates and decodes one chunk. `download` returns the body as received. */
export const loadChunk = async (ref: ChunkRef, download: (key: string) => Promise<Uint8Array>): Promise<Chunk> => {
  let body: Uint8Array
  try {
    body = await download(ref.key)
  } catch (e) {
    if ((e as { name?: string })?.name == 'AbortError') throw e
    // a browser that cannot decode `Content-Encoding: zstd` fails the fetch itself
    const status = (e as { status?: number })?.status ?? 0
    if (ref.zstd && status == 0) throw new ReplayLoadError('zstd_unsupported', ref.seq)
    throw new ReplayLoadError('download', ref.seq, e instanceof Error ? e.message : String(e))
  }
  const raw = await inflate(body, ref.seq)
  try {
    return decodeChunk(raw, true)
  } catch (e) {
    throw new ReplayLoadError('bad_chunk', ref.seq, e instanceof Error ? e.message : String(e))
  }
}
