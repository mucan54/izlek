import { utf8 } from './wire'

/** MurmurHash3 x86_32, seed 0, over the UTF-8 bytes of `input`; unsigned 32-bit result. */
export const murmur3 = (input: string): number => {
  const b = utf8(input)
  const n = b.length
  let h = 0
  let i = 0
  let k: number
  const mix = () => {
    k = Math.imul(k, 0xcc9e2d51)
    k = (k << 15) | (k >>> 17)
    h ^= Math.imul(k, 0x1b873593)
  }
  for (; i + 4 <= n; i += 4) {
    k = b[i] | (b[i + 1] << 8) | (b[i + 2] << 16) | (b[i + 3] << 24)
    mix()
    h = (h << 13) | (h >>> 19)
    h = (Math.imul(h, 5) + 0xe6546b64) | 0
  }
  if (n & 3) {
    // tail: missing bytes read as `undefined`, which the bit ops treat as 0
    k = b[i] | (b[i + 1] << 8) | (b[i + 2] << 16)
    mix()
  }
  h ^= n
  h = Math.imul(h ^ (h >>> 16), 0x85ebca6b)
  h = Math.imul(h ^ (h >>> 13), 0xc2b2ae35)
  return (h ^ (h >>> 16)) >>> 0
}
