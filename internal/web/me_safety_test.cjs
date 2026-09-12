// Run only against an owned, verified disposable loopback preview, like browser_test.cjs.
const {chromium} = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const assert = require('node:assert/strict');
const origin = process.env.SWARMMEMO_TEST_URL;
assert.match(origin || '', /^http:\/\/127\.0\.0\.1:\d+$/, 'requires an explicit disposable loopback preview');

(async () => {
  const browser = await chromium.launch({headless: true,
    ...(process.env.CHROMIUM_PATH ? {executablePath: process.env.CHROMIUM_PATH} : {}),
    args: process.env.PLAYWRIGHT_NO_SANDBOX === 'true' ? ['--no-sandbox'] : []});
  try {
    for (const mode of ['no-js', 'blocked-script', 'startup-error', 'no-crypto', 'unsupported-ed25519']) {
      const context = await browser.newContext({javaScriptEnabled: mode !== 'no-js', viewport: {width: 320, height: 780}});
      if (mode === 'blocked-script') await context.route('**/assets/app.js', route => route.abort());
      if (mode === 'startup-error') await context.addInitScript(() => {
        Object.defineProperty(window, 'TextEncoder', {get() {throw Error('Synthetic startup failure');}});
      });
      if (mode === 'no-crypto') await context.addInitScript(() => {
        Object.defineProperty(window.crypto, 'subtle', {value: undefined});
      });
      if (mode === 'unsupported-ed25519') await context.addInitScript(() => {
        crypto.subtle.importKey = async () => {throw Error('Synthetic unsupported algorithm');};
      });
      const page = await context.newPage(); const urls = []; const writes = [];
      page.on('request', request => {
        urls.push(request.url());
        if (request.method() === 'POST' || /^\/(w|w64|c64)\//.test(new URL(request.url()).pathname)) writes.push(request.url());
      });
      await page.goto(origin + '/me');
      if (['no-crypto', 'unsupported-ed25519'].includes(mode)) {
        await page.waitForFunction(() => document.getElementById('workspace-readiness').textContent.includes('unavailable'));
      }
      assert.equal(await page.locator('#workspace-controls').getAttribute('disabled'), '', mode);
      assert.equal(await page.locator('#workspace-controls input:enabled, #workspace-controls textarea:enabled, #workspace-controls select:enabled, #workspace-controls button:enabled').count(), 0, mode);
      assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), mode + ' mobile overflow');
      // Even a programmatic submission cannot serialize disabled private fields.
      // Real keyboard/pointer users cannot edit or submit these controls at all.
      const submit = () => page.evaluate(() => {
        const form = document.getElementById('private-create-form');
        form.elements.room.value = 'synthetic-private-room-sentinel';
        form.elements.members.value = 'a'.repeat(64);
        if (new FormData(form).has('room') || new FormData(form).has('members')) throw Error('Disabled metadata serialized');
        form.requestSubmit();
      });
      if (['no-js', 'blocked-script', 'startup-error'].includes(mode)) {
        await Promise.all([page.waitForNavigation({waitUntil: 'load'}), submit()]);
      } else {
        await submit();
        assert.equal((await page.locator('#private-status').textContent()).trim(), '', 'disabled handler must not attempt signing');
      }
      assert.ok(urls.every(url => !url.includes('sentinel') && !url.includes('members=') && !url.includes('room=')), mode + ' metadata entered URL');
      assert.equal(writes.length, 0, mode + ' unexpectedly wrote');
      assert.ok(!new URL(page.url()).search, mode + ' submitted private query');
      await context.close();
    }
    const context = await browser.newContext();
    await context.addInitScript(() => {
      window.workspaceFormsWithHandlers = [];
      const add = HTMLFormElement.prototype.addEventListener;
      HTMLFormElement.prototype.addEventListener = function(type, ...rest) {
        if (type === 'submit') window.workspaceFormsWithHandlers.push(this.id);
        return add.call(this, type, ...rest);
      };
      const original = crypto.subtle.importKey.bind(crypto.subtle);
      crypto.subtle.importKey = (...args) => new Promise((resolve, reject) => {
        window.finishWorkspaceProbe = () => original(...args).then(resolve, reject);
      });
    });
    const page = await context.newPage(); const writes = [];
    page.on('request', request => {if (request.method() === 'POST') writes.push(request.url());});
    await page.goto(origin + '/me');
    await page.waitForFunction(() => typeof window.finishWorkspaceProbe === 'function');
    assert.ok(await page.locator('#identity-create').isDisabled(), 'pending probe must not enable controls');
    assert.deepEqual(await page.evaluate(() => window.workspaceFormsWithHandlers.sort()),
      ['handle-form', 'member-form', 'private-compose-form', 'private-create-form', 'private-open-form', 'transfer-form'].sort());
    await page.evaluate(() => window.finishWorkspaceProbe());
    await page.waitForFunction(() => !document.getElementById('workspace-controls').disabled);
    assert.equal(await page.locator('#workspace-readiness').isVisible(), false);
    assert.equal(await page.evaluate(() => localStorage.getItem('swarmmemo.identity.v1')), null, 'probe must not create an identity');
    assert.equal(writes.length, 0, 'initialization must not write');
    await context.close();
    console.log('PASS: no-JS, blocked script, startup failure, missing crypto and unsupported Ed25519 keep controls inert; no private metadata URLs; 320px layout; handlers precede enablement; no automatic key creation or writes.');
  } finally {await browser.close();}
})().catch(error => {console.error(error); process.exitCode = 1;});
