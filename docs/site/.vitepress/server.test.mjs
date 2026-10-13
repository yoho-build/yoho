import assert from 'node:assert/strict'
import { mkdtemp, mkdir, readFile, rm, symlink, writeFile } from 'node:fs/promises'
import { request } from 'node:http'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { spawnSync } from 'node:child_process'
import { fileURLToPath } from 'node:url'
import { test } from 'node:test'
import { createServer as createDocsDevServer } from 'vitepress'
import config from './config.mjs'
import { outDir, pageURL, routePath, siteDir, sourcePaths } from './routes.mjs'
import { parsePort, startPreview } from './preview.mjs'

function get(port, path, method = 'GET') {
  return new Promise((accept, reject) => {
    const req = request({ host: '127.0.0.1', port, path, method }, res => {
      const chunks = []
      res.on('data', chunk => chunks.push(chunk))
      res.on('end', () => accept({ status: res.statusCode, headers: res.headers, body: Buffer.concat(chunks) }))
    })
    req.on('error', reject)
    req.end()
  })
}
async function close(server) {
  server.closeAllConnections()
  await new Promise((accept, reject) => server.close(error => error ? reject(error) : accept()))
}

test('editor guard precedes upstream middleware with identical Connect path matching', async t => {
  const guard = config.vite.plugins.find(plugin => plugin.name === 'yoho-disable-open-in-editor')
  assert.ok(guard)
  const server = await createDocsDevServer(siteDir, { host: config.vite.server.host, port: 0 })
  // Middleware-only requests do not load client modules; avoid starting an
  // unrelated dependency scan that would be canceled when this test closes.
  server.config.optimizeDeps.noDiscovery = true
  server.config.optimizeDeps.include = []
  t.after(() => server.close())
  const mounted = server.middlewares.stack.filter(layer => layer.route === '/__open-in-editor')
  assert.equal(mounted.length, 2, 'guard must be mounted before the upstream editor handler')
  let upstreamCalls = 0
  // Trap the upstream handler in this isolated server: never launch an editor
  // or probe UNC/SMB, even if the guard regresses.
  mounted[1].handle = (_req, res) => { upstreamCalls++; res.end('upstream editor handler') }
  await new Promise((accept, reject) => {
    server.httpServer.once('error', reject)
    server.httpServer.listen(0, '127.0.0.1', accept)
  })
  assert.equal(server.httpServer.address().address, '127.0.0.1')
  const port = server.httpServer.address().port
  for (const path of ['/__open-in-editor', '/__OPEN-IN-EDITOR', '/__open-in-editor/', '/__open-in-editor/child', '/__open-in-editor.js']) {
    const response = await get(port, path + '?file=never-open-this-file')
    assert.equal(response.status, 404, path)
    assert.equal(response.body.toString(), 'Not found\n', path)
  }
  assert.equal(upstreamCalls, 0)
})

test('preview binds IPv4 loopback and serves every built route, asset, HEAD and true 404', async t => {
  const server = await startPreview({ port: 0 })
  t.after(() => close(server))
  assert.equal(server.address().address, '127.0.0.1')
  assert.equal(server.address().family, 'IPv4')
  const port = server.address().port
  for (const source of sourcePaths) {
    const url = pageURL(source)
    const response = await get(port, url)
    assert.equal(response.status, 200, url)
    assert.match(response.headers['content-type'], /^text\/html/)
    assert.deepEqual(response.body, await readFile(join(outDir, routePath(source).replace(/\.md$/, '.html'))))
    if (url !== '/') {
      const redirect = await get(port, url.slice(0, -1))
      assert.equal(redirect.status, 301, url)
      assert.equal(redirect.headers.location, url)
    }
  }
  for (const [path, type] of [
    ['/install.sh', 'text/plain'], ['/schema/yoho.schema.json', 'application/json'],
    ['/schema/x-yoho.schema.json', 'application/json'], ['/assets/logo.svg', 'image/svg+xml'], ['/CNAME', 'text/plain']
  ]) {
    const response = await get(port, path)
    assert.equal(response.status, 200, path)
    assert.ok(response.headers['content-type'].startsWith(type), path)
    assert.deepEqual(response.body, await readFile(join(outDir, path)))
    const head = await get(port, path, 'HEAD')
    assert.equal(head.status, 200)
    assert.equal(head.body.length, 0)
    assert.equal(Number(head.headers['content-length']), response.body.length)
  }
  assert.equal((await get(port, '/not-a-page/')).status, 404)
  assert.equal((await get(port, '/', 'POST')).status, 405)
  await assert.rejects(startPreview({ port }), { code: 'EADDRINUSE' })
  const cli = spawnSync(process.execPath, [fileURLToPath(new URL('./preview.mjs', import.meta.url)), '--port', String(port)], { encoding: 'utf8' })
  assert.equal(cli.status, 1)
  assert.match(cli.stderr, /port is already in use/)
})

test('preview refuses traversal, hidden/source paths, malformed encodings and symlink escapes', async t => {
  const dir = await mkdtemp(join(tmpdir(), 'yoho-docs-preview-'))
  t.after(() => rm(dir, { recursive: true, force: true }))
  const root = join(dir, 'dist')
  await mkdir(root)
  await writeFile(join(root, 'index.html'), 'public')
  await writeFile(join(dir, 'outside.txt'), 'private fixture')
  await writeFile(join(root, '.env'), 'private fixture')
  for (const hidden of ['.git', '.yoho']) {
    await mkdir(join(root, hidden))
    await writeFile(join(root, hidden, 'config.json'), 'private fixture')
  }
  await writeFile(join(root, 'source.md'), 'not a static output type')
  await symlink(join(dir, 'outside.txt'), join(root, 'escape.txt'))
  await symlink(join(root, '.git', 'config.json'), join(root, 'hidden-alias.json'))
  await symlink(dir, join(root, 'escape-dir'))
  const server = await startPreview({ port: 0, root })
  t.after(() => close(server))
  const port = server.address().port
  for (const path of [
    '/../outside.txt', '/%2e%2e/outside.txt', '/%2E%2E%2Foutside.txt', '/%252e%252e/outside.txt',
    '/%5c..%5coutside.txt', '/.git/config.json', '/.yoho/config.json', '/.env', '/%2eenv',
    '/source.md', '/escape.txt', '/hidden-alias.json', '/escape-dir/outside.txt',
    '/%00', '/%0a', '/%', '//outside.txt', '/C:/outside.txt', '/index.html:stream'
  ]) {
    const response = await get(port, path)
    assert.equal(response.status, 404, path)
    assert.ok(!response.body.toString().includes('private fixture'), path)
  }
  const alias = join(dir, 'alias')
  await symlink(root, alias)
  await assert.rejects(startPreview({ root: alias, port: 0 }), /must not be a symlink/)
})

test('preview port arguments are conventional and cannot override host/root', () => {
  assert.equal(parsePort([]), 4173)
  assert.equal(parsePort(['--port', '4181']), 4181)
  assert.equal(parsePort(['--port=4181']), 4181)
  for (const args of [['--host', '0.0.0.0'], ['--port', '0'], ['--port', '65536'], ['--port', 'nan'], ['--port='], ['--root', '/']]) {
    assert.throws(() => parsePort(args), /Usage/)
  }
})
