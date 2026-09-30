// Requires an owned, verified disposable loopback preview; never production.
// /me/messages, the list: tabs with unread and request badges, rows with the
// sealed lock, the hosted-key badge and a text-only preview, the empty state,
// the first-open protection choice (§5.1), and 375px with no horizontal
// scroll. Conversation operations are answered by testdata/messages_mock.cjs
// from state the test controls; the page is real.
const assert = require('node:assert/strict');
const {identity, board, withIdentity, launch, noHorizontalScroll} = require('./testdata/messages_mock.cjs');
const origin = process.env.SWARMMEMO_TEST_URL;
assert.match(origin || '', /^http:\/\/127\.0\.0\.1:\d+$/, 'requires an explicit disposable loopback preview');
const shots = process.env.SWARMMEMO_SCREENSHOT_DIR;

(async () => {
  const browser = await launch();
  try {
    const me = identity('me'), ada = identity('ada'), bo = identity('bo'), host = identity('grok-7'), stranger = identity('stranger');
    const b = board();
    for (const id of [me, ada, bo, stranger]) b.addAgent(id);
    b.addAgent(host, {custody: 'hosted'});
    const dm = b.open('~' + 'a'.repeat(26), ada, [ada, me]);
    b.message(dm.room, ada, 'Hi <img src=x onerror="window.pwned=1"> are you there?');
    dm.unread = 3;
    const group = b.open('~' + 'b'.repeat(26), me, [me, ada, bo], {sealed: true});
    b.message(group.room, ada, 'sealed1.1.AAAAAAAAAAAAAAAA.AAAAAAAAAAAAAAAAAAAAAA', {format: 'sealed', sealed: true});
    const hosted = b.open('~' + 'c'.repeat(26), host, [host, me]);
    b.message(hosted.room, host, 'Hello from a hosted assistant');
    for (const [i, from] of [stranger, bo].entries()) {
      const request = b.open('~' + 'defg'[i].repeat(26), from, [from, me]);
      request.members[1].state = 'requested';
      b.message(request.room, from, 'May I ask you something?');
    }
    const context = await browser.newContext({viewport: {width: 1280, height: 900}});
    await withIdentity(context, me);
    const page = await context.newPage(); const errors = []; page.on('pageerror', e => errors.push(e.message));
    await b.attach(page);
    await page.goto(origin + '/me/messages');
    await page.locator('#conversation-list li').first().waitFor();
    assert.equal(await page.locator('#conversation-list li').count(), 3, 'three active conversations');
    assert.equal(await page.locator('.message-tabs a[aria-current="page"]').textContent().then(t => t.trim().startsWith('Active')), true);
    // Tab badges: one conversation with unread messages, two requests.
    await page.waitForFunction(() => !document.getElementById('tab-requests-count').hidden);
    assert.equal(await page.locator('#tab-active-count').textContent(), '1');
    assert.equal(await page.locator('#tab-requests-count').textContent(), '2');
    const rows = page.locator('#conversation-list li');
    const dmRow = rows.filter({hasText: 'ada'}).filter({hasText: 'Direct'});
    assert.equal(await dmRow.locator('.count-badge').textContent(), '3');
    assert.equal(await dmRow.locator('.count-badge').getAttribute('aria-label'), '3 unread');
    // Author text is text: the preview's markup is shown, never parsed.
    assert.match(await dmRow.locator('.conversation-preview').textContent(), /<img src=x onerror=/);
    assert.equal(await page.locator('#conversation-list img').count(), 0);
    assert.equal(await page.evaluate(() => window.pwned), undefined);
    const sealedRow = rows.filter({hasText: 'Group'});
    assert.equal(await sealedRow.locator('.sealed-badge').count(), 1, 'a sealed conversation shows the lock');
    assert.match(await sealedRow.locator('.conversation-preview').textContent(), /Sealed message/);
    assert.equal(await rows.filter({hasText: 'grok-7'}).locator('.badge', {hasText: 'hosted key'}).count(), 1, 'a hosted member shows its custody');
    assert.equal(await rows.filter({hasText: 'grok-7'}).locator('.badge[title*="SwarmMemo holds this agent"]').count(), 1);
    assert.equal(await page.locator('a.conversation-link').first().getAttribute('href'), '/me/messages/~' + 'a'.repeat(26));
    // First open: the browser identity screens on the server, and says so.
    await page.locator('.notice', {hasText: 'screened on the SwarmMemo server'}).waitFor();
    const policy = b.sent('messaging.policy.set');
    assert.equal(policy.length, 1);
    assert.deepEqual(JSON.parse(policy[0].data).inbound, {mode: 'server', threshold: 0.6, fail: 'closed'});
    await page.reload(); await page.locator('#conversation-list li').first().waitFor();
    assert.equal(b.sent('messaging.policy.set').length, 1, 'the choice is offered once');
    assert.equal(await page.locator('.notice', {hasText: 'screened on the SwarmMemo server'}).count(), 0);
    if (shots) await page.screenshot({path: shots + '/messages-list-1280.png', fullPage: true});

    // Requests tab, then Left (empty).
    await page.locator('.message-tabs a', {hasText: 'Requests'}).click();
    await page.locator('#conversation-list li').first().waitFor();
    assert.equal(await page.locator('#conversation-list li').count(), 2);
    assert.equal(await page.locator('#conversation-list .badge', {hasText: 'Request'}).count(), 2);
    await page.locator('.message-tabs a', {hasText: 'Left'}).click();
    await page.locator('#conversation-empty').waitFor({state: 'visible'});
    assert.match(await page.locator('#conversation-empty').textContent(), /not left any conversation/);

    // 375px: everything reachable, nothing scrolls sideways.
    await page.setViewportSize({width: 375, height: 800});
    await page.goto(origin + '/me/messages');
    await page.locator('#conversation-list li').first().waitFor();
    assert.ok(await noHorizontalScroll(page), 'no horizontal scroll at 375px');
    if (shots) await page.screenshot({path: shots + '/messages-list-375.png', fullPage: true});

    // Without a key, the page says how to get one and reads nothing.
    const bare = await (await browser.newContext()).newPage();
    const before = b.log.length; await b.attach(bare);
    await bare.goto(origin + '/me/messages');
    await bare.locator('#messages-readiness a[href="/me"]').waitFor();
    assert.equal(await bare.locator('#messages-app').isHidden(), true);
    assert.equal(b.log.length, before, 'no key, no signed reads');
    assert.deepEqual(errors, []);
  } finally { await browser.close(); }
})().catch(error => { console.error(error); process.exit(1); });
