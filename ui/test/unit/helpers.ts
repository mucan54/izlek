import { readFileSync } from 'node:fs'

/** Reads a JSON file from spec/vectors. */
export const vectors = <T>(name: string): T => JSON.parse(readFileSync(`${__UI_ROOT__}/../spec/vectors/${name}`, 'utf8')) as T

export const fromHex = (hex: string): Uint8Array => Uint8Array.from(hex.match(/../g) ?? [], (b) => parseInt(b, 16))

/** A fetch stand-in that records calls and replays canned responses. */
export const fakeFetch = (responses: (Response | Error)[]) => {
  const calls: { url: string; init: RequestInit }[] = []
  const fn = async (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
    calls.push({ url: String(input), init: init ?? {} })
    const next = responses.shift()
    if (!next) throw new Error('unexpected request: ' + String(input))
    if (next instanceof Error) throw next
    return next
  }
  return { fn: fn as typeof fetch, calls }
}

export const json = (status: number, body: unknown): Response =>
  new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })
