// Requires an owned, verified disposable loopback preview; never production.
// A sealed conversation end to end on the real board, two browsers: A starts
// it from /me/messages/new (its sealing key is made and published on the way),
// the board stores only envelopes, B reads the sealed request, accepts and
// replies (which rotates the epoch, since the members changed), and A reads the
// reply with B's join line and safety number. Then the downgrades: the board
// refuses cleartext, a page served an unsealed story about a pinned room refuses
// it, and a new sealing key shows as a key-change line.
const assert = require('node:assert/strict');
const crypto = require('node:crypto');
const {identity, withIdentity, launch, noHorizontalScroll, fp} = require('./testdata/messages_mock.cjs');
const origin = process.env.SWARMMEMO_TEST_URL;
assert.match(origin || '', /^http:\/\/127\.0\.0\.1:\d+$/, 'requires an explicit disposable loopback preview');
const shots = process.env.SWARMMEMO_SCREENSHOT_DIR;

async function command(id, body, expectOK = true) {
  const response = await fetch(origin + '/v1/command', {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify(id.sign(body).command)});
  const result = await response.json();
  if (expectOK) assert.equal(result.ok, true, body.operation + ': ' + JSON.stringify(result.error));
  return result;
}
function x25519Pair() {
  const pair = crypto.generateKeyPairSync('x25519');
  return {private_key: pair.privateKey.export({format: 'der', type: 'pkcs8'}).subarray(-32).toString('base64url'), public_key: pair.publicKey.export({format: 'der', type: 'spki'}).subarray(-32).toString('base64url')};
}
function safetyNumber(publicKey, x25519) {
  const digest = crypto.createHash('sha256').update(Buffer.concat([Buffer.from('swarmmemo-safety/1\0' + publicKey + '\0' + x25519)])).digest();
  return Array.from({length: 6}, (_, i) => String(Number(digest.readUIntBE(i * 5, 5) % 100000)).padStart(5, '0')).join(' ');
}
const status = (page, id, pattern) => page.waitForFunction(({id, source}) => new RegExp(source).test(document.getElementById(id)?.textContent || ''), {id, source: pattern.source});

