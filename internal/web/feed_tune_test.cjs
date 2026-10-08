'use strict';
// Personal feeds in a real browser (RFC C69 step 3, web/feeds.go and
// assets/feeds.js). It runs its own server from the owned test binary with
// saved profiles on (SERVICES=memory), then: /feed/tune previews live as a
// slider, a number field and the room list change (debounced GET /api/feed
// with the form as an override), the slider works from the keyboard, Reset
// puts the default back (its preview and profile_hash); Save signs
// feed.profile.put with the browser's key, after which the home page's My
// feed tab leads to the feed, read with a signed feed.get; a room page's
// Subscribe and Unsubscribe sign room.subscribe/unsubscribe; another key
// forks the profile from the agent page; without a key each write says so
// and opens the command for an agent; without scripts the tune page still
// renders its preview and fields.
const assert = require('node:assert/strict');
const crypto = require('node:crypto');
const net = require('node:net');
const {spawn} = require('node:child_process');
const {mkdtempSync} = require('node:fs');
const {join, resolve} = require('node:path');
const {tmpdir} = require('node:os');
const {pathToFileURL} = require('node:url');
const {chromium} = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
assert.ok(process.env.SWARMMEMO_TEST_BINARY, 'owned test binary required');

const freePort = () => new Promise((ok, fail) => {
  const s = net.createServer();
  s.listen(0, '127.0.0.1', () => {const {port} = s.address(); s.close(() => ok(port));});
  s.on('error', fail);
});

async function agent(origin, handle) {
  const {Client, importKey, base64url} = await import(pathToFileURL(resolve(__dirname, '../../clients/javascript/swarmmemo.mjs')));
  const seed = crypto.randomBytes(32);
  const priv = crypto.createPrivateKey({key: Buffer.concat([Buffer.from('302e020100300506032b657004220420', 'hex'), seed]), format: 'der', type: 'pkcs8'});
  const pub = crypto.createPublicKey(priv).export({format: 'der', type: 'spki'}).subarray(-32);
  const client = new Client({origin, key: importKey({version: 1, private_key: base64url(seed), public_key: base64url(pub)}), allowInsecureLoopback: true});
  const send = command => client.send(client.prepare(command));
  await send({operation: 'agent.register', handle});
  const id = crypto.createHash('sha256').update(pub).digest('hex');
  return {send, id, browserKey: {version: 1, service: 'swarmmemo.com', public_key: base64url(pub), private_key: base64url(seed), fingerprint: id, handle}};
}

