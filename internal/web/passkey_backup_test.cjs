// Passkey key backup on /me (RFC0014 §5). Run only against an owned,
// disposable loopback preview, like browser_test.cjs. Uses Chromium's WebAuthn
// virtual authenticator with the PRF extension; if this Chromium cannot
// emulate PRF, set PASSKEY_STUB_PRF=1 to stub only the PRF output and still
// exercise the real encryption, storage and restore path.
const {chromium} = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const assert = require('node:assert/strict');
// WebAuthn refuses IP-address origins, so a 127.0.0.1 preview is reached as localhost (same server).
const origin = (process.env.SWARMMEMO_TEST_URL || '').replace(/^http:\/\/127\.0\.0\.1:/, 'http://localhost:');
assert.match(origin, /^http:\/\/localhost:\d+$/, 'requires an explicit disposable loopback preview');

async function authenticator(page, prf) {
  const cdp = await page.context().newCDPSession(page);
  await cdp.send('WebAuthn.enable', {enableUI: false});
  const {authenticatorId} = await cdp.send('WebAuthn.addVirtualAuthenticator', {options: {
    protocol: 'ctap2', ctap2Version: 'ctap2_1', transport: 'internal', hasResidentKey: true, hasUserVerification: true,
    isUserVerified: true, automaticPresenceSimulation: true, hasPrf: prf}});
  return {cdp, authenticatorId};
}

async function ready(page) {
  await page.goto(origin + '/me#key');
  await page.waitForFunction(() => !document.getElementById('workspace-controls').disabled);
}

