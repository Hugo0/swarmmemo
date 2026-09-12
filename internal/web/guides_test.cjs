// Owned loopback only. No requests to external sources and no production posts.
const {chromium} = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const assert = require('node:assert/strict');
const origin = process.env.SWARMMEMO_TEST_URL;
assert.match(origin || '', /^http:\/\/127\.0\.0\.1:\d+$/);
const paths = ['/guides', '/guides/agent-message-board-incident', '/guides/agent-communication-networks', '/guides/http-agent-messaging', '/guides/4chan-for-agents', '/guides/what-people-try-on-agents', '/guides/post-with-one-http-request', '/guides/where-agents-can-post'];
(async () => {
  const browser = await chromium.launch({headless: true, ...(process.env.CHROMIUM_PATH ? {executablePath: process.env.CHROMIUM_PATH} : {})});
  try {
    for (const javaScriptEnabled of [true, false]) {
      for (const width of [320, 1280]) {
        const context = await browser.newContext({javaScriptEnabled, viewport: {width, height: 900}});
        const page = await context.newPage(), errors = [], requests = [];
        page.on('pageerror', e => errors.push(e.message));
        page.on('request', r => requests.push({url: r.url(), method: r.method()}));
        await context.route('**/*', route => new URL(route.request().url()).origin === origin ? route.continue() : route.abort());
        for (const path of paths) {
          const response = await page.goto(origin + path);
          assert.equal(response.status(), 200);
          await page.locator('article.prose').waitFor();
          assert.equal(await page.locator('h1').count(), 1);
          assert.ok((await page.locator('article.prose').innerText()).length > 500);
          assert.equal(await page.locator('link[rel=canonical]').getAttribute('href'), 'https://swarmmemo.com' + path);
          assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth + 1), true, path + ' horizontal overflow');
          assert.equal(await page.locator('form, iframe').count(), 0);
          const copy = page.getByRole('button', {name: 'Copy agent entry URL', exact: true});
          assert.equal(await copy.count(), javaScriptEnabled ? 1 : 0);
          if (javaScriptEnabled) {
            await copy.focus();
            assert.equal(await copy.evaluate(e => e === document.activeElement), true);
          }
          if (process.env.SWARMMEMO_SCREENSHOT_DIR && !javaScriptEnabled && path === '/guides') {
            await page.screenshot({path: process.env.SWARMMEMO_SCREENSHOT_DIR + '/guides-' + width + '.png', fullPage: true});
          }
        }
        assert.deepEqual(errors, []);
        assert.equal(requests.some(r => new URL(r.url).origin !== origin || r.method !== 'GET' || /^\/(w|w64|c64|v1)\//.test(new URL(r.url).pathname)), false, 'guide reading triggered external access or a write');
        await context.close();
      }
    }
    console.log('PASS guides: 16 renders, JS/no-JS, mobile/desktop, metadata, copy focus, read-only requests');
  } finally { await browser.close(); }
})().catch(e => { console.error(e); process.exitCode = 1; });
