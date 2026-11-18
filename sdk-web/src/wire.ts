// LA1 wire primitives and batch encoder (spec/wire.md).
//
// Everything is built on plain `number[]` byte lists: for the few hundred
// bytes of an analytics record this is as fast as a growable typed array and
// far smaller in code. `batch()` produces the final `Uint8Array`.

/** A growing list of byte values (0..255). */
export type Bytes = number[]

/** One typed property: `[key, tag, value]`. Tags are those of spec/wire.md §2. */
export type Prop = [key: string, tag: number, value?: unknown]

/** Anything accepted as a property bag by the public API. */
export type Props = Record<string, unknown>

const encoder = /* @__PURE__ */ new TextEncoder()

export const utf8 = (s: string): Uint8Array => encoder.encode(s)

/** Append all bytes of `a` to `o`. */
export const put = (o: Bytes, a: ArrayLike<number>): void => {
  for (let i = 0; i < a.length; i++) o.push(a[i])
}

/** Unsigned LEB128, shortest form. BigInt keeps the full u64 range exact. */
export const varint = (o: Bytes, n: number | bigint): void => {
  let v = BigInt(n)
  for (; v > 127n; v >>= 7n) o.push(Number(v & 127n) | 128)
  o.push(Number(v))
}

/** Zigzag-encoded signed int64 as a varint. */
export const zigzag = (o: Bytes, n: number | bigint): void => {
  const v = BigInt(n)
  varint(o, v < 0n ? ~(v << 1n) : v << 1n)
}

/** `varint(len)` + UTF-8 bytes. */
export const str = (o: Bytes, s: string): void => {
  const b = utf8(s)
  varint(o, b.length)
  put(o, b)
}

/** IEEE-754 double, little-endian; NaN is always the canonical quiet NaN. */
export const f64 = (o: Bytes, v: number): void => {
  const d = new DataView(new ArrayBuffer(8))
  d.setFloat64(0, v, true)
  put(o, v == v ? new Uint8Array(d.buffer) : [0, 0, 0, 0, 0, 0, 248, 127])
}

/** PropList := varint(count) (key tag value){count} */
export const propList = (o: Bytes, list: Prop[]): void => {
  varint(o, list.length)
  for (const [k, tag, v] of list) {
    str(o, k)
    o.push(tag)
    if (tag == 3) zigzag(o, v as number)
    else if (tag == 4) f64(o, v as number)
    else if (tag > 4) str(o, v as string)
  }
}

/**
 * Map a plain JS object to typed props, applying the client-side limits that
 * would otherwise make the server drop the whole record (spec/wire.md §3):
 * at most 200 props, keys ≤ 128 bytes, string/json values ≤ 8192 bytes.
 * Offending props are skipped; `undefined`, functions and symbols are ignored.
 */
export const toProps = (obj?: Props | null): Prop[] => {
  const list: Prop[] = []
  for (const k in obj) {
    let v: any = obj[k]
    const t = typeof v
    let tag = -1
    if (v === null) tag = 0
    else if (t == 'boolean') tag = v ? 2 : 1
    else if (t == 'number') tag = Number.isSafeInteger(v) ? 3 : 4
    else if (t == 'bigint') {
      // int64 when it fits, decimal string otherwise
      if (BigInt.asIntN(64, v) == v) tag = 3
      else (tag = 5), (v += '')
    } else if (t == 'string') tag = 5
    else if (t == 'object') {
      try {
        v = JSON.stringify(v)
        // Date and friends serialise to a JSON string: send them as strings
        tag = v[0] == '"' ? ((v = JSON.parse(v)), 5) : 6
      } catch {}
    }
    if (tag >= 0 && list.length < 200 && utf8(k).length <= 128 && !(tag > 4 && utf8(v).length > 8192))
      list.push([k, tag, v])
  }
  return list
}

/** `varint(len)` + body. */
export const frame = (body: ArrayLike<number>): Bytes => {
  const o: Bytes = []
  varint(o, body.length)
  put(o, body)
  return o
}

/** A framed Record (length prefix included). */
export const record = (kind: number, uuid: ArrayLike<number>, ts: number, name: string, props: Prop[]): Bytes => {
  const o: Bytes = [kind]
  put(o, uuid)
  varint(o, ts)
  str(o, name)
  propList(o, props)
  return frame(o)
}

/**
 * The identity part of a HeaderBody: everything after `batch_id` and `sent_at`.
 * It is fixed when a batch is opened, while `sent_at` is stamped on every
 * (re)send, so the two are kept apart.
 */
export const headerTail = (
  sdk: string,
  anonId: string,
  userId: string,
  sessionId: ArrayLike<number>,
  context: Prop[],
): Bytes => {
  const o: Bytes = []
  str(o, sdk)
  str(o, anonId)
  str(o, userId)
  put(o, sessionId)
  propList(o, context)
  return o
}

/** Batch := "LA1\0" Header Record* */
export const batch = (
  batchId: ArrayLike<number>,
  sentAt: number,
  tail: ArrayLike<number>,
  records: ArrayLike<number>,
): Uint8Array<ArrayBuffer> => {
  const h: Bytes = []
  put(h, batchId)
  varint(h, sentAt)
  put(h, tail)
  const o: Bytes = [76, 65, 49, 0]
  put(o, frame(h))
  put(o, records)
  return new Uint8Array(o)
}

/** Lower-case hex of a byte list. */
export const hex = (b: ArrayLike<number>): string => Array.from(b, (x) => (x + 256).toString(16).slice(1)).join('')

/** Inverse of `hex` (input length must be even). */
export const unhex = (s: string): Bytes => (s.match(/../g) || []).map((x) => parseInt(x, 16))
