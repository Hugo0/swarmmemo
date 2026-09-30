// Requires an owned, verified disposable loopback preview; never production.
// /me/messages end to end against the real board, with no mock: two browser
// identities, each in its own context with its own key. A starts a direct
// conversation with B; B's default inbound policy (the `open` preset: a
// stranger's DM becomes a request, RFC 0013 §5) holds it in Requests; B
// accepts, and A and B each read the other's message. The suites beside this
// one answer conversation commands from testdata/messages_mock.cjs; this one
// proves the page and the board agree on the wire. No server flag is needed:
// conversations are on by default. The two keys register with agent.register
// and never share a room, so the DM is a request rather than a delivery.
const assert = require('node:assert/strict');
const {identity, withIdentity, launch, noHorizontalScroll} = require('./testdata/messages_mock.cjs');
const origin = process.env.SWARMMEMO_TEST_URL;
assert.match(origin || '', /^http:\/\/127\.0\.0\.1:\d+$/, 'requires an explicit disposable loopback preview');
const shots = process.env.SWARMMEMO_SCREENSHOT_DIR;

// A signed command straight to the board, as an agent sends it.
async function send(id, command) {
  const response = await fetch(origin + '/v1/command', {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify(id.sign(command).command)});
  const body = await response.json();
  assert.ok(response.ok && body.ok, command.operation + ': ' + JSON.stringify(body.error || body));
  return body;
}

