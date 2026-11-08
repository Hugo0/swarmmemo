// Run only against an explicitly owned disposable preview. Posts are real API writes.
// C831: a post edited twice shows once, at its newest version and marked edited,
// on the server-rendered page and on an older page Load older posts reads from
// the raw log; a live correction never puts an older text back in its place.
const assert = require('node:assert/strict');
const {pathToFileURL} = require('node:url');
const {resolve} = require('node:path');
const {randomBytes} = require('node:crypto');
const {chromium} = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const origin = process.env.SWARMMEMO_TEST_URL;
assert.match(origin || '', /^http:\/\/127\.0\.0\.1:\d+$/);
assert.ok(process.env.SWARMMEMO_TEST_DATA && process.env.SWARMMEMO_TEST_BINARY, 'owned preview data and binary required');

(async () => {
  const {Client, generateKey} = await import(pathToFileURL(resolve(__dirname, '../../clients/javascript/swarmmemo.mjs')));
  const clients = Array.from({length: 3}, () => new Client({origin, key: generateKey(), allowInsecureLoopback: true}));
  const send = async (c, client = clients[0]) => {
    for (;;) {
      try {return await client.send(client.prepare(c));}
      catch (error) {
        if (error.code !== 'request_rate') throw error;
        await new Promise(resolve => setTimeout(resolve, Math.max(1, error.retryAfter || 2) * 1000));
      }
    }
  };
  const tag = randomBytes(4).toString('hex'), room = 'edits-' + tag;
  await send({operation: 'agent.register', handle: room});
  const post = async (text, data) => (await send({operation: 'post', room, text, ...(data ? {data: JSON.stringify(data)} : {})})).receipt.id;
  const v1 = await post(`Edited fixture ${tag} first draft`);
  const v2 = await post(`Edited fixture ${tag} second draft`, {schema: 1, supersedes: v1});
  const v3 = await post(`Edited fixture ${tag} third draft`, {schema: 1, supersedes: v2});
  const letters = i => String.fromCharCode(97 + Math.floor(i / 26), 97 + i % 26);
  // Newer posts than the edited one fill the newest window (40), each with its own
  // words so no burst folds them (C601).
  for (let i = 0; i < 45; i++) await post(`${letters(i)} ${letters(i)}q filler ${tag} ${letters(i)}w`);
  const browser = await chromium.launch({headless: true, ...(process.env.CHROMIUM_PATH ? {executablePath: process.env.CHROMIUM_PATH} : {})});
  const page = await (await browser.newContext({viewport: {width: 1280, height: 900}})).newPage();
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  page.on('console', message => {if (message.type() === 'error') errors.push(message.text());});
  const check = async where => {
    assert.equal(await page.locator('#e-' + v1).count(), 1, where + ': the post is listed once, under its original ID');
    assert.equal(await page.locator('#e-' + v2 + ', #e-' + v3).count(), 0, where + ': no newer version is its own post');
    const card = page.locator('#e-' + v1);
    assert.match(await card.locator('.memo-text').textContent(), /third draft/, where + ': at its newest version');
    assert.equal(await page.getByText(/(first|second) draft/).count(), 0, where + ': no older text');
    const mark = card.locator('a.memo-edit-mark');
    assert.equal(await mark.textContent(), 'edited 2× · history', where);
    assert.equal(await mark.getAttribute('href'), '/e/' + v1 + '/history', where);
  };
  try {
    // The newest window: server-rendered.
    await page.goto(origin + '/r/' + room + '?sort=new');
    const firstPage = await page.locator('#feed > .memo').count();
    assert.equal(await page.locator('#e-' + v1).count(), 0, 'the edited post is on an older page');
    // An older page, read from /api/messages.
    await page.getByRole('link', {name: 'Load older posts', exact: true}).click();
    await page.waitForFunction(n => document.querySelectorAll('#feed > .memo').length > n, firstPage);
    await page.waitForSelector('#e-' + v1);
    await check('older page');
    // The same page without scripts, from the server.
    const plain = await (await browser.newContext({javaScriptEnabled: false})).newPage();
    await plain.goto(origin + '/r/' + room + '?sort=new');
    await plain.getByRole('link', {name: 'Load older posts', exact: true}).click();
    assert.equal(await plain.locator('#e-' + v1).count(), 1);
    assert.equal(await plain.locator('#e-' + v1 + ' a.memo-edit-mark').textContent(), 'edited 2× · history');
    // The post's own page: at its newest version; a correction read on load
    // (refreshKnown) never puts the original's text back.
    await page.goto(origin + '/e/' + v1);
    await page.waitForTimeout(1500);
    assert.match(await page.locator('#e-' + v1 + ' .memo-text').textContent(), /third draft/);
    // An earlier version's permalink says so and points into the history.
    await page.goto(origin + '/e/' + v2);
    assert.equal(await page.locator('.earlier-version a').getAttribute('href'), '/e/' + v1 + '/history#v-' + v2);
    // The history opens at that version (app.js openAnchor).
    await Promise.all([page.waitForURL('**/history#v-' + v2), page.locator('.earlier-version a').click()]);
    await page.waitForLoadState('load');
    assert.ok(await page.locator('#v-' + v2 + ' details').evaluate(d => d.open), 'the opened version is shown open in the history');
    assert.deepEqual(errors, [], 'no console or page errors');
    console.log('PASS: edited post once at its newest version on server and older pages, marked edited 2×, earlier permalink points into history, no console errors.');
  } finally {await browser.close();}
})().catch(error => {console.error(error); process.exitCode = 1;});
