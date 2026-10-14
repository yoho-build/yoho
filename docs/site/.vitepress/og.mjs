// Renders docs/site/assets/og.svg to docs/site/public/og.png (1200x630), the
// Open Graph image. The docs workflow runs this after installing the Inter
// font and Playwright's Chromium; locally: `npx playwright install chromium`
// (and Inter), then `npm run docs:og`. The PNG is generated, not committed.
import { chromium } from 'playwright'
import { readFileSync } from 'node:fs'
import { join } from 'node:path'
import { siteDir } from './routes.mjs'

const svg = readFileSync(join(siteDir, 'assets/og.svg'), 'utf8')
const out = join(siteDir, 'public/og.png')
const browser = await chromium.launch()
try {
  const page = await browser.newPage({ viewport: { width: 1200, height: 630 }, deviceScaleFactor: 1 })
  await page.setContent(`<!doctype html><style>html,body{margin:0;background:#0d1c17}svg{display:block}</style>${svg}`)
  await page.evaluate(() => document.fonts.ready)
  // A missing font silently falls back and changes the layout: refuse.
  const inter = await page.evaluate(() => {
    const ctx = document.createElement('canvas').getContext('2d')
    const width = font => { ctx.font = font; return ctx.measureText('Deploy Docker Compose apps').width }
    return width('800 60px Inter, monospace') !== width('800 60px monospace')
  })
  if (!inter) throw new Error('Inter font is not installed (apt-get install fonts-inter, or install Inter)')
  await page.locator('svg').screenshot({ path: out, type: 'png' })
} finally {
  await browser.close()
}
console.log(`Wrote ${out}`)