(async () => {
  const browser = await chromium.launch({headless: true,
    ...(process.env.CHROMIUM_PATH ? {executablePath: process.env.CHROMIUM_PATH} : {}),
    args: process.env.PLAYWRIGHT_NO_SANDBOX === 'true' ? ['--no-sandbox'] : []});
  const stub = process.env.PASSKEY_STUB_PRF === '1';
  try {
    // 1. Back up, forget, restore with the same (synced) passkey.
    const context = await browser.newContext({viewport: {width: 390, height: 844}});
    if (stub) await context.addInitScript(() => {
      const fixed = new Uint8Array(32).fill(7).buffer;
      for (const name of ['create', 'get']) {
        const original = navigator.credentials[name].bind(navigator.credentials);
        navigator.credentials[name] = async (...args) => {
          const credential = await original(...args);
          const results = credential.getClientExtensionResults.bind(credential);
          credential.getClientExtensionResults = () => ({...results(), prf: {enabled: true, results: {first: fixed}}});
          return credential;
        };
      }
    });
    const page = await context.newPage();
    const errors = []; page.on('pageerror', e => errors.push(String(e))); page.on('console', m => {if (m.type() === 'error') errors.push(m.text());});
    page.on('dialog', dialog => dialog.accept());
    const bodies = [];
    page.on('request', r => {if (r.url().endsWith('/v1/command') && r.method() === 'POST') bodies.push(r.postData() || '');});
    let auth;
    try { auth = await authenticator(page, !stub); }
    catch (error) { throw Error('This Chromium cannot add a PRF virtual authenticator (' + error.message + '); rerun with PASSKEY_STUB_PRF=1.'); }
    await ready(page);
    await page.click('#identity-create');
    await page.waitForFunction(() => /registered/.test(document.getElementById('identity-status').textContent));
    const original = await page.evaluate(() => JSON.parse(localStorage.getItem('swarmmemo.identity.v1')));
    assert.match(await page.locator('#me-key-note').textContent(), /this browser only/);
    assert.ok(await page.locator('#passkey-backup').isVisible());
    assert.ok(await page.locator('#passkey-restore').isVisible());

    await page.click('#passkey-backup');
    await page.waitForFunction(() => /Backed up\.|cannot|could not|did not/.test(document.getElementById('backup-status').textContent));
    assert.match(await page.locator('#backup-status').textContent(), /^Backed up\./);
    await page.waitForFunction(() => !document.getElementById('passkey-backup-state').hidden);
    assert.match(await page.locator('#passkey-backup-state').textContent(), /Backed up with a passkey/);
    assert.match(await page.locator('#me-key-note').textContent(), /with a passkey backup/);
    const creds = (await auth.cdp.send('WebAuthn.getCredentials', {authenticatorId: auth.authenticatorId})).credentials;
    assert.equal(creds.length, 1, 'one passkey');
    assert.ok(creds[0].isResidentCredential, 'discoverable passkey');
    const account = Buffer.from(creds[0].userHandle, 'base64').toString('hex');
    assert.equal(account, original.fingerprint, 'the passkey user handle is the account');
    // The private key never leaves the browser in clear.
    const seed = original.private_key;
    assert.ok(bodies.some(b => b.includes('key.backup.put')), 'a backup was stored');
    assert.ok(bodies.every(b => !b.includes(seed)), 'private key sent to the server');

    // The restore read needs no key and finds the row only with the credential id.
    const restoreRead = cred => page.evaluate(async ([target, cred]) => (await fetch('/v1/command', {method: 'POST', headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({operation: 'key.backup.get', target, data: JSON.stringify({schema: 1, credential_id: cred})})})).json(), [account, cred]);
    const credID = Buffer.from(creds[0].credentialId, 'base64').toString('base64url');
    const stored = await restoreRead(credID);
    assert.equal(stored.ok, true); assert.equal(stored.data.key_id, original.fingerprint); assert.equal(stored.data.current, true);
    assert.ok(!JSON.stringify(stored).includes(seed), 'server returned plaintext');
    const miss = await restoreRead(Buffer.alloc(32, 1).toString('base64url'));
    assert.equal(miss.error?.code, 'key_backup_not_found');

    // Forget the key, then restore it from the passkey.
    await page.click('#identity-forget');
    await page.waitForFunction(() => /removed/.test(document.getElementById('identity-status').textContent));
    assert.equal(await page.evaluate(() => localStorage.getItem('swarmmemo.identity.v1')), null);
    await page.click('#passkey-restore');
    await page.waitForFunction(() => /restored|Nothing changed/.test(document.getElementById('identity-status').textContent));
    assert.match(await page.locator('#identity-status').textContent(), /Key restored from your passkey backup/);
    const back = await page.evaluate(() => JSON.parse(localStorage.getItem('swarmmemo.identity.v1')));
    assert.equal(back.fingerprint, original.fingerprint);
    assert.equal(back.public_key, original.public_key);
    assert.equal(back.private_key, original.private_key);
    // The restored key signs: a status read succeeds and shows the backup.
    await page.waitForFunction(() => !document.getElementById('passkey-backup-state').hidden);
    assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), 'no horizontal scroll at 390px');

    // Remove the backup; the restore read then misses.
    await page.click('#passkey-backup-remove');
    await page.waitForFunction(() => /removed/.test(document.getElementById('backup-status').textContent));
    assert.equal((await restoreRead(credID)).error?.code, 'key_backup_not_found');
    assert.deepEqual(errors.filter(e => !/404|Failed to load resource/.test(e)), [], 'console errors');
    await context.close();

    // 2. A passkey without PRF: plain refusal, nothing stored.
    if (!stub) {
      const ctx = await browser.newContext();
      const p = await ctx.newPage(); p.on('dialog', d => d.accept());
      const puts = []; p.on('request', r => {if ((r.postData() || '').includes('key.backup.put')) puts.push(r);});
      await authenticator(p, false);
      await ready(p);
      await p.click('#identity-create');
      await p.waitForFunction(() => /registered/.test(document.getElementById('identity-status').textContent));
      await p.click('#passkey-backup');
      await p.waitForFunction(() => /Nothing was stored/.test(document.getElementById('backup-status').textContent));
      assert.match(await p.locator('#backup-status').textContent(), /PRF|cannot encrypt|Nothing was stored/);
      assert.equal(puts.length, 0, 'no backup stored without PRF');
      assert.ok(await p.locator('#passkey-backup-state').isHidden());
      await ctx.close();
    }
    console.log('passkey backup: ok' + (stub ? ' (PRF output stubbed)' : ' (virtual authenticator with PRF)'));
  } finally {
    await browser.close();
  }
})().catch(error => { console.error(error); process.exit(1); });
