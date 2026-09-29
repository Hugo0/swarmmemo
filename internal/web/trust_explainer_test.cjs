// Isolated local-server browser regression for /trust, the explorable
// explanation of the allowance and trust model. Same environment as
// browser_test.cjs. The page is served only while ALLOWANCE_LEDGER or TRUST is
// on; against a server with both off the suite checks the 404 and stops.
//
// Every chapter's controls are driven by keyboard or click, in light and dark,
// with and without reduced motion, at 320, 390 and 1280 px. Then a click-fuzz
// (TX_FUZZ_MS, default 4 s) presses random controls and checks that no number
// on the page or in a drawing is NaN, infinite or negative, and that no script
// error was thrown. #tx[data-bad] counts every non-finite value the figures
// caught; it must stay absent.
const {chromium} = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const assert = require('node:assert/strict');
const FUZZ_MS = Number(process.env.TX_FUZZ_MS || 4000);

// Nothing on the page reads NaN, Infinity or undefined, no drawing has a
// negative size, and the figures caught no bad number.
const sane = async (page, where) => {
  const problems = await page.evaluate(() => {
    const out = [];
    const root = document.getElementById('tx');
    if (root.dataset.bad) out.push('data-bad=' + root.dataset.bad);
    const text = root.innerText;
    for (const word of ['NaN', 'Infinity', 'undefined']) if (text.includes(word)) out.push('text has ' + word);
    root.querySelectorAll('svg *').forEach(n => {
      for (const a of n.getAttributeNames()) {
        const v = n.getAttribute(a);
        if (/NaN|Infinity|undefined/.test(v)) out.push(n.nodeName + ' ' + a + '=' + v);
        if (['width', 'height', 'r', 'rx', 'ry'].includes(a) && Number(v) < 0) out.push(n.nodeName + ' ' + a + '=' + v);
      }
      if (n.textContent && n.childElementCount === 0 && /NaN|Infinity|undefined/.test(n.textContent)) out.push('svg text ' + n.textContent);
    });
    root.querySelectorAll('output,[data-out]').forEach(n => {
      if (n.closest('[data-fig="vouch"]')) return; // losses are negative by design
      if (/(^|[\s(:])[-−]\d/.test(n.textContent)) out.push('negative ' + n.dataset.out + ': ' + n.textContent);
    });
    root.querySelectorAll('[style]').forEach(n => { if (/NaN|-\d/.test(n.getAttribute('style').replace(/--[\w-]+/g, ''))) out.push('style ' + n.getAttribute('style')); });
    return out;
  });
  assert.deepEqual(problems, [], where);
};

// Scroll a step to the activation band, as a reader would.
const toStep = async (page, chapter, i) => {
  await page.evaluate(([id, i]) => {
    const el = document.querySelectorAll('#' + id + ' .tx-step')[i];
    const band = matchMedia('(max-width: 959px)').matches ? 0.7 : 0.5;
    window.scrollTo(0, el.getBoundingClientRect().top + scrollY - innerHeight * band + 16);
  }, [chapter, i]);
  await page.waitForFunction(([id, i]) => document.querySelectorAll('#' + id + ' .tx-step')[i].classList.contains('is-active'), [chapter, i]);
};
const out = (page, fig, name) => page.locator(`[data-fig="${fig}"] [data-out="${name}"]`).first();
const settle = (page, reduced) => page.waitForTimeout(reduced ? 60 : 3200);

(async () => {
  const browser = await chromium.launch({headless: true, ...(process.env.CHROMIUM_PATH ? {executablePath: process.env.CHROMIUM_PATH} : {}), args: process.env.PLAYWRIGHT_NO_SANDBOX === 'true' ? ['--no-sandbox'] : []});
  const origin = process.env.SWARMMEMO_TEST_URL || 'http://127.0.0.1:8089';
  try {
    const probe = await (await browser.newContext()).request.get(origin + '/trust', {headers: {Accept: 'text/html'}});
    if (probe.status() === 404) {
      console.log('trust explainer: /trust is off on this server (404), as with every RFC0012 flag off');
      return;
    }
    assert.equal(probe.status(), 200);
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
      // A failed load is checked by its response below (the lookup of an unknown agent is a 404 by design).
      page.on('console', m => { if (m.type() === 'error' && !/^Failed to load resource/.test(m.text())) errors.push(m.text()); });
      page.on('response', r => { if (r.status() >= 400 && !/\/api\/agent\/no-such-agent\/trust$/.test(r.url())) errors.push(r.status() + ' ' + r.url()); });
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
      // Every figure is described in text, and every control has a name.
      const unnamed = await page.evaluate(() => [...document.querySelectorAll('#tx button, #tx input')].filter(n => {
        const label = n.labels && n.labels.length ? n.labels[0].textContent.trim() : '';
        return !(label || n.getAttribute('aria-label') || n.textContent.trim());
      }).map(n => n.outerHTML));
      assert.deepEqual(unnamed, [], 'controls without a name');
      assert.equal(await page.locator('#tx svg.tx-svg:not([aria-labelledby])').count(), 0, 'a drawing without a title and description');

      // 1. You arrive: the first call promises a share and leaves the pool; a post spends from both.
      await toStep(page, 'arrive', 1);
      const pool = async () => (await out(page, 'arrive', 'read').textContent());
      const call = page.getByRole('button', {name: 'Make your first call'});
      await call.focus();
      await page.keyboard.press('Enter');
      await page.waitForFunction(() => /promised/.test(document.querySelector('[data-fig="arrive"] [data-out="read"]').textContent));
      assert.match(await pool(), /did not drop: still 60\.8 MB/);
      await toStep(page, 'arrive', 2);
      await page.getByRole('button', {name: 'Post 1 MB'}).click();
      assert.match(await pool(), /dropped by exactly that: 59\.8 MB left/);
      await toStep(page, 'arrive', 3);
      await page.getByRole('button', {name: 'Skip to midnight'}).click();
      assert.match(await pool(), /00:00 UTC/);
      await sane(page, 'arrive ' + tag);

      // 2. A million of you: the log slider by keyboard, and the day-old keys.
      await toStep(page, 'million', 0);
      const keys = page.locator('[data-fig="million"] [data-in="keys"]');
      await keys.focus();
      await page.keyboard.press('End');
      await page.waitForFunction(() => document.querySelector('[data-fig="million"] [data-out="each"]').textContent === '67 bytes');
      assert.equal(await out(page, 'million', 'keys').textContent(), '1,000,000');
      assert.match(await out(page, 'million', 'line').textContent(), /1,000,016 keys/);
      await toStep(page, 'million', 2);
      await page.getByRole('checkbox', {name: 'Make keys wait a day'}).check();
      assert.match(await out(page, 'million', 'line').textContent(), /a day old, and the split is the same: 67 bytes/);
      await sane(page, 'million ' + tag);

      // 3. Pour the day: pour, drag the clock, flood; trusted and proven keep what they ask for.
      await toStep(page, 'pour', 0);
      await page.getByRole('button', {name: 'Pour the day'}).click();
      await page.waitForFunction(() => /^Poured/.test(document.querySelector('[data-fig="pour"] [data-out="read"]').textContent), null, {timeout: 8000});
      await toStep(page, 'pour', 2);
      await settle(page, reduced);
      const clock = page.locator('[data-fig="pour"] [data-in="clock"]');
      await clock.focus();
      await page.keyboard.press('End');
      assert.equal(await out(page, 'pour', 'clock').textContent(), '24:00 UTC');
      assert.match(await out(page, 'pour', 'read').textContent(), /every agent has got what it asked for/);
      await toStep(page, 'pour', 3);
      await page.getByRole('radio', {name: 'A million keys'}).check({force: true});
      await page.waitForFunction(() => /million keys drank/.test(document.querySelector('[data-fig="pour"] [data-out="read"]').textContent));
      assert.match(await out(page, 'pour', 'read').textContent(), /Trusted agents got 100% of what they asked for, proven 100%/);
      await page.getByRole('radio', {name: 'A million networks'}).check({force: true});
      await page.waitForFunction(() => /million networks drank/.test(document.querySelector('[data-fig="pour"] [data-out="read"]').textContent));
      await sane(page, 'pour ' + tag);

      // 4. Collateral: keys on one root add nothing; age and answered days cross the proven line.
      await toStep(page, 'collateral', 1);
      await settle(page, reduced);
      const price = async () => Number((await out(page, 'collateral', 'read').textContent()).match(/To fake you: ([\d,]+)/)[1].replace(/,/g, ''));
      const one = await price();
      assert.ok(one > 0, 'a verified domain has a price');
      await toStep(page, 'collateral', 2);
      await settle(page, reduced);
      assert.equal(await price(), one, 'four more keys on one domain add nothing');
      await toStep(page, 'collateral', 3);
      await settle(page, reduced);
      assert.equal(await price(), one, 'unverified links add nothing');
      await toStep(page, 'collateral', 4);
      await page.waitForFunction(() => /You are proven/.test(document.querySelector('[data-fig="collateral"] [data-out="read"]').textContent), null, {timeout: 8000});
      const age = page.locator('[data-fig="collateral"] [data-in="age"]');
      await age.focus();
      await page.keyboard.press('Home');
      assert.match(await out(page, 'collateral', 'read').textContent(), /To fake you: 0\b/);
      await page.getByRole('checkbox', {name: 'Verify a domain'}).uncheck();
      await sane(page, 'collateral ' + tag);

      // 5. Trust flows: counting gives the ring nearly everything, flow gives it nothing, whatever its size.
      await toStep(page, 'flows', 0);
      await page.getByRole('radio', {name: 'Counting'}).check({force: true});
      assert.notEqual(await out(page, 'flows', 'ring-share').textContent(), '0%');
      await toStep(page, 'flows', 2);
      await settle(page, reduced);
      assert.equal(await out(page, 'flows', 'ring-share').textContent(), '0%');
      const ring = page.locator('[data-fig="flows"] [data-in="ring"]');
      await ring.focus();
      await page.keyboard.press('End');
      assert.equal(await out(page, 'flows', 'ring').textContent(), '1,000,000');
      assert.equal(await out(page, 'flows', 'ring-share').textContent(), '0%');
      if (reduced) assert.equal(await page.locator('[data-fig="flows"] .particle').count(), 0, 'reduced motion draws no moving particles');
      await sane(page, 'flows ' + tag);

      // 6. Would you sell your vouch: counted trust pays; liability makes the average seller lose.
      await toStep(page, 'vouch', 0);
      await page.getByRole('button', {name: /^Sell/}).click();
      await page.waitForFunction(() => /26 times/.test(document.querySelector('[data-fig="vouch"] [data-out="read"]').textContent), null, {timeout: 8000});
      await toStep(page, 'vouch', 2);
      assert.match(await out(page, 'vouch', 'read').textContent(), /offers 50 a day/);
      await page.getByRole('button', {name: /^Sell/}).click();
      await page.waitForFunction(() => /On average a seller ends −1,327/.test(document.querySelector('[data-fig="vouch"] [data-out="read"]').textContent), null, {timeout: 8000});
      await page.getByRole('button', {name: 'Keep'}).click();
      await page.waitForFunction(() => /keep your vouch/.test(document.querySelector('[data-fig="vouch"] [data-out="read"]').textContent), null, {timeout: 8000});
      await toStep(page, 'vouch', 4);
      await page.getByRole('radio', {name: 'Only your friends'}).check({force: true});
      assert.match(await out(page, 'vouch', 'read').textContent(), /you earn nothing/);
      await page.getByRole('radio', {name: "Agents you don't control"}).check({force: true});
      assert.match(await out(page, 'vouch', 'read').textContent(), /you earn a dividend/);
      await sane(page, 'vouch ' + tag);

      // 7. Rings: trading lifts the group under PageRank-style scoring only; the ceiling ignores the key count.
      await toStep(page, 'rings', 0);
      await page.getByRole('checkbox', {name: 'Trade endorsements'}).check();
      assert.match(await out(page, 'rings', 'read').textContent(), /Trading moved the group/);
      await toStep(page, 'rings', 2);
      const bought = page.locator('[data-fig="rings"] [data-in="bought"]');
      await bought.focus();
      await page.keyboard.press('End');
      assert.equal(await out(page, 'rings', 'bought').textContent(), '20');
      const capture = async () => (await out(page, 'rings', 'read').textContent()).match(/let in ([\d.]+) fair shares, against a ceiling of ([\d.]+)/);
      const [, got20, ceil20] = await capture();
      assert.ok(Number(got20) <= Number(ceil20) + 1e-9, 'the ring gets at most the ceiling');
      await toStep(page, 'rings', 3);
      await settle(page, reduced);
      const rkeys = page.locator('[data-fig="rings"] [data-in="keys"]');
      await rkeys.focus();
      await page.keyboard.press('Home');
      const [, gotFew] = await capture();
      await page.keyboard.press('End');
      const [, gotMany] = await capture();
      assert.equal(gotFew, gotMany, 'the capture does not depend on the number of keys');
      await sane(page, 'rings ' + tag);

      // 8. Bad days: levers change what the flood gets.
      await toStep(page, 'levers', 0);
      await settle(page, reduced);
      assert.match(await out(page, 'levers', 'read').textContent(), /The flood took/);
      await toStep(page, 'levers', 1);
      await page.getByRole('checkbox', {name: 'Pause new keys'}).check();
      assert.match(await out(page, 'levers', 'read').textContent(), /new keys get nothing/);
      await page.getByRole('radio', {name: 'Anonymous networks'}).check({force: true});
      await page.getByRole('checkbox', {name: 'Signed keys only'}).check();
      assert.match(await out(page, 'levers', 'read').textContent(), /flood is refused/);
      await page.getByRole('checkbox', {name: 'Cut the budget by half'}).check();
      assert.equal(await out(page, 'levers', 'budget').textContent(), '64 MB');
      await sane(page, 'levers ' + tag);

      // 9. Check: an unknown agent reads the API and says so; the input is escaped into the path.
      const lookup = page.locator('#tx-agent');
      if (await lookup.count()) {
        await lookup.fill('no-such-agent');
        await page.getByRole('button', {name: 'Show trust parts'}).click();
        await page.locator('.tx-parts .tx-error').waitFor();
        assert.match(await page.locator('.tx-parts').textContent(), /No public agent|could not be read/);
      }

      // A click-fuzz: random controls, pressed fast, in any order.
      if (run === runs[1] || process.env.TX_FUZZ_ALL) {
        const controls = page.locator('#tx .tx-fig button:visible, #tx .tx-fig input:visible');
        const end = Date.now() + FUZZ_MS;
        let presses = 0;
        let seed = 7;
        const rnd = () => { seed = (seed * 1103515245 + 12345) % 2147483648; return seed / 2147483648; };
        while (Date.now() < end) {
          const n = await controls.count();
          if (!n) break;
          const c = controls.nth(Math.floor(rnd() * n));
          const type = await c.getAttribute('type');
          try {
            await c.scrollIntoViewIfNeeded({timeout: 500});
            if (type === 'range') { await c.focus(); await page.keyboard.press(['ArrowRight', 'ArrowLeft', 'End', 'Home', 'PageUp'][Math.floor(rnd() * 5)]); }
            else await c.click({timeout: 500, force: type === 'radio'});
            presses++;
          } catch (_) { /* a control that scrolled away or was disabled mid-press */ }
          if (presses % 25 === 0) await sane(page, 'fuzz ' + tag);
        }
        await page.waitForTimeout(reduced ? 50 : 3000);
        await sane(page, 'after fuzz ' + tag);
        assert.ok(presses > 10, 'the fuzz pressed controls');
      }

      for (const [method, url] of requests) {
        assert.equal(url.origin, new URL(origin).origin, 'the page requests only its own origin');
        assert.equal(method, 'GET', 'the page never writes');
        assert.ok(!/^\/(w|w64|c64|v1\/command)/.test(url.pathname), 'the page never writes');
      }
      assert.deepEqual(errors, [], 'no script errors (' + tag + ')');
      await context.close();
    }
    console.log('trust explainer: nine chapters, their controls, keyboard, reduced motion, themes, 320/390/1280px, fuzz and read-only requests pass');
  } finally {
    await browser.close();
  }
})().catch(error => { console.error(error); process.exit(1); });
