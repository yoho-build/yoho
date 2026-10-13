import { readdirSync } from 'node:fs'
import { dirname, join, relative, resolve, sep, posix } from 'node:path'
import { fileURLToPath } from 'node:url'

export const siteDir = resolve(dirname(fileURLToPath(import.meta.url)), '..')
export const repoDir = resolve(siteDir, '../..')
export const outDir = join(siteDir, '.vitepress/dist')

export function markdownFiles(dir = siteDir) {
  return readdirSync(dir, { withFileTypes: true }).flatMap(entry => {
    if (entry.name.startsWith('.') || entry.name === 'public') return []
    const file = join(dir, entry.name)
    return entry.isDirectory() ? markdownFiles(file) : entry.name.endsWith('.md') ? [file] : []
  })
}

export const sourcePaths = markdownFiles().map(file => relative(siteDir, file).split(sep).join('/'))
// MkDocs' default directory URLs, backed by actual index.html files on Pages.
export function routePath(source) {
  return source === 'index.md' || source.endsWith('/index.md') ? source : source.replace(/\.md$/, '/index.md')
}
export function pageURL(source) {
  return '/' + routePath(source).replace(/index\.md$/, '')
}
export const rewrites = Object.fromEntries(sourcePaths.map(source => [source, routePath(source)]))

// Match Python Markdown's default toc slugify, retaining existing bookmarked anchors.
export function headingSlug(text) {
  return text.normalize('NFKD').replace(/[^\x00-\x7F]/g, '').toLowerCase()
    .replace(/[^\w\s-]/g, '').trim().replace(/[\s-]+/g, '-')
}

export function sourceLink(href, source) {
  if (/^(?:[a-z][a-z\d+.-]*:|\/\/|#)/i.test(href)) return href
  const match = href.match(/^([^?#]+\.md)([?#].*)?$/)
  if (!match) return href
  const target = posix.normalize(match[1].startsWith('/') ? match[1].slice(1) : posix.join(posix.dirname(source), match[1]))
  // Unknown Markdown targets still reach VitePress's strict dead-link checker.
  return pageURL(target) + (match[2] || '')
}
