// Renders docs/site/assets/og.svg to the social cards in docs/site/public:
// og.png (1200x630, Open Graph) and twitter.png (1200x600, X's 2:1 large
// card: the same design without the top and bottom 15px of background).
// The docs workflow runs this after installing the Inter font and
// Playwright's Chromium; locally: `npx playwright install chromium` (and
// Inter), then `npm run docs:og`. The PNGs are generated, not committed.
import { chromium } from 'playwright'
import { readFileSync } from 'node:fs'
import { join } from 'node:path'
import { siteDir } from './routes.mjs'

const svg = readFileSync(join(siteDir, 'assets/og.svg'), 'utf8')
const cards = [
  { file: 'og.png', width: 1200, height: 630, viewBox: '0 0 1200 630' },
  { file: 'twitter.png', width: 1200, height: 600, viewBox: '0 15 1200 600' }
]
const browser = await chromium.launch()
try {
  for (const card of cards) {
    const page = await browser.newPage({ viewport: { width: card.width, height: card.height }, deviceScaleFactor: 1 })
    const sized = svg.replace(/<svg [^>]*>/, `<svg xmlns="http://www.w3.org/2000/svg" width="${card.width}" height="${card.height}" viewBox="${card.viewBox}">`)
    await page.setContent(`<!doctype html><style>html,body{margin:0;background:#0d1c17}svg{display:block}</style>${sized}`)
    await page.evaluate(() => document.fonts.ready)
    // A missing font silently falls back and changes the layout: refuse.
    const inter = await page.evaluate(() => {
      const ctx = document.createElement('canvas').getContext('2d')
      const width = font => { ctx.font = font; return ctx.measureText('Deploy Docker Compose apps').width }
      return width('800 60px Inter, monospace') !== width('800 60px monospace')
    })
    if (!inter) throw new Error('Inter font is not installed (apt-get install fonts-inter, or install Inter)')
    const out = join(siteDir, 'public', card.file)
    await page.locator('svg').screenshot({ path: out, type: 'png' })
    await page.close()
    console.log(`Wrote ${out} (${card.width}x${card.height})`)
  }
} finally {
  await browser.close()
}
