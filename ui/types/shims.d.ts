// Ambient declarations for things the bundler resolves and for the few Node
// built-ins the unit tests use (the UI itself never imports them).
declare module '*.css'

declare module 'node:test' {
  export function test(name: string, fn: () => void | Promise<void>): void
}
declare module 'node:assert/strict' {
  interface Assert {
    (value: unknown, message?: string): asserts value
    ok(value: unknown, message?: string): asserts value
    equal(actual: unknown, expected: unknown, message?: string): void
    notEqual(actual: unknown, expected: unknown, message?: string): void
    deepEqual(actual: unknown, expected: unknown, message?: string): void
    notDeepEqual(actual: unknown, expected: unknown, message?: string): void
    throws(fn: () => unknown, expected?: unknown, message?: string): void
    rejects(fn: Promise<unknown> | (() => Promise<unknown>), expected?: unknown, message?: string): Promise<void>
    match(value: string, re: RegExp, message?: string): void
  }
  const assert: Assert
  export default assert
}
declare module 'node:fs' {
  export function readFileSync(path: string | URL, encoding: 'utf8'): string
}
declare module 'node:zlib' {
  export function gzipSync(data: Uint8Array): Uint8Array
  /** Node ≥ 22.15 */
  export const zstdCompressSync: ((data: Uint8Array) => Uint8Array) | undefined
}
declare module '*.mjs'

/** Absolute path of /ui, injected by scripts/unit.mjs. */
declare const __UI_ROOT__: string
