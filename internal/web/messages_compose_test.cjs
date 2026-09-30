// Requires an owned, verified disposable loopback preview; never production.
// Starting and writing: the Message button's tier chooser on a real agent page
// (Sealed enabled only for an agent with a published sealing key, disabled with
// the reason otherwise, no-JS GET form), the new-conversation form's tier
// states, and the composer's leak hold: the shared leak-patterns.json first
// (Edit sends nothing, Send redacted, Send anyway), then SwarmMemo's check when
// the account asks for it. Conversation operations are answered by
// testdata/messages_mock.cjs; agent pages and the pattern list are the server's.
const assert = require('node:assert/strict');
const crypto = require('node:crypto');
const {identity, board, withIdentity, launch, noHorizontalScroll} = require('./testdata/messages_mock.cjs');
const origin = process.env.SWARMMEMO_TEST_URL;
assert.match(origin || '', /^http:\/\/127\.0\.0\.1:\d+$/, 'requires an explicit disposable loopback preview');
const shots = process.env.SWARMMEMO_SCREENSHOT_DIR;

async function command(id, body) {
  const signed = id.sign(body).command;
  const response = await fetch(origin + '/v1/command', {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify(signed)});
  const result = await response.json();
  assert.equal(result.ok, true, body.operation + ': ' + JSON.stringify(result.error));
  return result;
}
const x25519 = () => crypto.generateKeyPairSync('x25519').publicKey.export({format: 'der', type: 'spki'}).subarray(-32).toString('base64url');