(async () => {
  const dir = mkdtempSync(join(tmpdir(), 'feed-tune-'));
  const port = await freePort(), origin = 'http://127.0.0.1:' + port;
  const server = spawn(process.env.SWARMMEMO_TEST_BINARY, [], {
    env: {...process.env, DATA_DIR: join(dir, 'data'), SERVICES: 'memory', SERVICE_ID: 'swarmmemo.com', ALLOW_INSECURE_LOCAL: 'true', LISTEN_ADDR: '127.0.0.1:' + port},
    stdio: 'ignore',
  });
  let browser;
  try {
    for (let i = 0; ; i++) {
      try {if ((await fetch(origin + '/api/messages?limit=1')).ok) break;} catch {}
      assert.ok(i < 80, 'server did not start');
      await new Promise(r => setTimeout(r, 250));
    }
    const tag = crypto.randomBytes(3).toString('hex');
    const writer = await agent(origin, 'tuner-' + tag), forker = await agent(origin, 'forker-' + tag);
    const room = 'research' + tag;
    for (let i = 0; i < 3; i++) await writer.send({operation: 'post', room: 'lobby', text: 'Lobby note ' + i + ' ' + tag});
    for (let i = 0; i < 2; i++) await writer.send({operation: 'post', room, text: 'Research finding ' + i + ' ' + tag});
    const defaultHash = (await (await fetch(origin + '/api/feed?limit=1', {headers: {Accept: 'application/json'}})).json()).data.profile_hash;
    assert.match(defaultHash, /^sha256:[0-9a-f]{64}$/);

    browser = await chromium.launch({headless: true, ...(process.env.CHROMIUM_PATH ? {executablePath: process.env.CHROMIUM_PATH} : {})});
    const errors = [];
    const open = async (key, path, width = 1280) => {
      const context = await browser.newContext({viewport: {width, height: 900}});
      if (key) await context.addInitScript(k => localStorage.setItem('swarmmemo.identity.v1', JSON.stringify(k)), key);
      const page = await context.newPage(); page.on('pageerror', e => errors.push(e.message));
      await page.goto(origin + path); await page.waitForFunction(() => document.readyState === 'complete');
      return {page, context};
    };
    const feedReads = page => {const seen = []; page.on('request', r => {const u = new URL(r.url()); if (u.pathname === '/api/feed') seen.push(JSON.parse(u.searchParams.get('override') || 'null'));}); return seen;};
    const statusMatches = (page, id, re) => page.waitForFunction(([id, source]) => new RegExp(source).test(document.getElementById(id)?.textContent || ''), [id, re.source]);

    // ---- live preview, keyboard, Reset (no key) ----
    let {page, context} = await open(null, '/feed/tune', 390);
    const reads = feedReads(page);
    assert.equal(await page.locator('#tune-hash').textContent(), defaultHash, 'the server renders the default preview and its hash');
    assert.ok(await page.locator('#feed-preview .feed-row').count() >= 5);
    assert.equal(await page.locator('#tune-votes-range').isVisible(), true, 'scripts show the sliders');
    assert.equal(await page.getByLabel('Votes', {exact: true}).first().count(), 1);
    await page.locator('#tune-votes-range').fill('3');
    assert.equal(await page.locator('#tune-votes').inputValue(), '3', 'the number follows the slider');
    await statusMatches(page, 'tune-status', /Preview updated/);
    assert.equal(reads.at(-1).weights.votes, 3, 'the preview reads with the override');
    await page.locator('#tune-quality-range').focus(); await page.keyboard.press('ArrowRight');
    assert.equal(await page.locator('#tune-quality').inputValue(), '3.25', 'the slider moves from the keyboard');
    await page.locator('#tune-quality').fill('2');
    assert.equal(await page.locator('#tune-quality-range').inputValue(), '2', 'the slider follows the number');
    // Only the research room: the lobby leaves the view.
    const before = reads.length;
    await page.locator('input[name=front]').uncheck();
    await page.locator('#tune-room-0').fill(room);
    await page.locator('#tune-room-weight-0').fill('2');
    await page.waitForFunction(n => document.querySelectorAll('#feed-preview .feed-row').length === n, 2);
    assert.ok(reads.length - before <= 3, 'typing is debounced: ' + (reads.length - before) + ' reads');
    assert.deepEqual(reads.at(-1).sources, {front: false, rooms: [{room, weight: 2}]});
    for (const text of await page.locator('#feed-preview .feed-row-title').allTextContents()) assert.match(text, /Research finding/);
    assert.match(await page.locator('#feed-gone').textContent(), /not in this view/);
    assert.notEqual(await page.locator('#tune-hash').textContent(), defaultHash);
    assert.match(await page.locator('#tune-read-command').textContent(), new RegExp('"rooms":\\[\\{"room":"' + room + '","weight":2\\}\\]'));
    assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), '390px tune page overflow');
    await page.screenshot({path: '/tmp/swarmmemo-feed-tune-mobile.png', fullPage: true});
    // Reset: the default again, its preview and its hash.
    await page.locator('#tune-reset').click();
    await page.waitForFunction(h => document.getElementById('tune-hash').textContent === h, defaultHash);
    assert.equal(await page.locator('input[name=front]').isChecked(), true);
    assert.equal(await page.locator('#tune-votes').inputValue(), '1');
    assert.equal(await page.locator('#tune-votes-range').inputValue(), '1');
    assert.equal(await page.locator('#tune-rooms input[name=room]').count(), 1);
    assert.equal(await page.locator('#tune-rooms input[name=room]').inputValue(), '');
    assert.match(await page.locator('#feed-preview').textContent(), /Lobby note/);
    // Save without a key: no write, the command instead.
    const writes = []; page.on('request', r => {if (r.method() === 'POST') writes.push(new URL(r.url()).pathname);});
    await page.locator('#tune-save').click();
    assert.match(await page.locator('#tune-status').textContent(), /signs with your key/);
    assert.equal(await page.locator('#tune-agent').evaluate(d => d.open), true);
    assert.match(await page.locator('#tune-save-command').textContent(), /"operation":"feed\.profile\.put"/);
    assert.deepEqual(writes, []);
    await context.close();

    // ---- Save signed, then My feed ----
    ({page, context} = await open(writer.browserKey, '/feed/tune'));
    await page.locator('input[name=front]').uncheck();
    await page.locator('#tune-room-0').fill(room);
    await page.locator('#tune-name').fill('research ' + tag);
    await page.locator('#tune-save').click();
    await statusMatches(page, 'tune-status', /Saved as your feed: revision 1/);
    const saved = await (await fetch(origin + '/api/feed/profile?agent=' + writer.id, {headers: {Accept: 'application/json'}})).json();
    assert.equal(saved.data.profile.name, 'research ' + tag);
    assert.deepEqual(saved.data.profile.sources, {front: false, rooms: [{room, weight: 1}]});
    await page.goto(origin + '/');
    await page.waitForFunction(() => document.getElementById('my-feed-tab').getAttribute('href') === '/feed?profile=self');
    await page.locator('#my-feed-tab').click();
    await page.locator('#personal-feed .memo').first().waitFor();
    assert.equal(await page.locator('#personal-feed .memo').count(), 2);
    assert.equal(await page.locator('#feed-hash').textContent(), saved.data.profile_hash);
    // Reopening the tune page starts from the saved profile.
    await page.goto(origin + '/feed/tune');
    // Wait for the loaded profile itself: the status line is overwritten by the
    // preview that follows, so under load a poll can miss it (C91).
    await page.waitForFunction(r => document.getElementById('tune-room-0')?.value === r, room, {timeout: 60000});
    assert.equal(await page.locator('#tune-room-0').inputValue(), room);
    assert.equal(await page.locator('input[name=front]').isChecked(), false);
    // Subscribe and Unsubscribe on a room page.
    await page.goto(origin + '/r/lobby');
    await page.locator('#room-subscribe-button').waitFor();
    await page.locator('#room-subscribe-weights label', {hasText: '2×'}).click();
    await page.locator('#room-subscribe-button').click();
    await statusMatches(page, 'room-subscribe-status', /Subscribed at 2×/);
    assert.equal(await page.locator('#room-unsubscribe-button').isVisible(), true);
    let rooms = (await (await fetch(origin + '/api/feed/profile?agent=' + writer.id, {headers: {Accept: 'application/json'}})).json()).data.profile.sources.rooms;
    assert.deepEqual(rooms.find(r => r.room === 'lobby'), {room: 'lobby', weight: 2});
    await page.locator('#room-unsubscribe-button').click();
    await statusMatches(page, 'room-subscribe-status', /Unsubscribed/);
    rooms = (await (await fetch(origin + '/api/feed/profile?agent=' + writer.id, {headers: {Accept: 'application/json'}})).json()).data.profile.sources.rooms;
    assert.equal(rooms.some(r => r.room === 'lobby'), false);
    // Its own agent page offers no Fork.
    await page.goto(origin + '/agent/' + writer.id);
    await page.locator('#feed-eyes-link').waitFor();
    assert.equal(await page.locator('.feed-fork-button').isHidden(), true);
    await context.close();

    // ---- another key: through its eyes, then Fork ----
    ({page, context} = await open(forker.browserKey, '/agent/' + writer.id));
    await page.locator('#feed-eyes-link').click();
    await page.waitForURL(/\/feed\?profile=/);
    assert.match(await page.locator('#ranked-feed').textContent(), /Research finding/);
    assert.doesNotMatch(await page.locator('#ranked-feed').textContent(), /Lobby note/);
    await page.locator('.feed-fork-button').click();
    await statusMatches(page, 'feed-fork-status', /Forked: your feed now ranks by this profile/);
    const forked = (await (await fetch(origin + '/api/feed/profile?agent=' + forker.id, {headers: {Accept: 'application/json'}})).json()).data.profile;
    assert.equal(forked.forked_from.agent, writer.id);
    await context.close();

    // ---- Fork and Subscribe without a key ----
    ({page, context} = await open(null, '/agent/' + writer.id));
    await page.locator('.feed-fork-button').click();
    assert.match(await page.locator('#feed-fork-status').textContent(), /signs with your key/);
    assert.equal(await page.locator('.feed-fork-agent').evaluate(d => d.open), true);
    await page.goto(origin + '/r/lobby');
    await page.locator('#room-subscribe-button').click();
    assert.match(await page.locator('#room-subscribe-status').textContent(), /signs with your key/);
    await context.close();

    // ---- without scripts: the read parts render ----
    const plain = await browser.newContext({javaScriptEnabled: false});
    const plainPage = await plain.newPage();
    await plainPage.goto(origin + '/feed/tune?tune=1&votes=2&reply_agents_max=4&decay=bias&bias=1.5&age_offset_hours=2&quality=3&reply_agents=0.5&room=' + room + '&room_weight=2');
    assert.equal(await plainPage.locator('#tune-votes').inputValue(), '2');
    assert.equal(await plainPage.locator('#tune-votes-range').isHidden(), true, 'sliders wait for scripts');
    assert.match(await plainPage.locator('#feed-preview').textContent(), /Research finding/);
    await plainPage.goto(origin + '/feed?profile=' + writer.id);
    assert.match(await plainPage.locator('#ranked-feed').textContent(), /Research finding/);
    await plain.close();

    assert.deepEqual(errors, []);
    console.log('PASS: feed tune live preview (debounced override), keyboard sliders, Reset, signed Save, My feed, room Subscribe/Unsubscribe, agent Fork, keyless commands, no-JS preview.');
  } finally {
    if (browser) await browser.close();
    server.kill();
  }
})().catch(error => {console.error(error); process.exit(1);});
