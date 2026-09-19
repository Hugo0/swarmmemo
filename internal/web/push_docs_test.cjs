// Webhooks must be documented where an agent looks, and must never appear as a
// browser form: a field that makes this service fetch a typed URL is the attack
// the feature is designed around.
const {chromium} = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const assert = require('node:assert/strict');
(async () => {
  const browser = await chromium.launch({headless: true, ...(process.env.CHROMIUM_PATH ? {executablePath: process.env.CHROMIUM_PATH} : {})});
  const origin = process.env.SWARMMEMO_TEST_URL || 'http://127.0.0.1:8089';
  const page = await (await browser.newContext()).newPage();
  try {
    await page.goto(origin + '/for-agents');
    const push = page.locator('#push');
    await push.waitFor();
    const text = await push.textContent();
    for (const needed of ['webhook.create', 'challenge', 'HTTPS', 'never message text']) {
      assert.ok(text.includes(needed), `/for-agents#push omits ${needed}`);
    }
    assert.equal(await push.locator('input, textarea, form').count(), 0, 'push docs must not offer a form');

    await page.goto(origin + '/me');
    const panel = page.locator('#push-delivery');
    await panel.waitFor();
    assert.ok((await panel.textContent()).includes('no form here'), '/me must say why there is no form');
    assert.equal(await panel.locator('input, textarea, button[type=submit]').count(), 0, '/me push panel must stay read-only');
    assert.equal(await panel.locator('a[href="/for-agents#push"]').count(), 1, '/me must link the how-to');

    const guide = await (await page.request.get(origin + '/llms.txt')).text();
    assert.match(guide, /webhook\.create/, 'llms.txt must mention push delivery');
    assert.match(guide, /for-agents#push/, 'llms.txt must point at the how-to');
    console.log('PASS: push delivery documented on /for-agents, /me and llms.txt, with no form anywhere.');
  } finally {await browser.close();}
})().catch(error => {console.error(error); process.exitCode = 1;});
