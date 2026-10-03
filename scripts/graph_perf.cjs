// Load test for /graph: zoom from the universe into the biggest galaxy, down
// to agents and their messages, and at each level pan for three seconds,
// reporting frames per second, points drawn and the bytes each level fetched.
// Run against a disposable server started with GRAPH_DATASETS_FILE pointing at
// a synthetic file (scripts/graph_synthetic.py). Not part of the gate.
//
//   SWARMMEMO_TEST_URL=http://127.0.0.1:PORT PLAYWRIGHT_MODULE=... CHROMIUM_PATH=... \
//     node scripts/graph_perf.cjs            (GPU=1 asks Chrome for the GPU)
const {chromium} = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const origin = process.env.SWARMMEMO_TEST_URL;
const args = process.env.GPU ? ['--enable-gpu', '--ignore-gpu-blocklist', '--use-angle=vulkan', '--enable-features=Vulkan'] : [];

(async () => {
  const browser = await chromium.launch({headless: true, executablePath: process.env.CHROMIUM_PATH, args});
  const page = await browser.newPage({viewport: {width: 1280, height: 900}});
  page.on('pageerror', (e) => console.log('pageerror', e.message));
  const t0 = Date.now();
  await page.goto(origin + '/graph', {waitUntil: 'load'});
  await page.waitForFunction(() => window.__swarmgraph?.ready, null, {timeout: 120000});
  const renderer = await page.evaluate(() => { const gl = document.querySelector('#graph-canvas canvas').getContext('webgl2'); const x = gl && gl.getExtension('WEBGL_debug_renderer_info'); return x ? gl.getParameter(x.UNMASKED_RENDERER_WEBGL) : 'unknown'; });
  console.log(`renderer: ${renderer}; first view in ${Date.now() - t0} ms`);
  const box = await page.locator('#graph-stage').boundingBox();
  const bytes = () => page.evaluate(() => performance.getEntriesByType('resource').filter((e) => e.name.includes('/api/graph/')).reduce((s, e) => s + (e.encodedBodySize || 0), 0));
  async function measure(label) {
    await page.waitForTimeout(1500);
    const before = await bytes();
    const f0 = await page.evaluate(() => window.__swarmgraph.frames());
    const start = Date.now();
    await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2);
    await page.mouse.down();
    for (let i = 0; i < 60; i++) { const a = i / 60 * Math.PI * 2; await page.mouse.move(box.x + box.width / 2 + Math.cos(a) * 120, box.y + box.height / 2 + Math.sin(a) * 80, {steps: 2}); await page.waitForTimeout(16); }
    await page.mouse.up();
    const secs = (Date.now() - start) / 1000;
    const f1 = await page.evaluate(() => window.__swarmgraph.frames());
    // The same pan driven inside the page (no driver round trips): frames
    // per second of requestAnimationFrame while the camera moves every frame.
    const inPage = await page.evaluate(() => new Promise((done) => {
      const c = document.querySelector('#graph-canvas canvas'), r = c.getBoundingClientRect();
      let n = 0; const t0 = performance.now();
      const step = (t) => {
        n++; const a = (t - t0) / 3000 * Math.PI * 2;
        c.dispatchEvent(new MouseEvent(n === 1 ? 'mousedown' : 'mousemove', { bubbles: true, view: window, clientX: r.left + r.width / 2 + Math.cos(a) * 120, clientY: r.top + r.height / 2 + Math.sin(a) * 80, buttons: 1 }));
        if (t - t0 < 3000) requestAnimationFrame(step); else { window.dispatchEvent(new MouseEvent('mouseup', { bubbles: true, view: window })); done(n / ((t - t0) / 1000)); }
      };
      requestAnimationFrame(step);
    }));
    const d = await page.evaluate(() => window.__swarmgraph.draw());
    console.log(`${label.padEnd(12)} ${inPage.toFixed(1).padStart(6)} fps in page, ${(((f1 - f0) / secs)).toFixed(1).padStart(5)} driven  ${String(d.count).padStart(7)} points  ${String(d.links).padStart(6)} links  level=${d.level}  fetched ${Math.round(((await bytes()) - before) / 1024)} KiB this level`);
  }
  await measure('universe');
  const galaxy = await page.evaluate(() => { const g = window.__swarmgraph; return g.find((n) => n.kind === 20).sort((a, b) => g.node(b).members - g.node(a).members)[0]; });
  await page.evaluate((id) => window.__swarmgraph.goTo(id), galaxy); await page.waitForTimeout(1500);
  await measure('galaxy');
  const community = await page.evaluate((gid) => { const g = window.__swarmgraph; return g.find((n) => n.parent === gid && n.kind === 10).sort((a, b) => g.node(b).members - g.node(a).members)[0]; }, galaxy);
  await page.evaluate((id) => window.__swarmgraph.goTo(id), community); await page.waitForTimeout(2000);
  await measure('community');
  const agent = await page.evaluate(() => { const g = window.__swarmgraph; return g.find((n) => n.kind === 0).sort((a, b) => g.node(b).posts - g.node(a).posts)[0]; });
  await page.evaluate((id) => window.__swarmgraph.goTo(id), agent); await page.waitForTimeout(2000);
  await measure('agent');
  for (const n of [100000, 250000]) {
    await page.evaluate((k) => window.__swarmgraph.stress(k), n);
    await measure(`+${n / 1000}k pts`);
  }
  await page.evaluate(() => window.__swarmgraph.stress(0));
  await browser.close();
})().catch((e) => { console.error(e); process.exitCode = 1; });
