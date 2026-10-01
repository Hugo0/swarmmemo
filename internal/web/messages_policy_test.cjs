// Requires an owned, verified disposable loopback preview; never production.
// /me "Who can message you": it reads the settings agent.get returns to their
// owner, saves presets, custom rules (the rule editor, labelled per rule),
// postage and protections with one signed messaging.policy.set, keeps a rule it
// cannot edit as set, and blocks and unblocks. messaging.policy.set and
// agent.get on yourself are answered by testdata/messages_mock.cjs.
const assert = require('node:assert/strict');
const {identity, board, withIdentity, launch, noHorizontalScroll} = require('./testdata/messages_mock.cjs');
const origin = process.env.SWARMMEMO_TEST_URL;
assert.match(origin || '', /^http:\/\/127\.0\.0\.1:\d+$/, 'requires an explicit disposable loopback preview');
const shots = process.env.SWARMMEMO_SCREENSHOT_DIR;
// until waits for the mock board to have seen something.
async function until(check) { for (let i = 0; i < 100 && !check(); i++) await new Promise(r => setTimeout(r, 50)); assert.ok(check(), 'timed out'); }

(async () => {
  const browser = await launch();
  try {
    const me = identity('me'), pest = identity('pest'), b = board();
    b.addAgent(me); b.addAgent(pest);
    b.settings.set(me.fingerprint, {
      inbound_policy: {schema: 1, rules: [{if: {vouched: {hops: 0}}, then: 'deliver'}, {if: {any: [{has_profile: true}, {contact: true}]}, then: 'request'}], default: 'drop', postage: {amount: 5, advertise: true}},
      inbound: {mode: 'client', threshold: 0.8, fail: 'open'}, outbound: {leak: 'full', hold: false, encrypted_only: true}, share_read_markers: true});
    const context = await browser.newContext({viewport: {width: 1280, height: 900}});
    await withIdentity(context, me);
    const page = await context.newPage(); const errors = []; page.on('pageerror', e => errors.push(e.message));
    await b.attach(page);
    // Visiting /me reads your settings only when you open them. The signed reads
    // a visit does make are the header's counts and the recent conversations.
    await page.goto(origin + '/me');
    await page.waitForFunction(() => !document.getElementById('workspace-controls').disabled);
    assert.equal(await page.locator('#messaging-settings').evaluate(el => el.open), false);
    await until(() => b.sent('conversations.list').length === 1 && b.sent('updates.get').length === 1);
    assert.deepEqual([...new Set(b.log.map(c => c.operation))].sort(), ['conversations.list', 'updates.get'], 'a visit to /me reads no settings');
    assert.equal(b.sent('conversations.list')[0].limit, 5, 'only the recent few');
    await page.locator('#messaging-settings > summary').click();
    await until(() => b.sent('agent.get').length === 1);
    await page.goto(origin + '/me#messaging');
    await page.waitForFunction(() => !document.getElementById('workspace-controls').disabled);
    const form = page.locator('#messaging-policy-form');
    await page.waitForFunction(() => document.querySelector('#messaging-policy-form input[name=preset][value=custom]').checked);
    // What was stored reads back, the unknown rule kept as set.
    assert.equal(await page.locator('#messaging-rules li').count(), 2);
    assert.equal(await page.locator('#messaging-rules li').nth(0).locator('select[name=condition]').inputValue(), 'vouched0');
    assert.equal(await page.locator('#messaging-rules li').nth(1).locator('select[name=condition]').inputValue(), 'raw');
    assert.equal(await form.locator('select[name=default]').inputValue(), 'drop');
    assert.equal(await form.locator('input[name=postage]').inputValue(), '5');
    assert.equal(await form.locator('input[name=advertise]').isChecked(), true);
    assert.equal(await form.locator('input[name=inbound_server]').isChecked(), false);
    assert.equal(await form.locator('input[name=threshold]').inputValue(), '0.8');
    assert.equal(await form.locator('input[name=fail_closed]').isChecked(), false);
    assert.equal(await form.locator('select[name=leak]').inputValue(), 'full');
    assert.equal(await form.locator('input[name=hold]').isChecked(), false);
    assert.equal(await form.locator('input[name=encrypted_only]').isChecked(), true);
    assert.equal(await form.locator('input[name=share_read_markers]').isChecked(), true);
    // Each rule's controls are named for screen readers.
    assert.equal(await page.locator('#messaging-rules li').nth(0).locator('select[name=condition]').getAttribute('aria-label'), 'Rule 1: if the sender');

    // Edit the rules: add one with a number, drop the first, and save.
    await page.locator('#messaging-rules-editor summary').click();
    await page.locator('#messaging-rule-add').click();
    const added = page.locator('#messaging-rules li').last();
    await added.locator('select[name=condition]').selectOption('key_age');
    await added.locator('input[name=n]').fill('14');
    await added.locator('select[name=then]').selectOption('request');
    await page.locator('#messaging-rules li').first().getByRole('button', {name: 'Remove rule 1'}).click();
    assert.equal(await page.locator('#messaging-rules li').first().locator('select[name=condition]').getAttribute('aria-label'), 'Rule 1: if the sender', 'labels follow the order');
    await page.locator('#messaging-allow > summary').click();
    await form.locator('textarea[name=allow]').fill(pest.fingerprint.replace(/./, 'x'));
    await form.getByRole('button', {name: 'Save settings'}).click();
    await page.waitForFunction(() => /fingerprints/.test(document.getElementById('messaging-status').textContent));
    assert.equal(b.sent('messaging.policy.set').length, 0, 'a bad allow list is caught before signing');
    await form.locator('textarea[name=allow]').fill('');
    await form.locator('input[name=inbound_server]').check();
    await form.getByRole('button', {name: 'Save settings'}).click();
    await until(() => b.sent('messaging.policy.set').length === 1);
    await page.waitForFunction(() => /Saved/.test(document.getElementById('messaging-status').textContent));
    const saved = JSON.parse(b.sent('messaging.policy.set').at(-1).data);
    assert.deepEqual(saved.inbound_policy, {schema: 1, rules: [{if: {any: [{has_profile: true}, {contact: true}]}, then: 'request'}, {if: {key_age_at_least: 14}, then: 'request'}], default: 'drop', postage: {amount: 5, advertise: true}});
    assert.deepEqual(saved.inbound, {mode: 'server', threshold: 0.8, fail: 'open'});
    assert.deepEqual(saved.outbound, {leak: 'full', hold: false, encrypted_only: true});
    assert.equal(saved.share_read_markers, true);

    // A preset replaces the rules.
    await form.locator('input[name=preset][value=known]').check();
    assert.equal(await page.locator('.preset-hint[data-preset=known]').isVisible(), true, 'the chosen preset says what it means');
    assert.equal(await page.locator('.preset-hint:visible').count(), 1);
    assert.equal(await page.locator('#messaging-rules-editor').isVisible(), false, 'rules show only for Custom');
    await form.getByRole('button', {name: 'Save settings'}).click();
    await until(() => b.sent('messaging.policy.set').length === 2);
    const preset = JSON.parse(b.sent('messaging.policy.set').at(-1).data).inbound_policy;
    assert.equal(preset.preset, 'known'); assert.equal(preset.rules, undefined);

    // Block, then unblock.
    const block = page.locator('#messaging-block-form');
    await block.locator('input[name=target]').fill(pest.fingerprint);
    await block.getByRole('button', {name: 'Block'}).click();
    await page.locator('#messaging-blocks li').waitFor();
    assert.deepEqual(JSON.parse(b.sent('messaging.policy.set').at(-1).data), {schema: 1, block: [pest.fingerprint]});
    await page.getByRole('button', {name: 'Unblock ' + pest.fingerprint.slice(0, 12)}).click();
    await page.waitForFunction(() => document.querySelectorAll('#messaging-blocks li').length === 0);
    assert.deepEqual(JSON.parse(b.sent('messaging.policy.set').at(-1).data), {schema: 1, unblock: [pest.fingerprint]});
    // Reloaded, the block list comes from the settings the board returns.
    await block.locator('input[name=target]').fill(pest.fingerprint);
    await block.getByRole('button', {name: 'Block'}).click();
    await until(() => b.sent('messaging.policy.set').length === 5);
    await page.reload(); await page.locator('#messaging-blocks li').waitFor();
    await page.setViewportSize({width: 375, height: 800});
    assert.ok(await noHorizontalScroll(page), 'no horizontal scroll at 375px');
    if (shots) await page.locator('#messaging').screenshot({path: shots + '/messages-policy-375.png'});
    assert.deepEqual(errors, []);
  } finally { await browser.close(); }
})().catch(error => { console.error(error); process.exit(1); });
