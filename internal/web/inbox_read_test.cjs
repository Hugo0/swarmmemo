// Local fixtures only. It runs its own owned test binary with INBOX_ENTRIES=read (the shared
// gate server keeps the inbox log off). C199: read state lives on the board, so two browsers
// with the same key agree: opening a reply in one clears it in the other at its next poll,
// and Mark all read clears both.
const {chromium} = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const assert = require('node:assert/strict');
const crypto = require('node:crypto');
const path = require('node:path');
const os = require('node:os');
const net = require('node:net');
const {spawn} = require('node:child_process');
const {mkdtempSync} = require('node:fs');
assert.ok(process.env.SWARMMEMO_TEST_BINARY, 'owned test binary required');
const fields = ['operation', 'room', 'page', 'text', 'kind', 'reply_to', 'to', 'request_id', 'public_key', 'timestamp', 'nonce', 'handle', 'visibility', 'members', 'target', 'amount', 'ttl', 'message_id', 'cursor', 'limit', 'query', 'before', 'reason', 'data', 'filename', 'media_type', 'attachments'];
const canonical = c => {const ordered = {}; for (const f of fields) if (c[f] !== undefined && c[f] !== '' && c[f] !== 0 && (!Array.isArray(c[f]) || c[f].length)) ordered[f] = c[f]; return Buffer.from(JSON.stringify({version: 1, service: 'swarmmemo.com', command: ordered}).replace(/\u2028/g, '\\u2028').replace(/\u2029/g, '\\u2029'));};
function key() {const pair = crypto.generateKeyPairSync('ed25519'); const raw = pair.publicKey.export({format: 'der', type: 'spki'}).subarray(-32); return {...pair, public_key: raw.toString('base64url'), private_key: pair.privateKey.export({format: 'der', type: 'pkcs8'}).subarray(-32).toString('base64url'), fingerprint: crypto.createHash('sha256').update(raw).digest('hex')};}
function sign(k, c) {const command = {...c, public_key: k.public_key, timestamp: Math.floor(Date.now() / 1000), nonce: crypto.randomUUID()}; command.signature = crypto.sign(null, canonical(command), k.privateKey).toString('base64url'); return command;}
const freePort = () => new Promise((ok, fail) => {const s = net.createServer(); s.listen(0, '127.0.0.1', () => {const {port} = s.address(); s.close(() => ok(port));}); s.on('error', fail);});

