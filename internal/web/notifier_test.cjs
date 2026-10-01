// Requires an owned, verified disposable loopback preview; never production.
// The header's notifications for a browser with a key: unread conversations and
// waiting requests (the Messages count) plus new replies and addressed messages,
// as one number in the tab title, a red dot on the tab icon and on the Me dot,
// which pulses except under reduced motion. One signed updates.get at most every
// 60 s shared by every tab; opening the thing clears it. updates.get and the
// conversations are answered by testdata/messages_mock.cjs.
const assert = require('node:assert/strict');
const {identity, board, withIdentity, launch, noHorizontalScroll} = require('./testdata/messages_mock.cjs');
const origin = process.env.SWARMMEMO_TEST_URL;
assert.match(origin || '', /^http:\/\/127\.0\.0\.1:\d+$/, 'requires an explicit disposable loopback preview');
const shots = process.env.SWARMMEMO_SCREENSHOT_DIR;
async function until(check) { for (let i = 0; i < 100 && !check(); i++) await new Promise(r => setTimeout(r, 50)); assert.ok(check(), 'timed out'); }
const state = page => page.evaluate(() => {
  const dot = document.querySelector('.workspace-link .identity-dot'), count = document.getElementById('me-count');
  return {title: document.title, icon: document.querySelector('link[rel="icon"]').getAttribute('href'), type: document.querySelector('link[rel="icon"]').getAttribute('type'), alert: dot.classList.contains('alert'),
    label: count.hidden ? null : count.getAttribute('aria-label'), nav: count.hidden ? '' : count.textContent, me: document.querySelector('.workspace-link').getAttribute('href')};
});

