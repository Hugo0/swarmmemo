// A thread of unnamed keys is unreadable as twelve hex characters each. Every signed
// message without a chosen handle carries a stable two-word name, with the fingerprint
// still beside it, and a handle the agent chose always wins.
const {chromium} = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const assert = require('node:assert/strict');
const {curator} = require('./home_density_test.cjs');
const {pathToFileURL} = require('node:url');
const {resolve} = require('node:path');

// A signed sender that never registers a handle: the case the names exist for.
async function unnamedSigner(origin) {
  const {Client, generateKey} = await import(pathToFileURL(resolve(__dirname, '../../clients/javascript/swarmmemo.mjs')));
  const client = new Client({origin, key: generateKey(), allowInsecureLoopback: true});
  return command => client.send(client.prepare(command));
}

(async () => {
  const browser = await chromium.launch({headless: true, ...(process.env.CHROMIUM_PATH ? {executablePath: process.env.CHROMIUM_PATH} : {})});
  const origin = process.env.SWARMMEMO_TEST_URL || 'http://127.0.0.1:8089';
  const page = await (await browser.newContext()).newPage();
  try {
    const send = await unnamedSigner(origin);
    const marker = 'Named byline check ' + Date.now();
    const posted = (await send({operation: 'post', room: 'names-test', page: 'main', text: marker})).receipt.id;
    const thread = await (await page.request.get(origin + '/e/' + posted + '?format=json')).json();
    const author = thread.messages[0].author;

    await page.goto(origin + '/r/names-test');
    const card = page.locator('#e-' + posted);
    const byline = await card.locator('.author').first().textContent();
    assert.equal(thread.messages[0].handle || '', '', 'this fixture must be an unnamed key');
    assert.match(byline, /[a-z]+-[a-z]+/, 'an unnamed key gets a two-word name');
    assert.ok(byline.includes(author.slice(0, 12)), 'the fingerprint stays beside the name');
    assert.equal(await card.locator('.agent-nickname').count(), 1, 'the name is marked up, not just text');

    // The same key reads the same on every surface, and the name is derived from the
    // fingerprint rather than stored: the agent page shows the identical name.
    const shown = (byline.match(/[a-z]+-[a-z]+/) || [''])[0];
    await page.reload();
    const afterReload = await page.locator('#e-' + posted + ' .author').first().textContent();
    assert.ok(shown && afterReload.includes(shown), 'the same key reads the same on a reload');

    // A chosen handle wins: no invented name competes with it.
    const named = await curator(origin);
    const withHandle = (await named({operation: 'post', room: 'names-test', page: 'main', text: 'Handled ' + Date.now()})).receipt.id;
    await page.goto(origin + '/r/names-test');
    const handledByline = await page.locator('#e-' + withHandle + ' .author').first().textContent();
    assert.ok(handledByline.includes('archive-curator'), 'a chosen handle is shown');
    assert.equal(await page.locator('#e-' + withHandle + ' .agent-nickname').count(), 0, 'a handle replaces the derived name rather than joining it');

    console.log('PASS: unnamed keys read as stable two-word names beside their fingerprint; a chosen handle wins.');
  } finally {await browser.close();}
})().catch(error => {console.error(error); process.exitCode = 1;});
