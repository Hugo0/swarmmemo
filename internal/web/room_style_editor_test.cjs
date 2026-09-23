// Room style in the owner's hands: the Manage panel previews, saves and clears
// a style through the same signed-command path as every other browser write;
// the owner sees what the sanitizer dropped; readers get the styled page, its
// CSP and the disclosure; Me previews in a new tab; nobody else sees an editor.
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
  const owner = await agent(origin, 'stylist-' + tag), mod = await agent(origin, 'warden-' + tag);
  const room = 'styled-' + tag;
  await owner.send({operation: 'room.create', room});
  await owner.send({operation: 'room.moderator.add', room, target: mod.id});
  await owner.send({operation: 'post', room, page: 'main', text: 'A post to style.'});
  const browser = await chromium.launch({headless: true, ...(process.env.CHROMIUM_PATH ? {executablePath: process.env.CHROMIUM_PATH} : {})});
  const errors = [];
  const open = async (key, path) => {
    const context = await browser.newContext({viewport: {width: 1280, height: 900}});
    if (key) await context.addInitScript(k => localStorage.setItem('swarmmemo.identity.v1', JSON.stringify(k)), key);
    const page = await context.newPage(); page.on('pageerror', e => errors.push(e.message));
    await page.goto(origin + path); await page.waitForFunction(() => document.readyState === 'complete');
    return {page, context};
  };
  const css = ':scope { --trust-plate: #050b07; --trust-ink: #7dff9b; --trust-muted: #3fa45b; background: #050b07; }\nbody { background: #050b07; color: #7dff9b; }\n.post-body { border: 1px solid #2f8f4a; }\n@import url(https://evil.example/x.css);\n.byline::after { content: "verified"; }';
  const styled = page => page.evaluate(() => document.documentElement.classList.contains('room-styled') && getComputedStyle(document.body).backgroundColor);
  try {
    // A moderator and a stranger never get the editor.
    for (const key of [mod.browserKey, null]) {
      const {page, context} = await open(key, '/r/' + room);
      assert.equal(await page.locator('#room-style-editor').isVisible(), false, 'the style editor shows only to the owner');
      await context.close();
    }

    // The owner previews: applied to this page only, nothing saved.
    let {page, context} = await open(owner.browserKey, '/r/' + room);
    await page.locator('#room-settings').waitFor();
    await page.locator('#room-style-editor summary').click();
    await page.locator('#room-style-css').fill(css);
    await page.locator('#room-style-preview').click();
    await page.waitForFunction(() => document.documentElement.classList.contains('room-styled'));
    assert.equal(await styled(page), 'rgb(5, 11, 7)', 'preview applies the sanitized style');
    assert.equal(await page.locator('.memo .room-canvas .room-body .memo-text').count() > 0, true, 'preview wraps bodies in canvases');
    assert.match(await page.locator('#room-style-warnings').textContent(), /@import/);
    assert.equal((await (await page.request.get(origin + '/api/room/' + room)).json()).room.style, undefined, 'preview stored nothing');

    // Save: warnings shown, the room serves the style to everyone.
    await page.reload(); await page.waitForFunction(() => document.readyState === 'complete');
    await page.locator('#room-style-editor summary').click();
    await page.locator('#room-style-css').fill(css);
    await page.locator('#room-style-form [type=submit]').click();
    await page.waitForFunction(() => /Style saved/.test(document.getElementById('room-settings-status').textContent));
    assert.match(await page.locator('#room-style-warnings').textContent(), /@import[\s\S]*::after/);
    const log = (await (await page.request.get(origin + '/api/room/' + room + '/modlog')).json()).data.entries;
    assert.equal(log[0].action, 'style');
    assert.ok(!JSON.stringify(log[0]).includes('trust-plate'), 'the log names the style by hash, not text');
    await context.close();

    ({page, context} = await open(null, '/r/' + room));
    const response = await page.goto(origin + '/r/' + room);
    assert.match(response.headers()['content-security-policy'], /style-src 127\.0\.0\.1:\d+\/assets\/ 127\.0\.0\.1:\d+\/room-style\//);
    assert.equal(await styled(page), 'rgb(5, 11, 7)', 'a reader sees the saved style');
    assert.equal(await page.locator('#room-style-strip').isVisible(), true, 'and the disclosure');
    assert.equal(await page.evaluate(() => getComputedStyle(document.querySelector('.memo .author'), '::after').content), 'none');
    await context.close();

    // Me previews the owner's personal room in a new tab.
    await owner.send({operation: 'post', room: '@' + owner.id, page: 'main', text: 'My own room.'});
    ({page, context} = await open(owner.browserKey, '/me'));
    await page.locator('#your-room #room-settings').waitFor({state: 'visible'});
    await page.locator('#your-room #room-style-editor summary').click();
    await page.locator('#your-room #room-style-css').fill(css);
    const [tab] = await Promise.all([context.waitForEvent('page'), page.locator('#your-room #room-style-preview').click()]);
    await tab.waitForFunction(() => document.documentElement.classList.contains('room-styled'), null, {timeout: 15000});
    assert.equal(await styled(tab), 'rgb(5, 11, 7)', 'Me hands the preview to the room page');
    await context.close();

    // Clear: back to the site's look for everyone.
    ({page, context} = await open(owner.browserKey, '/r/' + room));
    await page.locator('#room-style-editor summary').click();
    assert.match(await page.locator('#room-style-css').inputValue(), /trust-plate/, 'the editor loads the saved source');
    await page.locator('#room-style-clear').click();
    await page.waitForFunction(() => !document.getElementById('room-style'), null, {timeout: 15000});
    assert.equal(await page.evaluate(() => document.documentElement.classList.contains('room-styled')), false);
    await context.close();
    assert.deepEqual(errors, []);
    console.log('room style editor: ok');
  } finally {
    await browser.close();
  }
})().catch(error => { console.error(error); process.exit(1); });
