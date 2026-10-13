import { createServer } from 'node:http'
import { constants } from 'node:fs'
import { lstat, open, realpath, stat } from 'node:fs/promises'
import { extname, isAbsolute, join, relative, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { outDir } from './routes.mjs'

const mime = {
  '.html': 'text/html; charset=utf-8', '.css': 'text/css; charset=utf-8',
  '.js': 'text/javascript; charset=utf-8', '.json': 'application/json; charset=utf-8',
  '.svg': 'image/svg+xml', '.png': 'image/png', '.jpg': 'image/jpeg',
  '.jpeg': 'image/jpeg', '.gif': 'image/gif', '.webp': 'image/webp',
  '.ico': 'image/x-icon', '.woff': 'font/woff', '.woff2': 'font/woff2',
  '.ttf': 'font/ttf', '.otf': 'font/otf', '.txt': 'text/plain; charset=utf-8',
  '.xml': 'application/xml; charset=utf-8', '.sh': 'text/plain; charset=utf-8'
}

function inside(root, file) {
  const rel = relative(root, file)
  return !isAbsolute(rel) && rel !== '..' && !rel.startsWith('../') && !rel.startsWith('..\\')
    && !rel.split(/[\\/]/).some(part => part.startsWith('.'))
}

function requestPath(url) {
  // Check the raw request target before URL normalization can erase '..'.
  const raw = (url || '').split(/[?#]/, 1)[0]
  if (!raw.startsWith('/') || raw.startsWith('//')) return null
  let path
  try { path = decodeURIComponent(raw) } catch { return null }
  // Reject double encoding, Windows separators/ADS, controls and hidden paths.
  if (/[\x00-\x1f\x7f\\:%]/.test(path) || path.startsWith('//')) return null
  if (path.split('/').some(part => part.startsWith('.'))) return null
  return path
}

function reply(res, status, message) {
  res.writeHead(status, { 'Content-Type': 'text/plain; charset=utf-8', 'X-Content-Type-Options': 'nosniff' })
  res.end(message + '\n')
}

export async function startPreview({ port = 4173, root = outDir } = {}) {
  if ((await lstat(root)).isSymbolicLink()) throw new Error('Preview output must not be a symlink')
  const publicRoot = await realpath(root)
  if (!(await stat(publicRoot)).isDirectory()) throw new Error('Preview output is not a directory')
  const server = createServer(async (req, res) => {
    let handle
    try {
      if (req.method !== 'GET' && req.method !== 'HEAD') {
        res.setHeader('Allow', 'GET, HEAD')
        reply(res, 405, 'Method not allowed')
        return
      }
      const path = requestPath(req.url)
      if (path === null) { reply(res, 404, 'Not found'); return }
      let file = resolve(publicRoot, '.' + path)
      if (!inside(publicRoot, file)) { reply(res, 404, 'Not found'); return }
      file = await realpath(file)
      if (!inside(publicRoot, file)) { reply(res, 404, 'Not found'); return }
      if ((await stat(file)).isDirectory()) {
        if (!path.endsWith('/')) {
          // Build Location from the validated path, never a request Host header.
          res.writeHead(301, { Location: path.split('/').map(encodeURIComponent).join('/') + '/' })
          res.end()
          return
        }
        file = await realpath(join(file, 'index.html'))
        if (!inside(publicRoot, file)) { reply(res, 404, 'Not found'); return }
      }
      const type = mime[extname(file).toLowerCase()] || (relative(publicRoot, file) === 'CNAME' ? mime['.txt'] : null)
      if (!type) { reply(res, 404, 'Not found'); return }
      // Canonical-path containment rejects symlink escapes; don't follow a newly
      // substituted final symlink on platforms supporting O_NOFOLLOW.
      handle = await open(file, constants.O_RDONLY | (constants.O_NOFOLLOW || 0))
      const info = await handle.stat()
      if (!info.isFile()) { reply(res, 404, 'Not found'); return }
      const body = req.method === 'HEAD' ? null : await handle.readFile()
      res.writeHead(200, {
        'Content-Type': type, 'Content-Length': body?.length ?? info.size,
        'X-Content-Type-Options': 'nosniff'
      })
      res.end(body)
    } catch (error) {
      if (res.headersSent) res.destroy()
      else reply(res, ['ENOENT', 'ENOTDIR', 'EACCES', 'ELOOP'].includes(error.code) ? 404 : 500, 'Not found')
    } finally {
      await handle?.close()
    }
  })
  await new Promise((accept, reject) => {
    server.once('error', reject)
    server.listen(port, '127.0.0.1', () => {
      server.removeListener('error', reject)
      accept()
    })
  })
  return server
}

export function parsePort(args) {
  if (args.length === 0) return 4173
  const value = args.length === 2 && args[0] === '--port' ? args[1]
    : args.length === 1 && args[0].startsWith('--port=') ? args[0].slice(7) : ''
  if (!/^\d+$/.test(value) || Number(value) < 1 || Number(value) > 65535) {
    throw new Error('Usage: npm run docs:preview -- [--port 4173] (port 1–65535)')
  }
  return Number(value)
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  try {
    const server = await startPreview({ port: parsePort(process.argv.slice(2)) })
    console.log(`Docs preview: http://127.0.0.1:${server.address().port}/`)
    for (const signal of ['SIGINT', 'SIGTERM']) process.once(signal, () => server.close())
  } catch (error) {
    console.error(error.code === 'EADDRINUSE' ? 'Docs preview port is already in use; choose another --port.'
      : error.code === 'ENOENT' ? 'Docs output is missing; run npm run docs:build first.' : error.message)
    process.exitCode = 1
  }
}
