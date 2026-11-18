// Size check: minified bundles, gzip -9 (and brotli for reference).
// Fails when the core exceeds its 4 KB gzip budget (docs/design.md §5).
import { build } from 'esbuild'
import { brotliCompressSync, constants, gzipSync } from 'node:zlib'

import { coreFromGlobal, scripts } from '../build.mjs'

const CORE_BUDGET = 4096

const bundle = async (entry, options = {}) => {
  const r = await build({
    entryPoints: [entry],
    bundle: true,
    minify: true,
    target: 'es2020',
    format: 'esm',
    write: false,
    legalComments: 'none',
    logLevel: 'warning',
    ...options,
  })
  const code = r.outputFiles[0].contents
  return {
    min: code.length,
    gzip: gzipSync(code, { level: 9 }).length,
    brotli: brotliCompressSync(code, { params: { [constants.BROTLI_PARAM_QUALITY]: 11 } }).length,
  }
}

const iife = (plugins = []) => ({ format: 'iife', plugins })
// add-on cost on top of the core: the core is left out of the bundle
const addOn = { external: ['./index'] }

const rows = [
  ['core (ESM, src/index.ts)', await bundle('src/index.ts')],
  ['core (<script>, izlek.min.js)', await bundle(scripts[0][0], iife())],
  ['flags, on top of core (ESM)', await bundle('src/flags.ts', addOn)],
  ['flags (<script>, izlek-flags.min.js)', await bundle(scripts[1][0], iife([coreFromGlobal]))],
  ['replay, on top of core (ESM)', await bundle('src/replay.ts', addOn)],
  ['replay (<script>, izlek-replay.min.js)', await bundle(scripts[2][0], iife([coreFromGlobal]))],
  ['replay-decode (standalone, ESM)', await bundle('src/replay-decode.ts')],
  ['core + flags + replay (one ESM bundle)', await bundle('scripts/all.entry.ts')],
]

const pad = (s, n) => String(s).padStart(n)
console.log(`${'bundle'.padEnd(42)}${pad('min', 8)}${pad('gzip', 8)}${pad('brotli', 8)}`)
for (const [name, s] of rows) console.log(`${name.padEnd(42)}${pad(s.min, 8)}${pad(s.gzip, 8)}${pad(s.brotli, 8)}`)

const core = Math.max(rows[0][1].gzip, rows[1][1].gzip)
console.log(`\ncore gzip: ${core} bytes, budget ${CORE_BUDGET} → ${core <= CORE_BUDGET ? 'OK' : 'OVER BUDGET'}`)
process.exit(core <= CORE_BUDGET ? 0 : 1)
