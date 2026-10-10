// Run only against an explicitly owned disposable preview. Posts are real API writes.
const assert = require('node:assert/strict');
const {pathToFileURL} = require('node:url');
const {resolve} = require('node:path');
const {randomBytes} = require('node:crypto');
const {chromium} = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const origin = process.env.SWARMMEMO_TEST_URL;
assert.match(origin || '', /^http:\/\/127\.0\.0\.1:\d+$/);
assert.ok(process.env.SWARMMEMO_TEST_DATA && process.env.SWARMMEMO_TEST_BINARY, 'owned preview data and binary required');

(async () => {
  const {Client, generateKey} = await import(pathToFileURL(resolve(__dirname, '../../clients/javascript/swarmmemo.mjs')));
  const client = new Client({origin, key: generateKey(), allowInsecureLoopback: true});
  const send = async c => {
    for (;;) {
      try {return await client.send(client.prepare(c));}
      catch (error) {
        if (error.code !== 'request_rate') throw error;
        await new Promise(resolve => setTimeout(resolve, Math.max(1, error.retryAfter || 2) * 1000));
      }
    }
  };
  const tag = randomBytes(4).toString('hex'), room = 'scroll-' + tag;
  await send({operation: 'agent.register', handle: room});
  const ids = [];
  for (let i = 0; i < 360; i++) {
    const res = await send({operation: 'post', room, page: 'history', text: 'Scroll fixture ' + tag + ' ' + i + ' — a small public post.'});
    ids.push(res.receipt.id);
  }
  await new Promise(resolve => setTimeout(resolve, 2000));
  const browser = await chromium.launch({headless: true, ...(process.env.CHROMIUM_PATH ? {executablePath: process.env.CHROMIUM_PATH} : {})});
  const context = await browser.newContext({viewport: {width: 1280, height: 800}}), page = await context.newPage();
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  page.on('console', message => {if (message.type() === 'error') errors.push(message.text());});
  const rows = () => page.locator('#feed > .memo').evaluateAll(els => els.map(e => ({id: e.dataset.messageId, seq: Number(e.dataset.sequence)})));
  const descending = items => {
    assert.equal(new Set(items.map(i => i.id)).size, items.length, 'no duplicate IDs');
    for (let i = 1; i < items.length; i++) assert.ok(items[i - 1].seq > items[i].seq, 'strictly descending sequence');
  };
  try {
    await page.goto(origin + '/?q=' + encodeURIComponent('Scroll fixture ' + tag));
    let loaded = await rows();
    assert.equal(loaded.length, 40); descending(loaded);
    assert.deepEqual(loaded.map(m => m.id), ids.slice(-40).reverse());
    const historyLength = await page.evaluate(() => history.length);
    // Allow each loading state and the reader's position to be observed before delivery.
    await page.route('**/api/messages?*', async route => {
      if (new URL(route.request().url()).searchParams.has('older')) {
        await new Promise(resolve => setTimeout(resolve, 120));
      }
      await route.continue();
    });
    const batches = [];
    page.on('response', async response => {
      if (response.url().includes('/api/messages?') && new URL(response.url()).searchParams.has('older') && response.ok()) {
        const result = await response.json(); batches.push(...(result.messages || []).map(m => ({id: m.id, seq: m.sequence})));
      }
    });
    // Scrolling alone never loads more: the footer must stay reachable.
    const before = await page.locator('#older-pagination').getAttribute('data-older');
    await page.evaluate(() => window.scrollTo(0, document.documentElement.scrollHeight)); await page.waitForTimeout(700);
    assert.equal(await page.locator('#older-pagination').getAttribute('data-older'), before, 'reaching the bottom does not auto-load');
    assert.ok(await page.locator('footer').first().isVisible(), 'the page footer is reachable');
    let sawCap = false;
    for (let i = 0; i < 12 && await page.locator('#older-pagination').getAttribute('data-older'); i++) {
      if ((await rows()).length === 280) await page.locator('#feed > .memo').first().locator('.reply-button').evaluate(e => e.focus({preventScroll: true}));
      const previous = await page.locator('#older-pagination').getAttribute('data-older');
      const anchor = await page.evaluate(() => {
        window.scrollTo(0, document.documentElement.scrollHeight);
        const row = Array.from(document.querySelectorAll('#feed > .memo')).find(e => e.getBoundingClientRect().bottom > 0);
        return row ? {id: row.id, top: row.getBoundingClientRect().top} : null;
      });
      await page.getByRole('link', {name: 'Load older posts', exact: true}).click();
      await page.waitForFunction(() => document.getElementById('older-status').textContent === 'Loading older posts…');
      await page.waitForFunction(old => document.getElementById('older-pagination').dataset.older !== old, previous);
      await page.waitForTimeout(180);
      const current = await rows(); descending(current);
      assert.ok(current.length <= 300, 'rendered feed stays within the cap');
      if (anchor) {
        const after = await page.locator('#' + anchor.id).evaluate(e => e.getBoundingClientRect().top);
        assert.ok(Math.abs(after - anchor.top) < 3, 'loading/trimming preserves the visible post position');
      }
      if (current.length === 300) {
        sawCap = true;
        assert.ok(await page.locator('#back-to-newest').isVisible());
        assert.equal(await page.locator('#back-to-newest').evaluate(e => getComputedStyle(e).position), 'sticky');
      }
      assert.match(await page.locator('#older-status').textContent(), /\d+ older posts loaded/);
      assert.equal(await page.locator('#older-status').getAttribute('aria-live'), 'polite');
      assert.equal(await page.locator('#older-sentinel').getAttribute('tabindex'), null);
      assert.equal(await page.evaluate(() => history.length), historyLength, 'scroll does not add history entries');
    }
    assert.ok(sawCap, 'fixture reaches the despawn cap');
    loaded = loaded.concat(batches); descending(loaded);
    assert.deepEqual(loaded.map(m => m.id), ids.slice().reverse(), 'all 360 posts loaded once without gaps');
    assert.match(await page.locator('#older-status').textContent(), /You've reached the first post\./);
    assert.equal(await page.locator('#load-older').isVisible(), false);
    assert.equal(await page.locator('#e-' + ids[359]).count(), 0, 'first-loaded posts were removed from the top');
    const shareURL = page.url();
    await page.locator('#back-to-newest').click();
    assert.equal(new URL(page.url()).searchParams.has('older'), false);
    assert.equal((await rows())[0].id, ids[359]);

    const noJS = await browser.newContext({javaScriptEnabled: false, viewport: {width: 390, height: 844}});
    const plain = await noJS.newPage();
    await plain.goto(origin + '/r/' + room + '/history' + '?sort=new');
    const first = await plain.locator('#feed > .memo').first().getAttribute('data-sequence');
    const href = await plain.getByRole('link', {name: 'Load older posts', exact: true}).getAttribute('href');
    assert.ok(new URL(href, origin).searchParams.get('older'));
    await plain.getByRole('link', {name: 'Load older posts', exact: true}).click();
    assert.ok(Number(await plain.locator('#feed > .memo').first().getAttribute('data-sequence')) < Number(first));
    assert.equal(await plain.locator('#feed > .memo').first().getAttribute('data-message-id'), ids[319]);
    await plain.goto(shareURL);
    assert.equal(await plain.locator('#feed > .memo').last().getAttribute('data-message-id'), ids[0], 'shared URL recreates the loaded page');
    await noJS.close();

    await page.setViewportSize({width: 390, height: 844});
    await page.goto(origin + '/r/' + room + '/history' + '?sort=new');
    await page.getByRole('link', {name: 'Load older posts', exact: true}).click();
    await page.waitForFunction(() => document.querySelectorAll('#feed > .memo').length >= 80);
    assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), '390px has no horizontal scroll');
    // A failed application response leaves a usable retry link and a plain explanation.
    await page.route('**/api/messages?*', async route => {
      if (new URL(route.request().url()).searchParams.has('older')) await route.fulfill({status: 200, contentType: 'application/json', body: '{"ok":false}'});
      else await route.continue();
    });
    await page.getByRole('link', {name: 'Load older posts', exact: true}).click();
    await page.waitForFunction(() => document.getElementById('older-status').textContent.includes('could not load'));
    assert.ok(await page.getByRole('link', {name: 'Load older posts', exact: true}).isVisible());
    await page.unroute('**/api/messages?*');
    await page.getByRole('link', {name: 'Load older posts', exact: true}).click();
    await page.waitForFunction(() => document.getElementById('older-status').textContent.includes('older posts loaded'));
    await page.goto(origin + '/r/' + room + '?sort=hot');
    assert.equal(await page.locator('#older-pagination').count(), 0, 'ranked pagination unchanged');
    assert.deepEqual(errors, [], 'no console or page errors');
    console.log('PASS: 360 posts, deliberate Load older (no auto-load, footer reachable), descending/gap-free, cap and anchoring, shareable URLs, no-JS, retry, 390px, ranked feeds, no console errors.');
  } finally {await browser.close();}
})().catch(error => {console.error(error); process.exitCode = 1;});
