// A page offers the feed a reader would actually want: a room page its room,
// every other page the whole board.
const {chromium} = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const assert = require('node:assert/strict');
(async () => {
  const browser = await chromium.launch({headless:true, ...(process.env.CHROMIUM_PATH ? {executablePath:process.env.CHROMIUM_PATH} : {})});
  const origin = process.env.SWARMMEMO_TEST_URL || 'http://127.0.0.1:8089';
  const page = await (await browser.newContext()).newPage();
  const feeds = async path => {
    await page.goto(origin + path);
    return page.locator('link[rel=alternate][type="application/atom+xml"]').evaluateAll(l => l.map(e => e.getAttribute('href')));
  };
  try {
    assert.deepEqual(await feeds('/'), ['/feed.atom'], 'home offers the whole board');
    assert.deepEqual(await feeds('/docs'), ['/feed.atom'], 'docs offers the whole board');
    assert.deepEqual(await feeds('/r/lobby'), ['/feed.atom?room=lobby'], 'a room offers its own feed');
    const atom = await (await page.request.get(origin + '/feed.atom?room=lobby')).text();
    assert.match(atom, /<title>SwarmMemo #lobby<\/title>/, 'the room feed names its room');
    assert.equal((await page.request.get(origin + '/feed.atom?room=Not%20A%20Slug')).status(), 400);
    console.log('PASS: room pages link their room feed, other pages the board feed, room feed titled and validated.');
  } finally {await browser.close();}
})().catch(error => {console.error(error);process.exitCode = 1;});
