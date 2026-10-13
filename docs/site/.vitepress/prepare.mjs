import { copyFileSync, mkdirSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'
import { spawnSync } from 'node:child_process'
import { repoDir, siteDir } from './routes.mjs'

const publicDir = join(siteDir, 'public')
mkdirSync(join(publicDir, 'schema'), { recursive: true })
mkdirSync(join(publicDir, 'assets'), { recursive: true })
copyFileSync(join(repoDir, 'scripts/install.sh'), join(publicDir, 'install.sh'))
copyFileSync(join(siteDir, 'assets/logo.svg'), join(publicDir, 'assets/logo.svg'))
for (const [name, flags] of [['yoho', []], ['x-yoho', ['--ext']]]) {
  const result = spawnSync('go', ['run', './cmd/yoho', 'schema', ...flags], {
    cwd: repoDir, encoding: 'utf8', maxBuffer: 8 * 1024 * 1024
  })
  if (result.error || result.status !== 0) {
    throw new Error(`Cannot generate ${name} schema; install the Go version in go.mod.\n${result.error?.message || result.stderr}`)
  }
  JSON.parse(result.stdout)
  writeFileSync(join(publicDir, `schema/${name}.schema.json`), result.stdout)
}
console.log('Docs assets ready: /install.sh, /schema/{yoho,x-yoho}.schema.json, /assets/logo.svg')
