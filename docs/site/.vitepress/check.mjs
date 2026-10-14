import assert from 'node:assert/strict'
import { existsSync, readFileSync, readdirSync } from 'node:fs'
import { join } from 'node:path'
import { pathToFileURL } from 'node:url'
import { headingSlug, outDir, pageURL, repoDir, routePath, siteDir, sourceLink, sourcePaths } from './routes.mjs'

assert.equal(routePath('faq.md'), 'faq/index.md')
assert.equal(routePath('migrating/tool.md'), 'migrating/tool/index.md')
assert.equal(routePath('migrating/index.md'), 'migrating/index.md')
assert.equal(sourceLink('../faq.md#status', 'migrating/tool.md'), '/faq/#status')
assert.equal(sourceLink('getting-started.md', 'index.md'), '/getting-started/')
assert.equal(sourceLink('https://example.com/guide.md', 'index.md'), 'https://example.com/guide.md')
assert.equal(headingSlug('Targets and jobs (yoho.yml)'), 'targets-and-jobs-yohoyml')
assert.equal(headingSlug('60 seconds'), '60-seconds')

function files(dir) {
  return readdirSync(dir, { withFileTypes: true }).flatMap(entry => {
    const file = join(dir, entry.name)
    return entry.isDirectory() ? files(file) : [file]
  })
}
function decode(text) {
  return text.replaceAll('&amp;', '&').replaceAll('&quot;', '"').replaceAll('&#39;', "'")
}
let links = 0
for (const source of sourcePaths) {
  const url = pageURL(source)
  const htmlFile = join(outDir, routePath(source).replace(/\.md$/, '.html'))
  assert.ok(existsSync(htmlFile), `Missing old MkDocs directory URL: ${url}`)
  const html = readFileSync(htmlFile, 'utf8')
  // Python Markdown toc anchors for existing headings, ignoring fenced code.
  const markdown = readFileSync(join(siteDir, source), 'utf8').replace(/^(`{3,}|~{3,})[^\n]*\n[\s\S]*?^\1[^\n]*$/gm, '')
  for (const heading of markdown.matchAll(/^#{1,6}\s+(.+)$/gm)) {
    const id = headingSlug(heading[1].replace(/`/g, ''))
    assert.ok(html.includes(`id="${id}"`), `${url} lost bookmarked heading #${id}`)
  }
  assert.ok(!/^\s*(?:!!!|\?\?\?)\s+\w+/m.test(markdown), `${source}: convert MkDocs admonitions to VitePress ::: containers`)
  for (const match of html.matchAll(/\b(?:href|src)="([^"]+)"/g)) {
    const target = new URL(decode(match[1]), `https://yohobuild.com${url}`)
    if (target.origin !== 'https://yohobuild.com') continue
    let targetFile = join(outDir, decodeURIComponent(target.pathname))
    if (target.pathname.endsWith('/')) targetFile = join(targetFile, 'index.html')
    assert.ok(existsSync(targetFile), `${url} broken static link: ${match[1]}`)
    if (target.hash && targetFile.endsWith('.html')) {
      const id = decodeURIComponent(target.hash.slice(1))
      assert.ok(readFileSync(targetFile, 'utf8').includes(`id="${id}"`), `${url} broken anchor: ${match[1]}`)
    }
    links++
  }
}
assert.equal(readFileSync(join(outDir, 'install.sh'), 'utf8'), readFileSync(join(repoDir, 'scripts/install.sh'), 'utf8'))
for (const name of ['yoho', 'x-yoho']) {
  const path = `schema/${name}.schema.json`
  const schema = JSON.parse(readFileSync(join(outDir, path), 'utf8'))
  assert.ok(schema.$schema && (schema.properties || schema.$defs?.[schema.$ref?.split('/').at(-1)]?.properties), `${path}: not a JSON Schema`)
  assert.equal(readFileSync(join(outDir, path), 'utf8'), readFileSync(join(siteDir, 'public', path), 'utf8'))
}
assert.equal(readFileSync(join(outDir, 'CNAME'), 'utf8').trim(), 'yohobuild.com')
assert.equal(readFileSync(join(outDir, 'assets/logo.svg'), 'utf8'), readFileSync(join(siteDir, 'assets/logo.svg'), 'utf8'))
// og.png is stored in Git LFS: a checkout without LFS publishes a pointer file.
const og = readFileSync(join(outDir, 'og.png'))
assert.ok(og.subarray(0, 8).equals(Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a])),
  'og.png is not a PNG (Git LFS pointer? check out with LFS, e.g. actions/checkout lfs: true)')
assert.deepEqual([og.readUInt32BE(16), og.readUInt32BE(20)], [1200, 630], 'og.png must be 1200x630')
for (const source of sourcePaths) {
  const html = readFileSync(join(outDir, routePath(source).replace(/\.md$/, '.html')), 'utf8')
  for (const tag of ['property="og:image" content="https://yohobuild.com/og.png"', 'name="twitter:card" content="summary_large_image"', 'property="og:title"', 'property="og:url"']) {
    assert.ok(html.includes(tag), `${source}: missing <meta ${tag}>`)
  }
}
const searchFile = files(join(outDir, 'assets')).find(file => /localSearchIndex.*\.js$/.test(file))
assert.ok(searchFile, 'Missing local search index')
const searchIndex = JSON.parse((await import(pathToFileURL(searchFile).href)).default)
for (const source of sourcePaths) {
  assert.ok(Object.values(searchIndex.documentIds).some(id => id.startsWith(pageURL(source) + '#')), `${source}: missing from local search`)
}
console.log(`Docs checks passed: ${sourcePaths.length} directory pages, preserved heading anchors, ${links} local links, installer, schemas, logo, social card, CNAME and local search.`)
