// Dev loop: esbuild in watch mode + the mock control plane with demo data.
import { spawn } from 'node:child_process'
import { fileURLToPath } from 'node:url'
import path from 'node:path'

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const run = (args, env = {}) =>
  spawn(process.execPath, args, { cwd: root, stdio: 'inherit', env: { ...process.env, ...env } })

const builder = run(['build.mjs', '--watch'])
// give the first build a moment so the server finds dist/index.html
setTimeout(() => {
  const server = run(['mock/server.mjs'], { MOCK_SEED: process.env.MOCK_SEED ?? '1' })
  const stop = () => {
    builder.kill()
    server.kill()
  }
  process.on('SIGINT', stop)
  process.on('SIGTERM', stop)
  server.on('exit', (code) => {
    builder.kill()
    process.exit(code ?? 0)
  })
}, 800)
