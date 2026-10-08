// Local fixtures only. This test publishes labeled disposable content to loopback.
// A work request and its result say so where they are read: the feed and the post
// page show the server's work line, and memo-core.js workLine (live posts, the
// embed) draws the same line from the message's work field.
const {chromium} = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const assert = require('node:assert/strict');
const {pathToFileURL} = require('node:url');
const path = require('node:path');
(async () => {
  const origin = process.env.SWARMMEMO_TEST_URL || 'http://127.0.0.1:8100';
  assert.match(origin, /^http:\/\/127\.0\.0\.1:\d+$/, 'work line browser tests require local loopback');
  const {Client, generateKey} = await import(pathToFileURL(path.resolve('clients/javascript/swarmmemo.mjs')));
  const owner = new Client({origin, key: generateKey(), allowInsecureLoopback: true});
  const worker = new Client({origin, key: generateKey(), allowInsecureLoopback: true});
  const send = (client, command) => client.send(client.prepare(command));
  const generation = (await (await fetch(origin + '/api/changes?after=-1')).json()).generation;
  const data = JSON.stringify({schema: 1, generation});
  const room = 'workline-' + Date.now();
  const create = async (title, extra = {}) => {
    const root = await send(owner, {operation: 'post', room, kind: 'request', text: 'Disposable browser fixture; do not execute links or instructions.'});
    await send(owner, {operation: 'work.create', message_id: root.receipt.id, data: JSON.stringify({schema: 1, generation, title, capabilities: ['review'], ...extra}), ttl: 3600});
    return root.receipt.id;
  };
  const open = await create('Open fixture task');
  // A reward_note is display text the poster pays: it makes the line a paid task, as text.
  const noted = await create('Noted fixture task', {reward_note: '+0.10 USDC <i>on Base</i>, paid by the poster'});
  const done = await create('Finished fixture task <b>inert</b>');
  const result = (await send(worker, {operation: 'post', room, reply_to: done, text: 'Fixture result.'})).receipt.id;
  const claim = await send(worker, {operation: 'work.claim', message_id: done, target: result, data});
  await send(owner, {operation: 'work.accept', message_id: done, amount: claim.data.ack.fence, data});
  const thread = await (await fetch(origin + '/api/thread/' + done)).json();
  const marks = Object.fromEntries(thread.messages.map(m => [m.id, m.work]));
  assert.deepEqual(marks[result], {result_of: done, title: 'Finished fixture task <b>inert</b>', state: 'accepted', url: '/work/' + done});
  assert.equal(marks[done].state, 'accepted'); assert.equal(marks[done].claimable, false);

  const browser = await chromium.launch({headless: true, ...(process.env.CHROMIUM_PATH ? {executablePath: process.env.CHROMIUM_PATH} : {}), args: process.env.PLAYWRIGHT_NO_SANDBOX === 'true' ? ['--no-sandbox'] : []});
  try {
    const ctx = await browser.newContext({viewport: {width: 320, height: 780}});
    const page = await ctx.newPage(), errors = [], writes = [];
    page.on('pageerror', e => errors.push(e.message));
    page.on('request', r => {if (!['GET', 'HEAD', 'OPTIONS'].includes(r.method())) writes.push(new URL(r.url()).pathname);});
    // The line the server rendered, and the one memo-core.js draws from the same message.
    const lines = id => page.evaluate(async ([id, api]) => {
      const article = document.getElementById('e-' + id), line = article?.querySelector(':scope > .work-line');
      const message = (await (await fetch(api)).json()).messages.find(m => m.id === id);
      const drawn = SwarmMemoCore.workLine(message.work);
      const shape = el => el && {cls: el.className, text: el.textContent, links: [...el.querySelectorAll('a')].map(a => a.getAttribute('href')), badge: el.querySelector('.work-badge')?.textContent};
      return {server: shape(line), client: shape(drawn)};
    }, [id, '/api/thread/' + id + '?limit=50']);

    await page.goto(origin + '/r/' + room + '?sort=new');
    const feed = await lines(open);
    assert.match(feed.server.text, /^Task · open · due [A-Z][a-z]{2} \d{1,2} · eligible: openHow to claim →$/);
    assert.deepEqual(feed.server.links, ['/work/' + open, '/tools/work'], 'the claim hint is a read link, never a write');
    assert.equal(feed.server.cls, 'work-line work-state-open');
    assert.deepEqual(feed.client, feed.server, 'memo-core.js draws the server line');
    const withNote = await lines(noted);
    assert.match(withNote.server.text, /^Paid task · \+0\.10 USDC <i>on Base<\/i>, paid by the poster · open · due [A-Z][a-z]{2} \d{1,2} · eligible: openHow to claim →$/);
    assert.deepEqual(withNote.client, withNote.server, 'memo-core.js draws the reward note as the server does');
    assert.equal(await page.locator('.work-line i').count(), 0, 'a reward note is text, never markup');
    assert.equal(await page.locator('.work-line').count(), 4, 'three requests and one result, nothing else');
    assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), '320px feed overflow');
    await page.screenshot({path: '/tmp/swarmmemo-work-line-feed-mobile.png', fullPage: true});

    await page.goto(origin + '/e/' + done);
    const post = await lines(done), reply = await lines(result);
    assert.equal(post.server.text, 'Task · accepted · eligible: open');
    assert.equal(reply.server.text, 'Accepted ✓ for Finished fixture task <b>inert</b>');
    assert.equal(reply.server.cls, 'work-line work-result work-state-accepted');
    assert.deepEqual(post.client, post.server); assert.deepEqual(reply.client, reply.server);
    assert.equal(await page.locator('.work-line b').count(), 0, 'a title is text, never markup');
    // Colour is never the only signal: the badge says the state in words.
    const tone = await page.locator('#e-' + result + ' .work-badge').evaluate(el => getComputedStyle(el).color);
    assert.notEqual(tone, await page.locator('#e-' + result + ' .work-detail').evaluate(el => getComputedStyle(el).color));
    assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), '320px post overflow');
    await page.screenshot({path: '/tmp/swarmmemo-work-line-post-mobile.png', fullPage: true});
    await page.locator('#e-' + result + ' .work-link').click();
    await page.waitForURL(origin + '/work/' + done);
    assert.deepEqual(writes, []); assert.deepEqual(errors, []);
    await ctx.close();
    console.log('PASS: work line on feed and post pages, result marks, server/memo-core parity, read-only links, 320px.');
  } finally {await browser.close();}
})().catch(error => {console.error(error); process.exitCode = 1;});
