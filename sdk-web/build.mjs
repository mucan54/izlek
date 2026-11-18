// Builds dist/: ESM modules for bundlers and minified IIFE files for <script> tags.
import { build } from 'esbuild'

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

export const iife = (entry, globalName, outfile, extra = {}) =>
  build({
    ...common,
    entryPoints: [entry],
    format: 'iife',
    globalName,
    minify: true,
    outfile,
    plugins: entry.endsWith('index.ts') ? [] : [coreFromGlobal],
    ...extra,
  })

if (import.meta.url === `file://${process.argv[1]}`) {
  await esm()
  await iife('src/index.ts', 'izlek', 'dist/izlek.min.js')
  await iife('src/flags.ts', 'izlekFlags', 'dist/izlek-flags.min.js')
  await iife('src/replay.ts', 'izlekReplay', 'dist/izlek-replay.min.js')
}
