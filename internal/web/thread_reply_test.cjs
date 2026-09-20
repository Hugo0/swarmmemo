// Replying is the same act everywhere: one composer, in the column the reader is
// already reading, and the reply appears where it belongs rather than at the top of
// a feed. A thread page used to have no composer at all and sent the reader back to
// the room, where the box sat in a sidebar.
const {chromium} = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const assert = require('node:assert/strict');
const {curator} = require('./home_density_test.cjs');

(async () => {
  const browser = await chromium.launch({headless: true, ...(process.env.CHROMIUM_PATH ? {executablePath: process.env.CHROMIUM_PATH} : {})});
  const origin = process.env.SWARMMEMO_TEST_URL || 'http://127.0.0.1:8089';
  const context = await browser.newContext({viewport: {width: 1280, height: 900}});
  const page = await context.newPage();
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  try {
    const send = await curator(origin);
    const root = (await send({operation: 'post', room: 'thread-ui', page: 'main', text: 'Opening a conversation to reply into.'})).receipt.id;
    for (let i = 0; i < 6; i++) {
      await send({operation: 'post', room: 'thread-ui', page: 'main', text: `Filler reply ${i}, so the thread is taller than the viewport.`, reply_to: root});
    }

    // The composer lives in the thread, not in a sidebar, and is pre-addressed.
    await page.goto(origin + '/e/' + root);
    const composer = page.locator('#compose');
    await composer.waitFor();
    assert.equal(await composer.evaluate(e => !!e.closest('aside')), false, 'the thread composer must not be in a sidebar');
    assert.equal(await composer.evaluate(e => !!e.closest('.reading-width')), true, 'the composer belongs in the reading column');
    assert.equal(await page.locator('#reply-to').inputValue(), root, 'a thread composer answers the message being read');
    assert.equal(await page.getByRole('link', {name: /Reply in this room/}).count(), 0, 'replying must not send the reader away');

    // Posting a reply puts it in the thread and brings it into view, rather than
    // leaving the reader where they were.
    const marker = 'Reply written from inside the thread ' + Date.now();
    await page.locator('#memo-text').fill(marker);
    await page.locator('#compose-form button[type=submit]').click();
    const posted = page.locator('.memo', {hasText: marker});
    await posted.waitFor();
    assert.equal(await posted.evaluate(e => !!e.closest('#thread')), true, 'the reply belongs in the conversation');
    // Smooth scrolling settles asynchronously; wait for it rather than sampling once.
    await page.waitForFunction(text => {
      const el = [...document.querySelectorAll('.memo')].find(m => m.textContent.includes(text));
      if (!el) return false;
      const box = el.getBoundingClientRect();
      return box.top > -1 && box.bottom < innerHeight + 1;
    }, marker, {timeout: 5000}).catch(() => {throw new Error('the new reply was never scrolled into view');});

    // One placement everywhere: the composer sits in the reading column, below the
    // feed, never in a sidebar. Below matters — relocating it under a message must
    // not remove height from above the reader, which is what made the page jump.
    for (const path of ['/', '/r/thread-ui']) {
      await page.goto(origin + path);
      await page.locator('#compose').waitFor();
      assert.equal(await page.locator('#compose').evaluate(e => !!e.closest('aside')), false, `${path}: composer must not sit in a sidebar`);
      assert.equal(await page.locator('#compose').evaluate(e => {
        const feed = document.getElementById('feed');
        return feed ? !!(feed.compareDocumentPosition(e) & Node.DOCUMENT_POSITION_FOLLOWING) : false;
      }), true, `${path}: composer belongs below the feed`);
      assert.equal(await page.locator('#compose-cta').count(), 1, `${path}: posting stays reachable from the top`);
      assert.equal(await page.locator('#compose-settings').evaluate(e => e.open), false, `${path}: options stay closed`);
      // The action is reachable without scrolling past the options.
      assert.equal(await page.evaluate(() => {
        const button = document.querySelector('#compose-form button[type=submit]');
        const options = document.getElementById('compose-settings');
        return !!(options.compareDocumentPosition(button) & Node.DOCUMENT_POSITION_PRECEDING);
      }), true, `${path}: the post button comes before the options`);
    }

    // Posting folds the composer back and marks what landed.
    await page.goto(origin + '/r/thread-ui');
    await page.locator('#compose-cta').click();
    assert.equal(await page.locator('#compose').evaluate(e => e.open), true, 'the invitation opens the composer');
    const second = 'A second message ' + Date.now();
    await page.locator('#memo-text').fill(second);
    await page.locator('#compose-form button[type=submit]').click();
    await page.locator('.memo', {hasText: second}).waitFor();
    // The composer stays open and cleared, with the receipt beside it: folding meant
    // every following action began by reopening a panel.
    assert.equal(await page.locator('#memo-text').inputValue(), '', 'the composer is cleared for the next message');
    assert.match(await page.locator('#compose-status').textContent(), /Posted/, 'the receipt confirms with a mark');
    assert.equal(await page.locator('#compose-status').isVisible(), true, 'the receipt is readable, not folded away');

    // Without JavaScript the thread still offers a working reply form.
    const plain = await browser.newContext({javaScriptEnabled: false});
    const plainPage = await plain.newPage();
    await plainPage.goto(origin + '/e/' + root);
    assert.equal(await plainPage.locator('#compose-form').count(), 1, 'no-JS readers need the form too');
    assert.match(await plainPage.locator('#compose-form').getAttribute('action'), /^\/w\/thread-ui\//);
    await plain.close();

    // A pasted image attaches itself and shows what will be sent.
    await page.goto(origin + '/');
    await page.locator('#memo-text').waitFor();
    await page.evaluate(() => {
      const input = document.getElementById('memo-files');
      input.disabled = false;  // normally enabled once a signing identity is remembered
    });
    await page.locator('#memo-text').focus();
    await page.evaluate(() => {
      // A 1x1 PNG, pasted the way a screenshot arrives.
      const bytes = Uint8Array.from(atob('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=='), c => c.charCodeAt(0));
      const file = new File([bytes], 'screenshot.png', {type: 'image/png'});
      const data = new DataTransfer(); data.items.add(file);
      document.getElementById('memo-text').dispatchEvent(new ClipboardEvent('paste', {clipboardData: data, bubbles: true, cancelable: true}));
    });
    const strip = page.locator('#compose-attachments');
    await strip.locator('.compose-attachment').first().waitFor();
    assert.equal(await strip.locator('img').count(), 1, 'a pasted image previews as a thumbnail');
    assert.match(await strip.locator('.compose-attachment-name').textContent(), /screenshot\.png/);
    assert.equal(await page.locator('#memo-files').evaluate(i => i.files.length), 1, 'the paste reaches the file input the uploader reads');
    await strip.getByRole('button', {name: /Remove/}).click();
    assert.equal(await page.locator('#memo-files').evaluate(i => i.files.length), 0, 'removing an attachment detaches it');
    assert.equal(await strip.isHidden(), true, 'an empty strip stays out of the way');

    assert.deepEqual(errors, []);
    console.log('PASS: one composer in the reading column on thread, room and home; replies land in the thread, in view; pasted image previews and detaches; no-JS form intact.');
  } finally {
    await browser.close();
  }
})().catch(error => {console.error(error); process.exitCode = 1;});
