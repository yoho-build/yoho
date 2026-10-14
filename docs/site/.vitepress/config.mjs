import { defineConfig } from 'vitepress'
import { relative } from 'node:path'
import { siteDir, rewrites, sourceLink, headingSlug } from './routes.mjs'

const guide = [
  ['Getting started', 'getting-started'],
  ['Set up with your AI agent', 'agents'],
  ['Configuration', 'configuration'],
  ['Secrets', 'secrets'],
  ['Zero-downtime & proxy', 'zero-downtime'],
  ['Cloudflare Tunnel', 'cloudflare-tunnel'],
  ['Builds & image transport', 'builds'],
  ['Backups & restore', 'backups'],
  ['Scheduled jobs', 'scheduled-jobs'],
  ['Server setup', 'setup'],
  ['Swarm', 'swarm'],
  ['Dashboards (tui / ui)', 'dashboards'],
  ['Hooks & env vars', 'hooks'],
  ['CLI reference', 'cli']
].map(([text, slug]) => ({ text, link: `/${slug}/` }))

export default defineConfig({
  title: 'Yoho',
  description: 'Deploy Docker Compose apps to your own servers with zero downtime.',
  lang: 'en-US',
  // Rewrites emit directories, not extensionless .html aliases or SPA fallbacks.
  cleanUrls: true,
  rewrites,
  ignoreDeadLinks: false,
  head: [['link', { rel: 'icon', type: 'image/svg+xml', href: '/assets/logo.svg' }]],
  markdown: {
    anchor: { slugify: headingSlug },
    headers: { slugify: headingSlug },
    config(md) {
      // Keep repo-friendly .md links in source; resolve them before VP validates links.
      md.core.ruler.push('yoho-source-links', state => {
        const source = relative(siteDir, state.env.realPath || state.env.path).replaceAll('\\', '/')
        for (const block of state.tokens) {
          for (const token of block.children || []) {
            if (token.type === 'link_open') {
              token.attrSet('href', sourceLink(token.attrGet('href'), source))
            }
          }
        }
      })
    }
  },
  transformPageData(page) {
    page.frontmatter.head ??= []
    page.frontmatter.head.push(['link', {
      rel: 'canonical',
      href: 'https://yohobuild.com/' + page.relativePath.replace(/index\.md$/, '').replace(/\.md$/, '.html')
    }])
  },
  themeConfig: {
    logo: { src: '/assets/logo.svg', alt: 'Yoho' },
    nav: [
      { text: 'Guide', link: '/getting-started/' },
      { text: 'CLI reference', link: '/cli/' },
      { text: 'GitHub', link: 'https://github.com/yoho-build/yoho' }
    ],
    sidebar: [
      { text: 'Home', link: '/' },
      { text: 'Documentation', items: guide },
      { text: 'Migrating', items: [
        { text: 'From Kamal', link: '/migrating-from-kamal/' },
        { text: 'From Coolify', link: '/migrating-from-coolify/' },
        { text: 'From Dokploy', link: '/migrating-from-dokploy/' }
      ] },
      { text: 'FAQ', link: '/faq/' },
      { text: 'Contributing', link: '/contributing/' }
    ],
    search: { provider: 'local' },
    outline: [2, 3],
    editLink: {
      pattern: 'https://github.com/yoho-build/yoho/edit/main/docs/site/:path',
      text: 'Edit this page on GitHub'
    },
    footer: {
      message: 'Released under the <a href="https://github.com/yoho-build/yoho/blob/main/LICENSE">MIT License</a>.',
      copyright: '<a href="https://github.com/yoho-build/yoho">Yoho · open source, Compose-native</a>'
    }
  },
  vite: {
    server: { host: '127.0.0.1' },
    plugins: [{
      name: 'yoho-disable-open-in-editor',
      configureServer(server) {
        // Register before Vite's internal middleware. Using the same Connect
        // mount inherits its case-insensitive prefix + '/', '.' or end boundary.
        // Never inspect the file query: even existsSync(UNC) can leak credentials.
        server.middlewares.use('/__open-in-editor', (_req, res) => {
          res.statusCode = 404
          res.setHeader('Content-Type', 'text/plain; charset=utf-8')
          res.end('Not found\n')
        })
      }
    }]
  }
})
