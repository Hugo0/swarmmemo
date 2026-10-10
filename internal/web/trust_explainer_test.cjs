// Isolated local-server browser regression for /trust, the short guide to the
// allowance and trust model, and /trust/network. Same environment as
// browser_test.cjs. /trust is served only while ALLOWANCE_LEDGER or TRUST is
// on; against a server with both off the suite checks the 404 and stops.
//
// In light and dark, with and without reduced motion, at 320, 390 and 1280 px:
// no horizontal overflow, named controls, the stake figure driven by keyboard
// and click (its readout matches the published parameters), and, while trust
// is on, the network drawn from /api/trust/graph (hover or keyboard focus
// shows an agent, Enter would open it) or its text list when the graph is
// empty. A click-fuzz (TX_FUZZ_MS, default 3 s) presses random controls and
// checks that no number reads NaN, Infinity or a negative.
const {chromium} = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const assert = require('node:assert/strict');
const FUZZ_MS = Number(process.env.TX_FUZZ_MS || 3000);

// Nothing on the page reads NaN, Infinity or undefined, and no bar is negative.
const sane = async (page, where) => {
  const problems = await page.evaluate(() => {
    const out = [];
    const root = document.getElementById('tx');
    const text = root.innerText;
    for (const word of ['NaN', 'Infinity', 'undefined']) if (text.includes(word)) out.push('text has ' + word);
    root.querySelectorAll('[data-out]').forEach(n => {
      if (/(^|[\s(:])[-−]\d/.test(n.textContent)) out.push('negative ' + n.dataset.out + ': ' + n.textContent);
    });
    root.querySelectorAll('[style]').forEach(n => { if (/NaN|Infinity|-\d/.test(n.getAttribute('style'))) out.push('style ' + n.getAttribute('style')); });
    return out;
  });
  assert.deepEqual(problems, [], where);
};
const read = (page) => page.locator('[data-fig="stake"] [data-out="read"]').textContent();

(async () => {
  const browser = await chromium.launch({headless: true, ...(process.env.CHROMIUM_PATH ? {executablePath: process.env.CHROMIUM_PATH} : {}), args: process.env.PLAYWRIGHT_NO_SANDBOX === 'true' ? ['--no-sandbox'] : []});
  const origin = process.env.SWARMMEMO_TEST_URL || 'http://127.0.0.1:8089';
  try {
    const request = (await browser.newContext()).request;
    const probe = await request.get(origin + '/trust', {headers: {Accept: 'text/html'}});
    if (probe.status() === 404) {
      console.log('trust explainer: /trust is off on this server (404), as with every RFC0012 flag off');
      return;
    }
    assert.equal(probe.status(), 200);
    const graphResponse = await request.get(origin + '/api/trust/graph');
    const trustOn = graphResponse.status() === 200;
    const graph = trustOn ? (await graphResponse.json()).data : null;
    assert.equal((await request.get(origin + '/trust/network', {headers: {Accept: 'text/html'}})).status(), trustOn ? 200 : 404, '/trust/network follows trust');
    if (graph) {
      // Public only and capped, whatever the server holds.
      assert.ok(graph.nodes.length <= graph.limits.nodes && graph.edges.length <= graph.limits.edges, 'the graph keeps its caps');
      for (const e of graph.edges) assert.ok(['vouch', 'work_accept', 'witness', 'key_link'].includes(e.kind), 'edge kind ' + e.kind);
    }
    const runs = [
      {scheme: 'light', reduced: true, width: 390},
      {scheme: 'dark', reduced: false, width: 1280},
      {scheme: 'light', reduced: false, width: 390},
    ];
    for (const run of runs) {
      const {scheme, reduced, width} = run;
      const tag = `${scheme}, ${width}px${reduced ? ', reduced motion' : ''}`;
      const context = await browser.newContext({viewport: {width, height: width < 600 ? 844 : 900}, colorScheme: scheme, reducedMotion: reduced ? 'reduce' : 'no-preference'});
      const page = await context.newPage();
      const errors = [], requests = [];
      page.on('pageerror', e => errors.push(e.message));
      page.on('console', m => { if (m.type() === 'error' && !/^Failed to load resource/.test(m.text())) errors.push(m.text()); });
      page.on('response', r => { if (r.status() >= 400) errors.push(r.status() + ' ' + r.url()); });
      page.on('request', r => requests.push([r.method(), new URL(r.url())]));
      await page.goto(origin + '/trust');
      await page.waitForLoadState('networkidle');
      for (const w of width < 600 ? [320, width] : [width]) {
        await page.setViewportSize({width: w, height: w < 600 ? 844 : 900});
        await page.waitForTimeout(150);
        assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), `/trust overflows at ${w}px (${tag})`);
      }
      const background = await page.evaluate(() => getComputedStyle(document.body).backgroundColor);
      assert.equal(background === 'rgb(255, 255, 255)', scheme === 'light', `body background ${background} (${tag})`);
      // Short: about three screens of reading at desktop width.
      if (width >= 1280) {
        const screens = await page.evaluate(() => document.getElementById('tx').getBoundingClientRect().height / innerHeight);
        assert.ok(screens < 5, `/trust is ${screens.toFixed(1)} screens tall`);
      }
      const unnamed = await page.evaluate(() => [...document.querySelectorAll('#tx button, #tx input')].filter(n => {
        const label = n.labels && n.labels.length ? n.labels[0].textContent.trim() : '';
        return !(label || n.getAttribute('aria-label') || n.textContent.trim());
      }).map(n => n.outerHTML));
      assert.deepEqual(unnamed, [], 'controls without a name');
      for (const id of ['allowance', 'standing', 'check']) assert.equal(await page.locator('#' + id).count(), 1, '#' + id);

      // The stake figure: a vouch moves 0.25% x weight; a vote moves nothing.
      assert.match(await read(page), /A vouch at weight 10 stakes 2\.5% of your standing: 12\.5 of your 500 cents/);
      const weight = page.locator('[data-fig="stake"] [data-in="weight"]');
      await weight.focus();
      await page.keyboard.press('End');
      assert.equal(await page.locator('[data-fig="stake"] [data-out="weight"]').textContent(), '50');
      assert.match(await read(page), /weight 50 stakes 12\.5% of your standing: 62\.5 of your 500 cents/);
      await page.getByRole('radio', {name: 'Up vote'}).check({force: true});
      assert.match(await read(page), /An up vote moves nothing/);
      assert.ok(await page.locator('[data-fig="stake"] [data-in="weight"]').isHidden(), 'the vouch weight hides for a vote');
      const cents = page.locator('[data-fig="stake"] [data-in="cents"]');
      await cents.focus();
      await page.keyboard.press('Home');
      assert.match(await read(page), /ranks the post at your vote weight, 0\.00/);
      await page.getByRole('radio', {name: 'Accept work'}).check({force: true});
      assert.match(await read(page), /An accepted work item at weight 10/);
      await sane(page, 'stake ' + tag);

      // The network: drawn from the API, or its text when there is nothing to draw.
      if (graph) {
        const net = page.locator('[data-fig="network"]');
        await net.scrollIntoViewIfNeeded();
        if (graph.nodes.length) {
          await page.waitForFunction(() => {
            const f = document.querySelector('[data-fig="network"]');
            return f.dataset.ready || f.dataset.error || f.querySelector('.tn-msg');
          }, null, {timeout: 10000});
          const ready = await net.getAttribute('data-ready');
          if (ready) {
            assert.equal(Number(ready), graph.nodes.length, 'every node drawn');
            const canvas = page.locator('.tn-canvas');
            await canvas.focus();
            await page.keyboard.press('ArrowRight');
            const tip = page.locator('.tn-tip');
            await tip.waitFor({state: 'visible'});
            assert.match(await tip.textContent(), /Standing \d/);
            assert.match(await tip.textContent(), /Band: (trusted|proven|signed)/);
            await page.keyboard.press('Escape');
            assert.ok(await tip.isHidden(), 'Escape hides the agent');
          }
          assert.ok(await page.locator('#network-live li').count() > 0, 'the text list of the network');
        } else {
          assert.match(await net.textContent(), /No trust run with standing has finished yet|network is empty/);
        }
      } else {
        assert.match(await page.locator('#tx').textContent(), /Trust estimates are not published on this server yet/);
      }

      // A click-fuzz on the stake figure.
      if (run === runs[1] || process.env.TX_FUZZ_ALL) {
        const controls = page.locator('#tx [data-fig="stake"] input:visible');
        const end = Date.now() + FUZZ_MS;
        let presses = 0, seed = 7;
        const rnd = () => { seed = (seed * 1103515245 + 12345) % 2147483648; return seed / 2147483648; };
        while (Date.now() < end) {
          const n = await controls.count();
          if (!n) break;
          const c = controls.nth(Math.floor(rnd() * n));
          try {
            if (await c.getAttribute('type') === 'range') { await c.focus(); await page.keyboard.press(['ArrowRight', 'ArrowLeft', 'End', 'Home', 'PageUp'][Math.floor(rnd() * 5)]); }
            else await c.check({timeout: 500, force: true});
            presses++;
          } catch (_) { /* a control that hid mid-press */ }
          if (presses % 25 === 0) await sane(page, 'fuzz ' + tag);
        }
        await sane(page, 'after fuzz ' + tag);
        assert.ok(presses > 10, 'the fuzz pressed controls');
      }

      // /trust/network: the network alone.
      if (graph) {
        await page.goto(origin + '/trust/network');
        await page.waitForLoadState('networkidle');
        assert.equal(await page.locator('h1').textContent(), 'The trust network');
        assert.equal(await page.locator('[data-fig="stake"]').count(), 0);
        assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), `/trust/network overflows (${tag})`);
      }

      for (const [method, url] of requests) {
        assert.equal(url.origin, new URL(origin).origin, 'the page requests only its own origin');
        assert.equal(method, 'GET', 'the page never writes');
        assert.ok(!/^\/(w|w64|c64|v1\/command)/.test(url.pathname), 'the page never writes');
      }
      assert.deepEqual(errors, [], 'no script errors (' + tag + ')');
      await context.close();
    }
    console.log('trust explainer: the short page, the stake figure, the network (drawn, keyboard, text list), themes, 320/390/1280px, fuzz and read-only requests pass');
  } finally {
    await browser.close();
  }
})().catch(error => { console.error(error); process.exit(1); });
