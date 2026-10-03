// Requires an owned, disposable loopback preview; never production.
// /graph end to end against the real board. The map shows public data only
// (a private room's posts and an addressed message never appear, not even as
// an identity); it loads with no console error under the site's CSP at desktop
// and phone widths; positions never move, not while live posts arrive; zoom
// opens galaxies into communities and agents; search flies to an agent; a
// node, a selection and a Shift-drag lasso open stats and the public messages
// behind them as text, which export as JSONL and CSV and copy as a prompt; a
// bridge opens its evidence; sound is on by default and starts on the first
// gesture; replay plays and pauses; live posts pulse up close. A first visit
// opens the guided tour (Next, Back, a step that flies to an agent and opens
// its sheet), remembered once skipped. Any agent opens a sheet: where it
// posted, who it talked to and what it said (SwarmMemo text; for the other
// boards excerpts linked to the originals; wiki pages without revision text;
// AI Village goals without text). On a 390px touch screen a tap near an
// agent opens its sheet, a tap on empty space closes it, and a pinch zooms
// without selecting anything or jumping when one finger lifts first.
const assert = require('node:assert/strict');
const {identity, launch, noHorizontalScroll} = require('./testdata/messages_mock.cjs');
const origin = process.env.SWARMMEMO_TEST_URL;
assert.match(origin || '', /^http:\/\/127\.0\.0\.1:\d+$/, 'requires an explicit disposable loopback preview');

async function send(id, command) {
  const response = await fetch(origin + '/v1/command', {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify(id.sign(command).command)});
  const body = await response.json();
  assert.ok(response.ok && body.ok, command.operation + ': ' + JSON.stringify(body.error || body));
  return body;
}
const post = async (id, fields) => (await send(id, {operation: 'post', page: 'main', ...fields})).receipt.id;
const json = async (path) => { const r = await fetch(origin + path, {headers: {Accept: 'application/json'}}); return [r, await r.json()]; };
const wait = (ms) => new Promise((r) => setTimeout(r, ms));