(async () => {
  const browser = await launch();
  try {
    const a = identity(''), b = identity('');
    const tag = crypto.randomBytes(3).toString('hex');
    await command(a, {operation: 'agent.register', handle: 'sealer-a-' + tag});
    await command(b, {operation: 'agent.register', handle: 'sealer-b-' + tag});
    // B's sealing key is on B's device and published by B's key.
    const bSeal = x25519Pair(), bKid = fp(Buffer.from(bSeal.public_key, 'base64url')).slice(0, 32);
    await command(b, {operation: 'identity.link', data: JSON.stringify({schema: 1, kind: 'x25519', value: bSeal.public_key})});
    const errors = [];
    const open = async (id, seal) => {
      const context = await browser.newContext({viewport: {width: 1280, height: 900}});
      await withIdentity(context, id);
      if (seal) await context.addInitScript(value => { if (!localStorage.getItem('swarmmemo.seal.v1')) localStorage.setItem('swarmmemo.seal.v1', value); }, JSON.stringify({version: 1, service: 'swarmmemo.com', keys: [{owner: id.fingerprint, kid: bKid, ...seal, created_at: 1}]}));
      const page = await context.newPage(); page.on('pageerror', e => errors.push(e.message));
      return page;
    };
    const pageA = await open(a), pageB = await open(b, bSeal);

    // A starts a sealed conversation with B.
    const secret = 'The launch code is in the blue folder, ' + tag + '.';
    await pageA.goto(origin + '/me/messages/new?to=' + b.fingerprint + '&tier=sealed');
    await pageA.waitForFunction(() => { const s = document.querySelector('input[name=tier][value=sealed]'); return s && !s.disabled && s.checked; });
    await pageA.locator('#new-conversation-text').fill(secret);
    await Promise.all([pageA.waitForURL(/\/me\/messages\/~[a-z2-7]{26}$/), pageA.locator('#new-conversation-form button[type=submit]').click()]);
    const room = decodeURIComponent(new URL(pageA.url()).pathname.split('/').pop());
    await pageA.locator('.conversation-message .memo-text', {hasText: secret}).waitFor();
    assert.equal(await pageA.locator('.conversation-message .sealed-badge').count(), 1);
    assert.match(await pageA.locator('#conversation-badges').textContent(), /Sealed · only members can read/);
    // A's key was published on the way, and the board holds only an envelope.
    const selfA = (await command(a, {operation: 'agent.get', target: a.fingerprint})).agent;
    assert.ok(selfA.seal_key?.x25519, 'A published a sealing key');
    const stored = await command(a, {operation: 'conversation.get', room});
    assert.equal(stored.messages.length, 1);
    assert.match(stored.messages[0].text, /^sealed1\.1\./); assert.equal(stored.messages[0].format, 'sealed');
    assert.ok(!stored.messages[0].text.includes('blue folder'));
    assert.equal(stored.data.conversation.sealed, true);
    assert.equal(stored.data.seal.epoch, 1);

    // The board refuses cleartext into it, and an envelope of the wrong epoch.
    const refused = await command(a, {operation: 'post', room, text: 'cleartext'}, false);
    assert.equal(refused.error?.code, 'sealed_required');
    const stale = await command(a, {operation: 'post', room, text: stored.messages[0].text.replace(/^sealed1\.1\./, 'sealed1.7.'), data: JSON.stringify({schema: 1, format: 'sealed'})}, false);
    assert.equal(stale.error?.code, 'seal_rotation_required');

    // B reads the sealed request, accepts and replies.
    await pageB.goto(origin + '/me/messages?tab=requests');
    await pageB.locator('#conversation-list a.conversation-link').first().waitFor();
    assert.equal(await pageB.locator('#conversation-list .sealed-badge').count(), 1);
    await pageB.goto(origin + '/me/messages/' + room);
    await pageB.locator('.conversation-message .memo-text', {hasText: secret}).waitFor();
    await pageB.getByRole('button', {name: 'Accept'}).click();
    await pageB.locator('#conversation-compose').waitFor({state: 'visible'});
    const reply = 'Found it, ' + tag + '.';
    await pageB.locator('#conversation-text').fill(reply);
    await pageB.locator('#conversation-compose button[type=submit]').click();
    await status(pageB, 'conversation-status', /Sent\./);
    const after = await command(b, {operation: 'conversation.get', room});
    assert.equal(after.data.seal.epoch, 2, 'the join forced a new epoch before the reply');
    assert.match(after.messages.at(-1).text, /^sealed1\.2\./);

    // A reads the reply; B's join shows with B's safety number.
    await pageA.reload();
    await pageA.locator('.conversation-message .memo-text', {hasText: reply}).waitFor();
    const joined = pageA.locator('.system-line', {hasText: 'joined'});
    await joined.waitFor();
    assert.equal(await joined.locator('.safety-number').textContent(), safetyNumber(b.publicKey, bSeal.public_key));
    assert.match(await joined.locator('.term').getAttribute('title'), /Compare it with them/);
    await pageA.setViewportSize({width: 375, height: 800});
    assert.ok(await noHorizontalScroll(pageA), 'no horizontal scroll at 375px');
    if (shots) await pageA.screenshot({path: shots + '/messages-sealed-375.png', fullPage: true});
    await pageA.setViewportSize({width: 1280, height: 900});

    // B publishes a new sealing key: A sees the key change, with the new number.
    const next = x25519Pair();
    await command(b, {operation: 'identity.link', data: JSON.stringify({schema: 1, kind: 'x25519', value: next.public_key})});
    await pageA.reload();
    const changed = pageA.locator('.system-line.key-change');
    await changed.waitFor();
    assert.match(await changed.textContent(), /sealing key changed/);
    assert.equal(await changed.locator('.safety-number').textContent(), safetyNumber(b.publicKey, next.public_key));

    // A board that later claims the pinned room is not sealed, with a validly
    // signed creating command by another key, is refused: nothing is shown or
    // sent.
    const mallory = identity('');
    const forged = mallory.sign({operation: 'conversation.open', room, members: [b.fingerprint], data: JSON.stringify({schema: 1, kind: 'dm', sealed: false})});
    await pageA.route('**/v1/command', async route => {
      if (route.request().postDataJSON()?.operation !== 'conversation.get') return route.continue();
      const response = await route.fetch(); const body = await response.json();
      if (body.data?.conversation) { body.data.conversation.sealed = false; body.data.conversation.created = {public_key: forged.public_key, signature: forged.signature, signed_payload: forged.signed_payload}; delete body.data.seal; }
      await route.fulfill({response, json: body});
    });
    const posts = [];
    pageA.on('request', r => { if (r.url().endsWith('/v1/command') && r.postDataJSON()?.operation === 'post') posts.push(r); });
    await pageA.reload();
    await status(pageA, 'conversation-status', /Refusing this conversation/);
    assert.equal(await pageA.locator('.conversation-message').count(), 0, 'nothing from a downgraded room is shown');
    assert.equal(await pageA.locator('#conversation-compose').isHidden(), true);
    assert.equal(posts.length, 0);
    assert.deepEqual(errors, []);
  } finally { await browser.close(); }
})().catch(error => { console.error(error); process.exit(1); });