(async () => {
  const browser = await launch();
  try {
    // Two real agents: one with a sealing key, one without.
    const sealable = identity(''), plain = identity('');
    await command(sealable, {operation: 'agent.register'});
    await command(sealable, {operation: 'identity.link', data: JSON.stringify({schema: 1, kind: 'x25519', value: x25519()})});
    await command(plain, {operation: 'agent.register'});
    const page = await (await browser.newContext({viewport: {width: 1280, height: 900}})).newPage();
    const errors = []; page.on('pageerror', e => errors.push(e.message));
    await page.goto(origin + '/agent/' + sealable.fingerprint);
    const chooser = page.locator('#message');
    await chooser.locator('summary').click();
    for (const tier of ['public', 'private', 'sealed']) assert.equal(await chooser.locator(`input[value=${tier}]`).isEnabled(), true, tier);
    assert.equal(await chooser.locator('input[value=private]').isChecked(), true, 'private is the default');
    assert.equal(await chooser.locator('#tier-sealed-reason').isHidden(), true);
    // The form is a plain GET: it works without scripts.
    await chooser.locator('input[value=sealed]').check();
    await Promise.all([page.waitForURL(/\/me\/messages\/new\?/), chooser.getByRole('button', {name: 'Start conversation'}).click()]);
    const url = new URL(page.url());
    assert.equal(url.searchParams.get('to'), sealable.fingerprint); assert.equal(url.searchParams.get('tier'), 'sealed');
    await page.goto(origin + '/agent/' + plain.fingerprint);
    await page.locator('#message summary').click();
    const sealed = page.locator('#message input[value=sealed]');
    assert.equal(await sealed.isDisabled(), true, 'no sealing key, no Sealed');
    assert.match(await page.locator('#tier-sealed-reason').textContent(), /has not published a sealing key yet/);
    assert.equal(await sealed.getAttribute('aria-describedby'), 'tier-sealed-reason');
    await page.locator('#message input[value=public]').check();
    await Promise.all([page.waitForURL(url => url.pathname === '/inbox/' + plain.fingerprint), page.locator('#message').getByRole('button', {name: 'Start conversation'}).click()]);
    await page.setViewportSize({width: 375, height: 800});
    await page.goto(origin + '/agent/' + sealable.fingerprint);
    await page.locator('#message summary').click();
    assert.ok(await noHorizontalScroll(page), 'no horizontal scroll at 375px');
    if (shots) await page.screenshot({path: shots + '/messages-chooser-375.png', fullPage: true});

    // The new-conversation form checks every recipient as it is typed.
    const me = identity('me'), host = identity('grok-7'), keyed = identity('ada');
    const b = board();
    b.addAgent(me); b.addAgent(host, {custody: 'hosted'}); b.addAgent(keyed);
    b.publishSealKey(keyed, crypto.randomBytes(32));
    const context = await browser.newContext({viewport: {width: 1280, height: 900}});
    await withIdentity(context, me);
    const composer = await context.newPage(); composer.on('pageerror', e => errors.push(e.message));
    await b.attach(composer);
    await composer.goto(origin + '/me/messages/new?to=' + keyed.fingerprint + '&tier=sealed');
    const form = composer.locator('#new-conversation-form');
    await form.waitFor();
    await composer.waitForFunction(() => document.querySelector('input[name=tier][value=sealed]').checked);
    assert.equal(await form.locator('input[value=public]').count(), 0, 'a public message is not a conversation');
    await form.locator('textarea[name=members]').fill(keyed.fingerprint + '\n' + host.fingerprint);
    await composer.waitForFunction(() => document.querySelector('input[name=tier][value=sealed]').disabled);
    assert.match(await composer.locator('#tier-sealed-reason').textContent(), /grok-7 uses a hosted identity/);
    assert.equal(await form.locator('input[value=private]').isChecked(), true, 'a disabled tier falls back to private');
    await form.locator('textarea[name=members]').fill(keyed.fingerprint);
    await composer.waitForFunction(() => !document.querySelector('input[name=tier][value=sealed]').disabled);

    // The leak hold, in a private conversation.
    b.settings.set(me.fingerprint, {outbound: {leak: 'patterns', hold: true}});
    const room = '~' + 'l'.repeat(26);
    b.open(room, me, [me, keyed]).members.forEach(m => { m.acknowledged = true; });
    await composer.goto(origin + '/me/messages/' + room);
    await composer.locator('#conversation-compose').waitFor({state: 'visible'});
    const token = 'gh' + 'p_' + 'aB3'.repeat(12);
    const text = 'Deploy with ' + token + ' please, and mail jane.doe@example.org';
    const dialog = composer.locator('#leak-hold');
    const send = async choice => {
      await composer.locator('#conversation-text').fill(text);
      await composer.locator('#conversation-compose button[type=submit]').click();
      await dialog.waitFor({state: 'visible'});
      await dialog.locator(`button[value=${choice}]`).click();
      await dialog.waitFor({state: 'hidden'});
    };
    await send('edit');
    await composer.waitForFunction(() => /Not sent/.test(document.getElementById('conversation-status').textContent));
    assert.equal(b.sent('post').length, 0, 'Edit sends nothing');
    assert.equal(await composer.locator('#conversation-text').inputValue(), text, 'the draft is kept');
    await send('redacted');
    await composer.waitForFunction(() => /Sent\./.test(document.getElementById('conversation-status').textContent));
    assert.equal(b.sent('post').at(-1).text, 'Deploy with «REDACTED:github_token» please, and mail «REDACTED:email»');
    await send('send');
    await composer.waitForFunction(n => document.querySelectorAll('.conversation-message').length === n, 2);
    assert.equal(b.sent('post').at(-1).text, text, 'Send anyway sends it as written');
    // The dialog names what it found, never the secret.
    await composer.locator('#conversation-text').fill(text);
    await composer.locator('#conversation-compose button[type=submit]').click();
    await dialog.waitFor({state: 'visible'});
    const findings = await composer.locator('#leak-hold-findings').textContent();
    assert.match(findings, /a GitHub token/); assert.match(findings, /an email address/);
    assert.ok(!findings.includes(token));
    if (shots) await composer.screenshot({path: shots + '/messages-hold-1280.png'});
    await composer.keyboard.press('Escape');
    await dialog.waitFor({state: 'hidden'});
    assert.equal(b.sent('post').length, 2, 'Escape is Edit');

    // Contact details only warn (the list's actions table): sent at once,
    // and the status names what was shared.
    await composer.locator('#conversation-text').fill('mail jane.doe@example.org');
    await composer.locator('#conversation-compose button[type=submit]').click();
    await composer.waitForFunction(() => /Sent\. It shared an email address\./.test(document.getElementById('conversation-status').textContent));
    assert.equal(b.sent('post').length, 3, 'a warn sends');

    // Clean text sends at once; with leak "full", SwarmMemo's check holds too.
    b.settings.set(me.fingerprint, {outbound: {leak: 'full', hold: true}});
    b.overrides.set('service.call', c => ({status: 200, body: {ok: true, data: {result: {verdict: 'hold', findings: [], categories: {credentials: 0.91, private_infrastructure: 0.02}, threshold: 0.6, redacted: 'Our build box is down'}}}}));
    await composer.reload(); await composer.locator('#conversation-compose').waitFor({state: 'visible'});
    await composer.locator('#conversation-text').fill('Our build box is down');
    await composer.locator('#conversation-compose button[type=submit]').click();
    await dialog.waitFor({state: 'visible'});
    assert.match(await composer.locator('#leak-hold-summary').textContent(), /SwarmMemo's check/);
    assert.match(await composer.locator('#leak-hold-findings').textContent(), /credentials 0\.91/);
    const call = JSON.parse(b.sent('service.call').at(-1).data);
    assert.equal(call.method, 'leak'); assert.equal(call.args.mode, 'full'); assert.equal(call.args.audience, 'conversation');
    await dialog.locator('button[value=send]').click();
    await composer.waitForFunction(() => /Sent\./.test(document.getElementById('conversation-status').textContent));
    assert.deepEqual(errors, []);
  } finally { await browser.close(); }
})().catch(error => { console.error(error); process.exit(1); });
