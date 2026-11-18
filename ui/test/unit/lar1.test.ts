import assert from 'node:assert/strict'
import { test } from 'node:test'
import { gzipSync, zstdCompressSync } from 'node:zlib'
import { demoRecording, encodeChunk } from '../../mock/lar1.mjs'
import { ReplayLoadError, inflate, isZstd, loadChunk, sniff, type ChunkRef } from '../../src/replay/fetch'
import { ReplayDecodeError, decodeChunk, type Chunk } from '../../src/replay/lar1'
import { fromHex, vectors } from './helpers'

interface Vectors {
  format: string
  valid: { name: string; chunk: Chunk; hex: string }[]
  tolerant: { name: string; chunk: Chunk; hex: string; note?: string }[]
}
const v = vectors<Vectors>('replay.json')

test('vectors are the LAR1 ones and non-empty', () => {
  assert.equal(v.format, 'LAR1')
  assert.ok(v.valid.length >= 2)
  assert.ok(v.tolerant.length >= 1)
})

for (const c of v.valid) {
  test(`valid: ${c.name} decodes to the logical chunk`, () => {
    assert.deepEqual(decodeChunk(fromHex(c.hex)), c.chunk)
  })
}

for (const c of v.tolerant) {
  test(`tolerant: ${c.name}`, () => {
    assert.deepEqual(decodeChunk(fromHex(c.hex)), c.chunk)
  })
}

test('absolute frame times account for skipped unknown frames', () => {
  const c = v.tolerant.find((x) => x.name == 'unknown_frame_kind_skipped')
  assert.ok(c)
  const decoded = decodeChunk(fromHex(c.hex), true)
  // the unknown frame carried dt=5, the viewport frame dt=7 → 12 ms after start
  assert.equal(decoded.frames.length, 1)
  assert.equal(decoded.frames[0].t, c.chunk.start_ts + 12)
})

test('masked text arrives as bullets with the masked flag', () => {
  const login = v.valid.find((x) => x.name == 'login_screen')
  assert.ok(login)
  const snapshot = decodeChunk(fromHex(login.hex)).frames.find((f) => f.kind == 0)
  assert.ok(snapshot && snapshot.kind == 0)
  const input = snapshot.nodes.find((n) => n.type == 4)
  assert.ok(input)
  assert.equal(input.text, '••••••')
  assert.equal(input.flags & 4, 4)
  assert.equal(snapshot.nodes.find((n) => n.type == 5)?.y, -300, 'zigzag coordinates')
  assert.equal(snapshot.nodes[0].color, 0xffffffff, 'u32 colour stays unsigned')
})

test('malformed input is rejected, not misread', () => {
  const good = fromHex(v.valid[0].hex)
  assert.throws(() => decodeChunk(new Uint8Array([1, 2, 3])), ReplayDecodeError)
  const badMagic = good.slice()
  badMagic[3] = 0x32
  assert.throws(() => decodeChunk(badMagic), ReplayDecodeError)
  assert.throws(() => decodeChunk(good.slice(0, good.length - 3)), ReplayDecodeError, 'truncated frame')
})

test('the mock encoder produces exactly the vector bytes, and its demo session decodes', () => {
  for (const c of v.valid) assert.deepEqual(encodeChunk(c.chunk), fromHex(c.hex), c.name)
  const rec = demoRecording('0193f1a2b3c47d5e8a9b0c1d2e3f4a5c', 1795046400000)
  assert.ok(rec.chunks.length >= 4)
  rec.chunks.forEach((chunk: Chunk & { duration_ms: number }, i: number) => {
    const decoded = decodeChunk(encodeChunk(chunk))
    assert.equal(decoded.seq, i)
    assert.equal(decoded.session_id, '0193f1a2b3c47d5e8a9b0c1d2e3f4a5c')
    assert.equal(decoded.frames.length, chunk.frames.length)
    // every chunk can be played on its own: it starts with a full snapshot (after screen/viewport frames)
    assert.equal(decoded.frames.find((f) => f.kind == 0 || f.kind == 1 || f.kind == 2 || f.kind == 3)?.kind, 0)
  })
})

