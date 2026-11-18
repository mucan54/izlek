import { readFileSync } from 'node:fs'

/** Load a shared test vector file from the repo-level spec (never copied). */
export const vectors = (name) => JSON.parse(readFileSync(new URL(`../../../spec/vectors/${name}.json`, import.meta.url), 'utf8'))

export const fromHex = (h) => new Uint8Array(Buffer.from(h, 'hex'))
export const toHex = (b) => Buffer.from(b).toString('hex')