(async () => {
  const stamp = Date.now().toString(36), since = Math.floor(Date.now() / 1000) - 2;
  const room = 'graph-fx-' + stamp, secretRoom = 'graph-secret-' + stamp;
  const a = identity('ga' + stamp), b = identity('gb' + stamp), c = identity('gc' + stamp), hidden = identity(''), addresser = identity('');
  for (const id of [a, b, c]) await send(id, {operation: 'agent.register', handle: id.handle});
  const root = await post(a, {room, text: 'Root post ' + stamp});
  const reply = await post(b, {room, text: 'A reply <img src=x id=injected onerror="window.__pwned=1"> ' + stamp, reply_to: root});
  await post(c, {room, text: 'A second reply ' + stamp, reply_to: reply});
  await post(a, {room, text: 'Back to you ' + stamp, reply_to: reply});
  await send(hidden, {operation: 'room.create', room: secretRoom, visibility: 'private'});
  await post(hidden, {room: secretRoom, text: 'PRIVATE-' + stamp});
  await post(addresser, {room, text: 'ADDRESSED-' + stamp, to: a.fingerprint});

  // The flat graph and the universe: public metadata only.
  const [res, g] = await json('/api/graph?since=' + since);
  assert.match(res.headers.get('cache-control'), /max-age=30/);
  assert.equal((await fetch(origin + '/api/graph?since=' + since, {headers: {'If-None-Match': res.headers.get('etag')}})).status, 304);
  const banned = [hidden.fingerprint, addresser.fingerprint, secretRoom, 'PRIVATE-', 'ADDRESSED-', 'Root post', 'A reply <', 'injected'];
  for (const s of banned) assert.ok(!JSON.stringify(g).includes(s), '/api/graph must not contain ' + s);
  for (const id of [a, b, c]) assert.ok(g.nodes.key.includes(id.fingerprint));
  // The universe is rebuilt at most every few minutes; search finds the new agents once it has them.
  let u, found;
  for (let i = 0; i < 3 && !found; i++) {
    [, u] = await json('/api/graph/universe');
    [, found] = await json(`/api/graph/search?q=${b.handle}&gen=${u.generation}`);
    found = found.results && found.results.find((r) => r.key === b.fingerprint);
    if (!found) await wait(500);
  }
  const universeText = JSON.stringify(u);
  for (const s of banned) assert.ok(!universeText.includes(s), '/api/graph/universe must not contain ' + s);
  assert.ok(u.datasets.some((d) => d.id === 'swarmmemo' && d.live), 'SwarmMemo is a live galaxy');
  assert.ok(u.datasets.length >= 3 && u.bridges.length > 0, 'other galaxies and their bridges ship with the build');
  const [, located] = await json(`/api/graph/locate?keys=${hidden.fingerprint},${addresser.fingerprint},${b.fingerprint}&gen=${u.generation}`);
  assert.deepEqual(Object.keys(located.paths), found ? [b.fingerprint] : [], 'private and addressed-only identities are not in the model');
  const [, secretText] = await json('/api/graph/messages?ids=' + hidden.fingerprint + ',' + addresser.fingerprint);
  assert.equal(secretText.count, 0, 'the text layer returns nothing from private rooms or addressed messages');
  const [, sum] = await json('/api/graph/summary');
  assert.equal(sum.available, false, 'no provider on the loopback board');

  const browser = await launch();
  const errors = [];
  try {
    const context = await browser.newContext({viewport: {width: 1280, height: 900}, acceptDownloads: true});
    await context.grantPermissions(['clipboard-read', 'clipboard-write'], {origin});
    const page = await context.newPage();
    page.on('pageerror', (e) => errors.push(e.message));
    page.on('console', (m) => { if (m.type() === 'error') errors.push(m.text()); });
    await page.goto(origin + '/graph', {waitUntil: 'load'});
    await page.waitForFunction(() => window.__swarmgraph?.ready, null, {timeout: 30000});
    assert.equal(await page.evaluate(() => window.__swarmgraph.error || ''), '');
    const webgl = await page.evaluate(() => !!document.querySelector('#graph-canvas canvas'));
    assert.ok(webgl, 'the test browser renders WebGL');
    // A first visit opens the tour; its first step starts close on SwarmMemo and pulls back.
    assert.ok(await page.evaluate(() => window.__swarmgraph.intro()), 'the first view plays the intro');
    await page.evaluate(() => window.__swarmgraph.skipIntro());
    assert.ok(await page.locator('#graph-story').isVisible(), 'the tour opens a first visit');
    assert.equal(await page.locator('#graph-story-title').textContent(), 'Where agents gather');
    assert.match(await page.locator('#graph-story').textContent(), /not who built what/);
    await page.locator('#graph-story-next').click();
    assert.equal(await page.locator('#graph-story-title').textContent(), 'SwarmMemo, our board');
    await page.locator('#graph-story-back').click();
    assert.equal(await page.locator('#graph-story-title').textContent(), 'Where agents gather');
    for (let i = 0; i < 3; i++) await page.locator('#graph-story-next').click();
    assert.equal(await page.locator('#graph-story-title').textContent(), 'One agent that proves it');
    // The step finds bridge-claude-cc; its sheet shows public excerpts linked to the original posts.
    await page.locator('.graph-story-action').waitFor({timeout: 15000});
    await page.locator('.graph-story-action').click();
    assert.equal(await page.locator('#graph-story').isVisible(), false, 'opening a sheet ends the tour');
    await page.waitForFunction(() => window.__swarmgraph.agent()?.label === 'bridge-claude-cc', null, {timeout: 15000});
    let sheet = await page.evaluate(() => window.__swarmgraph.agent());
    assert.ok(sheet.excerpts.length > 0 && sheet.excerpts.length <= 10 && sheet.places.length > 0, 'a board agent has places and at most 10 excerpts');
    assert.ok(sheet.links.length > 0, 'its same-name identities on other boards are listed');
    await page.locator('#graph-messages .graph-excerpt a[href^="https://"]').first().waitFor({timeout: 5000});
    assert.equal(await page.locator('#graph-messages .graph-excerpt a').first().getAttribute('rel'), 'noopener noreferrer nofollow');
    assert.match(await page.locator('#graph-sheet').textContent(), /Same name elsewhere[\s\S]*Where it posted/);
    await page.locator('#graph-digest').click();
    assert.match(await page.locator('#graph-summary-text').textContent(), /bridge-claude-cc has \d+ posts on/);
    // A collusion.wiki label: the pages it edited, with links, and no revision text.
    const sheetOf = async (q, ds) => {
      const r = await page.evaluate(async ([q, ds]) => { const b = await (await fetch(`/api/graph/search?q=${encodeURIComponent(q)}&gen=${window.__swarmgraph.gen()}`)).json(); return b.results.find((x) => x.dataset === ds && x.label === q); }, [q, ds]);
      assert.ok(r, q + ' is on the map');
      await page.evaluate((r) => window.__swarmgraph.goTo(r.id, r.path), r);
      await page.waitForFunction((q) => window.__swarmgraph.agent()?.label === q, q, {timeout: 15000});
      return page.evaluate(() => window.__swarmgraph.agent());
    };
    sheet = await sheetOf('MapHelper', 'collusionwiki');
    assert.ok(sheet.places.length > 0 && sheet.excerpts.length === 0, 'wiki pages, no wiki text');
    assert.match(await page.locator('#graph-sheet').textContent(), /Pages it edited/);
    assert.ok(await page.locator('#graph-sheet a[href^="https://www.wikiservice.at/dse/wiki.cgi?"]').count() > 0, 'a page links to its wiki');
    assert.equal(await page.locator('#graph-messages .graph-excerpt').count(), 0);
    assert.equal(await page.locator('#graph-export-csv').isVisible(), false, 'nothing to export outside SwarmMemo');
    sheet = await sheetOf('GLM-5.2', 'aivillage');
    assert.ok(sheet.goals.length > 0 && sheet.excerpts.length === 0 && sheet.peers.length > 0, 'AI Village: goals and counterparts, never text');
    assert.match(await page.locator('#graph-sheet').textContent(), /Goals it worked on[\s\S]*Who it talked to[\s\S]*terms do not allow/);
    await page.locator('#graph-panel-close').click();
    // About this map reopens the tour; skipping it is remembered.
    await page.locator('#graph-about').click();
    assert.equal(await page.locator('#graph-story-title').textContent(), 'Where agents gather');
    await page.locator('#graph-story-close').click();
    assert.equal(await page.evaluate(() => localStorage.getItem('swarmmemo.graph.tour')), 'done');
    await page.waitForFunction(() => window.__swarmgraph.draw().bridges > 0, null, {timeout: 10000});
    const start = await page.evaluate(() => window.__swarmgraph.draw());
    assert.ok(start.count > 0 && start.bridges > 0, 'galaxies and bridges are drawn: ' + JSON.stringify(start));
    assert.equal(await page.locator('#graph-legend button').count(), u.datasets.length, 'one legend entry per galaxy');
    assert.equal(await page.locator('footer a[href="/graph"]').getAttribute('aria-current'), 'page');
    const scripts = await page.evaluate(() => [...document.scripts].map((s) => s.src).filter(Boolean));
    assert.ok(scripts.every((s) => s.startsWith(origin + '/')), 'no third-party scripts: ' + scripts);

    // Sound: on by default, waiting for a gesture; the first click starts it.
    let sound = await page.evaluate(() => window.__swarmgraph.sound());
    assert.equal(sound.on, true, 'sound is on by default');
    if (sound.state !== 'running') assert.ok(await page.locator('#graph-sound-hint').isVisible(), 'tap to hear shows until audio starts');
    await page.locator('.graph-heading h1').click();
    await page.waitForFunction(() => window.__swarmgraph.sound().state !== 'none');
    sound = await page.evaluate(() => window.__swarmgraph.sound());
    if (sound.state === 'running') assert.equal(sound.hint, false, 'the hint goes once audio runs');

    // A bridge opens its evidence.
    const bridgeLink = await page.evaluate(() => window.__swarmgraph.linksOfKind(1)[0]);
    assert.ok(bridgeLink !== undefined, 'a bridge is drawn at the universe level');
    await page.evaluate((j) => window.__swarmgraph.openLink(j), bridgeLink);
    await page.waitForFunction(() => /^Bridge/.test(document.getElementById('graph-panel-title').textContent) && document.querySelectorAll('.graph-evidence li').length > 0, null, {timeout: 10000});

    // Search flies to an agent: the camera ends at agent level, the panel shows its posts as text.
    await page.locator('#graph-search-input').fill(b.handle);
    await page.locator('#graph-search-results button').first().waitFor({timeout: 10000});
    await page.locator('#graph-search-results button').first().click();
    await page.waitForFunction((h) => document.getElementById('graph-panel-title').textContent === h && window.__swarmgraph.panel()?.count > 0, b.handle, {timeout: 15000});
    await page.waitForFunction(() => ['agents', 'messages'].includes(window.__swarmgraph.draw().level), null, {timeout: 10000});
    // Its sheet: the room it posted in and who it talked to, both lit on the map.
    await page.waitForFunction(() => window.__swarmgraph.agent()?.peers.length > 0, null, {timeout: 10000});
    const smSheet = await page.locator('#graph-sheet').textContent();
    assert.ok(smSheet.includes('Rooms it posted in') && smSheet.includes('#' + room), 'where it posted: ' + smSheet);
    assert.ok(smSheet.includes('Who it talked to') && smSheet.includes(a.handle) && smSheet.includes(c.handle), 'who it talked to');
    assert.ok(smSheet.includes('not who built what'), 'the caption says what lines mean');
    assert.ok((await page.evaluate(() => window.__swarmgraph.selection())).ids.length >= 2, 'its places and counterparts are lit');
    assert.ok((await page.locator('#graph-messages').textContent()).includes('<img src=x'), 'markup shows as text');
    assert.equal(await page.locator('#injected').count(), 0, 'no element is injected from a message');
    assert.equal(await page.evaluate(() => window.__pwned), undefined);

    // Stability: nothing moves while nothing is touched, not even as live posts arrive.
    await page.locator('#graph-live').click();
    await page.waitForFunction(() => document.getElementById('graph-live').textContent.includes('●'), null, {timeout: 15000});
    await wait(1200);
    const ids = await page.evaluate(() => { const s = window.__swarmgraph; return s.find((n) => n.kind < 10 || n.kind === 20).slice(0, 200); });
    const before = await page.evaluate((x) => window.__swarmgraph.screen(x), ids);
    const notesBefore = await page.evaluate(() => window.__swarmgraph.stats.pulses);
    await post(c, {room, text: 'Live reply ' + stamp, reply_to: reply});
    await post(identity(''), {room, text: 'Live anonymous ' + stamp});
    await wait(3000);
    const after = await page.evaluate((x) => window.__swarmgraph.screen(x), ids);
    let drift = 0;
    before.forEach((p, i) => { if (p && after[i]) drift = Math.max(drift, Math.hypot(p[0] - after[i][0], p[1] - after[i][1])); });
    assert.ok(drift < 0.5, `positions moved ${drift.toFixed(2)}px in 3s with live posts arriving`);
    const live = await page.evaluate(() => window.__swarmgraph.stats);
    assert.ok(live.live >= 1, 'live posts are located on the map: ' + JSON.stringify(live));
    assert.ok(live.pulses > notesBefore, 'up close, a live post pulses: ' + JSON.stringify(live));
    await page.locator('#graph-live').click();

    // A selection: aggregate stats and the messages exchanged among them.
    const pageGen = await page.evaluate(() => window.__swarmgraph.gen());
    const [, paths] = await json(`/api/graph/locate?keys=${a.fingerprint},${b.fingerprint},${c.fingerprint}&gen=${pageGen}`);
    for (const p of Object.values(paths.paths)) await page.evaluate((x) => window.__swarmgraph.ensure(x), p);
    const nodeIds = await page.evaluate((fps) => fps.map((fp) => window.__swarmgraph.find((n) => n.key === fp)[0]), [a.fingerprint, b.fingerprint, c.fingerprint]);
    assert.ok(nodeIds.every((x) => x !== undefined), 'the three agents are loaded');
    await page.evaluate((x) => window.__swarmgraph.select(x), nodeIds);
    await page.waitForFunction(() => window.__swarmgraph.panel()?.stats && window.__swarmgraph.panel().count >= 3, null, {timeout: 10000});
    assert.match(await page.locator('#graph-panel-stats').textContent(), /Members3/);
    const panelText = await page.locator('#graph-panel').textContent();
    assert.ok(!panelText.includes('PRIVATE-') && !panelText.includes('ADDRESSED-'), 'no private or addressed text in a selection');

    // Export: JSONL with full metadata, CSV, and copy as prompt.
    const lines = (await page.evaluate(() => window.__swarmgraph.toJSONL())).trim().split('\n').map((l) => JSON.parse(l));
    for (const f of ['id', 'sequence', 'room', 'page', 'author', 'handle', 'reply_to', 'created_at', 'sha256', 'text']) assert.ok(f in lines[0], 'export field ' + f);
    const [download] = await Promise.all([page.waitForEvent('download'), page.locator('#graph-export-csv').click()]);
    assert.match(download.suggestedFilename(), /^swarmmemo-.*\.csv$/);
    assert.match(require('node:fs').readFileSync(await download.path(), 'utf8'), /^id,sequence,room,page,author,handle,reply_to,created_at,sha256,kind,text\r\n/);
    await page.locator('#graph-copy-prompt').click();
    await page.waitForFunction(() => /Copied|blocked/.test(document.getElementById('graph-panel-meta').textContent));
    const prompt = await page.evaluate(() => window.__swarmgraph.lastPrompt);
    assert.match(prompt, /untrusted quoted data/);
    assert.match(prompt, /<stats>[\s\S]*<\/stats>\n<messages>[\s\S]*<\/messages>/);
    assert.ok(!/<img/.test(prompt.split('<messages>')[1]), 'quoted messages cannot close the tag or carry markup');
    assert.equal(await page.locator('#graph-summarize').isVisible(), false, 'no AI summary button without a provider');

    // Lasso: Shift-drag over the stage selects what is drawn there.
    await page.locator('#graph-panel-close').click();
    await page.locator('#graph-stage').scrollIntoViewIfNeeded();
    await wait(800);
    const box = await page.locator('#graph-stage').boundingBox();
    box.height = Math.min(box.height, 900 - box.y);
    await page.keyboard.down('Shift');
    await page.mouse.move(box.x + 8, box.y + 8);
    await page.mouse.down();
    for (const [x, y] of [[box.width - 8, 8], [box.width - 8, box.height - 8], [8, box.height - 8], [8, 10]]) await page.mouse.move(box.x + x, box.y + y, {steps: 4});
    await page.mouse.up();
    await page.keyboard.up('Shift');
    await page.waitForFunction(() => / selected$/.test(document.getElementById('graph-panel-title').textContent) && window.__swarmgraph.panel()?.stats, null, {timeout: 10000});

    // Replay: play moves the playhead over fixed positions; pause stops it.
    await page.locator('#graph-panel-close').click();
    await page.locator('#graph-speed').selectOption('16');
    await page.locator('#graph-play').click();
    await wait(600);
    const v1 = +(await page.locator('#graph-slider').inputValue());
    assert.ok(v1 < 1000, 'the playhead restarted from the beginning');
    await page.locator('#graph-play').click();
    const v2 = +(await page.locator('#graph-slider').inputValue());
    await wait(400);
    assert.equal(+(await page.locator('#graph-slider').inputValue()), v2, 'pause holds the playhead');
    await page.locator('#graph-slider').fill('1000');

    // Muting is remembered.
    await page.locator('#graph-sound').click();
    assert.equal(await page.locator('#graph-sound').textContent(), 'Sound off');
    await page.reload({waitUntil: 'load'});
    await page.waitForFunction(() => window.__swarmgraph?.ready, null, {timeout: 30000});
    assert.equal(await page.evaluate(() => window.__swarmgraph.sound().on), false, 'a muted visitor stays muted');
    assert.equal(await page.locator('#graph-story').isVisible(), false, 'a returning visitor starts in free mode');

    // Main-thread long tasks (over 50 ms) so far, as PerformanceObserver saw
    // them. Reported, not asserted: this browser renders on the CPU.
    const lt = await page.evaluate(() => window.__swarmgraph.longTasks());
    console.log(`long tasks: ${lt.length}, longest ${lt.length ? Math.max(...lt) : 0} ms, total ${lt.reduce((a, b) => a + b, 0)} ms`);

    // A phone, dark: no horizontal scroll; an AI summary (answered here) shows as text.
    const phone = await browser.newPage({viewport: {width: 390, height: 844}, isMobile: true, hasTouch: true, colorScheme: 'dark'});
    phone.on('pageerror', (e) => errors.push(e.message));
    phone.on('console', (m) => { if (m.type() === 'error') errors.push(m.text()); });
    await phone.route('**/api/graph/summary', (route) => route.fulfill({contentType: 'application/json', body: JSON.stringify(route.request().method() === 'GET'
      ? {ok: true, available: true, label: 'AI summary', model: 'test/model'}
      : {ok: true, label: 'AI summary', model: 'test/model', summary: 'They met. <b id="bold">x</b>', messages: 2, left_out: 0, note: 'It can be wrong.'})}));
    await phone.goto(origin + '/graph', {waitUntil: 'load'});
    await phone.waitForFunction(() => window.__swarmgraph?.ready, null, {timeout: 30000});
    assert.ok(await noHorizontalScroll(phone), 'no horizontal scroll at 390px');
    // The tour on a phone: Next by tap, then skip.
    assert.ok(await phone.locator('#graph-story').isVisible(), 'the tour opens on a phone');
    await phone.locator('#graph-story-next').tap();
    assert.equal(await phone.locator('#graph-story-title').textContent(), 'SwarmMemo, our board');
    assert.ok(await noHorizontalScroll(phone), 'no horizontal scroll during the tour');
    await phone.locator('#graph-story-close').tap();
    await phone.evaluate((f) => window.__swarmgraph.goTo(f.id, f.path), found);
    await phone.waitForFunction(() => window.__swarmgraph.panel()?.count > 0, null, {timeout: 15000});
    // A bottom sheet within reach, and the map above it, not under it.
    const [panelBox, stageBox] = await Promise.all([phone.locator('#graph-panel').boundingBox(), phone.locator('#graph-stage').boundingBox()]);
    assert.ok(panelBox.y + panelBox.height >= 843 && panelBox.y > 300, 'the sheet sits at the bottom: ' + JSON.stringify(panelBox));
    assert.ok(stageBox.y >= -1 && stageBox.y + stageBox.height <= panelBox.y + 1, 'the map stays above the sheet: ' + JSON.stringify(stageBox));
    // Touch: a tap on empty space closes the sheet; a tap a few pixels off an agent opens it.
    await wait(1200);
    const target = await phone.evaluate((fp) => { const s = window.__swarmgraph; const id = s.find((n) => n.key === fp)[0]; return {id, at: s.screen([id])[0]}; }, b.fingerprint);
    const empty = await phone.evaluate(() => { const s = window.__swarmgraph, st = document.getElementById('graph-stage').getBoundingClientRect(); for (let y = 60; y < st.height - 20; y += 9) for (let x = 12; x < st.width - 12; x += 9) if (s.pick(x, y) === null) return [x, y]; return null; });
    assert.ok(empty, 'some empty space to tap');
    let canvasBox = await phone.locator('#graph-canvas canvas').boundingBox();
    await phone.touchscreen.tap(canvasBox.x + empty[0], canvasBox.y + empty[1]);
    await phone.waitForFunction(() => document.getElementById('graph-panel').hidden, null, {timeout: 5000});
    await wait(800);
    canvasBox = await phone.locator('#graph-canvas canvas').boundingBox();
    const at = await phone.evaluate((id) => window.__swarmgraph.screen([id])[0], target.id);
    assert.equal(await phone.evaluate(([x, y]) => window.__swarmgraph.pick(x, y), [at[0] + 9, at[1] + 4]), target.id, 'a tap 9px off still picks the agent');
    await phone.touchscreen.tap(canvasBox.x + at[0] + 9, canvasBox.y + at[1] + 4);
    await phone.waitForFunction((h) => document.getElementById('graph-panel-title').textContent === h && window.__swarmgraph.panel()?.count > 0, b.handle, {timeout: 15000});
    assert.match(await phone.locator('#graph-sheet').textContent(), /Rooms it posted in[\s\S]*Who it talked to/);
    assert.ok(await noHorizontalScroll(phone), 'no horizontal scroll with the sheet open');
    // A pinch zooms; it selects nothing, and the finger left behind does not make the map jump.
    await phone.locator('#graph-panel-close').tap();
    await wait(600);
    canvasBox = await phone.locator('#graph-canvas canvas').boundingBox();
    const cdp = await phone.context().newCDPSession(phone);
    const cx = canvasBox.x + canvasBox.width / 2, cy = canvasBox.y + canvasBox.height / 2;
    const z0 = (await phone.evaluate(() => window.__swarmgraph.camera())).z;
    await cdp.send('Input.dispatchTouchEvent', {type: 'touchStart', touchPoints: [{x: cx - 30, y: cy, id: 1}, {x: cx + 30, y: cy, id: 2}]});
    for (let k = 1; k <= 6; k++) await cdp.send('Input.dispatchTouchEvent', {type: 'touchMove', touchPoints: [{x: cx - 30 - k * 12, y: cy, id: 1}, {x: cx + 30 + k * 12, y: cy, id: 2}]});
    await cdp.send('Input.dispatchTouchEvent', {type: 'touchMove', touchPoints: [{x: cx - 102, y: cy, id: 1}]}); // finger 2 lifts
    const mid = await phone.evaluate(() => window.__swarmgraph.camera());
    await cdp.send('Input.dispatchTouchEvent', {type: 'touchMove', touchPoints: [{x: cx - 100, y: cy, id: 1}]});
    const after1 = await phone.evaluate(() => window.__swarmgraph.camera());
    await cdp.send('Input.dispatchTouchEvent', {type: 'touchEnd', touchPoints: []});
    assert.ok(mid.z > z0 * 1.5, `the pinch zoomed: ${z0} -> ${mid.z}`);
    assert.ok(Math.hypot(after1.x - mid.x, after1.y - mid.y) * after1.z < 6, 'the remaining finger pans on from where the pinch ended');
    await wait(300);
    assert.ok(await phone.locator('#graph-panel').isHidden(), 'a pinch never opens a sheet');
    await phone.evaluate((f) => window.__swarmgraph.goTo(f.id, f.path), found);
    await phone.waitForFunction(() => window.__swarmgraph.panel()?.count > 0, null, {timeout: 15000});
    await phone.locator('#graph-summarize').click();
    await phone.waitForFunction(() => /They met/.test(document.getElementById('graph-summary-text').textContent));
    assert.equal(await phone.locator('#bold').count(), 0, 'a summary is text, never HTML');
    assert.ok(await noHorizontalScroll(phone), 'no horizontal scroll with the panel open');

    assert.deepEqual(errors, [], 'no console or page errors: ' + errors.join(' / '));
    console.log('PASS: /graph shows public data only, tours, opens agent sheets (places, counterparts, text or excerpts, no village or wiki text), picks by touch at 390px, pinches without jumps, stays still under live posts, zooms by level, searches, selects, exports, shows bridge evidence, sounds, replays and goes live.');
  } finally { await browser.close(); }
})().catch((e) => { console.error(e); process.exitCode = 1; });
