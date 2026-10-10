// Sign in with SwarmMemo in the comment embed (C157), end to end on loopback:
// a plain HTML host on its own origin, a reader whose SwarmMemo key lives in the
// board origin's localStorage, the /connect/embed window, a room-scoped grant,
// delegated comment, like, hide and restore, the account panel, sign out with
// revocation, and the grant listed in Me. Only loopback services.
const {chromium} = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const assert = require('node:assert/strict');
const http = require('node:http');
const {randomBytes, createPrivateKey, createPublicKey, createHash} = require('node:crypto');
const {pathToFileURL} = require('node:url');
const {resolve} = require('node:path');
const {curator} = require('./home_density_test.cjs');

(async () => {
  const origin = process.env.SWARMMEMO_TEST_URL;
  assert.match(origin || '', /^http:\/\/127\.0\.0\.1:\d+$/);
  const {Client, importKey, base64url} = await import(pathToFileURL(resolve(__dirname, '../../clients/javascript/swarmmemo.mjs')));
  // The reader's own SwarmMemo key, as Me keeps it in the board origin's storage.
  const seed = randomBytes(32);
  const priv = createPrivateKey({key: Buffer.concat([Buffer.from('302e020100300506032b657004220420', 'hex'), seed]), format: 'der', type: 'pkcs8'});
  const pub = createPublicKey(priv).export({format: 'der', type: 'spki'}).subarray(-32);
  const fingerprint = createHash('sha256').update(pub).digest('hex');
  const stored = {version: 1, service: 'swarmmemo.com', public_key: base64url(pub), private_key: base64url(seed), fingerprint, handle: 'site-reader'};
  const reader = new Client({origin, key: importKey({version: 1, private_key: base64url(seed), public_key: base64url(pub)}), allowInsecureLoopback: true});
  const asReader = command => reader.send(reader.prepare(command));
  await asReader({operation: 'agent.register', handle: 'site-reader'});
  // The reader owns the room, so the grant carries hide and restore.
  const room = 'signin-' + Date.now();
  await asReader({operation: 'room.create', room, visibility: 'public'});
  await asReader({operation: 'post', room, page: 'post', text: 'Welcome, signed by the room owner.'});
  const send = await curator(origin);
  const other = (await send({operation: 'post', room, page: 'post', text: 'A comment by someone else'})).receipt.id;

  const host = http.createServer((request, response) => {
    if (request.url === '/favicon.ico') { response.writeHead(204); response.end(); return; }
    response.setHeader('Content-Security-Policy', `default-src 'self'; script-src ${origin}; connect-src ${origin}; img-src ${origin}; style-src 'self'; object-src 'none'`);
    response.setHeader('Content-Type', 'text/html');
    response.end(`<!doctype html><html><head><meta name="viewport" content="width=device-width,initial-scale=1"><title>Host</title></head><body><div id="comments"></div>
    <script src="${origin}/embed/v1.js" defer data-room="${room}" data-page="post" data-target="#comments"></script></body></html>`);
  });
  await new Promise(done => host.listen(0, '127.0.0.1', done));
  const hostOrigin = 'http://127.0.0.1:' + host.address().port;
  const browser = await chromium.launch({headless: true, ...(process.env.CHROMIUM_PATH ? {executablePath: process.env.CHROMIUM_PATH} : {})});
  try {
    // The sign-in window is never framed and answers only a valid request.
    const head = await fetch(origin + '/connect/embed?' + new URLSearchParams({room, origin: hostOrigin, pub: base64url(randomBytes(32))}));
    assert.equal(head.status, 200); assert.equal(head.headers.get('x-frame-options'), 'DENY');
    assert.match(head.headers.get('content-security-policy'), /frame-ancestors 'none'/);
    assert.equal((await fetch(origin + '/connect/embed?' + new URLSearchParams({room, origin: hostOrigin + '/path', pub: base64url(randomBytes(32))}))).status, 400);

    const context = await browser.newContext({viewport: {width: 390, height: 844}});
    await context.addInitScript(({origin, stored}) => { if (location.origin === origin) localStorage.setItem('swarmmemo.identity.v1', stored); }, {origin, stored: JSON.stringify(stored)});
    const page = await context.newPage();
    const errors = [], writes = [], requests = [];
    page.on('pageerror', error => errors.push(error.message));
    page.on('request', request => { requests.push(request.url()); if (request.url() === origin + '/v1/command') writes.push(request.postDataJSON()); });
    page.on('dialog', dialog => dialog.accept(dialog.message().includes('hiding') ? 'Spam' : 'Not spam'));
    await page.goto(hostOrigin);
    await page.locator('article').first().waitFor();
    assert.equal(requests.filter(url => url.includes('/embed/signin-v1.js')).length, 0, 'the sign-in module loads only on click');

    // Sign in: the window opens on SwarmMemo with this room, this origin and a fresh worker key.
    const opened = page.waitForEvent('popup');
    await page.getByRole('button', {name: 'Sign up', exact: true}).click();
    const popup = await opened;
    await popup.waitForURL(url => url.pathname === '/connect/embed');
    const asked = new URL(popup.url());
    assert.equal(asked.searchParams.get('room'), room); assert.equal(asked.searchParams.get('origin'), hostOrigin);
    assert.match(asked.searchParams.get('pub'), /^[A-Za-z0-9_-]{43}$/);
    assert.notEqual(asked.searchParams.get('pub'), stored.public_key, 'the site gets its own key');
    await popup.getByText('site-reader', {exact: true}).waitFor();
    assert.match(await popup.locator('.connect-embed-ask').textContent(), new RegExp('Let ' + hostOrigin.replace(/[.:/]/g, '\\$&') + ' comment and vote as site-reader in #' + room));
    assert.equal(await popup.locator('#connect-embed-moderate').isVisible(), true, 'the owner is told about hide and restore');
    await popup.locator('#connect-embed-allow:not([disabled])').waitFor();
    const closed = popup.waitForEvent('close');
    await popup.getByRole('button', {name: 'Allow', exact: true}).click();
    await closed;
    await page.getByRole('status').filter({hasText: 'Signed in with SwarmMemo.'}).waitFor();
    const create = writes.find(w => w.operation === 'delegation.create');
    assert.equal(create.room, room); assert.equal(create.ttl, 90 * 86400); assert.ok(create.proof && create.signature);
    assert.equal(create.public_key, stored.public_key, 'the reader\'s key signed the grant');
    const grantData = JSON.parse(create.data);
    assert.equal(grantData.origin, hostOrigin); assert.ok(grantData.operations.includes('vote') && grantData.operations.includes('room.hide'));
    const saved = await page.evaluate(room => JSON.parse(localStorage.getItem('swarmmemo.embed.grant.v1'))[room], room);
    assert.equal(saved.public_key, create.target); assert.equal(saved.agent, fingerprint);
    assert.equal(await page.evaluate(() => localStorage.getItem('swarmmemo.identity.v1')), null, 'the reader\'s own key never reaches the host origin');

    // A comment, signed by the worker key with the grant; no handle field.
    assert.equal(await page.getByLabel('Name (optional handle)').isVisible(), false);
    await page.getByLabel('Comment', {exact: true}).fill('Signed in through SwarmMemo');
    await page.getByRole('button', {name: 'Post comment', exact: true}).click();
    await page.getByText('Signed in through SwarmMemo', {exact: true}).waitFor();
    const post = writes.find(w => w.operation === 'post');
    assert.equal(post.public_key, saved.public_key); assert.equal(post.visibility, 'public'); assert.equal(post.handle, undefined);
    assert.deepEqual(post.delegation, {schema: 1, grant_id: saved.grant_id, generation: saved.generation});
    assert.match(await page.getByText(/^Posting as/).textContent(), /Posting as site-reader · signed in with SwarmMemo/);

    // A like through the grant counts for the reader's account.
    const voted = page.waitForResponse(r => r.url() === origin + '/v1/command' && r.request().postDataJSON()?.operation === 'vote');
    await page.locator(`article[data-id="${other}"] > .actions > button.heart`).click();
    const vote = await (await voted).json();
    assert.equal(vote.ok, true); assert.equal(vote.data.votes.up, 1);
    assert.ok(writes.find(w => w.operation === 'vote').delegation);

    // Hide and restore, as the room's owner, on the public log.
    await page.locator(`article[data-id="${other}"] > .actions`).getByRole('button', {name: 'Hide', exact: true}).click();
    await page.getByRole('status').filter({hasText: 'Hidden; logged publicly.'}).waitFor();
    await page.locator(`article[data-id="${other}"] > .body.removed`).waitFor();
    await page.locator(`article[data-id="${other}"] > .actions`).getByRole('button', {name: 'Restore', exact: true}).click();
    await page.getByRole('status').filter({hasText: 'Restored; logged publicly.'}).waitFor();
    await page.locator(`article[data-id="${other}"] > .body`).getByText('A comment by someone else', {exact: true}).waitFor();
    const log = await (await fetch(origin + '/api/room/' + room + '/modlog')).text();
    assert.match(log, /"hide"/); assert.match(log, /"restore"/);

    // The panel: who, manage, sign out. Reload keeps the sign-in.
    await page.reload();
    await page.locator('article').first().waitFor();
    const menu = page.getByRole('button', {name: /^Signed in as site-reader/});
    await menu.click();
    assert.equal(await menu.getAttribute('aria-expanded'), 'true');
    assert.equal(await page.getByRole('link', {name: 'Manage on SwarmMemo', exact: true}).getAttribute('href'), 'https://swarmmemo.com/me#site-grants');
    const revokeWindow = page.waitForEvent('popup');
    await page.getByRole('button', {name: 'Sign out of this site', exact: true}).click();
    const out = await revokeWindow;
    await page.getByRole('button', {name: 'Sign up', exact: true}).waitFor();
    assert.equal(await page.evaluate(() => localStorage.getItem('swarmmemo.embed.grant.v1')), null, 'the worker key is forgotten');
    await out.waitForURL(url => url.searchParams.get('action') === 'signout');
    await out.locator('#connect-embed-allow:not([disabled])').waitFor();
    await out.getByRole('button', {name: 'Sign out', exact: true}).click();
    await out.getByText(/^Revoked/).waitFor().catch(() => { /* It may close first. */ });
    const state = async () => (await (await fetch(origin + '/api/delegation/' + saved.grant_id)).json()).data.delegation.state;
    for (let i = 0; i < 50 && await state() !== 'revoked'; i++) await new Promise(r => setTimeout(r, 100));
    assert.equal(await state(), 'revoked');

    // Me lists the site with its state.
    const me = await context.newPage();
    await me.goto(origin + '/me#site-grants');
    try {
      await me.locator('#site-grants-list li').filter({hasText: hostOrigin.replace(/^https:\/\//, '')}).filter({hasText: 'revoked'}).waitFor();
    } catch (error) {
      const seen = await me.evaluate(() => ({status: document.getElementById('site-grants-status')?.textContent, list: document.getElementById('site-grants-list')?.innerText, hidden: document.getElementById('site-grants-list')?.hidden, hash: location.hash}));
      console.error('Me #site-grants on failure:', JSON.stringify(seen));
      throw error;
    }
    assert.deepEqual(await context.cookies(), []);
    assert.deepEqual(errors, []);

    // A reader with no SwarmMemo key signs up in one click: the window makes a
    // key on SwarmMemo's origin and grants the site in the same step.
    const freshContext = await browser.newContext({viewport: {width: 390, height: 844}});
    const fresh = await freshContext.newPage();
    const freshErrors = []; fresh.on('pageerror', error => freshErrors.push(error.message));
    await fresh.goto(hostOrigin);
    await fresh.locator('article').first().waitFor();
    const signupWindow = fresh.waitForEvent('popup');
    await fresh.getByRole('button', {name: 'Sign up', exact: true}).click();
    const signup = await signupWindow;
    await signup.getByRole('heading', {name: 'Sign up with SwarmMemo'}).waitFor();
    assert.equal(await signup.locator('#connect-embed-new').isVisible(), true, 'the window says a free key is made, no email or password');
    await signup.locator('#connect-embed-allow:not([disabled])').waitFor();
    const signupClosed = signup.waitForEvent('close');
    await signup.getByRole('button', {name: 'Sign up and comment', exact: true}).click();
    await signupClosed;
    await fresh.getByRole('status').filter({hasText: 'Signed in with SwarmMemo.'}).waitFor();
    const made = await (await freshContext.newPage()).goto(origin + '/me').then(r => r.frame().evaluate(() => JSON.parse(localStorage.getItem('swarmmemo.identity.v1'))));
    assert.match(made.fingerprint, /^[a-f0-9]{64}$/, 'the new key is kept on swarmmemo.com like Me keeps it');
    assert.equal(await fresh.evaluate(() => localStorage.getItem('swarmmemo.identity.v1')), null, 'the new key never reaches the host origin');
    assert.deepEqual(freshErrors, []);
    await freshContext.close();
    await context.close();
    console.log('PASS: embed sign-in window (never framed, strict query), room-scoped 90-day grant posted to the host origin only, delegated comment, like, owner hide and restore on the public log, panel, sign out with revocation, Me lists the site, no cookies.');
  } finally { await browser.close(); await new Promise(done => host.close(done)); }
})().catch(error => { console.error(error); process.exitCode = 1; });
