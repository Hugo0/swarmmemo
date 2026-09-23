// Room policy in the browser: the composer follows the room's rules, the owner
// alone gets its controls, and a moderator hides a reply with a public reason
// through the same signed-command path as every other browser write.
const {chromium} = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const assert = require('node:assert/strict');
const crypto = require('node:crypto');
const {pathToFileURL} = require('node:url');
const {resolve} = require('node:path');

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
  const origin = process.env.SWARMMEMO_TEST_URL;
  assert.match(origin || '', /^http:\/\/127\.0\.0\.1:\d+$/, 'requires an explicit disposable loopback preview');
  const tag = crypto.randomBytes(3).toString('hex');
  const owner = await agent(origin, 'host-' + tag), mod = await agent(origin, 'keeper-' + tag);
  const room = 'policy-' + tag;
  await owner.send({operation: 'room.create', room});
  await owner.send({operation: 'room.policy.set', room, data: JSON.stringify({write: 'owner', reply: 'anyone', rules: 'Stay on topic.'})});
  await owner.send({operation: 'room.moderator.add', room, target: mod.id});
  const root = (await owner.send({operation: 'post', room, page: 'main', text: 'The host opens a thread.'})).receipt.id;
  const browser = await chromium.launch({headless: true, ...(process.env.CHROMIUM_PATH ? {executablePath: process.env.CHROMIUM_PATH} : {})});
  const errors = [];
  const open = async (key, path, width = 1280) => {
    const context = await browser.newContext({viewport: {width, height: 900}});
    if (key) await context.addInitScript(k => localStorage.setItem('swarmmemo.identity.v1', JSON.stringify(k)), key);
    const page = await context.newPage(); page.on('pageerror', e => errors.push(e.message));
    await page.goto(origin + path); await page.waitForFunction(() => document.readyState === 'complete');
    return {page, context};
  };
  try {
    // A stranger: no composer for a top-level post, a clear line, Reply still offered.
    let {page, context} = await open(null, '/r/' + room, 320);
    assert.equal(await page.locator('#compose').isHidden(), true, 'owner-only room hides the composer');
    assert.match(await page.locator('#composer-gate').textContent(), /Only the owner starts posts here/);
    assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), 'mobile overflow');
    await page.locator('#e-' + root + ' .reply-button').click();
    assert.equal(await page.locator('#compose').isVisible(), true, 'replying reopens the composer');
    assert.equal(await page.locator('#composer-gate').isHidden(), true);
    await page.locator('#clear-reply').click();
    assert.equal(await page.locator('#compose').isHidden(), true, 'cancelling the reply closes it again');
    assert.equal(await page.locator('#room-settings').isHidden(), true, 'strangers never see owner controls');
    assert.equal(await page.locator('.mod-button:visible').count(), 0);
    await context.close();

    // The owner: composer and controls.
    ({page, context} = await open(owner.browserKey, '/r/' + room));
    await page.locator('#room-settings').waitFor();
    assert.equal(await page.locator('#compose').isVisible(), true, 'the owner may start posts');
    assert.equal(await page.locator('#room-policy-form select[name=write]').inputValue(), 'owner');
    await context.close();

    // A moderator hides an anonymous reply, with a public reason, never the owner's post.
    const reply = await fetch(origin + '/v1/command', {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify({operation: 'post', room, text: 'Buy followers', reply_to: root, request_id: 'spam-' + tag})}).then(r => r.json());
    ({page, context} = await open(mod.browserKey, '/e/' + root));
    const target = page.locator('#e-' + reply.receipt.id);
    await target.locator('.mod-button').waitFor({state: 'visible'});
    assert.equal(await page.locator('#e-' + root + ' .mod-button').isHidden(), true, "a moderator cannot hide the owner's post");
    await target.locator('.mod-button').click();
    await target.locator('.mod-form input[name=reason]').fill('Promotion.');
    await target.locator('.mod-form button[type=submit]').click();
    await page.waitForFunction(id => document.querySelector('#e-' + id + ' .removed')?.textContent.includes("Hidden by this room's moderators: Promotion."), reply.receipt.id);
    await context.close();
    const log = await fetch(origin + '/api/room/' + room + '/modlog').then(r => r.json());
    assert.equal(log.data.entries[0].action, 'hide');
    assert.equal(log.data.entries[0].actor, mod.id);

    // Me: the owner's own personal room, opened by saving a policy.
    ({page, context} = await open(owner.browserKey, '/me'));
    await page.locator('#your-room-link-row').waitFor();
    await page.waitForFunction(() => !document.getElementById('workspace-controls').disabled);
    await page.locator('#room-policy-form select[name=reply]').selectOption('members');
    await page.locator('#room-policy-form button[type=submit]').click();
    await page.waitForFunction(() => document.getElementById('room-settings-status').textContent.includes('Policy saved'));
    const personal = await fetch(origin + '/api/room/%40' + owner.id).then(r => r.json());
    assert.equal(personal.room.policy.write, 'owner');
    assert.equal(personal.room.policy.reply, 'members');
    await context.close();
    assert.deepEqual(errors, []);
    console.log('room policy browser behaviour passed');
  } finally {
    await browser.close();
  }
})().catch(error => { console.error(error); process.exit(1); });
