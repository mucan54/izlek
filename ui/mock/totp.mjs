// TOTP (RFC 6238: SHA-1, 30 s step, 6 digits) with base32 secrets.
import { createHmac, randomBytes } from 'node:crypto'

const ALPHABET = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567'

export const base32 = (bytes) => {
  let bits = ''
  for (const b of bytes) bits += b.toString(2).padStart(8, '0')
  let out = ''
  for (let i = 0; i < bits.length; i += 5) out += ALPHABET[parseInt(bits.slice(i, i + 5).padEnd(5, '0'), 2)]
  return out
}

export const unbase32 = (text) => {
  let bits = ''
  for (const c of text.replace(/=+$/, '').toUpperCase()) bits += ALPHABET.indexOf(c).toString(2).padStart(5, '0')
  const out = []
  for (let i = 0; i + 8 <= bits.length; i += 8) out.push(parseInt(bits.slice(i, i + 8), 2))
  return Buffer.from(out)
}

export const newSecret = () => base32(randomBytes(20))

export const totp = (secret, nowMs = Date.now(), step = 0) => {
  const counter = Buffer.alloc(8)
  counter.writeBigUInt64BE(BigInt(Math.floor(nowMs / 30000) + step))
  const mac = createHmac('sha1', unbase32(secret)).update(counter).digest()
  const offset = mac[mac.length - 1] & 15
  const code = (mac.readUInt32BE(offset) & 0x7fffffff) % 1_000_000
  return String(code).padStart(6, '0')
}

/** Accepts the current step and one step either side (clock skew). */
export const verifyTotp = (secret, code, nowMs = Date.now()) =>
  typeof code == 'string' && [-1, 0, 1].some((step) => totp(secret, nowMs, step) == code.trim())
