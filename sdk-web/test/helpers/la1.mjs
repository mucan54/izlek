// Minimal LA1 decoder for tests (spec/wire.md). Produces the logical batch
// shape of spec/vectors/wire.json: ints as decimal strings, floats as numbers.

const TAGS = ['null', 'bool', 'bool', 'int', 'float', 'string', 'json']

export function decodeBatch(bytes, { strict = false } = {}) {
  let i = 0
  let end = bytes.length
  const dec = new TextDecoder('utf-8', { fatal: true })
  const fail = (m) => {
    throw new Error('LA1: ' + m)
  }
  const u8 = () => (i < end ? bytes[i++] : fail('truncated'))
  const varint = () => {
    let v = 0n
    for (let n = 0; n < 10; n++) {
      const c = u8()
      v |= BigInt(c & 127) << BigInt(7 * n)
      if (c < 128) return v > 0xffffffffffffffffn ? fail('varint overflow') : v
    }
    fail('varint too long')
  }
  const num = () => Number(varint())
  const take = (n) => {
    if (i + n > end) fail('truncated')
    return bytes.subarray(i, (i += n))
  }
  const hex = (b) => Buffer.from(b).toString('hex')
  const str = () => dec.decode(take(num()))
  const props = () => {
    const out = []
    for (let n = num(); n > 0; n--) {
      const k = str()
      const tag = u8()
      let v = null
      if (tag == 1) v = false
      else if (tag == 2) v = true
      else if (tag == 3) {
        const z = varint()
        v = ((z >> 1n) ^ -(z & 1n)).toString()
      } else if (tag == 4) v = new DataView(take(8).slice().buffer).getFloat64(0, true)
      else if (tag == 5 || tag == 6) v = str()
      else if (tag != 0) fail('unknown tag ' + tag)
      out.push({ k, t: TAGS[tag], v })
    }
    return out
  }
  /** Run `f` on the next length-prefixed body; trailing bytes are an error only in strict mode. */
  const framed = (min, f) => {
    const len = num()
    if (len < min || len > 65536 || i + len > end) fail('bad length ' + len)
    const outer = end
    const next = i + len
    end = next
    const v = f()
    if (strict && i != next) fail('trailing bytes in frame')
    i = next
    end = outer
    return v
  }

  if (hex(take(4)) != '4c413100') fail('bad magic')
  const batch = framed(16, () => ({
    batch_id: hex(take(16)),
    sent_at: num(),
    sdk: str(),
    anon_id: str(),
    user_id: str(),
    session_id: hex(take(16)),
    context: props(),
  }))
  batch.records = []
  while (i < end) {
    const r = framed(1, () => {
      const kind = u8()
      if (kind > 3) return null
      return { kind, uuid: hex(take(16)), ts: num(), name: str(), props: props() }
    })
    if (r) batch.records.push(r)
  }
  if (batch.records.length > 500) fail('too many records')
  return batch
}

/** Typed prop list → plain object (last key wins; ints become numbers or bigints, json is parsed). */
export function propsToObject(list) {
  const o = {}
  for (const { k, t, v } of list) {
    if (t == 'int') o[k] = Number.isSafeInteger(Number(v)) ? Number(v) : BigInt(v)
    else if (t == 'json') o[k] = JSON.parse(v)
    else o[k] = v
  }
  return o
}
