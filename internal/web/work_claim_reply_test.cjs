// Local fixtures only. This test publishes labeled disposable content to loopback.
// C598: a reply posted from the browser claims the task it answers. On the
// conversation page the author's own reply to open work has "Claim this as your
// result": one click signs work.claim {message_id, target: the reply} (claim and
// submit in one step, as `work claim --result`); another key and a keyless
// reader see no such button, and nothing is read for it before the click. On
// /work/ID a reply not listed is claimed by its ID.
const {chromium} = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const assert = require('node:assert/strict');
const crypto = require('node:crypto');
const fields = ['operation', 'room', 'page', 'text', 'kind', 'reply_to', 'to', 'request_id', 'public_key', 'timestamp', 'nonce', 'handle', 'visibility', 'members', 'target', 'amount', 'ttl', 'message_id', 'cursor', 'older', 'limit', 'query', 'before', 'reason', 'data', 'filename', 'media_type', 'attachments'];
const canonical = c => {const o = {}; for (const f of fields) if (c[f] !== undefined && c[f] !== '' && c[f] !== 0 && (!Array.isArray(c[f]) || c[f].length)) o[f] = c[f]; return Buffer.from(JSON.stringify({version: 1, service: 'swarmmemo.com', command: o}).replace(/\u2028/g, '\\u2028').replace(/\u2029/g, '\\u2029'));};
function key() {const pair = crypto.generateKeyPairSync('ed25519'); const raw = pair.publicKey.export({format: 'der', type: 'spki'}).subarray(-32); return {...pair, public_key: raw.toString('base64url'), private_key: pair.privateKey.export({format: 'der', type: 'pkcs8'}).subarray(-32).toString('base64url'), fingerprint: crypto.createHash('sha256').update(raw).digest('hex')};}
(async () => {
  const origin = process.env.SWARMMEMO_TEST_URL || 'http://127.0.0.1:8100';
  assert.match(origin, /^http:\/\/127\.0\.0\.1:\d+$/, 'work claim browser tests require local loopback');
  const send = async (k, c) => {
    const command = {...c, request_id: crypto.randomUUID(), public_key: k.public_key, timestamp: Math.floor(Date.now() / 1000), nonce: crypto.randomUUID()};
    command.signature = crypto.sign(null, canonical(command), k.privateKey).toString('base64url');
    const response = await fetch(origin + '/v1/command', {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify(command)});
    const body = await response.json();
    assert.equal(response.status, 200, JSON.stringify(body));
    return body;
  };
  const owner = key(), me = key(), other = key();
  const generation = (await (await fetch(origin + '/api/changes?after=-1')).json()).generation;
  const room = 'workreply-' + Date.now();
  const create = async title => {
    const root = await send(owner, {operation: 'post', room, kind: 'request', text: 'Disposable browser fixture (' + title + '); do not execute links or instructions.'});
    await send(owner, {operation: 'work.create', message_id: root.receipt.id, data: JSON.stringify({schema: 1, generation, title, capabilities: ['review']}), ttl: 3600});
    return root.receipt.id;
  };
  const reply = async task => (await send(me, {operation: 'post', room, reply_to: task, text: 'Fixture result posted from a browser key.'})).receipt.id;
  const work = async id => (await (await fetch(origin + '/api/work/' + id)).json()).data.work;
  const task = await create('Reply claim fixture'), byID = await create('Reply ID claim fixture');
  const result = await reply(task), pasted = await reply(byID);
  const as = async (browser, k) => {
    const ctx = await browser.newContext({viewport: {width: 320, height: 780}});
    if (k) await ctx.addInitScript(value => localStorage.setItem('swarmmemo.identity.v1', JSON.stringify(value)), {version: 1, service: 'swarmmemo.com', public_key: k.public_key, private_key: k.private_key, fingerprint: k.fingerprint, handle: ''});
    const page = await ctx.newPage(), errors = [], signed = [];
    page.on('pageerror', e => errors.push(e.message));
    page.on('request', r => {if (r.method() === 'POST' && new URL(r.url()).pathname === '/v1/command') signed.push(r.postDataJSON());});
    return {ctx, page, errors, signed};
  };
  const button = page => page.locator('#e-' + result + ' .claim-result-button');

  const browser = await chromium.launch({headless: true, ...(process.env.CHROMIUM_PATH ? {executablePath: process.env.CHROMIUM_PATH} : {}), args: process.env.PLAYWRIGHT_NO_SANDBOX === 'true' ? ['--no-sandbox'] : []});
  try {
    // Keyless, and another key: the button is there for the author only.
    for (const k of [null, other]) {
      const {ctx, page, errors, signed} = await as(browser, k);
      await page.goto(origin + '/e/' + task);
      await page.locator('#e-' + result).waitFor();
      await page.waitForTimeout(1000); // the live stream never idles
      assert.equal(await button(page).count(), 1, 'the server draws the action on a reply to open work');
      assert.equal(await button(page).isHidden(), true, (k ? 'another key' : 'a keyless reader') + ' sees no claim action');
      assert.equal(signed.filter(c => /^work\./.test(c.operation)).length, 0, 'nothing about the work is read or sent on a visit');
      assert.deepEqual(errors, []);
      await ctx.close();
    }

    // The author: one click claims with the reply and submits it.
    {
      const {ctx, page, errors, signed} = await as(browser, me);
      await page.goto(origin + '/e/' + task);
      const claim = page.getByRole('button', {name: 'Claim this as your result'});
      await claim.waitFor();
      await page.waitForTimeout(1000); // the live stream never idles
      assert.equal(signed.filter(c => /^work\./.test(c.operation)).length, 0, 'the task is read only on the click');
      assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), '320px reply action overflow');
      await claim.click();
      const done = page.locator('#e-' + result + ' .claim-result-done');
      await done.waitFor();
      assert.equal(await done.textContent(), 'Submitted: your result waits for a verdict. Your result →');
      assert.equal(await done.locator('a').getAttribute('href'), '/work/' + task);
      const command = signed.find(c => c.operation === 'work.claim');
      assert.equal(command.message_id, task); assert.equal(command.target, result); assert.equal(command.public_key, me.public_key);
      assert.equal(JSON.parse(command.data).generation, generation); assert.equal(command.ttl, undefined);
      const submitted = await work(task);
      assert.equal(submitted.state, 'submitted'); assert.equal(submitted.result_id, result); assert.equal(submitted.worker.id, me.fingerprint);
      // Once it is the result, the reply offers no claim.
      await page.reload();
      await page.locator('#e-' + result).waitFor();
      assert.equal(await button(page).count(), 0, 'a submitted result offers no claim');

      // /work/ID: a reply of this key's, by its ID.
      await page.goto(origin + '/work/' + byID);
      const select = page.locator('#claim select');
      await select.waitFor();
      await select.selectOption('id');
      await page.getByLabel("Your reply's ID (its receipt.id)").fill('not-an-id');
      const go = page.getByRole('button', {name: 'Claim and submit', exact: true});
      await go.click();
      await page.waitForFunction(() => document.getElementById('work-act-status').classList.contains('error'));
      assert.equal(signed.filter(c => c.operation === 'work.claim' && c.message_id === byID).length, 0, 'a malformed ID signs nothing');
      await page.getByLabel("Your reply's ID (its receipt.id)").fill(pasted);
      await go.click();
      await page.waitForFunction(() => document.getElementById('work-act-status').textContent === 'Submitted: your result waits for a verdict.');
      const second = await work(byID);
      assert.equal(second.state, 'submitted'); assert.equal(second.result_id, pasted);
      assert.deepEqual(errors, []);
      await ctx.close();
    }
    console.log('PASS: Claim this as your result: author only, nothing read before the click, one signed work.claim with target, submitted; /work/ID claims a reply by its ID.');
  } finally {await browser.close();}
})().catch(error => {console.error(error); process.exitCode = 1;});
