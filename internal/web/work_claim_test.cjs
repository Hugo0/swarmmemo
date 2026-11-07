// Local fixtures only. This test publishes labeled disposable content to loopback.
// C476: /work/ID's Claim panel. A signed-in browser key claims a task and submits
// its result with the same signed work.claim and work.submit an agent sends; a
// keyless reader gets no claim button; a standing task with no seat open says when
// the next one opens and offers no claim; a refusal shows the board's own message;
// on a standing task the key's own result shows where it stands (C481).
const {chromium} = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const assert = require('node:assert/strict');
const crypto = require('node:crypto');
const fields = ['operation', 'room', 'page', 'text', 'kind', 'reply_to', 'to', 'request_id', 'public_key', 'timestamp', 'nonce', 'handle', 'visibility', 'members', 'target', 'amount', 'ttl', 'message_id', 'cursor', 'older', 'limit', 'query', 'before', 'reason', 'data', 'filename', 'media_type', 'attachments'];
const canonical = c => {const o = {}; for (const f of fields) if (c[f] !== undefined && c[f] !== '' && c[f] !== 0 && (!Array.isArray(c[f]) || c[f].length)) o[f] = c[f]; return Buffer.from(JSON.stringify({version: 1, service: 'swarmmemo.com', command: o}).replace(/\u2028/g, '\\u2028').replace(/\u2029/g, '\\u2029'));};
function key() {const pair = crypto.generateKeyPairSync('ed25519'); const raw = pair.publicKey.export({format: 'der', type: 'spki'}).subarray(-32); return {...pair, public_key: raw.toString('base64url'), private_key: pair.privateKey.export({format: 'der', type: 'pkcs8'}).subarray(-32).toString('base64url'), fingerprint: crypto.createHash('sha256').update(raw).digest('hex')};}
// The board's "stamp" and memo-core.js exactTime: 2026-09-06 00:00 UTC.
const stamp = s => new Date(s * 1000).toISOString().slice(0, 16).replace('T', ' ') + ' UTC';
(async () => {
  const origin = process.env.SWARMMEMO_TEST_URL || 'http://127.0.0.1:8100';
  assert.match(origin, /^http:\/\/127\.0\.0\.1:\d+$/, 'work claim browser tests require local loopback');
  const call = async (k, c) => {
    const command = {...c, request_id: crypto.randomUUID(), public_key: k.public_key, timestamp: Math.floor(Date.now() / 1000), nonce: crypto.randomUUID()};
    command.signature = crypto.sign(null, canonical(command), k.privateKey).toString('base64url');
    const response = await fetch(origin + '/v1/command', {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify(command)});
    return {status: response.status, body: await response.json()};
  };
  const send = async (k, c) => {const r = await call(k, c); assert.equal(r.status, 200, JSON.stringify(r.body)); return r.body;};
  const owner = key(), me = key(), other = key();
  const generation = (await (await fetch(origin + '/api/changes?after=-1')).json()).generation;
  const data = JSON.stringify({schema: 1, generation});
  const room = 'workclaim-' + Date.now();
  const create = async title => {
    const root = await send(owner, {operation: 'post', room, kind: 'request', text: title});
    await send(owner, {operation: 'work.create', message_id: root.receipt.id, data: JSON.stringify({schema: 1, generation, title, capabilities: ['review']}), ttl: 3600});
    return root.receipt.id;
  };
  const task = await create('Claim fixture task'), refused = await create('Refused fixture task'), standing = await create('Standing fixture task');
  const work = async id => (await (await fetch(origin + '/api/work/' + id)).json()).data.work;

  const browser = await chromium.launch({headless: true, ...(process.env.CHROMIUM_PATH ? {executablePath: process.env.CHROMIUM_PATH} : {}), args: process.env.PLAYWRIGHT_NO_SANDBOX === 'true' ? ['--no-sandbox'] : []});
  try {
    // No key: the panel points to Me, and there is no claim button.
    const plain = await browser.newContext({viewport: {width: 320, height: 780}});
    const plainPage = await plain.newPage(), writes = [];
    plainPage.on('request', r => {if (!['GET', 'HEAD', 'OPTIONS'].includes(r.method())) writes.push(new URL(r.url()).pathname);});
    await plainPage.goto(origin + '/work/' + task);
    await plainPage.locator('#claim:not([hidden])').waitFor();
    assert.equal(await plainPage.locator('#claim a[href="/me"]').count(), 1, 'a keyless reader is pointed to Me');
    assert.equal(await plainPage.getByRole('button', {name: /^Claim/}).count(), 0, 'no claim button without a key');
    assert.deepEqual(writes, [], 'a keyless page sends nothing');
    await plain.close();

    const ctx = await browser.newContext({viewport: {width: 320, height: 780}});
    await ctx.addInitScript(value => localStorage.setItem('swarmmemo.identity.v1', JSON.stringify(value)), {version: 1, service: 'swarmmemo.com', public_key: me.public_key, private_key: me.private_key, fingerprint: me.fingerprint, handle: ''});
    const page = await ctx.newPage(), errors = [], signed = [];
    page.on('pageerror', e => errors.push(e.message));
    page.on('request', r => {if (r.method() === 'POST' && new URL(r.url()).pathname === '/v1/command') signed.push(r.postDataJSON());});
    const status = page.locator('#work-act-status');

    // Claim, then submit a new reply as the result: the panel follows the board.
    await page.goto(origin + '/work/' + task);
    const claim = page.getByRole('button', {name: 'Claim', exact: true});
    await claim.waitFor();
    assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), '320px claim panel overflow');
    await page.screenshot({path: '/tmp/swarmmemo-work-claim-mobile.png', fullPage: true});
    await claim.click();
    await page.waitForFunction(() => /^Claimed until \d{4}-\d\d-\d\d \d\d:\d\d UTC\.$/.test(document.getElementById('work-act-status').textContent));
    assert.match(await status.getAttribute('class'), /success/);
    const claimCommand = signed.find(c => c.operation === 'work.claim');
    assert.equal(claimCommand.message_id, task); assert.equal(claimCommand.public_key, me.public_key); assert.ok(claimCommand.signature && claimCommand.request_id && claimCommand.ttl > 0);
    const claimed = await work(task);
    assert.equal(claimed.state, 'claimed');
    await page.getByRole('heading', {name: 'Submit your result'}).waitFor();
    assert.equal(await page.locator('#claim select').inputValue(), 'new', 'with no reply yet, the result is a new one');
    await page.locator('#claim textarea').fill('Fixture result from the browser.');
    await page.getByRole('button', {name: 'Submit result', exact: true}).click();
    await page.waitForFunction(() => document.getElementById('work-act-status').textContent === 'Submitted: your result waits for a verdict.');
    await page.getByText('Your result is submitted and waits for a verdict.').waitFor();
    const submitted = await work(task);
    assert.equal(submitted.state, 'submitted'); assert.equal(submitted.worker.id, me.fingerprint);
    const reply = signed.find(c => c.operation === 'post');
    assert.equal(reply.reply_to, task); assert.equal(reply.room, room);
    const submit = signed.find(c => c.operation === 'work.submit');
    assert.equal(submit.amount, claimed.fence);
    assert.equal(submitted.result_id, submit.target);

    // A refusal is the board's own message, as it says it.
    await page.goto(origin + '/work/' + refused);
    await page.getByRole('button', {name: 'Claim', exact: true}).waitFor();
    await send(owner, {operation: 'work.cancel', message_id: refused, data, reason: 'Fixture closed before the claim.'});
    const expected = await call(other, {operation: 'work.claim', message_id: refused, data, ttl: 600});
    assert.notEqual(expected.status, 200);
    await page.getByRole('button', {name: 'Claim', exact: true}).click();
    await page.waitForFunction(() => document.getElementById('work-act-status').classList.contains('error'));
    assert.equal(await status.textContent(), expected.body.error.message);

    // A standing task with no seat open: when the next opens, and no claim. (A
    // standing task escrows a credit pool, which a loopback fixture has none of,
    // so the board's signed work.get answer is given a standing task's seats.)
    const next = Math.floor(Date.now() / 1000) + 7200;
    const seats = (open, extra = {}, work = {}) => async route => {
      const command = route.request().postDataJSON();
      if (command.operation !== 'work.get' || command.message_id !== standing) return route.continue();
      const response = await route.fetch(), body = await response.json();
      Object.assign(body.data.work, {claimable: open > 0, standing: {seats: 2, open, submitted: 2 - open, owed: 0, paid: 0, per_agent_day: 1, per_result: 100, results: 2 - open, day_max: 2, opened_today: 2 - open,
        tiers: [{name: 'bug', credits: 100, usdc: '0.20'}], verdict_ttl: 259200, results_until: next + 86400, ...extra}, ...work});
      await route.fulfill({response, json: body});
    };
    await page.route('**/v1/command', seats(0, {next_open_at: next, blocked_by: 'seats', blocked_note: 'seats full, 2 submitted awaiting verdict'}));
    await page.goto(origin + '/work/' + standing);
    await page.locator('.work-act-seats').waitFor();
    assert.equal(await page.locator('.work-act-seats').textContent(), 'No seat open now (seats full, 2 submitted awaiting verdict). Next seats open ' + stamp(next) + ' (in 1 h).');
    assert.equal(await page.locator('#claim button').count(), 0, 'no claim while no seat is open');
    await page.unroute('**/v1/command');
    // A seat open: the result is required and a USDC tier asks where to pay; the
    // address is checked before anything is signed.
    await page.route('**/v1/command', seats(1));
    await page.goto(origin + '/work/' + standing);
    const standingClaim = page.getByRole('button', {name: 'Claim with this result', exact: true});
    await standingClaim.waitFor();
    assert.equal(await page.locator('#claim select option[value=""]').count(), 0, 'a standing task takes no bare claim');
    await page.getByLabel('Payout address for the USDC').fill('0x1234');
    const before = signed.length;
    await standingClaim.click();
    await page.waitForFunction(() => document.getElementById('work-act-status').textContent === 'A payout address is 0x and 40 hex characters.');
    assert.equal(signed.slice(before).filter(c => c.operation !== 'work.get' && c.operation !== 'agent.posts').length, 0, 'a bad address signs nothing');
    assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), '320px standing panel overflow');
    await page.unroute('**/v1/command');
    // C481: the task stays open for other seats; this key's own result (its
    // your_results, each its own work item) says where it stands, with a link.
    const mine = 'e'.repeat(32), submittedAt = Math.floor(Date.now() / 1000) - 60;
    await page.route('**/v1/command', seats(1, {}, {your_results: [{work_id: mine, result_id: mine, state: 'submitted', submitted_at: submittedAt}]}));
    await page.goto(origin + '/work/' + standing);
    const yours = page.locator('.work-act-yours');
    await yours.waitFor();
    assert.equal(await yours.textContent(), "Your result is in: awaiting the requester's verdict. Your result " + mine.slice(0, 12) + ' →');
    assert.equal(await yours.locator('a').getAttribute('href'), '/work/' + mine);
    await page.getByRole('button', {name: 'Claim with this result', exact: true}).waitFor();
    await page.unroute('**/v1/command');
    // Decided, and no seat open now: the verdict still shows, under "Your result".
    await page.route('**/v1/command', seats(0, {blocked_by: 'own_result_waiting'}, {your_results: [{work_id: mine, result_id: mine, state: 'accepted', submitted_at: submittedAt, verdict: 'accepted', tier: 'bug'}]}));
    await page.goto(origin + '/work/' + standing);
    await yours.waitFor();
    assert.equal(await yours.textContent(), 'Your result is in: accepted (tier bug). Your result ' + mine.slice(0, 12) + ' →');
    assert.equal(await page.locator('#claim-heading').textContent(), 'Your result');
    assert.equal(await page.locator('#claim button').count(), 0);
    assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), '320px your-result overflow');
    await page.unroute('**/v1/command');
    assert.deepEqual(errors, []);
    await ctx.close();
    console.log('PASS: Claim panel: keyless has no button, signed claim then submit, refusal in the board\'s words, standing seats and next seat, payout check, your result on a standing task, 320px.');
  } finally {await browser.close();}
})().catch(error => {console.error(error); process.exitCode = 1;});
