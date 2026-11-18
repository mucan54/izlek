// Bundles the TypeScript unit tests with esbuild, then runs them with node:test.
import { build } from 'esbuild'
import { readdirSync, rmSync } from 'node:fs'
import { spawnSync } from 'node:child_process'
import { fileURLToPath } from 'node:url'
import path from 'node:path'

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const out = path.join(root, 'build/unit')
rmSync(out, { recursive: true, force: true })

const tests = readdirSync(path.join(root, 'test/unit')).filter((f) => f.endsWith('.test.ts'))
await build({
  absWorkingDir: root,
  entryPoints: tests.map((f) => 'test/unit/' + f),
  bundle: true,
  format: 'esm',
  platform: 'node',
  target: 'node22',
  jsx: 'automatic',
  jsxImportSource: 'preact',
  outdir: out,
  outExtension: { '.js': '.mjs' },
  // tests locate spec/vectors relative to the repo, not to the bundle
  define: { __UI_ROOT__: JSON.stringify(root) },
  logLevel: 'warning',
})

const files = tests.map((f) => path.join(out, f.replace(/\.ts$/, '.mjs')))
const run = spawnSync(process.execPath, ['--test', ...files], { stdio: 'inherit' })
process.exit(run.status ?? 1)
