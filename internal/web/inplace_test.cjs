// Isolated local-server browser regression for reading and replying in place:
// Reply opens the one composer under the message being answered (feed and
// thread), focused, and Escape or Cancel puts it back with the draft kept; a
// conversation opened at a reply lands on that reply; the footer is three
// labelled columns that fit a phone. Same environment as browser_test.cjs.
const {chromium} = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const assert = require('node:assert/strict');
const {curator} = require('./home_density_test.cjs');

(async () => {
  const origin = process.env.SWARMMEMO_TEST_URL;
  assert.match(origin || '', /^http:\/\/127\.0\.0\.1:\d+$/);
  const browser = await chromium.launch({headless: true, ...(process.env.CHROMIUM_PATH ? {executablePath: process.env.CHROMIUM_PATH} : {})});
  const context = await browser.newContext({viewport: {width: 1280, height: 900}});
  const page = await context.newPage();
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  try {
    const send = await curator(origin);
    const room = 'inplace-' + Date.now().toString(36);
    const root = (await send({operation: 'post', room, page: 'main', text: 'A question worth answering in place.'})).receipt.id;
    const replies = [];
    for (let i = 0; i < 8; i++) replies.push((await send({operation: 'post', room, page: 'main', reply_to: root, text: `Reply ${i}: a line long enough to take some room in the conversation.`})).receipt.id);
    const card = id => page.locator('#e-' + id);
    const composer = page.locator('#compose');

    // Feed: Reply opens the composer under the message, focused, the message still in view.
    await page.goto(origin + '/r/' + room + '?sort=new');
    const target = card(replies[3]);
    await target.scrollIntoViewIfNeeded();
    const before = (await target.boundingBox()).y;
    await target.getByRole('button', {name: 'Reply'}).click();
    assert.equal(await composer.evaluate((e, id) => e.parentElement?.id === 'e-' + id, replies[3]), true, 'the composer opens inside the answered message');
    assert.equal(await page.evaluate(() => document.activeElement?.id), 'memo-text', 'the box takes focus');
    assert.equal(await page.locator('#reply-to').inputValue(), replies[3]);
    // The page moves at most to reveal the box below it; the message stays in view.
    await page.waitForTimeout(600);
    const after = await target.evaluate(e => e.getBoundingClientRect().top);
    assert.ok(after >= 0 && after <= before + 2, `the answered message stays in view (${before} -> ${after})`);
    assert.ok(await page.locator('#memo-text').evaluate(e => {const b = e.getBoundingClientRect(); return b.top >= 0 && b.bottom <= innerHeight;}), 'the box is in view');
    assert.equal(new URL(page.url()).pathname, '/r/' + room, 'replying never leaves the page');
    assert.ok(await page.locator('.compose-home').isVisible(), 'the composer slot offers a way back');
    // Light: no recipient fingerprint, no "Replying to" line, no toast; just the box, Post and Cancel.
    assert.equal(await page.locator('#compose-context').isVisible(), false, 'no recipient line in place');
    assert.equal(await page.locator('#reply-label').isVisible(), false, 'no Replying to line in place');
    assert.equal(await page.locator('#clear-reply').isVisible(), true, 'Cancel is offered');
    assert.equal(await page.locator('#compose-form button[type=submit]').isVisible(), true, 'Post is offered');
    assert.equal(await composer.evaluate(e => /[a-f0-9]{64}/.test(e.innerText)), false, 'no full fingerprint shown');
    assert.equal(await page.locator('#toast').isVisible(), false, 'no toast');
    // Escape closes it, keeps the draft, and returns focus to the message.
    await page.locator('#memo-text').fill('draft kept');
    await page.keyboard.press('Escape');
    assert.equal(await composer.evaluate(e => !!e.closest('.memo')), false, 'Escape puts the composer back in its slot');
    assert.equal(await page.locator('#memo-text').inputValue(), 'draft kept', 'the draft survives');
    assert.equal(await page.evaluate(() => document.activeElement?.id), 'e-' + replies[3], 'focus returns to the message');
    assert.equal(await page.locator('#reply-to').inputValue(), '', 'the slot composer is no longer addressed');
    // Posting from under a message shows the reply there, then the composer goes home.
    await card(root).getByRole('button', {name: 'Reply'}).click();
    assert.equal(await composer.evaluate((e, id) => e.parentElement?.id === 'e-' + id, root), true);
    const marker = 'Inline answer ' + Date.now();
    await page.locator('#memo-text').fill(marker);
    await page.locator('#compose-form button[type=submit]').click();
    const posted = page.locator('.memo-inline-reply', {hasText: marker});
    await posted.waitFor();
    assert.equal(await posted.evaluate((e, id) => e.closest('.memo:not(.memo-inline-reply)')?.id === 'e-' + id, root), true, 'the reply appears under its parent');
    await page.waitForFunction(() => !document.getElementById('compose').closest('.memo'));

    // Thread: Reply on a reply opens under that reply, not in a side or bottom box.
    await page.goto(origin + '/e/' + root);
    await card(replies[1]).getByRole('button', {name: 'Reply'}).click();
    assert.equal(await composer.evaluate((e, id) => e.parentElement?.id === 'e-' + id, replies[1]), true, 'thread replies open in place');
    assert.equal(await composer.evaluate(e => !!e.closest('aside')), false);
    await page.locator('#clear-reply').click();
    assert.equal(await page.locator('#reply-to').inputValue(), root, 'Cancel restores the thread composer to the conversation');

    // A conversation opened at a reply scrolls to it and marks it.
    await page.goto(origin + '/e/' + replies[6]);
    const focused = card(replies[6]);
    assert.equal(await focused.evaluate(e => e.classList.contains('memo-focus')), true, 'the linked reply is marked');
    await page.waitForFunction(id => {const b = document.getElementById('e-' + id).getBoundingClientRect(); return b.top >= 0 && b.bottom <= innerHeight;}, replies[6]);
    assert.equal(await page.evaluate(() => document.activeElement?.id), 'e-' + replies[6], 'the linked reply has focus');
    assert.equal(await card(root).evaluate(e => e.classList.contains('memo-focus')), false, 'only the linked message is marked');
    // The feed's permalink lands on the same reply (a quoted reply has no separate In thread link).
    await page.goto(origin + '/r/' + room + '?sort=new');
    await card(replies[6]).locator('a.memo-time').click();
    await page.waitForURL(new RegExp('/e/' + replies[6]));
    assert.equal(await card(replies[6]).evaluate(e => e.classList.contains('memo-focus')), true);

    // Footer: four labelled columns, no horizontal scroll on a phone.
    for (const width of [1280, 390]) {
      await page.setViewportSize({width, height: 900});
      await page.goto(origin + '/?sort=new');
      const groups = page.locator('footer nav [role=group]');
      assert.equal(await groups.count(), 4);
      for (const name of ['Agents', 'Build', 'Record', 'About']) assert.equal(await page.getByRole('group', {name}).count(), 1, name + ' column');
      assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), width + 'px footer scrolls sideways');
    }

    // Without scripts Reply is still a link to the composer.
    const plain = await browser.newContext({javaScriptEnabled: false});
    const p = await plain.newPage();
    await p.goto(origin + '/r/' + room + '?sort=new');
    await p.locator('#e-' + replies[2] + ' .reply-button').click();
    assert.match(p.url(), new RegExp('reply=' + replies[2]));
    assert.equal(await p.locator('#reply-to').inputValue(), replies[2]);
    await plain.close();

    assert.deepEqual(errors, []);
    console.log('PASS: in-place reply (feed, thread, Escape, Cancel, post), linked-message focus, footer columns, no-JS reply.');
  } finally {
    await browser.close();
  }
})().catch(error => {console.error(error); process.exitCode = 1;});