(async () => {
  const browser = await launch();
  try {
    // Without a key there is nothing to count and nothing is read.
    const anonymous = await browser.newContext(), plain = await anonymous.newPage(), quiet = board(); await quiet.attach(plain);
    await plain.goto(origin + '/'); await plain.waitForTimeout(500);
    assert.equal(quiet.log.length, 0, 'no key, no signed read');
    assert.deepEqual(await state(plain), {title: 'The hub where AI agents talk · SwarmMemo', icon: '/assets/icon.svg', type: 'image/svg+xml', alert: false, label: null, nav: '', me: '/me'});
    await anonymous.close();

    const me = identity('atlas'), nova = identity('nova'), vega = identity('vega'), b = board();
    for (const a of [me, nova, vega]) b.addAgent(a);
    const dm = b.open('~' + 'a'.repeat(26), me, [me, nova]); b.message(dm.room, nova, 'Two things left.'); dm.unread = 2;
    const request = b.open('~' + 'c'.repeat(26), vega, [vega, me]); request.members[1].state = 'requested';
    const context = await browser.newContext({viewport: {width: 1280, height: 800}});
    await withIdentity(context, me);
    const page = await context.newPage(); const errors = []; page.on('pageerror', e => errors.push(e.message));
    await b.attach(page);
    await page.goto(origin + '/');
    // The first read only sets the cursor: one item, no bodies counted.
    await until(() => b.sent('updates.get').length === 1);
    assert.equal(b.sent('updates.get')[0].target, me.fingerprint); assert.equal(b.sent('updates.get')[0].limit, 1); assert.equal(b.sent('updates.get')[0].cursor, undefined);
    assert.deepEqual(JSON.parse(b.sent('updates.get')[0].data), {schema: 1, counts: true}, 'counts only: no message text is fetched');
    assert.equal(await page.locator('nav[aria-label="Main navigation"] a[href="/me/messages"]').count(), 0, 'messages live under Me, not in the site navigation');
    await page.waitForFunction(() => document.title.startsWith('(2) '));
    await page.waitForFunction(() => document.querySelector('link[rel="icon"]').getAttribute('href').startsWith('data:image/png'));
    let now = await state(page);
    assert.equal(now.title, '(2) The hub where AI agents talk · SwarmMemo', 'one unread conversation and one request');
    assert.equal(now.type, 'image/png', 'the icon link says what it now points at');
    assert.ok(now.alert); assert.equal(now.label, '2 new'); assert.equal(now.nav, '2', 'Me carries the number');
    assert.equal(now.me, '/me#messages', 'with messages waiting, Me opens Messages');
    if (shots) await page.locator('header.topbar').screenshot({path: shots + '/notifier-header-1280.png'});

    // The dot pulses; under reduced motion it holds still and stays red.
    assert.equal(await page.evaluate(() => getComputedStyle(document.querySelector('.identity-dot'), '::after').animationName), 'alert-ring');
    await page.emulateMedia({reducedMotion: 'reduce'});
    assert.equal(await page.evaluate(() => getComputedStyle(document.querySelector('.identity-dot'), '::after').animationName), 'none');
    assert.notEqual(await page.evaluate(() => getComputedStyle(document.querySelector('.identity-dot')).backgroundColor), 'rgba(0, 0, 0, 0)');
    await page.emulateMedia({reducedMotion: 'no-preference'});

    // A reply to one of your posts: a real post, so its thread page exists.
    const posted = await (await context.request.get(origin + '/w/lobby/main?format=json&text=' + encodeURIComponent('A reply for the notifier ' + Date.now()))).json();
    b.updates.push({reason: 'reply', id: posted.receipt.id});
    await page.evaluate(() => window.SwarmPage.refreshNotifications());
    await until(() => b.sent('updates.get').length === 2);
    assert.equal(b.sent('updates.get')[1].cursor, 'u0', 'later reads continue from the saved cursor'); assert.equal(b.sent('updates.get')[1].limit, 50);
    await page.waitForFunction(() => document.title.startsWith('(3) '));

    // Another tab shows the same number from the shared state, without reading again.
    const second = await context.newPage(); await b.attach(second);
    await second.goto(origin + '/rooms'); await second.waitForFunction(() => document.title.startsWith('(3) '));
    await second.waitForTimeout(300);
    assert.equal(b.sent('updates.get').length, 2, 'at most one read a minute across tabs');
    assert.equal((await state(second)).label, '3 new');

    // Reading the conversation and answering the request clear the Messages part;
    // the reply is still new, and Me now leads to it.
    dm.unread = 0; request.members[1].state = 'active';
    await page.evaluate(() => window.SwarmPage.refreshNotifications());
    await page.waitForFunction(() => document.title.startsWith('(1) '));
    now = await state(page);
    assert.equal(now.nav, '1'); assert.equal(now.me, '/e/' + posted.receipt.id);
    await second.waitForFunction(() => document.title.startsWith('(1) '));

    // Opening the reply's thread counts as seen: the title, icon and dot return to rest.
    await page.goto(origin + '/e/' + posted.receipt.id);
    await page.waitForFunction(() => !/^\(\d/.test(document.title));
    now = await state(page);
    assert.equal(now.icon, '/assets/icon.svg'); assert.equal(now.type, 'image/svg+xml'); assert.equal(now.alert, false); assert.equal(now.label, null); assert.equal(now.nav, ''); assert.equal(now.me, '/me');
    await second.waitForFunction(() => !/^\(\d/.test(document.title));
    assert.equal(await page.evaluate(() => JSON.parse(localStorage.getItem('swarmmemo.notify.v1')).replies.length), 0);
    // A background tab keeps counting: the badge is for the tab you are not looking at.
    const later = await (await context.request.get(origin + '/w/lobby/main?format=json&text=' + encodeURIComponent('A reply while hidden ' + Date.now()))).json();
    b.updates.push({reason: 'reply', id: later.receipt.id});
    await page.evaluate(() => Object.defineProperty(document, 'hidden', {configurable: true, get: () => true}));
    const reads = b.sent('updates.get').length;
    await page.evaluate(() => window.SwarmPage.refreshNotifications());
    await until(() => b.sent('updates.get').length === reads + 1);
    await page.waitForFunction(() => document.title.startsWith('(1) '));
    await page.evaluate(() => { delete document.hidden; });
    await page.setViewportSize({width: 390, height: 844});
    assert.ok(await noHorizontalScroll(page), 'no horizontal scroll at 390px');
    assert.deepEqual(errors, []);
    console.log('PASS: notifications — one shared signed read a minute, counts-only reads, title count, favicon dot, red Me dot and number (still under reduced motion), Me leads to the news, seen clears everywhere, hidden tabs keep counting.');
  } finally { await browser.close(); }
})().catch(error => { console.error(error); process.exit(1); });
