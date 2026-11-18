// Builds dist/: one hashed JS bundle, one hashed CSS file and index.html.
// `node build.mjs --watch` rebuilds on change (used by scripts/dev.mjs).
import { context } from 'esbuild'
import { mkdir, readFile, rm, writeFile } from 'node:fs/promises'
import { gzipSync } from 'node:zlib'
import { fileURLToPath } from 'node:url'
import path from 'node:path'

const root = path.dirname(fileURLToPath(import.meta.url))
const dist = path.join(root, 'dist')
const watch = process.argv.includes('--watch')

/** Writes index.html pointing at the hashed outputs and prints their sizes. */
const htmlPlugin = {
  name: 'html',
  setup(b) {
    b.onEnd(async (result) => {
      if (result.errors.length || !result.metafile) return
      const outputs = Object.keys(result.metafile.outputs).map((f) => path.relative(dist, path.join(root, f)))
      const js = outputs.find((f) => f.endsWith('.js'))
      const css = outputs.find((f) => f.endsWith('.css'))
      const template = await readFile(path.join(root, 'src/index.html'), 'utf8')
      const html = template.replace('%CSS%', '/' + css).replace('%JS%', '/' + js)
      await writeFile(path.join(dist, 'index.html'), html)
      for (const f of [js, css]) {
        const bytes = await readFile(path.join(dist, f))
        console.log(`${f.padEnd(32)} ${String(bytes.length).padStart(8)} B   gzip ${String(gzipSync(bytes, { level: 9 }).length).padStart(7)} B`)
      }
    })
  },
}

await rm(dist, { recursive: true, force: true })
await mkdir(dist, { recursive: true })

const ctx = await context({
  absWorkingDir: root,
  entryPoints: { app: 'src/main.tsx' },
  bundle: true,
  format: 'esm',
  target: 'es2022',
  jsx: 'automatic',
  jsxImportSource: 'preact',
  minify: !watch,
  sourcemap: watch ? 'linked' : false,
  entryNames: 'assets/[name]-[hash]',
  outdir: 'dist',
  metafile: true,
  legalComments: 'none',
  logLevel: 'warning',
  plugins: [htmlPlugin],
})

if (watch) {
  await ctx.watch()
  console.log('watching src/ …')
} else {
  await ctx.rebuild()
  await ctx.dispose()
}
