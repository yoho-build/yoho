// Renders docs/site/assets/og.svg to docs/site/public/og.png (1200x630), the
// Open Graph image. The PNG is committed through Git LFS; rerun this after
// editing the SVG. Needs Playwright with Chromium, e.g.:
//   npx -y -p playwright@1.56.1 node docs/site/.vitepress/og.mjs
// Fonts come from the machine (Inter, then system sans), so commit the PNG
// rather than rendering it in CI.
import { readFileSync } from 'node:fs'
import { createRequire } from 'node:module'
import { join } from 'node:path'
import { siteDir } from './routes.mjs'

const require = createRequire(import.meta.url)
let chromium
try {
  ({ chromium } = require('playwright'))
} catch {
  console.error('Playwright not found: npx -y -p playwright@1.56.1 node docs/site/.vitepress/og.mjs')
  process.exit(1)
}
const svg = readFileSync(join(siteDir, 'assets/og.svg'), 'utf8')
const out = join(siteDir, 'public/og.png')
const browser = await chromium.launch()
const page = await browser.newPage({ viewport: { width: 1200, height: 630 }, deviceScaleFactor: 1 })
await page.setContent(`<!doctype html><style>html,body{margin:0;background:#0d1c17}svg{display:block}</style>${svg}`)
await page.evaluate(() => document.fonts.ready)
await page.locator('svg').screenshot({ path: out, type: 'png' })
await browser.close()
console.log(`Wrote ${out}`)
