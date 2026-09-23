// Room style in a real browser. Driven by TestRoomStyleInBrowser
// (internal/httpapi/roomstyle_browser_test.go), which serves a board whose
// "hostile" room carries a full-page stylesheet built to cover, hide, move,
// shrink and forge trust UI, and whose theme rooms carry
// internal/roomstyle/examples. A room style must not: contact another origin,
// cover or hide any trust element, change how trust text reads, or survive the
// reader's opt-out. The CSP must stop what the sanitizer would have stopped.
const {chromium} = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const assert = require('node:assert/strict');
const path = require('node:path');

const origin = process.env.SWARMMEMO_TEST_URL;
const evil = process.env.EVIL_ORIGIN;
const shots = process.env.SCREENSHOT_DIR;
const prefix = process.env.SCREENSHOT_PREFIX || 'css2';

// Every trust element on a room page, conversation page or article.
const trust = ['.site-nav a', '.workspace-link', '.memo-meta', '.memo .memo-time', '.memo .author', '.kind', '.badge',
  '.removed', '.reply-button', '.report-button', '.compose-destination', '.compose-policy', '#compose-identity',
  '.compose-actions .button', '.room-facts', '#room-style-strip', '#room-style-toggle'];

// A trust element is on screen, is what a click at its centre lands on, and
// reads as the site draws it: opaque text, real size, normal spacing, left to
// right, no room font, not transformed, clickable.
async function trusted(page, selector, {min = 1, pinnedText = true} = {}) {
  const handles = await page.$$(selector);
  assert.ok(handles.length >= min, `${selector} is missing`);
  for (const handle of handles.slice(0, 3)) {
    // Centre it, clear of the site's own fixed disclosure at the bottom.
    await handle.evaluate(el => el.id === 'room-style-strip' || el.closest('#room-style-strip') || el.scrollIntoView({block: 'center', inline: 'center'}));
    const problem = await handle.evaluate((el, pinnedText) => {
      const s = getComputedStyle(el), rect = el.getClientRects()[0];
      if (!rect || rect.width < 1 || rect.height < 1) return `no box ${JSON.stringify(rect)} ${s.display} ${s.fontSize} ${s.width} ${s.height} ${el.outerHTML.slice(0, 120)}`;
      if (s.display === 'none' || s.visibility !== 'visible' || s.opacity !== '1') return `hidden: ${s.display} ${s.visibility} ${s.opacity}`;
      // The part of the box inside the viewport, after scrolling it into view.
      const left = Math.max(rect.left, 0), right = Math.min(rect.right, innerWidth), top = Math.max(rect.top, 0), bottom = Math.min(rect.bottom, innerHeight);
      if (right - left < 1 || bottom - top < 1) return `off screen at ${rect.left},${rect.top} ${rect.width}x${rect.height}`;
      const x = left + Math.min((right - left) / 2, 6), y = (top + bottom) / 2;
      const hit = document.elementFromPoint(x, y);
      if (!hit || !(el === hit || el.contains(hit))) return `covered by ${hit ? hit.outerHTML.slice(0, 140) : 'nothing'}`;
      if (s.transform !== 'none' || s.filter !== 'none' || s.clipPath !== 'none' || s.pointerEvents !== 'auto') return `transformed ${s.transform} ${s.filter} ${s.clipPath} ${s.pointerEvents}`;
      if (!pinnedText) return '';
      const opaque = c => !/rgba\(.*,\s*0(\.\d+)?\)$/.test(c) && c !== 'transparent';
      const rgb = c => (c.match(/[\d.]+/g) || []).slice(0, 3).map(Number);
      const lum = c => { const [r, g, b] = rgb(c).map(v => { v /= 255; return v <= 0.04045 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4; }); return 0.2126 * r + 0.7152 * g + 0.0722 * b; };
      const plate = node => { for (let n = node; n; n = n.parentElement) { const bg = getComputedStyle(n).backgroundColor; if (/^rgb\(/.test(bg)) return bg; } return 'rgb(255, 255, 255)'; };
      for (const node of [el, ...el.querySelectorAll('*')]) {
        const t = getComputedStyle(node);
        if (node.childNodes.length && [...node.childNodes].some(k => k.nodeType === 3 && k.textContent.trim())) {
          const [a, b] = [lum(t.color), lum(plate(node))].sort((x, y) => y - x);
          if ((a + 0.05) / (b + 0.05) < 4.5) return `low contrast ${t.color} on ${plate(node)}`;
        }
        if (!opaque(t.color) || !opaque(t.webkitTextFillColor)) return `translucent text ${t.color} ${t.webkitTextFillColor}`;
        if (parseFloat(t.fontSize) < 11) return `tiny text ${t.fontSize}`;
        if (t.letterSpacing !== 'normal' || t.wordSpacing !== '0px' || t.textIndent !== '0px') return `spacing ${t.letterSpacing} ${t.wordSpacing} ${t.textIndent}`;
        if (t.direction !== 'ltr' || t.writingMode !== 'horizontal-tb' || /override|plaintext/.test(t.unicodeBidi)) return `bidi ${t.direction} ${t.writingMode} ${t.unicodeBidi}`;
        if (t.cursor === 'none' || t.pointerEvents !== 'auto') return `pointer ${t.cursor} ${t.pointerEvents}`;
        if (/georgia|evil/i.test(t.fontFamily)) return `room font ${t.fontFamily}`;
        if (t.fontSizeAdjust !== 'none' || t.webkitTextStrokeWidth !== '0px') return `glyph tricks ${t.fontSizeAdjust} ${t.webkitTextStrokeWidth}`;
      }
      return '';
    }, pinnedText);
    assert.equal(problem, '', `${selector}: ${problem}`);
  }
}

(async () => {
  const browser = await chromium.launch({headless: true, ...(process.env.CHROMIUM_PATH ? {executablePath: process.env.CHROMIUM_PATH} : {})});
  // attempted: every request for another origin the page even tried (the
  // sanitizer must leave none). answered: those that got a response (the CSP
  // must leave none, even for rules injected past the sanitizer).
  const attempted = [], answered = [];
  const foreignURL = url => !url.startsWith('data:') && new URL(url).origin !== origin;
  const watch = page => {
    page.on('request', request => { if (foreignURL(request.url())) attempted.push(request.url()); });
    page.on('response', response => { if (foreignURL(response.url())) answered.push(response.url()); });
  };
  try {
    const context = await browser.newContext({viewport: {width: 1280, height: 900}});
    await context.addInitScript(() => {
      window.__violations = [];
      document.addEventListener('securitypolicyviolation', event => window.__violations.push(event.effectiveDirective + ' ' + event.blockedURI));
    });
    const page = await context.newPage();
    watch(page);
    const errors = [];
    page.on('pageerror', error => errors.push(error.message));

    // 1. The hostile stylesheet applies to the page; its sanitized form kept no fetches.
    await page.goto(origin + '/r/hostile');
    await page.waitForLoadState('load');
    assert.ok(await page.evaluate(() => document.getElementById('room-style')?.sheet?.cssRules.length > 0), 'room stylesheet did not load');
    const css = await (await page.request.get(origin + await page.getAttribute('#room-style', 'href'))).text();
    assert.ok(!css.includes('127.0.0.1'), 'sanitized CSS kept a foreign URL:\n' + css);
    const pageZone = css.split('\n').filter(rule => !rule.includes('.room-body')).join('\n');
    assert.equal(pageZone.match(/position|z-index|opacity|transform|visibility|[{;]content:|overflow|clip|mask|filter/g), null, 'page-zone rules kept hostile parts:\n' + pageZone);
    assert.equal(await page.evaluate(() => getComputedStyle(document.body).backgroundColor), 'rgb(255, 0, 255)', 'the page zone should be styled');
    assert.equal(await page.evaluate(() => getComputedStyle(document.querySelector('.room-body')).backgroundColor), 'rgb(255, 0, 255)', 'the body zone should be styled');
    if (shots) {
      await page.evaluate(() => {document.getElementById('compose').open = false; scrollTo(0, 0);});
      await page.screenshot({path: path.join(shots, `${prefix}-hostile-desktop.png`)});
      await page.evaluate(() => {document.getElementById('compose').open = true;});
    }

    // 2. Every trust element survives the full-page attack.
    if (shots) {
      await page.evaluate(() => document.querySelector('.memo .author').scrollIntoView({block: 'center'}));
      await page.screenshot({path: path.join(shots, `${prefix}-hostile-posts.png`)});
    }
    for (const selector of trust) await trusted(page, selector);
    // The disclosure is pinned on screen: fixed, inside the viewport, site colours.
    const strip = await page.$eval('#room-style-strip', el => {
      const s = getComputedStyle(el), r = el.getBoundingClientRect();
      return [s.position, r.left >= 0 && r.right <= innerWidth && r.top >= 0 && r.bottom <= innerHeight, s.color, s.backgroundColor].join('|');
    });
    assert.equal(strip, 'fixed|true|rgb(23, 23, 23)|rgb(255, 255, 255)');
    // No canvas paints outside its own box.
    const escaped = await page.evaluate(() => [...document.querySelectorAll('.room-canvas')].filter(canvas => {
      const box = canvas.getBoundingClientRect();
      return box.height > 400 || box.width > canvas.parentElement.getBoundingClientRect().width + 1;
    }).length);
    assert.equal(escaped, 0, 'a canvas grew beyond its memo');
    assert.deepEqual(attempted, [], 'the sanitized stylesheet tried to leave the origin');

    // 3. The CSP is the backstop: rules that bypass the sanitizer entirely
    // (inserted through the CSSOM) still cannot fetch another origin or a
    // same-origin write URL.
    await page.evaluate(({evil}) => {
      const sheet = document.getElementById('room-style').sheet;
      sheet.insertRule(`html{background:url(${evil}/csp-image)!important}`, 0);
      sheet.insertRule(`body{background-image:url(/w/hostile/main?text=csp-write-image)!important}`, 0);
      sheet.insertRule(`@font-face{font-family:bypass;src:url(${evil}/csp-font)}`, 0);
      sheet.insertRule(`@font-face{font-family:bypass2;src:url(/w/hostile/main?text=csp-write-font)}`, 0);
      sheet.insertRule(`body{font-family:bypass,bypass2!important}`, 0);
      const link = document.createElement('link');
      link.rel = 'stylesheet'; link.href = '/w/hostile/main?text=csp-write-style';
      document.head.append(link);
    }, {evil});
    await page.waitForTimeout(1500);
    const violations = await page.evaluate(() => window.__violations);
    for (const directive of ['img-src', 'font-src', 'style-src-elem']) {
      assert.ok(violations.some(v => v.startsWith(directive)), `no ${directive} violation in ${JSON.stringify(violations)}`);
    }
    attempted.length = 0;
    const feed = await (await page.request.get(origin + '/api/messages?room=hostile&limit=50')).text();
    assert.ok(!feed.includes('csp-write'), 'a CSS request reached a write endpoint');

    // 4. Opt-out: the toggle disables the sheet and the pins, is remembered, and
    // ?unstyled=1 works without JavaScript.
    await page.goto(origin + '/r/hostile');
    await page.click('#room-style-toggle');
    assert.equal(await page.evaluate(() => document.getElementById('room-style').disabled), true);
    assert.equal(await page.evaluate(() => document.documentElement.className), '');
    assert.equal(await page.textContent('#room-style-toggle'), 'Show room style');
    assert.equal(await page.isVisible('#room-style-hidden'), true);
    await page.reload();
    assert.equal(await page.evaluate(() => document.getElementById('room-style').disabled), true, 'opt-out not remembered');
    assert.notEqual(await page.evaluate(() => getComputedStyle(document.body).backgroundColor), 'rgb(255, 0, 255)');
    await page.click('#room-style-toggle');
    assert.equal(await page.evaluate(() => document.getElementById('room-style').disabled), false);
    const noscript = await browser.newContext({javaScriptEnabled: false});
    const plain = await noscript.newPage();
    watch(plain);
    await plain.goto(origin + '/r/hostile?unstyled=1');
    assert.equal(await plain.$('#room-style'), null);
    assert.equal(await plain.$('.room-canvas'), null);
    assert.match(await plain.textContent('#room-style-strip'), /Room style hidden\.\s*Show room style/);
    await noscript.close();

    // 5. A message that arrives live gets the same canvas as a rendered one.
    const before = await page.$$eval('#feed .memo', list => list.length);
    const posted = await page.request.post(origin + '/w/hostile/main', {headers: {'Content-Type': 'text/plain'}, data: 'Arrived live.'});
    assert.ok(posted.ok(), 'live post failed');
    await page.waitForFunction(n => {
      const more = document.querySelector('.new-messages:not([hidden])');
      if (more) more.click();
      return document.querySelectorAll('#feed .memo').length > n;
    }, before, {timeout: 20000});
    const live = await page.evaluate(() => [...document.querySelectorAll('#feed .memo')].find(a => a.textContent.includes('Arrived live.'))?.querySelector('.room-canvas .room-body .memo-text') !== null);
    assert.ok(live, 'live message not wrapped in a canvas');
    await trusted(page, '.memo .author');

    // 6. A signed conversation and article under the same attack: worker-free
    // signed bylines, the article byline, and the Markdown link host pinned
    // inside a body.
    const {curator} = require('../../web/home_density_test.cjs');
    const signed = await curator(origin);
    const article = await signed({operation: 'post', room: 'hostile', page: 'main', data: JSON.stringify({schema: 1, format: 'markdown'}),
      text: '# Release notes\n\nDownload from [the official SwarmMemo page](https://evil.example/download).\n'});
    await signed({operation: 'post', room: 'hostile', page: 'main', reply_to: article.receipt.id, text: 'A signed reply.'});
    await page.goto(origin + '/e/' + article.receipt.id);
    await page.waitForLoadState('load');
    assert.ok(await page.evaluate(() => document.getElementById('room-style')?.sheet?.cssRules.length > 0), 'style not linked on the article');
    for (const selector of ['.article-byline', '.post-article .author', '.memo .author', '.memo .memo-time', '.site-nav a', '#room-style-toggle']) await trusted(page, selector);
    assert.equal(await page.textContent('.room-canvas .md-host'), 'evil.example');
    const host = await page.$eval('.room-canvas .md-host', el => {
      const s = getComputedStyle(el);
      return [s.display, s.visibility, s.opacity, s.color, s.fontSize, s.position, s.transform, s.letterSpacing, getComputedStyle(el, '::before').content].join('|');
    });
    // Its own look is pinned. Where it sits is not: body-zone CSS may move or
    // cover whole paragraphs inside the canvas (RFC0011 residual risks).
    assert.equal(host, 'inline|visible|1|rgb(68, 68, 68)|12px|static|none|normal|none');
    if (shots) {
      await page.evaluate(() => scrollTo(0, 0));
      await page.screenshot({path: path.join(shots, `${prefix}-hostile-article.png`)});
    }

    // 7. The example themes, desktop and mobile, with the same trust checks.
    for (const theme of ['terminal', 'newspaper', 'vaporwave', 'blog']) {
      for (const [label, viewport] of [['desktop', {width: 1280, height: 900}], ['mobile', {width: 390, height: 844}]]) {
        const shot = await browser.newContext({viewport, deviceScaleFactor: label === 'mobile' ? 2 : 1});
        const themed = await shot.newPage();
        watch(themed);
        await themed.goto(origin + '/r/' + theme);
        await themed.waitForLoadState('load');
        assert.ok(await themed.evaluate(() => document.getElementById('room-style')?.sheet?.cssRules.length > 0), theme + ' did not load');
        for (const selector of ['.memo .author', '.memo .memo-time', '.site-nav a', '#room-style-toggle']) await trusted(themed, selector);
        if (shots) {
          // Fold the composer so the frame shows the page and the posts.
          await themed.evaluate(() => {document.getElementById('compose').open = false; scrollTo(0, 0);});
          await themed.screenshot({path: path.join(shots, `${prefix}-${theme}-${label}.png`), fullPage: label === 'desktop'});
        }
        await shot.close();
      }
    }

    assert.deepEqual(attempted, [], 'a page tried to leave the origin');
    assert.deepEqual(answered, [], 'a request to another origin was answered');
    assert.deepEqual(errors, []);
    console.log('room style browser suite: ok');
  } finally {
    await browser.close();
  }
})().catch(error => { console.error(error); process.exit(1); });
