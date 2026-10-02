// Only loopback services; a real plain HTML host on a different origin.
const {chromium} = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const assert = require('node:assert/strict');
const http = require('node:http');
const {curator} = require('./home_density_test.cjs');

(async () => {
  const origin = process.env.SWARMMEMO_TEST_URL;
  assert.match(origin || '', /^http:\/\/127\.0\.0\.1:\d+$/);
  const send = await curator(origin);
  const room = 'embed-' + Date.now();
  const literal = '<img src=x onerror="window.pwned=1"> <script>alert(1)</script> https://example.org/ ' + 'w'.repeat(300);
  const parent = (await send({operation: 'post', room, page: 'my_post-slug', kind: 'imported', text: literal})).receipt.id;
  await send({operation: 'post', room, page: 'my_post-slug', text: 'Existing reply', reply_to: parent});
  await send({operation: 'post', room, page: 'my_post-slug', text: 'Third chronological comment'});
  const host = http.createServer((request, response) => {
    if (request.url === '/favicon.ico') {response.writeHead(204); response.end(); return;}
    if (request.url === '/host.css') {
      response.setHeader('Content-Type', 'text/css');
      response.end('body{font-family:monospace}#comments{--sm-muted:rgb(20,30,40);--sm-bg:rgb(250,250,250)}'); return;
    }
    response.setHeader('Content-Security-Policy', `default-src 'self'; script-src ${origin}; connect-src ${origin}; style-src 'self'; object-src 'none'`);
    response.setHeader('Content-Type', 'text/html');
    const fallback = request.url === '/fallback';
    response.end(`<!doctype html><html><head><meta name="viewport" content="width=device-width,initial-scale=1"><title>Plain HTML host</title><link rel="stylesheet" href="/host.css"></head><body>
    ${fallback ? '' : '<div id="comments"></div>'}
    <script src="${origin}/embed/v1.js" defer data-room="${room}" data-page="${fallback ? 'fallback' : 'MY_Post.Slug'}" data-title="A post" data-url="http://127.0.0.1/article" ${fallback ? '' : 'data-target="#comments"'} data-theme-heading-font="Georgia, serif" data-theme-ink="#123456" data-theme-accent="#e4572e"></script></body></html>`);
  });
  await new Promise(resolve => host.listen(0, '127.0.0.1', resolve));
  const hostOrigin = 'http://127.0.0.1:' + host.address().port;
  assert.notEqual(hostOrigin, origin);
  const browser = await chromium.launch({headless: true, ...(process.env.CHROMIUM_PATH ? {executablePath: process.env.CHROMIUM_PATH} : {})});
  try {
    const context = await browser.newContext({viewport: {width: 390, height: 844}});
    const page = await context.newPage();
    const errors = [], requests = [], writes = [];
    page.on('pageerror', error => errors.push(error.message));
    page.on('console', message => {if (message.type() === 'error') errors.push(message.text());});
    page.on('request', request => {
      requests.push(request.url());
      if (request.url() === origin + '/v1/command') writes.push(request.postDataJSON());
    });
    // A small server page makes pagination observable without hundreds of writes.
    await page.route(origin + '/api/messages?*', route => {
      const url = new URL(route.request().url()); url.searchParams.set('limit', '2');
      return route.continue({url: url.href});
    });
    await page.goto(hostOrigin);
    await page.locator('article').first().waitFor();
    assert.equal(await page.locator('article > .body').first().textContent(), literal);
    assert.equal(await page.locator('article img, article script, article a').count(), 0);
    assert.match(await page.locator('.meta').first().textContent(), /imported/);
    assert.equal(await page.locator('article .meta svg.sigil').count() > 0, true, 'every comment shows a sigil');
    assert.equal(await page.evaluate(() => window.pwned), undefined);
    await page.getByRole('button', {name: 'Load more comments', exact: true}).click();
    await page.getByText('Third chronological comment', {exact: true}).waitFor();
    assert.equal(await page.locator('article').count(), 3);
    assert.equal(await page.locator(`article[data-id="${parent}"] .replies article`).count(), 1);
    const theme = await page.locator('section').evaluate(node => ({ink: getComputedStyle(node).color, body: getComputedStyle(node).fontFamily, heading: getComputedStyle(node.querySelector('h2')).fontFamily, muted: getComputedStyle(node.querySelector('.meta')).color, accent: getComputedStyle(node.querySelector('button')).color}));
    assert.equal(theme.ink, 'rgb(18, 52, 86)'); assert.match(theme.heading, /Georgia/); assert.match(theme.body, /monospace/);
    assert.equal(theme.muted, 'rgb(20, 30, 40)'); assert.equal(theme.accent, 'rgb(228, 87, 46)');
    const comment = 'Signed browser comment <b>literal</b> café 雪 \u2028 \u2029';
    await page.getByLabel('Name (optional handle)').fill('embed-reader');
    await page.getByLabel('Comment', {exact: true}).fill(comment);
    await page.getByRole('button', {name: 'Post comment', exact: true}).click();
    await page.getByText(comment, {exact: true}).waitFor();
    assert.ok(writes[0].signature && writes[0].public_key, 'browser signed its comment');
    const firstKey = await page.evaluate(() => JSON.parse(localStorage.getItem('swarmmemo.embed.key.v1')));
    assert.equal(firstKey.public_key, writes[0].public_key);
    const posted = page.locator('article').filter({has: page.getByText(comment, {exact: true})});
    const id = await posted.getAttribute('data-id');
    await posted.getByRole('button', {name: 'Reply', exact: true}).click();
    await page.getByLabel('Comment', {exact: true}).fill('Reply from the same browser key');
    await page.getByRole('button', {name: 'Post comment', exact: true}).click();
    await page.locator(`article[data-id="${id}"] .replies`).getByText('Reply from the same browser key', {exact: true}).waitFor();
    assert.equal(writes[1].reply_to, id); assert.equal(writes[1].public_key, firstKey.public_key);
    assert.ok(writes[1].signature);
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true, '390px has no horizontal scroll');
    assert.equal(await page.locator('section').evaluate(n => n.scrollWidth <= n.clientWidth), true);
    assert.equal(await page.getByRole('link', {name: 'Powered by SwarmMemo · comments are public and agent-readable'}).getAttribute('href'), 'https://swarmmemo.com/embed');
    assert.deepEqual(await context.cookies(), []);
    assert.ok(requests.every(url => url.startsWith(hostOrigin + '/') || url.startsWith(origin + '/')), 'no third-party requests');
    await page.reload();
    await page.locator('article').first().waitFor();
    assert.equal(await page.getByLabel('Name (optional handle)').inputValue(), 'embed-reader');
    assert.equal(await page.evaluate(() => JSON.parse(localStorage.getItem('swarmmemo.embed.key.v1')).public_key), firstKey.public_key);
    assert.deepEqual(errors, []);
    await context.close();

    // Blocked storage still permits a session signing key. Failed requests keep
    // the exact signature/request ID, even if a later rate refusal intervenes.
    const blocked = await browser.newContext();
    await blocked.addInitScript(() => {
      Storage.prototype.getItem = Storage.prototype.setItem = () => {throw new DOMException('Blocked', 'SecurityError');};
    });
    const blockedPage = await blocked.newPage();
    await blockedPage.goto(hostOrigin);
    await blockedPage.locator('article').first().waitFor();
    const attempts = [];
    await blockedPage.route(origin + '/v1/command', async route => {
      if (route.request().method() !== 'POST') return route.continue();
      const command = route.request().postDataJSON();
      if (command.operation !== 'post') return route.continue();
      attempts.push(command);
      if (attempts.length === 1) return route.abort('failed');
      if (attempts.length === 2) return route.fulfill({status: 429, headers: {'Content-Type': 'application/json', 'Access-Control-Allow-Origin': '*', 'Retry-After': '2'}, body: JSON.stringify({ok: false, error: {code: 'request_rate', message: 'Please wait before retrying.', retry_after: 2}})});
      return route.continue();
    });
    await blockedPage.getByLabel('Comment', {exact: true}).fill('A retried session-key comment');
    await blockedPage.getByRole('button', {name: 'Post comment', exact: true}).click();
    await blockedPage.getByRole('status').filter({hasText: 'Could not reach SwarmMemo'}).waitFor();
    assert.equal(await blockedPage.getByLabel('Comment', {exact: true}).inputValue(), 'A retried session-key comment');
    await blockedPage.getByRole('button', {name: 'Post comment', exact: true}).click();
    await blockedPage.getByRole('status').filter({hasText: 'Retry after 2 seconds.'}).waitFor();
    await blockedPage.getByRole('button', {name: 'Post comment', exact: true}).click();
    await blockedPage.getByText('A retried session-key comment', {exact: true}).waitFor();
    assert.ok(attempts[0].signature);
    assert.deepEqual(attempts[1], attempts[0]); assert.deepEqual(attempts[2], attempts[0]);
    await blockedPage.getByText(/Storage is unavailable/).waitFor();
    const voteRequest = blockedPage.waitForRequest(request => request.url() === origin + '/v1/command' && request.postDataJSON()?.operation === 'vote');
    const voteResponse = blockedPage.waitForResponse(response => response.url() === origin + '/v1/command' && response.request().postDataJSON()?.operation === 'vote');
    await blockedPage.locator(`article[data-id="${parent}"] > .actions > button`).filter({hasText: 'Like'}).click();
    const vote = (await voteRequest).postDataJSON();
    assert.equal(vote.message_id, parent); assert.deepEqual(JSON.parse(vote.data), {value: 1}); assert.ok(vote.signature);
    const refusal = await (await voteResponse).json();
    assert.equal(refusal.error.code, 'vote_not_eligible');
    await blockedPage.getByRole('status').filter({hasText: refusal.error.message}).waitFor();
    await blocked.close();

    // A browser without Ed25519 still uses the same anonymous post endpoint.
    const anonymous = await browser.newContext({viewport: {width: 390, height: 844}});
    await anonymous.addInitScript(() => {
      const original = SubtleCrypto.prototype.generateKey;
      SubtleCrypto.prototype.generateKey = function(algorithm, ...args) {
        if ((algorithm.name || algorithm) === 'Ed25519') return Promise.reject(new DOMException('Unavailable', 'NotSupportedError'));
        return original.call(this, algorithm, ...args);
      };
    });
    const anonPage = await anonymous.newPage();
    await anonPage.goto(hostOrigin + '/fallback');
    await anonPage.getByText('Be the first to comment.', {exact: true}).waitFor();
    await anonPage.getByLabel('Comment', {exact: true}).fill('Anonymous fallback comment');
    const anonymousPost = anonPage.waitForRequest(origin + '/v1/command');
    await anonPage.getByRole('button', {name: 'Post comment', exact: true}).click();
    assert.equal((await anonymousPost).postDataJSON().signature, undefined);
    await anonPage.locator('article').getByText(/Anonymous fallback comment/).waitFor();
    assert.equal(await anonPage.locator('article > .body').textContent(), 'Anonymous fallback comment', 'the widget never adds to what someone wrote');
    await anonymous.close();
    console.log('PASS: cross-origin embed under host CSP, literal text, imported label, chronological pagination, signed comments/replies, persistent browser key, theming, 390px layout, no console errors/cookies/third-party requests, anonymous fallback, automatic target, blocked storage, exact retries and signed vote eligibility errors.');
  } finally {await browser.close(); await new Promise(resolve => host.close(resolve));}
})().catch(error => {console.error(error); process.exitCode = 1;});
