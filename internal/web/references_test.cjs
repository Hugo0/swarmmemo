// Read-only browser checks against a synthetic, already-published loopback fixture.
// Launched by curation/test_reference_http.py; never seeds a public server.
const {chromium} = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const assert = require('node:assert/strict');

(async () => {
  const origin = process.env.SWARMMEMO_TEST_URL;
  assert.match(origin || '', /^http:\/\/127\.0\.0\.1:\d+$/, 'reference fixture must be loopback');
  const before = await (await fetch(origin + '/api/stats')).json();
  const projection = await (await fetch(origin + '/api/references')).json();
  assert.equal(projection.references.length, 3, 'requires the synthetic three-row fixture');
  const browser = await chromium.launch({headless: true,
    ...(process.env.CHROMIUM_PATH ? {executablePath: process.env.CHROMIUM_PATH} : {}),
    args: process.env.PLAYWRIGHT_NO_SANDBOX === 'true' ? ['--no-sandbox'] : []});
  try {
    for (const javaScriptEnabled of [true, false]) {
      const context = await browser.newContext({javaScriptEnabled, viewport: {width: 320, height: 780}});
      try {
        await context.addInitScript(() => Object.defineProperty(navigator, 'clipboard', {
          value: {writeText: async value => { window.referenceCopied = value; }}
        }));
        const errors = [], forbidden = [];
        await context.route('**/*', async route => {
          const request = route.request(), url = new URL(request.url());
          if (url.origin !== origin || !['GET', 'HEAD'].includes(request.method()) ||
              /^\/(w|w64|c64|v1|mcp)(\/|$)/.test(url.pathname) ||
              /^\/api\/(stream|changes|events|works|identities)(\/|$)/.test(url.pathname)) {
            forbidden.push(request.method() + ' ' + url.pathname);
            return route.abort();
          }
          return route.continue();
        });
        const page = await context.newPage();
        page.on('pageerror', error => errors.push(error.message));
        const inspect = async () => {
          assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), '320px overflow');
          assert.equal(await page.locator('.reference-card script,.reference-card img').count(), 0);
          assert.equal(await page.evaluate(() => Boolean(window.referenceXSS)), false);
        };
        assert.equal((await page.goto(origin + '/references')).status(), 200);
        assert.equal(await page.locator('.reference-card').count(), 3);
        assert.match(await page.locator('body').textContent(), /not native posts or available jobs/);
        await inspect();
        const copy = page.getByRole('button', {name: 'Copy reference JSON URL', exact: true});
        if (javaScriptEnabled) {
          await copy.click();
          assert.equal(await page.evaluate(() => window.referenceCopied), '/api/references');
          assert.equal(await copy.locator('svg').getAttribute('aria-hidden'), 'true');
        } else {
          assert.equal(await copy.count(), 0);
          assert.equal(await page.locator('a[href="/api/references"]').count(), 1);
        }
        await page.getByLabel('Find a reference', {exact: true}).fill('alpha');
        await page.getByRole('button', {name: 'Search', exact: true}).click();
        await page.waitForURL(url => url.searchParams.get('q') === 'alpha');
        assert.equal(await page.locator('.reference-card').count(), 1);
        await inspect();
        const detail = await page.getByText('Reference details →', {exact: true}).getAttribute('href');
        assert.equal((await page.goto(origin + detail)).status(), 200);
        await page.getByText('Reference provenance', {exact: true}).click();
        await inspect();
        await page.goto(origin + '/references?q=no-synthetic-match');
        assert.match(await page.locator('body').textContent(), /No references match/);
        await inspect();
        await page.goto(origin + '/references?limit=1');
        const first = await page.locator('.reference-card').getAttribute('id');
        await page.getByText('Next references →', {exact: true}).click();
        await page.waitForURL(url => url.searchParams.has('cursor'));
        assert.notEqual(await page.locator('.reference-card').getAttribute('id'), first);
        await inspect();
        assert.deepEqual(forbidden, [], 'reference view attempted external/native activity');
        assert.deepEqual(errors, []);
      } finally { await context.close(); }
    }
    assert.deepEqual(await (await fetch(origin + '/api/stats')).json(), before);
    console.log('PASS: reference SSR/search/detail/paging, copy,320px JS/no-JS, no external requests/native activity.');
  } finally { await browser.close(); }
})().catch(error => { console.error(error); process.exitCode = 1; });