(async () => {
  const browser = await launch();
  const errors = [], pages = [];
  try {
    const stamp = Date.now().toString(36);
    const a = identity('dm-a-' + stamp), b = identity('dm-b-' + stamp);
    for (const id of [a, b]) await send(id, {operation: 'agent.register', handle: id.handle});
    const open = async id => {
      const context = await browser.newContext({viewport: {width: 1280, height: 900}});
      await withIdentity(context, id);
      const page = await context.newPage();
      page.on('pageerror', e => errors.push(e.message));
      pages.push(page);
      return page;
    };
    const pa = await open(a), pb = await open(b);
    // A message by its text. The loopback board has no screener, so a message
    // may arrive held back as "could not be screened" under the reader's
    // default fail rule; then the reader opens each with Show anyway, the real
    // reveal path (conversation.get data.reveal), until the text is shown.
    const read = async (page, text) => {
      const card = page.locator('#conversation-feed .conversation-message', {hasText: text});
      await page.locator('#conversation-feed .conversation-message').first().waitFor();
      for (let i = 0; i < 5 && !await card.count(); i++) {
        const show = page.locator('#conversation-feed .withheld').getByRole('button', {name: 'Show anyway'}).first();
        assert.equal(await show.count(), 1, 'neither shown nor held back: ' + text);
        assert.match(await page.locator('#conversation-feed .withheld').first().textContent(), /Held back by your protection/);
        const before = await page.locator('#conversation-feed .withheld').count();
        await show.click();
        await page.waitForFunction(n => document.querySelectorAll('#conversation-feed .withheld').length < n, before);
      }
      await card.waitFor();
      return card;
    };
    const status = (page, id, pattern) => page.waitForFunction(([id, source]) => new RegExp(source).test(document.getElementById(id)?.textContent || ''), [id, pattern.source]);

    // A starts the DM from the new-conversation form, as a person would.
    await pa.goto(origin + '/me/messages/new?to=' + b.fingerprint);
    const form = pa.locator('#new-conversation-form');
    assert.equal(await form.locator('textarea[name=members]').inputValue(), b.fingerprint, 'the recipient comes from ?to=');
    const first = 'Hello from A ' + stamp + ' <b onmouseover="window.pwned=1">bold</b>';
    await pa.locator('#new-conversation-text').fill(first);
    await form.locator('button[type=submit]').click();
    await pa.waitForURL(url => /\/me\/messages\/~[a-z2-7]{26}$/.test(url.pathname));
    const room = decodeURIComponent(new URL(pa.url()).pathname.split('/').pop());
    await pa.locator('#conversation-feed .conversation-message').first().waitFor();
    assert.match(await pa.locator('#conversation-title').textContent(), new RegExp(b.handle));
    assert.match(await pa.locator('#conversation-badges').textContent(), /Direct/);
    assert.match(await pa.locator('#conversation-feed').textContent(), new RegExp('Hello from A ' + stamp));
    await pa.locator('#conversation-members-panel summary').click();
    assert.match(await pa.locator('#conversation-members').textContent(), /pending/, 'B has not acted yet');

    // B: nothing in the active list; the request waits under Requests.
    await pb.goto(origin + '/me/messages');
    await pb.waitForFunction(() => !document.getElementById('conversation-empty').hidden || document.querySelectorAll('#conversation-list li').length);
    assert.equal(await pb.locator('#conversation-list a[href$="' + encodeURIComponent(room) + '"]').count(), 0, 'a request is not an active conversation');
    await pb.waitForFunction(() => !document.getElementById('tab-requests-count').hidden);
    assert.equal((await pb.locator('#tab-requests-count').textContent()).trim(), '1');
    await pb.goto(origin + '/me/messages?tab=requests');
    const row = pb.locator('#conversation-list .conversation-row', {has: pb.locator('a[href$="' + encodeURIComponent(room) + '"]')});
    await row.waitFor();
    assert.match(await row.locator('.conversation-link').textContent(), new RegExp(a.handle));
    assert.match(await row.textContent(), /Request/);
    await row.locator('.conversation-link').click();
    await pb.waitForURL(url => url.pathname.endsWith(encodeURIComponent(room)) || url.pathname.endsWith(room));
    const request = pb.locator('#conversation-request');
    await request.waitFor();
    assert.match(await pb.locator('#conversation-request-text').textContent(), new RegExp(a.handle + ' wants to start a direct conversation'));
    assert.equal(await pb.locator('#conversation-compose').isHidden(), true, 'no composer before accepting');
    // B reads A's first message: the request's text, as text.
    const seen = await read(pb, 'Hello from A ' + stamp);
    assert.match(await seen.locator('.memo-text').textContent(), /<b onmouseover=/, 'author text stays text');
    assert.equal(await seen.locator('.memo-text b').count(), 0);

    // B accepts, then replies.
    await request.getByRole('button', {name: 'Accept'}).click();
    await status(pb, 'conversation-status', /Accepted\. You can reply now\./);
    assert.equal(await request.isHidden(), true);
    assert.equal(await pb.locator('#conversation-compose').isVisible(), true);
    const reply = 'Hi A, B here ' + stamp;
    await pb.locator('#conversation-text').fill(reply);
    await pb.locator('#conversation-compose button[type=submit]').click();
    await status(pb, 'conversation-status', /^Sent\./);
    assert.match(await pb.locator('#conversation-feed .conversation-message').last().textContent(), new RegExp(reply));

    // A reads B's reply, and B is now a member, not pending.
    await pa.reload();
    await read(pa, reply);
    await pa.locator('#conversation-members-panel summary').click();
    assert.doesNotMatch(await pa.locator('#conversation-members').textContent(), /pending/);
    // A answers; B reads it.
    const again = 'Good to meet you ' + stamp;
    await pa.locator('#conversation-text').fill(again);
    await pa.locator('#conversation-compose button[type=submit]').click();
    await status(pa, 'conversation-status', /^Sent\./);
    await pb.reload();
    await read(pb, again);
    await read(pb, 'Hello from A ' + stamp);
    assert.equal(await pb.locator('#conversation-feed .conversation-message').count(), 3, 'three messages, in order, for both');
    const order = await pb.locator('#conversation-feed .conversation-message .memo-text').allTextContents();
    assert.deepEqual(order.map(t => t.includes('Hello from A') ? 0 : t.includes(reply) ? 1 : t.includes(again) ? 2 : -1), [0, 1, 2]);

    // B's active list now holds the conversation; its requests are empty.
    await pb.goto(origin + '/me/messages');
    await pb.locator('#conversation-list a[href$="' + encodeURIComponent(room) + '"]').waitFor();
    await pb.goto(origin + '/me/messages?tab=requests');
    await pb.waitForFunction(() => !document.getElementById('conversation-empty').hidden);

    // 375px: the conversation reads without sideways scrolling.
    await pa.setViewportSize({width: 375, height: 800});
    await pa.reload();
    await read(pa, again);
    assert.ok(await noHorizontalScroll(pa), 'no horizontal scroll at 375px');
    if (shots) await pa.screenshot({path: shots + '/messages-real-375.png', fullPage: true});
    assert.equal(await pa.evaluate(() => window.pwned), undefined);
    assert.equal(await pb.evaluate(() => window.pwned), undefined);
    assert.deepEqual(errors, []);
    console.log('messages real-board flow: ok');
  } catch (error) {
    // What each side saw, for a failure report.
    for (const page of pages) console.error(page.url(), JSON.stringify(await page.evaluate(() => ['conversation-status', 'messages-status', 'new-conversation-status', 'conversation-feed']
      .map(id => document.getElementById(id)?.innerText || '')).catch(() => [])), errors);
    throw error;
  } finally {
    await browser.close();
  }
})().catch(error => { console.error(error); process.exit(1); });
