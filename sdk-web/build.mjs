// Builds dist/: ESM modules for bundlers and minified IIFE files for <script> tags.
import { build } from 'esbuild'
import { rmSync } from 'node:fs'

const common = { bundle: true, target: 'es2020', logLevel: 'warning', legalComments: 'none' }

/**
 * The flags and replay <script> builds must share the page's single core
 * instance instead of bundling a second copy, so `./index` resolves to the
 * global set by izlek.min.js.
 */
export const coreFromGlobal = {
  name: 'core-from-global',
  setup(b) {
    b.onResolve({ filter: /^\.\/index$/ }, () => ({ path: 'core', namespace: 'core-from-global' }))
    b.onLoad({ filter: /.*/, namespace: 'core-from-global' }, () => ({
      contents: 'export const client = globalThis.izlek.client',
      loader: 'js',
    }))
  },
}

export const esm = (extra = {}) =>
  build({
    ...common,
    entryPoints: ['src/index.ts', 'src/flags.ts', 'src/replay.ts', 'src/replay-decode.ts', 'src/wire.ts'],
    format: 'esm',
    splitting: true,
    chunkNames: 'chunk-[hash]',
    outdir: 'dist',
    ...extra,
  })

export const iife = (entry, outfile, extra = {}) =>
  build({
    ...common,
    entryPoints: [entry],
    format: 'iife',
    minify: true,
    outfile,
    plugins: entry.includes('core') ? [] : [coreFromGlobal],
    ...extra,
  })

/** [entry, output] of the three <script> bundles. */
export const scripts = [
  ['src/script-core.ts', 'dist/izlek.min.js'],
  ['src/script-flags.ts', 'dist/izlek-flags.min.js'],
  ['src/script-replay.ts', 'dist/izlek-replay.min.js'],
]

if (import.meta.url === `file://${process.argv[1]}`) {
  rmSync('dist', { recursive: true, force: true })
  await esm()
  for (const [entry, outfile] of scripts) await iife(entry, outfile)
}
