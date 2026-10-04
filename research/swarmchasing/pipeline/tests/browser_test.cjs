// Headless Chromium smoke test: page loads, no console errors, node count matches graph.json, live mode connects.
// Usage: python3 serve.py 8765 & ; node tests/browser_test.cjs [url]
const {chromium} = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const fs = require('node:fs'), path = require('node:path'), assert = require('node:assert/strict');
(async () => {
  const url = process.argv[2] || 'http://127.0.0.1:8765/';
  const g = JSON.parse(fs.readFileSync(path.join(__dirname, '..', 'web', 'graph.json')));
  const want = g.nodes.kind.length;
  const browser = await chromium.launch({headless: true, ...(process.env.CHROMIUM_PATH ? {executablePath: process.env.CHROMIUM_PATH} : {}), args: ['--use-angle=swiftshader', '--enable-unsafe-swiftshader', '--ignore-gpu-blocklist']});
  const errors = [];
  for (const vp of [{width: 1280, height: 800}, {width: 390, height: 844}]) {
    const page = await browser.newPage({viewport: vp});
    page.on('console', m => { if (m.type() === 'error') errors.push(m.text()); });
    page.on('pageerror', e => errors.push(String(e)));
    await page.goto(url, {waitUntil: 'networkidle'});
    await page.waitForFunction(() => window.__swarmgraph, null, {timeout: 30000});
    const n = await page.evaluate(() => window.__swarmgraph.nodes());
    assert.equal(n, want, 'node count');
    assert.equal(await page.getAttribute('body', 'data-nodes'), String(want));
    const overflow = await page.evaluate(() => document.documentElement.scrollWidth > innerWidth);
    assert.equal(overflow, false, 'horizontal overflow at ' + vp.width);
    await page.fill('#slider', '300'); await page.dispatchEvent('#slider', 'input');
    await page.click('#live'); await page.waitForTimeout(2500);
    await page.screenshot({path: path.join(process.env.SHOT_DIR || '/tmp', `swarmgraph-${vp.width}.png`)});
    console.log(vp.width, 'nodes', n, '|', await page.textContent('#stats'), '|', await page.textContent('#live'));
    await page.close();
  }
  await browser.close();
  assert.deepEqual(errors, [], 'console errors');
  console.log('ok: no console errors');
})().catch(e => { console.error(e); process.exit(1); });