test('sniffing and inflating chunk bodies', async () => {
  const raw = fromHex(v.valid[0].hex)
  assert.equal(sniff(raw), 'raw')
  const gz = gzipSync(raw)
  assert.equal(sniff(gz), 'gzip')
  assert.equal(sniff(new Uint8Array([0x28, 0xb5, 0x2f, 0xfd, 0])), 'zstd')
  assert.equal(sniff(new Uint8Array([9, 9])), 'unknown')

  // already decoded by the browser (Content-Encoding) → used as is
  assert.deepEqual(await inflate(raw), raw)
  // served without Content-Encoding → gunzipped here
  assert.deepEqual(await inflate(gz), raw)
  await assert.rejects(inflate(new Uint8Array([9, 9, 9, 9]), 3), (e: unknown) => e instanceof ReplayLoadError && e.code == 'bad_chunk' && e.seq == 3)
  await assert.rejects(inflate(gz.slice(0, 20)), (e: unknown) => e instanceof ReplayLoadError && e.code == 'bad_chunk')
})

test('zstd bodies: decoded where the runtime can, a typed error where it cannot', async () => {
  const raw = fromHex(v.valid[0].hex)
  const Native = globalThis.DecompressionStream
  let supported = true
  try {
    new Native('zstd' as CompressionFormat)
  } catch {
    supported = false
  }
  if (supported && typeof zstdCompressSync == 'function') assert.deepEqual(await inflate(zstdCompressSync(raw)), raw)
  // garbage after the magic is a broken chunk where zstd is supported, "unsupported" where it is not
  const fake = new Uint8Array([0x28, 0xb5, 0x2f, 0xfd, 1, 2, 3, 4])
  await assert.rejects(inflate(fake, 7), (e: unknown) => e instanceof ReplayLoadError && e.code == (supported ? 'bad_chunk' : 'zstd_unsupported') && e.seq == 7)

  // a browser without zstd in DecompressionStream
  class NoZstd extends Native {
    constructor(format: CompressionFormat) {
      if ((format as string) == 'zstd') throw new TypeError('Unsupported compression format')
      super(format)
    }
  }
  globalThis.DecompressionStream = NoZstd
  try {
    await assert.rejects(inflate(fake, 7), (e: unknown) => e instanceof ReplayLoadError && e.code == 'zstd_unsupported' && e.seq == 7)
    assert.deepEqual(await inflate(gzipSync(raw)), raw, 'gzip is unaffected')
  } finally {
    globalThis.DecompressionStream = Native
  }

  assert.equal(isZstd(1, 'x.bin.gz'), true)
  assert.equal(isZstd('zstd', 'x'), true)
  assert.equal(isZstd(2, 'x.bin.zst'), false)
  assert.equal(isZstd('gzip', 'x.bin.zst'), false)
  assert.equal(isZstd(undefined, 'a/000001.bin.zst'), true)
  assert.equal(isZstd(undefined, 'a/000001.bin.gz'), false)
})

test('loadChunk maps failures to typed errors', async () => {
  const ref: ChunkRef = { seq: 2, key: 'replay/p=1/x', ts: 0, durationMs: 0, bytes: 0, zstd: false }
  const raw = fromHex(v.valid[0].hex)
  const ok = await loadChunk(ref, async () => gzipSync(raw))
  assert.equal(ok.frames.length, v.valid[0].chunk.frames.length)
  assert.equal((ok.frames[0] as { t?: number }).t, v.valid[0].chunk.start_ts, 'frames carry absolute time')

  const httpError = Object.assign(new Error('Replay parçası bulunamadı'), { status: 404 })
  await assert.rejects(loadChunk(ref, async () => Promise.reject(httpError)), (e: unknown) => e instanceof ReplayLoadError && e.code == 'download' && e.seq == 2)
  // the browser refusing `Content-Encoding: zstd` surfaces as a network error on a zstd chunk
  const netError = Object.assign(new Error('network'), { status: 0 })
  await assert.rejects(loadChunk({ ...ref, zstd: true }, async () => Promise.reject(netError)), (e: unknown) => e instanceof ReplayLoadError && e.code == 'zstd_unsupported')
  await assert.rejects(loadChunk(ref, async () => raw.slice(0, 30)), (e: unknown) => e instanceof ReplayLoadError && e.code == 'bad_chunk')
  const abort = Object.assign(new Error('aborted'), { name: 'AbortError' })
  await assert.rejects(loadChunk(ref, async () => Promise.reject(abort)), (e: unknown) => (e as Error).name == 'AbortError')
})