(async () => {
  const dir = mkdtempSync(path.join(os.tmpdir(), 'inbox-read-'));
  const port = await freePort(), origin = 'http://127.0.0.1:' + port;
  const server = spawn(process.env.SWARMMEMO_TEST_BINARY, [], {
    env: {...process.env, DATA_DIR: path.join(dir, 'data'), SERVICES: 'memory', SERVICE_ID: 'swarmmemo.com', ALLOW_INSECURE_LOCAL: 'true', LISTEN_ADDR: '127.0.0.1:' + port, INBOX_ENTRIES: 'read'},
    stdio: 'ignore',
  });
  process.on('exit', () => server.kill());
  for (let i = 0; ; i++) {
    try {if ((await fetch(origin + '/api/messages?limit=1')).ok) break;} catch {}
    assert.ok(i < 80, 'server did not start');
    await new Promise(r => setTimeout(r, 250));
  }
  const call = async c => {const response = await fetch(origin + '/v1/command', {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify(c)}); const result = await response.json(); assert.equal(response.status, 200, JSON.stringify(result)); return result;};
  const signed = (k, c) => call(sign(k, {...c, request_id: crypto.randomUUID()}));
  const unread = async k => (await call(sign(k, {operation: 'updates.get', target: k.fingerprint, data: JSON.stringify({schema: 1, counts: true})}))).data.unread.updates;

  const inbox = await (await fetch(origin + '/capabilities/inbox')).json();
  assert.equal(inbox?.enabled, true, 'the owned server reads the inbox log');
  assert.equal(inbox?.mark_read?.operation, 'updates.mark_read');
  const me = key(), sender = key();
  for (const k of [me, sender]) await signed(k, {operation: 'agent.register', handle: 'read-' + k.fingerprint.slice(0, 10)});
  const root = (await signed(me, {operation: 'post', room: 'lobby', text: 'Disposable root for the read-state test'})).receipt.id;
  const reply = (await signed(sender, {operation: 'post', room: 'lobby', reply_to: root, text: 'Disposable reply <img src=x onerror=alert(1)>'})).receipt.id;
  const dm = (await signed(sender, {operation: 'post', room: 'lobby', to: me.fingerprint, text: 'Disposable public DM'})).receipt.id;
  assert.equal((await unread(me)).total, 2);

  const browser = await chromium.launch({headless: true, ...(process.env.CHROMIUM_PATH ? {executablePath: process.env.CHROMIUM_PATH} : {}), args: process.env.PLAYWRIGHT_NO_SANDBOX === 'true' ? ['--no-sandbox'] : []});
  try {
    const stored = {version: 1, service: 'swarmmemo.com', public_key: me.public_key, private_key: me.private_key, fingerprint: me.fingerprint, handle: ''};
    const errors = [], marks = [];
    const device = async name => {
      const context = await browser.newContext({viewport: {width: 1280, height: 900}});
      await context.addInitScript(value => localStorage.setItem('swarmmemo.identity.v1', JSON.stringify(value)), stored);
      const page = await context.newPage();
      page.on('pageerror', e => errors.push(name + ': ' + e.message));
      page.on('dialog', d => {errors.push(name + ' dialog: ' + d.message()); d.dismiss();});
      page.on('request', r => {if (r.url().endsWith('/v1/command') && r.method() === 'POST') {const c = r.postDataJSON(); if (c.operation === 'updates.mark_read') marks.push({name, data: JSON.parse(c.data), signed: !!c.signature, request_id: !!c.request_id});}});
      return {context, page};
    };
    const count = page => page.evaluate(() => {const el = document.getElementById('me-count'); return el.hidden ? 0 : Number(el.textContent);});
    const rowState = (page, id) => page.locator('#inbox-list a.inbox-row-link[data-updates~="' + id + '"] .inbox-row-state').textContent();
    const a = await device('A'), b = await device('B');

    // Both devices see the board's two unread items.
    await b.page.goto(origin + '/me');
    await b.page.waitForFunction(() => document.title.startsWith('(2) '));
    await b.page.locator('#inbox-list a.inbox-row-link[data-updates~="' + reply + '"]').waitFor();
    assert.equal(await rowState(b.page, reply), 'Unread');
    assert.equal(await rowState(b.page, dm), 'Unread');
    await a.page.goto(origin + '/?sort=new');
    await a.page.waitForFunction(() => document.title.startsWith('(2) '));
    assert.equal(await count(a.page), 2);

    // Device A opens the reply: one signed, batched updates.mark_read.
    await a.page.goto(origin + '/e/' + reply);
    await a.page.waitForFunction(() => document.title.startsWith('(1) '));
    for (let i = 0; i < 100 && !marks.length; i++) await new Promise(r => setTimeout(r, 50));
    assert.deepEqual(marks.map(m => [m.name, m.data, m.signed, m.request_id]), [['A', {schema: 1, ids: [reply]}, true, true]]);
    for (let i = 0; i < 100 && (await unread(me)).total !== 1; i++) await new Promise(r => setTimeout(r, 50));
    assert.deepEqual((await unread(me)).replies, [], 'the board has the reply read');

    // Device B's next poll agrees: its badge and Inbox list follow the board.
    await b.page.evaluate(() => window.SwarmPage.refreshNotifications());
    await b.page.waitForFunction(() => document.title.startsWith('(1) '));
    await b.page.waitForFunction(id => document.querySelector('#inbox-list a.inbox-row-link[data-updates~="' + id + '"] .inbox-row-state')?.textContent === 'Read', reply);
    assert.equal(await rowState(b.page, dm), 'Unread');
    assert.equal(await count(b.page), 1);

    // Mark all read on B clears the board, and A at its next poll.
    await b.page.locator('#inbox-mark-read').click();
    await b.page.waitForFunction(() => !/^\(\d/.test(document.title));
    for (let i = 0; i < 100 && (await unread(me)).total !== 0; i++) await new Promise(r => setTimeout(r, 50));
    assert.equal((await unread(me)).total, 0);
    assert.deepEqual(marks.at(-1).data, {schema: 1, all: true});
    assert.equal(await rowState(b.page, dm), 'Read');
    assert.equal(await b.page.locator('#inbox-mark-read').isHidden(), true);
    await a.page.evaluate(() => window.SwarmPage.refreshNotifications());
    await a.page.waitForFunction(() => !/^\(\d/.test(document.title));
    assert.equal(await count(a.page), 0);

    // A new reply is unread on both; opening its Inbox row on A reads it everywhere.
    const later = (await signed(sender, {operation: 'post', room: 'lobby', reply_to: root, text: 'Disposable later reply'})).receipt.id;
    await a.page.goto(origin + '/me');
    await a.page.evaluate(() => window.SwarmPage.refreshNotifications());
    await a.page.waitForFunction(() => document.title.startsWith('(1) '));
    await a.page.locator('#inbox-list a.inbox-row-link[data-updates~="' + later + '"]').click();
    await a.page.waitForURL('**/e/' + later);
    for (let i = 0; i < 100 && (await unread(me)).total !== 0; i++) await new Promise(r => setTimeout(r, 50));
    assert.equal((await unread(me)).total, 0, 'opening the row read it on the board');
    await b.page.evaluate(() => window.SwarmPage.refreshNotifications());
    await b.page.waitForFunction(() => !/^\(\d/.test(document.title));
    assert.equal(await b.page.locator('#inbox-list img, #inbox-list script').count(), 0, 'previews stay text');

    // A mention, once its page is opened, never comes back: not on later polls, not on the other device.
    const mention = (await signed(sender, {operation: 'post', room: 'lobby', text: 'Disposable note about @mentions for @read-' + me.fingerprint.slice(0, 10)})).receipt.id;
    assert.deepEqual((await unread(me)).mentions, [mention]);
    await a.page.goto(origin + '/me');
    await a.page.evaluate(() => window.SwarmPage.refreshNotifications());
    await a.page.waitForFunction(() => document.title.startsWith('(1) '));
    await a.page.goto(origin + '/e/' + mention);
    await a.page.waitForFunction(() => !/^\(\d/.test(document.title));
    for (let i = 0; i < 100 && (await unread(me)).total !== 0; i++) await new Promise(r => setTimeout(r, 50));
    for (const page of [a.page, b.page, a.page, b.page]) {
      await page.evaluate(() => window.SwarmPage.refreshNotifications());
      await page.waitForTimeout(400);
      assert.equal(await count(page), 0, 'the read mention stays read');
    }
    assert.equal((await unread(me)).total, 0);
    assert.deepEqual(errors, []);
    console.log('PASS: read state on the board — a reply opened on one device clears on the other at its next poll, Mark all read and an opened Inbox row clear both, signed batched updates.mark_read.');
  } finally { await browser.close(); server.kill(); }
})().catch(error => { console.error(error); process.exit(1); });
