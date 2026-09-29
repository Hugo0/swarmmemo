// /trust: the figures of the explorable explanation of the allowance and trust
// model (RFC0012). Vanilla JS, no dependencies, CSP-clean: no markup is built
// from strings, and sizes are set through attributes or the CSSOM. Chapters 1
// to 8 are models of the published defaults; the only live numbers are the
// server-rendered cells of chapter 9 (data-key/data-value, held to their JSON
// API by the parity test) and the answer of /api/agent/AGENT/trust, whose text
// is only ever written with textContent.
//
// One visual language: an agent is a circle (Glyph), water is blue, trust is
// gold, the attacker is vermilion, a tier is a basin (Cascade). Colours are CSS
// classes, so a theme change needs no redraw (except the canvas of chapter 2).
//
// Every animation reads its own clock (performance.now() inside the frame,
// clamped to [0, 1]); a requestAnimationFrame timestamp can predate the click
// that started it. Every number written to the page or to an SVG attribute
// passes through a finite, non-negative check; a failed check is counted in
// data-bad on #tx, which the browser suite requires to stay absent.
(() => {
  'use strict';
  const root = document.getElementById('tx');
  if (!root) return;
  root.classList.add('tx-js');
  const NS = 'http://www.w3.org/2000/svg';
  const MiB = 1048576, KiB = 1024;
  const media = q => (window.matchMedia ? matchMedia(q) : null);
  const motionQ = media('(prefers-reduced-motion: reduce)');
  const narrowQ = media('(max-width: 959px)');
  const reduced = () => !!(motionQ && motionQ.matches);
  const narrow = () => !!(narrowQ && narrowQ.matches);

  // ---------- numbers ----------
  const clamp = (x, a, b) => Math.min(b, Math.max(a, x));
  let bad = 0;
  const flag = () => { bad++; root.dataset.bad = String(bad); };
  const fin = x => { if (typeof x !== 'number' || !isFinite(x)) { flag(); return 0; } return x; };
  const lerp = (a, b, k) => a + (b - a) * k;
  const ease = t => { t = clamp(t, 0, 1); return t < 0.5 ? 4 * t * t * t : 1 - Math.pow(-2 * t + 2, 3) / 2; };
  const count = n => Math.round(Math.max(0, fin(n))).toLocaleString('en-US');
  // Binary MB and KB, as the server's size() writes them.
  const bytes = n => {
    n = Math.max(0, Math.round(fin(n)));
    if (n >= 10 * MiB) return Math.round(n / MiB) + ' MB';
    if (n >= MiB) return (n / MiB).toFixed(1).replace(/\.0$/, '') + ' MB';
    if (n >= 10 * KiB) return Math.round(n / KiB) + ' KB';
    if (n >= KiB) return (n / KiB).toFixed(1).replace(/\.0$/, '') + ' KB';
    return n + (n === 1 ? ' byte' : ' bytes');
  };
  const pct = x => {
    x = Math.max(0, fin(x));
    if (x <= 0) return '0%';
    if (x >= 0.99995) return '100%';
    const p = x * 100;
    if (p >= 99.5) return p.toFixed(1) + '%';
    if (p >= 10) return Math.round(p) + '%';
    if (p >= 1) return p.toFixed(1).replace(/\.0$/, '') + '%';
    if (p >= 0.1) return p.toFixed(1) + '%';
    return 'under 0.1%';
  };
  const signed = n => (n > 0 ? '+' : n < 0 ? '−' : '') + count(Math.abs(n));
  // Log sliders: 0 is one key, 10 is ten, 60 is a million.
  const logCount = v => Math.max(1, Math.round(Math.pow(10, clamp(fin(Number(v)), 0, 60) / 10)));
  // A small deterministic PRNG, so every reader sees the same illustration.
  const prng = seed => () => {
    seed |= 0; seed = seed + 0x6D2B79F5 | 0;
    let t = Math.imul(seed ^ seed >>> 15, 1 | seed);
    t = t + Math.imul(t ^ t >>> 7, 61 | t) ^ t;
    return ((t ^ t >>> 14) >>> 0) / 4294967296;
  };

  // ---------- DOM ----------
  const NONNEG = {width: 1, height: 1, r: 1, rx: 1, ry: 1};
  const set = (node, attrs) => {
    for (const k in attrs) {
      let v = attrs[k];
      if (typeof v === 'number') {
        v = fin(v);
        if (NONNEG[k] && v < 0) v = 0;
        v = Math.round(v * 100) / 100;
      }
      node.setAttribute(k, v);
    }
    return node;
  };
  const S = (tag, attrs, parent, text) => {
    const n = document.createElementNS(NS, tag);
    if (attrs) set(n, attrs);
    if (text !== undefined) n.textContent = text;
    if (parent) parent.appendChild(n);
    return n;
  };
  const H = (tag, cls, parent, text) => {
    const n = document.createElement(tag);
    if (cls) n.className = cls;
    if (text !== undefined) n.textContent = text;
    if (parent) parent.appendChild(n);
    return n;
  };
  const clear = n => { while (n.firstChild) n.removeChild(n.firstChild); };
  const txt = (n, s) => { if (n && n.textContent !== s) n.textContent = s; };
  const out = (scope, name) => scope.querySelector('[data-out="' + name + '"]');
  const inp = (scope, name) => scope.querySelector('[data-in="' + name + '"]');
  const act = (scope, name) => scope.querySelector('[data-act="' + name + '"]');
  const radios = (scope, name) => [...scope.querySelectorAll('[data-in="' + name + '"]')];
  const radioValue = (scope, name) => { const r = radios(scope, name).find(x => x.checked); return r ? r.value : ''; };
  const setRadio = (scope, name, value) => radios(scope, name).forEach(r => { r.checked = r.value === String(value); });
  const setWidth = (node, f) => { if (node) node.style.width = (clamp(fin(f), 0, 1) * 100).toFixed(2) + '%'; };
  const show = (node, on) => { if (node) node.classList.toggle('hide', !on); };
  // Keep an SVG's title and desc (its text equivalent) first and current.
  const describe = (svg, text) => { const d = svg.querySelector('desc'); if (d) txt(d, text); };
  const resetSvg = svg => { [...svg.childNodes].forEach(n => { if (n.nodeName !== 'title' && n.nodeName !== 'desc') svg.removeChild(n); }); };
  const sizeSvg = (svg, w, h) => { set(svg, {viewBox: '0 0 ' + Math.round(w) + ' ' + Math.round(h), height: Math.round(h)}); };

  // ---------- time ----------
  // A tween over dur ms; its clock starts inside the first frame and is clamped.
  const tween = (dur, frame, done) => {
    if (reduced() || dur <= 0) { frame(1); if (done) done(); return () => {}; }
    let start = -1, id = 0, live = true;
    const tick = () => {
      if (!live) return;
      const now = performance.now();
      if (start < 0) start = now;
      const k = clamp((now - start) / dur, 0, 1);
      frame(k);
      if (k < 1) id = requestAnimationFrame(tick);
      else { live = false; if (done) done(); }
    };
    id = requestAnimationFrame(tick);
    return () => { live = false; cancelAnimationFrame(id); };
  };
  // A frame loop with a clamped delta in seconds, for particles; run only on screen.
  const loop = fn => {
    let id = 0, last = -1, on = false;
    const tick = () => {
      if (!on) return;
      const now = performance.now();
      const dt = last < 0 ? 0 : clamp((now - last) / 1000, 0, 0.05);
      last = now;
      fn(dt);
      id = requestAnimationFrame(tick);
    };
    return {
      start() { if (on || reduced()) return; on = true; last = -1; id = requestAnimationFrame(tick); },
      stop() { on = false; cancelAnimationFrame(id); },
      get running() { return on; },
    };
  };

  // ---------- the agent glyph ----------
  // A ring, filled from the centre by what the agent holds (water or trust).
  const Glyph = (parent, cls, fillCls) => {
    const g = S('g', null, parent);
    const o = S('circle', {class: cls || 'ag', r: 0}, g);
    const i = S('circle', {class: fillCls || 'ag-fill', r: 0}, g);
    return {
      g, o, i,
      at(x, y, r, f) {
        set(o, {cx: x, cy: y, r});
        set(i, {cx: x, cy: y, r: Math.max(0, (r - 2) * Math.sqrt(clamp(fin(f), 0, 1)))});
      },
      cls(c) { o.setAttribute('class', c); },
      fillCls(c) { i.setAttribute('class', c); },
    };
  };

  // ---------- the basin ----------
  // A trough with gently tapered sides; d is its depth.
  const troughPath = (x, y, w, d) => {
    const t = Math.min(5, w / 20), c = Math.min(6, d / 2);
    return 'M' + x + ' ' + y + 'L' + (x + t) + ' ' + (y + d - c) + 'Q' + (x + t + 1) + ' ' + (y + d) + ' ' + (x + t + c) + ' ' + (y + d) +
      'L' + (x + w - t - c) + ' ' + (y + d) + 'Q' + (x + w - t - 1) + ' ' + (y + d) + ' ' + (x + w - t) + ' ' + (y + d - c) + 'L' + (x + w) + ' ' + y;
  };
  const troughFill = (x, y, w, d) => troughPath(x, y, w, d) + 'Z';

  // ---------- the waterfall model (RFC0012 §2.3, published defaults) ----------
  const TIER = ['', 'Trusted', 'Proven', 'Signed', 'Anonymous'];
  const WF = {
    RESERVE: [0, 0.10, 0.10, 0.20], HEAD: 0.5,
    CAP: [0, 16 * MiB, 8 * MiB, 4 * MiB, 4 * MiB], FLOOR: [0, 64 * KiB, 64 * KiB, 64 * KiB, 16 * KiB],
    // A quiet day's honest demand, what each tier asked for in all.
    DEMAND: [0, 2.4 * MiB, 1.6 * MiB, 3.2 * MiB, 1.2 * MiB], AGENTS: [0, 3, 2, 12, 40],
  };
  // One UTC day of the published waterfall, hour by hour: reserves, hourly
  // spill from 01:00, own water first then borrowing from lower tiers only,
  // and a flood that asks for everything it is entitled to at once.
  const simulateDay = o => {
    const levers = o.levers || new Set();
    let B = Math.max(0, fin(o.B));
    if (levers.has('cut-budget')) B = Math.floor(B / 2);
    const demand = WF.DEMAND.slice(), agents = WF.AGENTS.slice();
    const want = [0, 0, 0, 0], size = [0, 0, 0, 0, 0];
    let rem = B;
    for (let t = 1; t <= 3; t++) {
      want[t] = Math.min(B, Math.ceil(demand[t] * (1 + WF.HEAD)) + B * WF.RESERVE[t]);
      if (t === 3 && levers.has('proven-only')) want[t] = 0;
      size[t] = Math.min(rem, want[t]); rem -= size[t];
    }
    size[4] = levers.has('tier4-shrink') ? Math.min(rem, B * 0.1) : rem;
    if (levers.has('signed-only') || levers.has('proven-only')) size[4] = 0;
    rem -= size[4];
    const unallocated = Math.max(0, rem);
    const refused = [false, false, false, levers.has('proven-only'), levers.has('signed-only') || levers.has('proven-only')];
    const ft = o.floodTier === 4 ? 4 : 3;
    let flood = Math.max(0, fin(o.flood || 0));
    let floodBlocked = '';
    if (flood > 0 && refused[ft]) floodBlocked = ft === 4 ? 'refused' : 'nothing';
    else if (flood > 0 && ft === 3 && levers.has('pause-new-keys')) floodBlocked = 'paused';
    const P = t => ({size: size[t], spillIn: 0, spillOut: 0, claimed: 0, lent: 0, honest: 0, flood: 0});
    const pools = [null, P(1), P(2), P(3), P(4)];
    const water = t => pools[t].size + pools[t].spillIn - pools[t].spillOut;
    const avail = t => Math.max(0, water(t) - pools[t].claimed - pools[t].lent);
    const asked = [0, 0, 0, 0, 0], served = [0, 0, 0, 0, 0];
    const floodAsk = flood > 0 && !floodBlocked
      ? flood * clamp(water(ft) / (flood + agents[ft]), WF.FLOOR[ft], WF.CAP[ft]) : 0;
    let floodGot = 0;
    const take = (t, amount, who) => { // own water first, then borrow from the lowest tier up
      let got = 0;
      const from = (u, x) => {
        if (x <= 0) return;
        if (u === t) pools[u].claimed += x; else pools[u].lent += x;
        pools[u][who] += x; got += x;
      };
      from(t, Math.min(amount, avail(t)));
      for (let u = 4; u > t && got < amount; u--) from(u, Math.min(amount - got, avail(u)));
      return got;
    };
    const frames = [];
    const snap = hr => frames.push({hour: hr, floodGot, tiers: [1, 2, 3, 4].map(t => ({
      tier: t, size: size[t], water: water(t), left: avail(t), honest: pools[t].honest, flood: pools[t].flood,
      spilled: pools[t].spillOut, asked: asked[t], served: served[t]}))});
    snap(0);
    for (let hr = 0; hr < 24; hr++) {
      if (hr >= 1) { // the spill at each hourly boundary from 01:00
        const f = hr / 24;
        for (let t = 1; t <= 3; t++) {
          const keep = Math.ceil(Math.max(0, want[t] - pools[t].claimed) * (1 - f));
          const x = Math.max(0, avail(t) - keep);
          pools[t].spillOut += x; pools[t + 1].spillIn += x;
        }
      }
      for (let t = 1; t <= 4; t++) asked[t] += demand[t] / 24;
      for (let t = 1; t <= 4; t++) {
        const hPend = refused[t] ? 0 : Math.max(0, asked[t] - served[t]);
        const fPend = t === ft ? Math.max(0, floodAsk - floodGot) : 0;
        if (hPend + fPend <= 0) continue;
        // Honest agents and the flood of one tier draw side by side.
        let reach = avail(t);
        for (let u = t + 1; u <= 4; u++) reach += avail(u);
        const scale = Math.min(1, reach / (hPend + fPend));
        served[t] += take(t, hPend * scale, 'honest');
        if (fPend > 0) floodGot += take(t, fPend * scale, 'flood');
      }
      snap(hr + 1);
    }
    const cap = [0, 0, 0, 0].map((_, i) => Math.max(size[i + 1], ...frames.map(f => f.tiers[i].water)));
    return {B, size, want, cap, unallocated, frames, flood, ft, floodBlocked, refused, demand, agents};
  };
  // A frame at a fractional hour, and a mix of two frames.
  const mixFrame = (a, b, k) => ({hour: lerp(a.hour, b.hour, k), floodGot: lerp(a.floodGot, b.floodGot, k), tiers: a.tiers.map((x, i) => {
    const y = b.tiers[i], o = {tier: x.tier};
    for (const key of ['size', 'water', 'left', 'honest', 'flood', 'spilled', 'asked', 'served']) o[key] = lerp(x[key], y[key], k);
    return o;
  })});
  const frameAt = (model, hour) => {
    const h = clamp(fin(hour), 0, 24), i = Math.min(23, Math.floor(h));
    return mixFrame(model.frames[i], model.frames[i + 1], h - i);
  };

  // ---------- the cascade: four basins stepping down, the same in chapters 3, 8 and 9 ----------
  const Cascade = (svg, id) => {
    let g = null, geo = null, parts = null, streamLoop = null, dashOffset = 0;
    const layout = (W, Hh) => {
      resetSvg(svg);
      sizeSvg(svg, W, Hh);
      g = S('g', null, svg);
      const labelW = W < 420 ? 92 : 124, top = 30, rowH = (Hh - top - 6) / 4;
      geo = {W, H: Hh, labelW, top, rowH, avail: W - labelW - 4, D: clamp(rowH - 18, 14, 64)};
      const defs = S('defs', null, g);
      parts = {src: S('text', {x: 0, y: 14, class: 't-lbl'}, g), srcNote: S('text', {x: W, y: 14, 'text-anchor': 'end', class: 't-mut'}, g),
        pourStream: S('path', {class: 'stream', d: 'M0 0'}, g), rows: []};
      for (let i = 0; i < 4; i++) {
        const row = {};
        const clip = S('clipPath', {id: id + '-clip-' + i}, defs);
        row.clipPath = S('path', {d: 'M0 0'}, clip);
        row.bg = S('path', {class: 'basin-bg', d: 'M0 0'}, g);
        row.waterG = S('g', {'clip-path': 'url(#' + id + '-clip-' + i + ')'}, g);
        row.water = S('rect', {class: 'water', x: 0, y: 0, width: 0, height: 0}, row.waterG);
        row.floodG = S('g', null, row.waterG);
        row.basin = S('path', {class: 'basin', d: 'M0 0'}, g);
        row.name = S('text', {class: 't-lbl'}, g, TIER[i + 1]);
        row.num = S('text', {class: 't-mut'}, g);
        row.agentsG = S('g', null, g);
        row.more = S('text', {class: 't-mut'}, g);
        row.glyphs = [];
        row.dots = [];
        row.spill = S('path', {class: 'stream-dash', d: 'M0 0'}, g);
        parts.rows.push(row);
      }
      parts.blocked = S('text', {class: 't-att', 'text-anchor': 'end'}, g);
      streamLoop = loop(dt => { dashOffset = (dashOffset - dt * 40) % 100; parts.rows.forEach(r => set(r.spill, {'stroke-dashoffset': dashOffset})); set(parts.pourStream, {'stroke-dashoffset': dashOffset}); });
    };
    // s: {B, cap[4], level[4], fill[4] (0..1 or null), agents[4], flood[4] (dots per basin), num[4], src, srcNote, pourTo (-1..3), spill[3] (0..1), blocked}
    const draw = s => {
      if (!geo) return;
      const {labelW, top, rowH, avail, D} = geo;
      // A basin's width is its capacity; each starts under the one above, so
      // what spills from a right lip falls into the next basin.
      const maxCap = Math.max(1, ...s.cap), OVER = 0.55;
      const cw = s.cap.map(c => Math.max(0.12 * maxCap, c));
      const k = avail / (OVER * (cw[0] + cw[1] + cw[2]) + cw[3]);
      const wid = cw.map(c => c * k), xs = [labelW];
      for (let i = 1; i < 4; i++) xs.push(xs[i - 1] + OVER * wid[i - 1]);
      txt(parts.src, s.src || ''); txt(parts.srcNote, s.srcNote || '');
      const rim = i => top + i * rowH + 12;
      const depth = () => D;
      const x0 = i => xs[i];
      let streaming = false;
      parts.rows.forEach((row, i) => {
        const x = x0(i), y = rim(i), d = depth(i), bw = wid[i];
        row.basin.setAttribute('class', s.cap[i] > 0 ? 'basin' : 'basin basin-closed');
        const shape = troughFill(x, y, bw, d);
        set(row.bg, {d: shape}); set(row.clipPath, {d: shape});
        set(row.basin, {d: troughPath(x, y, bw, d)});
        const lv = s.cap[i] > 0 ? clamp(s.level[i] / s.cap[i], 0, 1) : 0;
        set(row.water, {x, y: y + d * (1 - lv), width: bw, height: d * lv + 1});
        const compact = geo.W < 420;
        set(row.name, {x: 0, y: y + 4});
        txt(row.num, compact ? '' : s.num[i] || ''); set(row.num, {x: 0, y: y + 19});
        const gy = compact ? y + 18 : y + 32;
        // Agents: up to five glyphs, filled by how much of what they asked they got.
        const n = Math.max(0, Math.round(s.agents[i] || 0)), shown = Math.min(5, n);
        while (row.glyphs.length < shown) row.glyphs.push(Glyph(row.agentsG));
        row.glyphs.forEach((gl, j) => {
          gl.g.style.display = j < shown ? '' : 'none';
          if (j < shown) gl.at(6 + j * 13, gy, 5, s.fill[i] === null ? 0 : s.fill[i]);
        });
        txt(row.more, n > shown ? '+' + count(n - shown) : '');
        set(row.more, {x: 6 + shown * 13 - 2, y: gy + 4});
        // The flood: red dots inside the basin, standing in what is left.
        const dots = Math.max(0, Math.round(s.flood[i] || 0));
        while (row.dots.length < dots) row.dots.push(S('circle', {class: 'flood', r: 1.8}, row.floodG));
        const rnd = prng(97 + i);
        row.dots.forEach((dot, j) => {
          const u = rnd(), v = rnd();
          if (j >= dots) { dot.style.display = 'none'; return; }
          dot.style.display = '';
          set(dot, {cx: x + 8 + u * (bw * 0.9 - 8), cy: y + d - 2 - v * Math.max(2, d - 5)});
        });
        // Spill from this basin's right lip into the next.
        if (i < 3) {
          const sp = clamp(fin(s.spill ? s.spill[i] : 0), 0, 1);
          const lx = x + bw - 1, ly = y, ny = rim(i + 1) + depth(i + 1) * (1 - (s.cap[i + 1] > 0 ? clamp(s.level[i + 1] / s.cap[i + 1], 0, 1) : 0));
          set(row.spill, {d: sp > 0.02 ? 'M' + lx + ' ' + ly + 'C' + (lx + 7) + ' ' + ly + ' ' + (lx + 5) + ' ' + (ly + 7) + ' ' + (lx + 5) + ' ' + (ly + 12) + 'L' + (lx + 5) + ' ' + Math.max(ly + 12, ny) : 'M0 0', 'stroke-width': 1.2 + 2.3 * sp});
          if (sp > 0.02) streaming = true;
        }
      });
      // The pour, from the source into the basin that is filling.
      if (s.pourTo >= 0) {
        const x = x0(0) + wid[0] * 0.35, lvl = s.cap[0] > 0 ? clamp(s.level[0] / s.cap[0], 0, 1) : 0;
        set(parts.pourStream, {d: 'M' + x + ' 20L' + x + ' ' + (rim(0) + depth(0) * (1 - lvl)), class: 'stream-dash', 'stroke-width': 3});
        streaming = true;
      } else set(parts.pourStream, {d: 'M0 0'});
      txt(parts.blocked, s.blocked || '');
      set(parts.blocked, {x: geo.W, y: rim(s.blockedRow || 3) - 6});
      if (streaming) streamLoop.start(); else streamLoop.stop();
    };
    return {layout, draw, stop: () => streamLoop && streamLoop.stop(), get geo() { return geo; }};
  };
  // What a model frame looks like in a cascade.
  const cascadeState = (model, fr, extra) => {
    const dotsFor = t => {
      const i = t - 1;
      if (!model.flood || model.floodBlocked) return 0;
      const drank = fr.tiers[i].flood;
      if (drank <= 0 && t !== model.ft) return 0;
      return Math.round(clamp(Math.log10(model.flood) * 9, 6, 60) * (t === model.ft ? 1 : 0.5));
    };
    return Object.assign({
      B: model.B, cap: model.cap.slice(), level: fr.tiers.map(t => t.left),
      fill: fr.tiers.map((t, i) => model.refused[i + 1] ? 0 : t.asked > 0 ? t.served / t.asked : 1),
      agents: model.agents.slice(1), flood: [1, 2, 3, 4].map(dotsFor),
      num: fr.tiers.map((t, i) => model.refused[i + 1] ? 'closed' : bytes(t.left) + ' left'),
      src: "The day's budget: " + bytes(model.B), srcNote: model.unallocated > 0 ? bytes(model.unallocated) + ' not poured' : '',
      pourTo: -1, spill: [0, 0, 0],
    }, extra || {});
  };
  // The share of what a tier asked for that it got, by the end of a frame.
  const servedShare = (model, fr, t) => model.refused[t] ? 0 : fr.tiers[t - 1].asked > 0 ? fr.tiers[t - 1].served / fr.tiers[t - 1].asked : 1;

  // ---------- graphs: PageRank-style rank and stake-bounded capacity flow ----------
  const pagerank = (n, edges, seeds) => {
    const d = 0.85, outs = Array.from({length: n}, () => []);
    edges.forEach(e => outs[e.u].push(e.v));
    const isSeed = new Array(n).fill(false); seeds.forEach(s => { isSeed[s] = true; });
    let r = new Array(n).fill(0); seeds.forEach(s => { r[s] = 1 / seeds.length; });
    for (let it = 0; it < 60; it++) {
      const next = new Array(n).fill(0);
      seeds.forEach(s => { next[s] += (1 - d) / seeds.length; });
      let dangling = 0;
      for (let i = 0; i < n; i++) {
        if (!outs[i].length) { dangling += r[i]; continue; }
        const share = d * r[i] / outs[i].length;
        outs[i].forEach(v => { next[v] += share; });
      }
      seeds.forEach(s => { next[s] += d * dangling / seeds.length; });
      r = next;
    }
    return {score: r, edgeFlow: edges.map(e => r[e.u] / Math.max(1, outs[e.u].length))};
  };
  // Capacity flow from one seed set: node splitting, each node absorbs at most
  // U, a non-seed passes on at most transit[i], each edge carries at most cap;
  // progressive filling in five sink levels (max-min fair under scarcity).
  const U = 20;
  const maxflow = (n, edges, seeds, transit, pool, edgeCap) => {
    const SRC = 2 * n, SNK = 2 * n + 1, V = 2 * n + 2, g = Array.from({length: V}, () => []);
    const arc = (a, b, c) => { const e = {to: b, cap: c, flow: 0}, r = {to: a, cap: 0, flow: 0}; e.rev = r; r.rev = e; g[a].push(e); g[b].push(r); return e; };
    const isSeed = new Array(n).fill(false); seeds.forEach(s => { isSeed[s] = true; });
    seeds.forEach(s => arc(SRC, 2 * s, pool / seeds.length));
    for (let i = 0; i < n; i++) arc(2 * i, 2 * i + 1, isSeed[i] ? 1e12 : Math.max(0, transit[i]));
    const sinks = []; for (let i = 0; i < n; i++) sinks.push(arc(2 * i, SNK, 0));
    const edgeArcs = edges.map(e => arc(2 * e.u + 1, 2 * e.v, e.cap !== undefined ? e.cap : edgeCap));
    const augment = () => {
      for (let guard = 0; guard < 4000; guard++) {
        const prev = new Array(V).fill(null); prev[SRC] = {};
        const queue = [SRC];
        for (let q = 0; q < queue.length && !prev[SNK]; q++) {
          const x = queue[q];
          for (const e of g[x]) if (!prev[e.to] && e.cap - e.flow > 1e-9) { prev[e.to] = e; queue.push(e.to); }
        }
        if (!prev[SNK]) return;
        let f = Infinity;
        for (let x = SNK; x !== SRC; x = prev[x].rev.to) f = Math.min(f, prev[x].cap - prev[x].flow);
        for (let x = SNK; x !== SRC; x = prev[x].rev.to) { prev[x].flow += f; prev[x].rev.flow -= f; }
      }
    };
    for (let level = 1; level <= 5; level++) { sinks.forEach(s => { s.cap = U * level / 5; }); augment(); }
    return {absorbed: sinks.map(s => Math.max(0, s.flow)), edgeFlow: edgeArcs.map(a => Math.max(0, a.flow))};
  };
  const flowScores = (n, edges, A, Bs, transit, pool) => {
    const fa = maxflow(n, edges, A, transit, pool, U), fb = maxflow(n, edges, Bs, transit, pool, U);
    return {score: fa.absorbed.map((x, i) => Math.min(x, fb.absorbed[i])), edgeFlow: fa.edgeFlow.map((x, i) => (x + fb.edgeFlow[i]) / 2)};
  };
  // A deterministic force layout for the free nodes of a graph (unit square, aspect a).
  const forceLayout = (pos, free, edges, box, aspect, iters) => {
    const kk = Math.sqrt((box.x1 - box.x0) * (box.y1 - box.y0) * aspect / Math.max(1, free.length));
    const isFree = new Set(free);
    for (let it = 0, temp = 0.06; it < iters; it++, temp *= 0.985) {
      const disp = new Map(free.map(n => [n, {x: 0, y: 0}]));
      free.forEach(i => pos.forEach((p, j) => {
        if (i === j || !p) return;
        const dx = pos[i].x - p.x, dy = (pos[i].y - p.y) * aspect, d = Math.max(0.01, Math.hypot(dx, dy));
        const f = kk * kk / d, dd = disp.get(i); dd.x += dx / d * f; dd.y += dy / d * f;
      }));
      edges.forEach(e => {
        if (!pos[e.u] || !pos[e.v] || (!isFree.has(e.u) && !isFree.has(e.v))) return;
        const dx = pos[e.u].x - pos[e.v].x, dy = (pos[e.u].y - pos[e.v].y) * aspect, d = Math.max(0.01, Math.hypot(dx, dy));
        const f = d * d / kk * (e.k || 1);
        if (disp.has(e.u)) { const a = disp.get(e.u); a.x -= dx / d * f; a.y -= dy / d * f; }
        if (disp.has(e.v)) { const b = disp.get(e.v); b.x += dx / d * f; b.y += dy / d * f; }
      });
      free.forEach(n => {
        const dd = disp.get(n), len = Math.max(1e-9, Math.hypot(dd.x, dd.y)), s = Math.min(len, temp);
        pos[n].x = clamp(pos[n].x + dd.x / len * s, box.x0, box.x1);
        pos[n].y = clamp(pos[n].y + dd.y / len * s / aspect, box.y0, box.y1);
      });
    }
  };
  // Particles along edges, a few per edge in proportion to what it carries.
  const Particles = (layer, cls) => {
    let items = [];
    return {
      set(list, flows, P, badOf) {
        items.forEach(p => p.node.remove()); items = [];
        if (reduced()) return;
        const total = flows.reduce((a, b) => a + b, 0);
        if (total <= 0) return;
        list.forEach((e, k) => {
          const n = Math.min(4, Math.round(flows[k] / total * 90));
          for (let i = 0; i < n; i++) {
            const bad = badOf ? badOf(e) : false;
            items.push({e, t: (i + 0.37 * k) / Math.max(1, n) % 1, speed: 0.22 + 0.06 * ((k * 7 + i * 3) % 5), node: S('circle', {r: bad ? 2.2 : 1.9, class: bad ? 'particle-bad' : (cls || 'particle')}, layer)});
          }
        });
        this.P = P;
      },
      step(dt) {
        const P = this.P;
        if (!P) return;
        items.forEach(p => {
          p.t = (p.t + dt * p.speed) % 1;
          const a = P(p.e.u), b = P(p.e.v);
          set(p.node, {cx: lerp(a.x, b.x, p.t), cy: lerp(a.y, b.y, p.t)});
        });
      },
      clear() { items.forEach(p => p.node.remove()); items = []; },
      get size() { return items.length; },
    };
  };

  // ---------- figures and the scroll ----------
  const figs = [];
  const maxFigH = () => narrow() ? Math.max(170, Math.round(innerHeight * 0.36)) : Math.max(260, Math.min(460, Math.round(innerHeight * 0.56)));
  const widthOf = el => Math.max(240, Math.round(el.clientWidth || 0));
  const register = (name, make) => {
    const el = root.querySelector('[data-fig="' + name + '"]');
    if (!el) return;
    let api;
    try { api = make(el); } catch (e) { flag(); throw e; }
    if (!api) return;
    Object.assign(api, {el, name, active: -1, onScreen: false, lastW: 0});
    const section = el.closest('.tx-ch');
    api.steps = section ? [...section.querySelectorAll('.tx-step')] : [];
    figs.push(api);
  };
  // Controls a step has not reached yet (data-from) or has passed (data-to) are hidden.
  const gate = (api, i) => {
    api.el.querySelectorAll('[data-from],[data-to]').forEach(n => {
      const from = n.dataset.from === undefined ? -1 : Number(n.dataset.from);
      const to = n.dataset.to === undefined ? 99 : Number(n.dataset.to);
      const off = i < from || i > to;
      if (n.hidden !== off) n.hidden = off;
    });
  };
  const activate = (api, i, instant) => {
    if (i === api.active) return;
    api.active = i;
    api.steps.forEach((s, j) => s.classList.toggle('is-active', j === i));
    gate(api, i);
    if (api.step) api.step(i, !!instant);
  };

  // ======================= the cover: a day under a flood =======================
  register('hero', fig => {
    const svg = fig.querySelector('svg');
    const cas = Cascade(svg, 'hero');
    const m = simulateDay({B: 64 * MiB, flood: 1000000, floodTier: 3});
    const layout = () => { if (!fig.offsetParent) return; cas.layout(widthOf(fig), clamp(widthOf(fig) * 0.72, 220, 340)); cas.draw(cascadeState(m, frameAt(m, 8))); };
    return {resize: layout, init: layout, step() {}, hidden() { cas.stop(); }};
  });

  // ======================= 1. You arrive =======================
  register('arrive', fig => {
    const svg = fig.querySelector('svg'), read = out(fig, 'read');
    const bCall = act(fig, 'call'), bPost = act(fig, 'post'), bMid = act(fig, 'midnight');
    const B = 64, SHARE = 4, N = 16;
    const rnd = prng(1601);
    const honestSpent = Array.from({length: N}, () => Math.round((0.05 + rnd() * 0.35) * 100) / 100);
    const state = {called: false, spent: 0, midnight: false};
    let shown = null, stopTween = () => {}, geo = null, p = null;
    const target = () => ({
      honestPromised: state.midnight ? 0 : 1,
      honestSpent: state.midnight ? 0 : 1,
      you: state.called ? 1 : 0,
      youSpent: state.called ? state.spent : 0,
      youIn: state.midnight ? 0 : 1,
    });
    const layout = () => {
      const W = widthOf(fig), Hh = Math.min(maxFigH(), W < 420 ? 214 : 230);
      resetSvg(svg); sizeSvg(svg, W, Hh);
      const g = S('g', null, svg);
      const unit = (W - 2) / (B + SHARE + 4); // the scale fits 72 MB
      geo = {W, Hh, unit, x0: 1, poolY: 30, agentsY: 88, promY: 140, spentY: 188};
      p = {};
      p.poolLbl = S('text', {x: 0, y: 14, class: 't-lbl'}, g, "Today's budget");
      p.poolNum = S('text', {x: geo.x0 + unit * B, y: 14, 'text-anchor': 'end', class: 't-water'}, g);
      p.poolBg = S('rect', {class: 'basin-bg', x: geo.x0, y: geo.poolY, width: unit * B, height: 20, rx: 2}, g);
      p.pool = S('rect', {class: 'water', x: geo.x0, y: geo.poolY, width: 0, height: 20, rx: 2}, g);
      p.poolFrame = S('rect', {class: 'basin', x: geo.x0, y: geo.poolY, width: unit * B, height: 20, rx: 2}, g);
      p.limit = S('line', {class: 'dash', x1: geo.x0 + unit * B, x2: geo.x0 + unit * B, y1: geo.poolY - 4, y2: geo.spentY + 18}, g);
      p.limitLbl = S('text', {class: 't-mut t-halo', x: geo.x0 + unit * B - 6, y: geo.promY - 6, 'text-anchor': 'end'}, g, '');
      const gap = Math.min(22, (W - 40) / (N + 2));
      p.glyphs = honestSpent.map((_, i) => Glyph(g));
      p.gap = gap;
      p.you = Glyph(g, 'ag-you');
      p.youLbl = S('text', {class: 't-lbl', 'text-anchor': 'middle'}, g, 'you');
      p.promLbl = S('text', {x: 0, y: geo.promY - 6, class: 't-lbl'}, g, 'Promised');
      p.promNum = S('text', {x: 0, y: geo.promY - 6, class: 't-mut'}, g);
      p.proms = honestSpent.map(() => ({box: S('rect', {class: 'promise', height: 14, y: geo.promY, width: 0}, g), fill: S('rect', {class: 'spent', height: 14, y: geo.promY, width: 0}, g)}));
      p.youProm = S('rect', {class: 'promise-you', height: 14, y: geo.promY, width: 0}, g);
      p.youPromFill = S('rect', {class: 'spent', height: 14, y: geo.promY, width: 0}, g);
      p.spentLbl = S('text', {x: 0, y: geo.spentY - 6, class: 't-lbl'}, g, 'Spent');
      p.spentNum = S('text', {x: 0, y: geo.spentY - 6, class: 't-mut'}, g);
      p.spent = S('rect', {class: 'spent', x: geo.x0, y: geo.spentY, height: 14, width: 0}, g);
      p.spentYou = S('rect', {class: 'spent', x: geo.x0, y: geo.spentY, height: 14, width: 0}, g);
      p.clock = S('text', {x: W, y: geo.spentY + 11, 'text-anchor': 'end', class: 't-mut'}, g);
      if (shown) draw(shown);
    };
    const draw = v => {
      shown = v;
      if (!p) return;
      const {unit, x0, agentsY, promY} = geo;
      const hs = honestSpent.reduce((a, b) => a + b, 0) * v.honestSpent;
      const spentAll = hs + v.youSpent;
      set(p.pool, {width: unit * Math.max(0, B - spentAll)});
      txt(p.poolNum, (B - spentAll).toFixed(1).replace(/\.0$/, '') + ' MB of 64 left');
      // Agents: sixteen rings, filled by what they spent of their share; you at the end.
      const r = geo.W < 420 ? 6.5 : 8;
      p.glyphs.forEach((gl, i) => gl.at(x0 + r + 1 + i * p.gap, agentsY, r, honestSpent[i] / SHARE * v.honestSpent * 3));
      const yx = x0 + r + 1 + N * p.gap + p.gap * 0.6;
      p.you.g.style.opacity = String(clamp(v.youIn, 0, 1));
      p.you.at(yx, agentsY, r + 2, v.you > 0 ? v.youSpent / SHARE : 0);
      set(p.youLbl, {x: yx, y: agentsY + r + 16});
      p.youLbl.style.opacity = String(clamp(v.youIn, 0, 1));
      // Promised: one dashed box per share, at the pool's scale; spent inside each.
      let x = x0;
      p.proms.forEach((pr, i) => {
        const w = unit * SHARE * v.honestPromised;
        set(pr.box, {x: x + 0.5, width: Math.max(0, w - 1)});
        set(pr.fill, {x: x + 0.5, width: Math.max(0, unit * honestSpent[i] * v.honestSpent)});
        x += w;
      });
      set(p.youProm, {x: x + 0.5, width: Math.max(0, unit * SHARE * v.you - 1)});
      set(p.youPromFill, {x: x + 0.5, width: Math.max(0, unit * v.youSpent)});
      const promised = N * SHARE * v.honestPromised + SHARE * v.you;
      txt(p.promNum, promised > 0.05 ? count(promised) + ' MB' : 'nothing yet');
      set(p.promNum, {x: 70});
      txt(p.limitLbl, promised > B + 0.5 ? 'more than the day holds →' : '');
      set(p.spent, {width: unit * hs});
      set(p.spentYou, {x: x0 + unit * hs, width: unit * v.youSpent});
      txt(p.spentNum, spentAll.toFixed(1).replace(/\.0$/, '') + ' MB');
      set(p.spentNum, {x: 48});
      txt(p.clock, state.midnight ? '00:00 UTC, the next day' : '');
    };
    const go = instant => {
      stopTween();
      const from = shown || target(), to = target();
      stopTween = tween(instant ? 0 : 700, k => {
        const e = ease(k), m = {};
        for (const key in to) m[key] = lerp(from[key], to[key], e);
        draw(m);
      });
      buttons(); readout();
    };
    const buttons = () => {
      bCall.disabled = state.called && !state.midnight;
      bPost.disabled = !state.called || state.midnight || state.spent >= SHARE;
      bMid.disabled = !state.called || state.midnight;
      txt(bCall, state.midnight ? 'Make your first call today' : 'Make your first call');
    };
    const readout = () => {
      const left = B - honestSpent.reduce((a, b) => a + b, 0) * (state.midnight ? 0 : 1) - (state.midnight ? 0 : state.spent);
      let s;
      if (state.midnight) s = '00:00 UTC. A new day: the pool is full again, and each share appears on its agent\'s first call.';
      else if (!state.called) s = 'Sixteen agents hold 4 MB shares and have spent ' + honestSpent.reduce((a, b) => a + b, 0).toFixed(1) + ' MB between them. ' + left.toFixed(1) + ' MB of the pool is left.';
      else if (state.spent === 0) s = 'Your share: 4 MB, promised. The pool did not drop: still ' + left.toFixed(1) + ' MB. Shares promised today: 68 MB, more than the 64 MB day.';
      else if (state.spent < SHARE) s = 'You spent ' + state.spent + ' MB. The pool dropped by exactly that: ' + left.toFixed(1) + ' MB left. Your share has ' + (SHARE - state.spent) + ' MB left.';
      else s = 'Your 4 MB share is spent. The rest of the pool is for the others, and for the tiers below.';
      txt(read, s);
      describe(svg, "A 64 MB day. " + s);
    };
    bCall.addEventListener('click', () => { state.called = true; state.midnight = false; state.spent = 0; go(); });
    bPost.addEventListener('click', () => { if (state.called && state.spent < SHARE) { state.spent += 1; go(); } });
    bMid.addEventListener('click', () => { state.midnight = true; go(); });
    return {
      resize: layout,
      step(i, instant) {
        if (i <= 1) { state.called = false; state.spent = 0; state.midnight = false; }
        if (i >= 2 && (!state.called || state.midnight)) { state.called = true; state.midnight = false; }
        if (i >= 3 && state.spent === 0) state.spent = 1;
        go(instant);
      },
      init() { layout(); draw(target()); buttons(); readout(); },
    };
  });

  // ======================= 2. A million of you =======================
  register('million', fig => {
    const canvas = fig.querySelector('canvas'), ctx = canvas.getContext('2d');
    if (!ctx) return null;
    const keysIn = inp(fig, 'keys'), waitIn = inp(fig, 'wait');
    const HONEST = 16, MAXKEYS = 1000000, PER = 16, B = 64 * MiB, CAP = 4 * MiB;
    const layer = document.createElement('canvas'), lctx = layer.getContext('2d');
    let w = 0, hh = 0, dpr = 1, painted = 0, keys = 1, stopTween = () => {}, dayFlash = 0, pending = 0;
    const rnd = prng(20260929);
    const honest = [];
    const hr = prng(16);
    while (honest.length < HONEST) {
      const c = [0.08 + 0.84 * hr(), 0.14 + 0.72 * hr()];
      if (Math.hypot(c[0] - 0.22, (c[1] - 0.5) * 0.6) > 0.12 && honest.every(o => Math.hypot(o[0] - c[0], (o[1] - c[1]) * 0.5) > 0.075)) honest.push(c);
    }
    const you = [0.22, 0.5];
    // Your keys, one dot for every sixteen, ordered outward from you so the field grows from you.
    const pts = [];
    for (let i = 0; i < MAXKEYS / PER; i++) pts.push([rnd(), rnd()]);
    pts.forEach(p => { p[2] = Math.hypot((p[0] - you[0]) * 1.6, p[1] - you[1]) + rnd() * 0.25; });
    pts.sort((a, b) => a[2] - b[2]);
    let colours = null;
    const css = n => { if (!colours) { const cs = getComputedStyle(root); colours = {}; ['--attack', '--tx-bg', '--agent', '--ink', '--water', '--sans', '--serif'].forEach(k => { colours[k] = cs.getPropertyValue(k).trim(); }); } return colours[n] || ''; };
    const size = () => {
      w = widthOf(fig);
      hh = Math.round(clamp(w * (narrow() ? 0.5 : 0.52), 150, maxFigH() - (narrow() ? 60 : 110)));
      dpr = Math.min(2, window.devicePixelRatio || 1);
      canvas.width = layer.width = Math.round(w * dpr);
      canvas.height = layer.height = Math.round(hh * dpr);
      canvas.style.height = hh + 'px';
      lctx.setTransform(dpr, 0, 0, dpr, 0, 0);
      lctx.clearRect(0, 0, w, hh);
      painted = 0;
      paint();
    };
    // Dots are painted once into a layer; the frame composites it with the agents.
    const paint = () => {
      const want = Math.min(pts.length, Math.floor(Math.max(0, keys - 1) / PER));
      if (want < painted) { lctx.clearRect(0, 0, w, hh); painted = 0; }
      lctx.fillStyle = css('--attack') || '#d4452c';
      lctx.globalAlpha = 0.75;
      // Fewer keys, bigger dots, so a thousand keys are still seen.
      const s = want < 400 ? 3 : want < 4000 ? 2 : w < 420 ? 1.3 : 1.5;
      for (let i = painted; i < want; i++) lctx.fillRect(pts[i][0] * w, pts[i][1] * hh, s, s);
      lctx.globalAlpha = 1;
      painted = want;
      frame();
    };
    const ring = (x, y, r, stroke, fill, inner) => {
      ctx.beginPath(); ctx.arc(x, y, r, 0, Math.PI * 2); ctx.fillStyle = fill; ctx.fill();
      ctx.lineWidth = stroke[1]; ctx.strokeStyle = stroke[0]; ctx.stroke();
      if (inner > 0) { ctx.beginPath(); ctx.arc(x, y, Math.max(0, (r - 2) * Math.sqrt(clamp(inner, 0, 1))), 0, Math.PI * 2); ctx.fillStyle = css('--water'); ctx.fill(); }
    };
    const frame = () => {
      ctx.setTransform(1, 0, 0, 1, 0, 0);
      ctx.clearRect(0, 0, canvas.width, canvas.height);
      ctx.drawImage(layer, 0, 0);
      ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
      const each = Math.min(CAP, B / (HONEST + keys));
      const bg = css('--tx-bg'), ag = css('--agent'), ink = css('--ink');
      const r = w < 420 ? 6.5 : 8;
      honest.forEach(([x, y]) => ring(x * w, y * hh, r, [ag, 1.5], bg, each / CAP));
      ring(you[0] * w, you[1] * hh, r + 2, [ink, 2.25], bg, 0);
      ctx.font = '600 12px ' + css('--sans');
      ctx.fillStyle = ink; ctx.textAlign = 'center';
      ctx.fillText('you', you[0] * w, you[1] * hh + r + 16);
      if (dayFlash > 0) {
        ctx.globalAlpha = clamp(dayFlash, 0, 1);
        ctx.font = '400 20px ' + css('--serif');
        ctx.fillStyle = ink; ctx.textAlign = 'right';
        ctx.fillText('One day later', w - 8, 24);
        ctx.globalAlpha = 1;
      }
    };
    const readout = () => {
      const K = HONEST + keys, each = Math.min(CAP, B / K);
      const mine = keys * each, theirs = HONEST * each;
      txt(out(fig, 'keys'), count(keys));
      txt(out(fig, 'each'), bytes(each));
      const bar = out(fig, 'bar');
      setWidth(bar, theirs / B);
      let yours = bar.parentNode.querySelector('.tx-split-you');
      if (!yours) { yours = H('span', 'tx-split-you', bar.parentNode); }
      yours.style.left = (clamp(theirs / B, 0, 1) * 100).toFixed(2) + '%';
      setWidth(yours, mine / B);
      const line = keys === 1
        ? 'Seventeen keys split the 64 MB day, at most 4 MB each: ' + bytes(each) + ' for each honest agent, and for you.'
        : count(K) + ' keys split the same day. Your ' + count(keys) + ' keys take ' + pct(mine / B) + ' of it; each honest agent is left ' + bytes(each) + '.';
      txt(out(fig, 'line'), waitIn.checked && keys > 1 ? 'A day later all ' + count(keys) + ' of your keys are a day old, and the split is the same: ' + bytes(each) + ' for each honest agent.' : line);
      canvas.setAttribute('aria-label', 'Sixteen honest agents and you' + (keys > 1 ? ', among ' + count(keys) + ' keys you made' : '') + '. Each honest agent gets ' + bytes(each) + '.');
    };
    const setKeys = v => { keysIn.value = String(clamp(Math.round(v), 0, 60)); keys = logCount(keysIn.value); readout(); if (!pending) pending = requestAnimationFrame(() => { pending = 0; paint(); }); };
    keysIn.addEventListener('input', () => { stopTween(); setKeys(Number(keysIn.value)); });
    waitIn.addEventListener('change', () => {
      stopTween();
      readout();
      if (!waitIn.checked) { dayFlash = 0; frame(); return; }
      stopTween = tween(1600, k => { dayFlash = k < 0.5 ? k * 2 : 1; frame(); });
    });
    const themeQ = media('(prefers-color-scheme: dark)');
    if (themeQ && themeQ.addEventListener) themeQ.addEventListener('change', () => { colours = null; painted = 0; lctx.clearRect(0, 0, w, hh); paint(); });
    return {
      resize: size,
      step(i, instant) {
        stopTween();
        if (i === 0) { waitIn.checked = false; dayFlash = 0; setKeys(0); return; }
        if (i >= 1 && Number(keysIn.value) < 60) {
          const from = Number(keysIn.value);
          stopTween = tween(instant ? 0 : 2400, k => setKeys(lerp(from, 60, ease(k))));
        }
        if (i === 1) { waitIn.checked = false; dayFlash = 0; readout(); frame(); }
      },
      init() { size(); readout(); },
    };
  });

  // ======================= 3. Pour the day =======================
  register('pour', fig => {
    const svg = fig.querySelector('svg'), read = out(fig, 'read');
    const clockIn = inp(fig, 'clock'), bPour = act(fig, 'pour'), bPlay = act(fig, 'play');
    const cas = Cascade(svg, 'pour');
    const B = 64 * MiB;
    const models = {};
    const modelFor = f => models[f] || (models[f] = simulateDay({B, flood: f ? 1000000 : 0, floodTier: f === 4 ? 4 : 3}));
    const st = {poured: 0, hour: 0, flood: 0};
    let shown = null, stopTween = () => {};
    // The cascade state for a poured fraction, an hour and a flood.
    const view = s => {
      const m = modelFor(s.flood);
      if (s.poured < 1) {
        const total = m.size.slice(1).reduce((a, b) => a + b, 0), P = s.poured * total;
        let before = 0, to = -1;
        const level = [1, 2, 3, 4].map(t => { const l = clamp(P - before, 0, m.size[t]); if (to < 0 && P - before < m.size[t]) to = t - 1; before += m.size[t]; return l; });
        return cascadeState(m, m.frames[0], {level, fill: [0, 0, 0, 0], flood: [0, 0, 0, 0], pourTo: s.poured > 0 && s.poured < 1 ? Math.max(0, to) : -1,
          num: level.map(l => l > 0 ? bytes(l) : ''), spill: [0, 1, 2].map(i => s.poured > 0 && (to > i || to < 0) ? 0.6 : 0)});
      }
      const fr = frameAt(m, s.hour);
      const h = Math.min(23, Math.floor(s.hour)), next = m.frames[h + 1].tiers, prev = m.frames[h].tiers;
      const spill = [0, 1, 2].map(i => s.hour > 0.2 && s.hour < 23.9 ? clamp((next[i].spilled - prev[i].spilled) / (0.01 * B), 0, 1) : 0);
      return cascadeState(m, fr, {spill});
    };
    const mix = (a, b, k) => {
      const o = Object.assign({}, b);
      for (const key of ['cap', 'level', 'fill', 'flood', 'spill']) o[key] = b[key].map((v, i) => v === null ? null : lerp(a[key][i] === null ? 0 : a[key][i], v, k));
      o.flood = o.flood.map(Math.round);
      return o;
    };
    const draw = v => { shown = v; cas.draw(v); };
    const go = (dur, keepShown) => {
      stopTween();
      const to = view(st), from = keepShown && shown ? shown : to;
      stopTween = tween(dur, k => draw(mix(from, to, ease(k))));
      controls(); readout();
    };
    const controls = () => {
      clockIn.value = String(st.hour);
      const hh = Math.floor(st.hour), mm = Math.round((st.hour - hh) * 60);
      txt(out(fig, 'clock'), String(Math.min(24, hh + (mm === 60 ? 1 : 0))).padStart(2, '0') + ':' + String(mm === 60 ? 0 : mm).padStart(2, '0') + ' UTC');
      txt(bPour, st.poured >= 1 ? 'Pour again' : 'Pour the day');
      setRadio(fig, 'flood', st.flood);
    };
    const readout = () => {
      const m = modelFor(st.flood);
      let s;
      if (st.poured <= 0) s = 'The basins are empty.';
      else if (st.poured < 1) s = 'Pouring: trusted fills to its reserve first, then proven, then signed; the anonymous basin gets what is left.';
      else {
        const fr = frameAt(m, st.hour), clock = out(fig, 'clock').textContent;
        const left = fr.tiers.map((t, i) => TIER[i + 1].toLowerCase() + ' ' + bytes(t.left)).join(', ');
        if (!st.flood) s = st.hour < 0.05 ? 'Poured: trusted ' + bytes(m.size[1]) + ', proven ' + bytes(m.size[2]) + ', signed ' + bytes(m.size[3]) + ', anonymous ' + bytes(m.size[4]) + '.'
          : 'At ' + clock + ' every agent has got what it asked for. Left: ' + left + '.';
        else {
          const who = st.flood === 4 ? 'million networks' : 'million keys';
          s = 'By ' + clock + ' the ' + who + ' drank ' + bytes(fr.floodGot) + ', ' + pct(fr.floodGot / B) + ' of the day. Trusted agents got ' + pct(servedShare(m, fr, 1)) +
            ' of what they asked for, proven ' + pct(servedShare(m, fr, 2)) + ', signed ' + pct(servedShare(m, fr, 3)) + ', anonymous ' + pct(servedShare(m, fr, 4)) + '.';
        }
      }
      txt(read, s);
      describe(svg, 'Four basins, trusted, proven, signed and anonymous, fed from a 64 MB day. ' + s);
    };
    const pour = instant => {
      st.hour = 0; st.poured = 0;
      stopTween();
      stopTween = tween(instant ? 0 : 2600, k => { st.poured = k; draw(view(st)); if (k === 0 || k === 1 || Math.round(k * 20) % 5 === 0) readout(); }, () => { st.poured = 1; controls(); readout(); });
      controls();
    };
    const playTo = (h, dur) => {
      if (st.poured < 1) st.poured = 1;
      const from = st.hour;
      stopTween();
      stopTween = tween(dur, k => { st.hour = lerp(from, h, k); draw(view(st)); controls(); if (k === 1 || Math.round(k * 60) % 6 === 0) readout(); });
    };
    bPour.addEventListener('click', () => pour(false));
    clockIn.addEventListener('input', () => { stopTween(); if (st.poured < 1) st.poured = 1; st.hour = clamp(Number(clockIn.value), 0, 24); draw(view(st)); controls(); readout(); });
    bPlay.addEventListener('click', () => { if (st.hour >= 24) st.hour = 0; playTo(24, Math.max(600, (24 - st.hour) / 24 * 8000)); });
    radios(fig, 'flood').forEach(r => r.addEventListener('change', () => { st.flood = Number(radioValue(fig, 'flood')) || 0; if (st.poured < 1) st.poured = 1; if (st.flood && st.hour < 1) st.hour = 8; go(900, true); }));
    const layout = () => { cas.layout(widthOf(fig), narrow() ? clamp(maxFigH(), 220, 290) : clamp(widthOf(fig) * 0.62, 220, maxFigH())); draw(view(st)); };
    return {
      resize: layout,
      step(i, instant) {
        if (i === 0) { stopTween(); st.poured = 0; st.hour = 0; st.flood = 0; go(0); return; }
        if (i === 1) { st.flood = 0; st.hour = 0; if (st.poured < 1) pour(instant); else go(instant ? 0 : 600, true); return; }
        if (i === 2) { st.flood = 0; st.poured = 1; if (st.hour < 0.5) { go(0, true); playTo(12, instant ? 0 : 4500); } else go(instant ? 0 : 600, true); return; }
        if (i >= 3) { if (!st.flood) st.flood = 3; st.poured = 1; if (i === 3) st.hour = 8; go(instant ? 0 : 1200, true); }
      },
      init() { layout(); controls(); readout(); },
      hidden() { cas.stop(); },
    };
  });

  // ======================= 4. What would it cost to fake you? =======================
  register('collateral', fig => {
    const svg = fig.querySelector('svg'), read = out(fig, 'read');
    const domainIn = inp(fig, 'domain'), keysIn = inp(fig, 'keys'), linksIn = inp(fig, 'links'), ageIn = inp(fig, 'age');
    // Published trust parameters: domain min(forge 1,200, rent 400), ramp h = 180 d;
    // 10 per answered day, at most 90 days, ramp h = 90 d; proven at 200.
    const ramp = (age, h) => 1 - Math.pow(2, -Math.max(0, age) / h);
    const DOMAIN = 400, DAY = 10, DAYS_CAP = 90, THETA = 200, MAX = 1500;
    const posting = () => !!fig.dataset.posting;
    const target = () => {
      const age = Number(ageIn.value);
      const d = domainIn.checked ? DOMAIN * ramp(age, 180) : 0;
      const h = posting() ? DAY * Math.min(DAYS_CAP, Math.floor(age / 2)) * ramp(age, 90) : 0;
      return {domain: d, history: h, ghosts: domainIn.checked && keysIn.checked ? 1 : 0, links: linksIn.checked ? 1 : 0, domainOn: domainIn.checked ? 1 : 0, age};
    };
    let shown = null, stopTween = () => {}, geo = null, p = null;
    const layout = () => {
      const W = widthOf(fig), Hh = Math.min(maxFigH(), W < 420 ? 236 : 260);
      resetSvg(svg); sizeSvg(svg, W, Hh);
      const g = S('g', null, svg);
      geo = {W, Hh, barY: 118, barH: 22, x0: 0, x1: W - 2};
      p = {};
      // You and your proofs.
      p.you = Glyph(g, 'ag-you', 'ag-gold');
      p.youLbl = S('text', {class: 't-lbl', 'text-anchor': 'middle'}, g, 'you');
      p.domLine = S('line', {class: 'rule-strong'}, g);
      p.dom = S('rect', {class: 'chip', rx: 2, height: 22}, g);
      p.domTxt = S('text', {class: 't-lbl'}, g, 'your-domain.example');
      p.keyLines = [0, 1, 2, 3].map(() => S('line', {class: 'rule-strong'}, g));
      p.keys = [0, 1, 2, 3].map(() => Glyph(g));
      p.links = ['board', 'url', 'nostr'].map(t => ({box: S('rect', {class: 'chip-off', rx: 2, height: 20}, g), t: S('text', {class: 't-mut'}, g, t + ' · 0')}));
      // The price.
      p.priceLbl = S('text', {x: 0, y: geo.barY - 10, class: 't-lbl'}, g, 'What it would cost to fake you');
      p.priceNum = S('text', {x: geo.x1, y: geo.barY - 10, class: 't-gold', 'text-anchor': 'end'}, g);
      p.track = S('rect', {class: 'basin-bg', x: 0, y: geo.barY, width: geo.x1, height: geo.barH, rx: 2}, g);
      const clipId = 'collateral-clip';
      S('rect', {x: 0, y: geo.barY - 2, width: geo.x1, height: geo.barH + 4}, S('clipPath', {id: clipId}, S('defs', null, g)));
      const bars = S('g', {'clip-path': 'url(#' + clipId + ')'}, g);
      p.segD = S('rect', {class: 'gold', y: geo.barY, height: geo.barH, x: 0, width: 0}, bars);
      p.segH = S('rect', {class: 'gold', y: geo.barY, height: geo.barH, x: 0, width: 0, opacity: 0.55}, bars);
      p.ghosts = [0, 1, 2, 3].map(() => S('rect', {class: 'gold-ghost', y: geo.barY + 1, height: geo.barH - 2, x: 0, width: 0}, bars));
      p.ghostTxt = S('text', {class: 't-mut t-halo', y: geo.barY + geo.barH + 15}, g);
      [0, 500, 1000, 1500].forEach(v => {
        const x = v / MAX * geo.x1;
        S('text', {x, y: geo.barY + geo.barH + 30, class: 't-mut', 'text-anchor': v === 0 ? 'start' : v === MAX ? 'end' : 'middle'}, g, count(v));
      });
      const tx = THETA / MAX * geo.x1;
      S('line', {class: 'dash', x1: tx, x2: tx, y1: geo.barY - 4, y2: geo.barY + geo.barH + 4}, g);
      S('text', {class: 't-lbl t-halo', x: tx + 4, y: geo.barY + geo.barH + 15}, g, 'proven');
      // The tier you land in: the chapter 3 basins, small.
      const by = geo.barY + geo.barH + 52, bw = Math.min(64, (W - 110) / 4 - 10);
      p.tierLbl = S('text', {x: 0, y: by + 12, class: 't-lbl'}, g, 'Your tier');
      p.basins = [1, 2, 3, 4].map((t, i) => {
        const x = 84 + i * (bw + 10);
        S('path', {class: 'basin-bg', d: troughFill(x, by, bw, 16)}, g);
        S('path', {class: 'basin', d: troughPath(x, by, bw, 16)}, g);
        S('text', {class: 't-mut', x: x + bw / 2, y: by + 32, 'text-anchor': 'middle'}, g, TIER[t].toLowerCase());
        return x + bw / 2;
      });
      p.dot = Glyph(g, 'ag-you', 'ag-gold');
      if (shown) draw(shown);
    };
    const draw = v => {
      shown = v;
      if (!p) return;
      const {W, barY, barH, x1} = geo;
      const sx = u => clamp(u / MAX, 0, 4) * x1;
      const total = v.domain + v.history;
      // You, and your proofs around you.
      const yx = 20, yy = 44;
      p.you.at(yx, yy, 13, total / 1000);
      set(p.youLbl, {x: yx, y: yy + 30});
      const dx = 50, dw = Math.min(170, W * 0.42);
      p.dom.style.opacity = p.domTxt.style.opacity = p.domLine.style.opacity = String(v.domainOn);
      set(p.dom, {x: dx, y: yy - 11, width: dw}); set(p.domTxt, {x: dx + 8, y: yy + 4});
      set(p.domLine, {x1: yx + 13, y1: yy, x2: dx, y2: yy});
      p.keys.forEach((k, i) => {
        const kx = dx + dw + 16 + i * 16, ky = yy - 22 + (i % 2) * 8;
        k.g.style.opacity = String(v.ghosts);
        k.at(kx, ky, 5, 0);
        set(p.keyLines[i], {x1: dx + dw, y1: yy - 6, x2: kx, y2: ky});
        p.keyLines[i].style.opacity = String(v.ghosts * 0.6);
      });
      let lx = dx;
      p.links.forEach(l => {
        const lw = narrow() ? 64 : 76;
        l.box.style.opacity = l.t.style.opacity = String(v.links);
        set(l.box, {x: lx, y: yy + 18, width: lw}); set(l.t, {x: lx + 6, y: yy + 32});
        lx += lw + 6;
      });
      // The price bar: the domain, answered days, and the same-root keys as dashed ghosts.
      set(p.segD, {x: 0, width: sx(v.domain)});
      set(p.segH, {x: sx(v.domain), width: sx(v.history)});
      let gx = sx(total);
      p.ghosts.forEach(r => { set(r, {x: gx + 1, width: Math.max(0, sx(v.domain) * v.ghosts - 2)}); gx += sx(v.domain) * v.ghosts; });
      txt(p.ghostTxt, v.ghosts > 0.5 && v.domain > 5 ? 'four more keys, same root: counted once' : '');
      const gxl = Math.max(sx(total) + 4, THETA / MAX * x1 + 58);
      set(p.ghostTxt, gxl + 236 > x1 ? {x: x1, 'text-anchor': 'end'} : {x: gxl, 'text-anchor': 'start'});
      txt(p.priceNum, count(total) + ' units');
      // Your tier: signed until the proven line.
      const t = total >= THETA ? 1 : 2, bx = p.basins[t];
      p.dot.at(bx, barY + barH + 58, 6, total / 1000);
    };
    const go = instant => {
      stopTween();
      const to = target(), from = shown || to;
      stopTween = tween(instant ? 0 : 650, k => {
        const e = ease(k), m = {};
        for (const key in to) m[key] = lerp(from[key], to[key], e);
        draw(m);
      });
      readout(to);
    };
    const ageText = a => a === 0 ? 'new' : a < 60 ? a + ' days' : a >= 365 && a % 365 < 15 ? Math.round(a / 365) + (a < 700 ? ' year' : ' years') : Math.round(a / 30.4) + ' months';
    const readout = v => {
      const total = v.domain + v.history;
      txt(out(fig, 'age'), ageText(v.age));
      const parts = [];
      if (v.domainOn) parts.push('the domain ' + count(v.domain));
      if (v.history > 0) parts.push('answered days ' + count(v.history));
      let s = 'To fake you: ' + count(total) + (parts.length ? ' (' + parts.join(', ') + ')' : '') + '.';
      if (v.ghosts) s += ' Four more keys on your domain add nothing: same root.';
      if (v.links) s += ' Unverified links add nothing.';
      s += total >= THETA ? ' You are proven.' : ' The proven tier starts at ' + THETA + '.';
      txt(read, s);
      describe(svg, s);
    };
    [domainIn, keysIn, linksIn].forEach(n => n.addEventListener('change', () => go()));
    ageIn.addEventListener('input', () => go(true));
    const preset = (d, k, l, age, post) => { domainIn.checked = d; keysIn.checked = k; linksIn.checked = l; ageIn.value = String(age); if (post) fig.dataset.posting = '1'; else delete fig.dataset.posting; };
    return {
      resize: layout,
      step(i, instant) {
        stopTween();
        if (i === 0) preset(false, false, false, 0, false);
        else if (i === 1) preset(true, false, false, Math.max(30, Number(ageIn.value)), false);
        else if (i === 2) preset(true, true, false, Math.max(30, Number(ageIn.value)), false);
        else if (i === 3) preset(true, true, true, Math.max(30, Number(ageIn.value)), false);
        if (i >= 4) {
          preset(domainIn.checked, keysIn.checked, linksIn.checked, Number(ageIn.value), true);
          const from = Number(ageIn.value);
          if (from < 365) {
            stopTween = tween(instant ? 0 : 2600, k => { ageIn.value = String(Math.round(lerp(from, 365, ease(k)) / 5) * 5); const v = target(); draw(v); readout(v); });
            return;
          }
        }
        go(instant);
      },
      init() { layout(); const v = target(); draw(v); readout(v); },
    };
  });

  // ======================= 5. Trust flows =======================
  register('flows', fig => {
    const svg = fig.querySelector('svg'), ringIn = inp(fig, 'ring');
    const rnd = prng(4242);
    // An illustrative graph: two seed sets of three, twenty honest agents and you, and a ring.
    const kinds = [];
    const add = k => { kinds.push(k); return kinds.length - 1; };
    const A = [0, 1, 2].map(() => add('a')), Bs = [0, 1, 2].map(() => add('b'));
    const Hn = []; for (let i = 0; i < 20; i++) Hn.push(add('h'));
    const YOU = add('you');
    const R = []; for (let i = 0; i < 24; i++) R.push(add('r'));
    const n = kinds.length, edges = [], seen = new Set();
    const edge = (u, v) => { const k = u + '>' + v; if (u === v || seen.has(k)) return; seen.add(k); edges.push({u, v}); };
    const pick = l => l[Math.floor(rnd() * l.length)];
    A.concat(Bs).forEach(s => { for (let k = 0; k < 3; k++) edge(s, pick(Hn)); });
    Hn.forEach(u => { const c = 1 + Math.floor(rnd() * 2); for (let k = 0; k < c; k++) edge(u, pick(Hn)); });
    edge(Hn[3], YOU); edge(Hn[7], YOU); edge(YOU, Hn[11]);
    const ringEdges = [];
    R.forEach((u, i) => { [1, 3, 8].forEach(o => { const e = {u, v: R[(i + o) % R.length]}; ringEdges.push(e); }); });
    const all = edges.concat(ringEdges);
    // Positions: seeds on the left, the ring on the right, the rest laid out between.
    const pos = new Array(n).fill(null);
    A.forEach((s, i) => { pos[s] = {x: 0.05, y: 0.12 + 0.13 * i}; });
    Bs.forEach((s, i) => { pos[s] = {x: 0.05, y: 0.62 + 0.13 * i}; });
    Hn.concat([YOU]).forEach(h => { pos[h] = {x: 0.2 + 0.36 * rnd(), y: 0.08 + 0.84 * rnd()}; });
    forceLayout(pos, Hn.concat([YOU]), edges.map(e => ({u: e.u, v: e.v, k: kinds[e.u] === 'h' ? 1 : 0.3})), {x0: 0.18, x1: 0.58, y0: 0.08, y1: 0.92}, 0.62, 240);
    const ringPos = (i, m) => { const a = 2 * Math.PI * i / m - Math.PI / 2; return {x: 0.83 + 0.12 * Math.cos(a), y: 0.5 + 0.36 * Math.sin(a)}; };
    // Scores. Counting: endorsements received. Flow: capacity flow from each seed set, the smaller.
    const transit = kinds.map(k => (k === 'r' ? 0 : U));
    const flow = flowScores(n, all, A, Bs, transit, U * 12);
    const inDeg = new Array(n).fill(0); edges.forEach(e => inDeg[e.v]++);
    const st = {mode: 'count', ring: 24, seeds: false};
    let shown = null, stopTween = () => {}, geo = null, p = null;
    const particles = loop(dt => p && p.parts.step(dt));
    const honestEdgeCount = edges.length;
    // The share of all counted endorsements the ring holds: each key endorses the others, up to 64.
    const ringCountShare = N => { const re = N * Math.min(N - 1, 64); return re / (re + honestEdgeCount); };
    const target = () => {
      const N = st.ring;
      return {count: st.mode === 'count' ? 1 : 0, ringShown: Math.min(24, N), N, seeds: st.seeds ? 1 : 0};
    };
    const layout = () => {
      const W = widthOf(fig), Hh = clamp(W * 0.62, 220, maxFigH() - 30);
      resetSvg(svg); sizeSvg(svg, W, Hh);
      geo = {W, Hh};
      const g = S('g', null, svg);
      p = {};
      p.region = S('rect', {class: 'region', rx: 10}, g);
      p.regionLbl = S('text', {class: 't-att', 'text-anchor': 'middle'}, g);
      p.edgesG = S('g', null, g);
      p.edges = all.map(e => S('line', {class: kinds[e.u] === 'r' ? 'edge-bad' : 'edge'}, p.edgesG));
      p.partG = S('g', {'aria-hidden': 'true'}, g);
      p.parts = Particles(p.partG);
      p.nodes = kinds.map(k => Glyph(g, k === 'a' || k === 'b' ? 'ag-seed' : k === 'you' ? 'ag-you' : k === 'r' ? 'ag-fake-ring' : 'ag', 'ag-gold'));
      p.letters = kinds.map((k, i) => k === 'a' || k === 'b' ? S('text', {class: 'ag-letter'}, g, k.toUpperCase()) : null);
      p.youLbl = S('text', {class: 't-lbl t-halo', 'text-anchor': 'middle'}, g, 'you');
      p.setA = S('text', {class: 't-gold t-halo'}, g, 'seed set A');
      p.setB = S('text', {class: 't-gold t-halo'}, g, 'seed set B');
      if (shown) draw(shown);
      particlesFor();
    };
    const P = i => {
      const q = kinds[i] === 'r' ? ringPos(R.indexOf(i), Math.max(1, Math.round(shown ? shown.ringShown : 24))) : pos[i];
      return {x: 14 + q.x * (geo.W - 28), y: q.y * geo.Hh};
    };
    const draw = v => {
      shown = v;
      if (!p) return;
      const m = Math.max(1, Math.round(v.ringShown));
      // Scores to draw: a mix of counting and flow, so a switch morphs.
      const cMax = Math.max(1, ...inDeg), fMax = Math.max(1, ...flow.score);
      const ringCount = Math.min(v.N - 1, 64);
      kinds.forEach((k, i) => {
        const gl = p.nodes[i];
        if (k === 'r') {
          const j = R.indexOf(i), on = j < m;
          gl.g.style.display = on ? '' : 'none';
          if (!on) return;
          const q = P(i);
          gl.at(q.x, q.y, 5.5, v.count * (ringCount / cMax > 1 ? 1 : ringCount / cMax));
          return;
        }
        const q = P(i), seed = k === 'a' || k === 'b';
        const c = seed ? 0 : inDeg[i] / Math.max(cMax, ringCount), f = seed ? 0 : flow.score[i] / fMax;
        gl.at(q.x, q.y, seed ? 9 : k === 'you' ? 8 : 6, lerp(f, c, v.count));
        if (p.letters[i]) set(p.letters[i], {x: q.x, y: q.y + 0.5});
        if (seed) gl.o.style.strokeWidth = String(2.5 + 2 * v.seeds);
      });
      all.forEach((e, k) => {
        const line = p.edges[k];
        const hide = kinds[e.u] === 'r' && (R.indexOf(e.u) >= m || R.indexOf(e.v) >= m);
        line.style.display = hide ? 'none' : '';
        if (hide) return;
        const a = P(e.u), b = P(e.v);
        set(line, {x1: a.x, y1: a.y, x2: b.x, y2: b.y});
      });
      const q = P(YOU); set(p.youLbl, {x: q.x, y: q.y - 13});
      const ra = P(A[0]), rb = P(Bs[0]);
      set(p.setA, {x: ra.x - 9, y: ra.y - 16}); set(p.setB, {x: rb.x - 9, y: rb.y - 16});
      p.setA.style.opacity = p.setB.style.opacity = String(0.35 + 0.65 * v.seeds);
      const rx = 14 + 0.71 * (geo.W - 28), rw = 0.24 * (geo.W - 28);
      set(p.region, {x: rx - 6, y: geo.Hh * 0.1, width: rw + 12, height: geo.Hh * 0.8});
      txt(p.regionLbl, count(v.N) + (v.N === 1 ? ' fake key' : ' fake keys'));
      set(p.regionLbl, {x: rx + rw / 2, y: geo.Hh * 0.1 - 6 < 12 ? geo.Hh * 0.95 : geo.Hh * 0.1 - 6});
    };
    const particlesFor = () => {
      if (!p) return;
      if (st.mode !== 'flow') { p.parts.clear(); particles.stop(); return; }
      p.parts.set(all, flow.edgeFlow, P);
      if (api.onScreen) particles.start();
    };
    const readout = () => {
      const share = st.mode === 'count' ? ringCountShare(st.ring) : 0;
      txt(out(fig, 'ring-share'), pct(share));
      setWidth(out(fig, 'ring-bar'), share);
      txt(out(fig, 'ring'), count(st.ring));
      const s = st.mode === 'count'
        ? 'of all the endorsements counted. Its ' + count(st.ring) + ' keys endorse each other, so the ring holds ' + pct(share) + ' of the count.'
        : 'of all the trust handed out: no endorsement from outside reaches the ring, whatever its size.';
      txt(out(fig, 'read'), s);
      describe(svg, 'Two seed sets of three, twenty honest agents and you, and a ring of ' + count(st.ring) + ' fake keys that endorse one another. Scored by ' + (st.mode === 'count' ? 'counting' : 'flow from the seeds') + ', the ring holds ' + pct(share) + '.');
    };
    const go = instant => {
      stopTween();
      const to = target(), from = shown || to;
      stopTween = tween(instant ? 0 : 800, k => { const e = ease(k), mm = {}; for (const key in to) mm[key] = lerp(from[key], to[key], e); draw(mm); }, particlesFor);
      setRadio(fig, 'mode', st.mode);
      readout();
    };
    radios(fig, 'mode').forEach(r => r.addEventListener('change', () => { st.mode = radioValue(fig, 'mode') === 'flow' ? 'flow' : 'count'; go(); }));
    ringIn.addEventListener('input', () => { stopTween(); st.ring = logCount(ringIn.value); const to = target(); draw(to); readout(); });
    const api = {
      resize: layout,
      step(i, instant) {
        stopTween();
        st.seeds = i === 3;
        if (i === 0) { st.mode = 'count'; st.ring = 24; ringIn.value = '14'; }
        if (i >= 1) st.mode = 'flow';
        if (i === 2 && st.ring < MAXRING) {
          const from = Number(ringIn.value);
          go(instant);
          stopTween = tween(instant ? 0 : 2400, k => { ringIn.value = String(Math.round(lerp(from, 60, ease(k)))); st.ring = logCount(ringIn.value); draw(target()); readout(); });
          return;
        }
        go(instant);
      },
      init() { layout(); draw(target()); readout(); },
      visible(on) { if (on && st.mode === 'flow') particles.start(); else particles.stop(); },
    };
    const MAXRING = 1000000;
    return api;
  });

  // ======================= 6. Would you sell your vouch? =======================
  register('vouch', fig => {
    const svg = fig.querySelector('svg'), read = out(fig, 'read');
    const bSell = act(fig, 'sell'), bKeep = act(fig, 'keep');
    // The illustration's numbers: standing 100 a day; a vouch carries at most
    // half of it (λ = 0.5); 20% a week that the ring is caught; 30 days lost.
    const A_S = 100, LAMBDA = 0.5, P_DAY = 1 - Math.pow(0.8, 1 / 7), T = 30, DAYS = 90;
    const OFFER = {count: 10, flow: A_S * LAMBDA}, VALUE = {count: 260, flow: A_S * LAMBDA};
    const LOSS = T * A_S;
    const expected = d => { let e = 0, s = 1; for (let j = 1; j <= d; j++) { e += s * (OFFER.flow - P_DAY * LOSS); s *= 1 - P_DAY; } return e; };
    const RANGE = {count: [-600, 1500], flow: [-3500, 4600]};
    const st = {rules: 'count', choice: '', run: null, mode: 'sell', who: 'others'};
    let p = null, geo = null, stopTween = () => {}, progress = 0;
    const particles = loop(dt => { if (p) p.parts.step(dt); });
    const layout = () => {
      const W = widthOf(fig), Hh = clamp(W * 0.7, 260, maxFigH());
      resetSvg(svg); sizeSvg(svg, W, Hh);
      const g = S('g', null, svg);
      const sceneH = Math.max(124, Math.round(Hh * 0.38)), cx = 26, cy = Math.round(sceneH * 0.42);
      geo = {W, Hh, sceneH, cx, cy, chart: {x0: 40, x1: W - 6, y0: sceneH + 22, y1: Hh - 22}};
      p = {};
      // The scene.
      p.offer = S('line', {class: 'edge-sold'}, g);
      p.offerTxt = S('text', {class: 't-mut t-halo', 'text-anchor': 'middle'}, g);
      p.you = Glyph(g, 'ag-you', 'ag-gold');
      p.youTxt = S('text', {class: 't-lbl'}, g, 'you');
      p.youSub = S('text', {class: 't-mut'}, g);
      const rx = W - 62;
      p.ringG = S('g', null, g);
      p.ring = [];
      for (let i = 0; i < 9; i++) {
        const a = 2 * Math.PI * i / 9;
        const gl = Glyph(p.ringG, 'ag-fake-ring', 'ag-gold');
        gl.at(rx + 26 * Math.cos(a), cy + 22 * Math.sin(a), 5, 0);
        p.ring.push({gl, x: rx + 26 * Math.cos(a), y: cy + 22 * Math.sin(a)});
      }
      p.ringTxt = S('text', {class: 't-att', x: rx, y: cy + 44, 'text-anchor': 'middle'}, p.ringG, 'a ring of fake keys');
      // The sponsor scene: a newcomer, strangers and friends.
      p.sponsorG = S('g', null, g);
      p.newX = W * 0.62; p.newY = cy;
      p.vouchLine = S('line', {class: 'edge-vouch', x1: cx + 16, y1: cy, x2: p.newX - 10, y2: cy}, p.sponsorG);
      p.newcomer = Glyph(p.sponsorG, 'ag', 'ag-gold');
      p.newTxt = S('text', {class: 't-lbl', x: p.newX, y: cy + 26, 'text-anchor': 'middle'}, p.sponsorG, 'your newcomer');
      p.strangers = [0, 1, 2].map(i => ({x: W - 30, y: cy - 34 + i * 34}));
      p.friends = [0, 1, 2].map(i => ({x: cx + 40 + i * 26, y: cy + 46}));
      p.sEdges = p.strangers.map(s => S('line', {class: 'edge', x1: s.x, y1: s.y, x2: p.newX, y2: p.newY}, p.sponsorG));
      p.fEdges = p.friends.map(s => S('line', {class: 'edge', x1: s.x, y1: s.y, x2: p.newX, y2: p.newY}, p.sponsorG));
      p.friendLinks = p.friends.map(s => S('line', {class: 'rule-strong', x1: cx, y1: cy, x2: s.x, y2: s.y}, p.sponsorG));
      p.sG = p.strangers.map(s => { const gl = Glyph(p.sponsorG, 'ag', 'ag-gold'); gl.at(s.x, s.y, 6, 0.6); return gl; });
      p.fG = p.friends.map(s => { const gl = Glyph(p.sponsorG, 'ag', 'ag-gold'); gl.at(s.x, s.y, 6, 0.4); return gl; });
      S('text', {class: 't-mut', x: W - 30, y: cy - 50, 'text-anchor': 'middle'}, p.sponsorG, 'others');
      S('text', {class: 't-mut', x: cx + 40 + 26 * 3 - 8, y: cy + 50}, p.sponsorG, 'your friends');
      p.partG = S('g', {'aria-hidden': 'true'}, p.sponsorG);
      p.parts = Particles(p.partG);
      // The chart: your net units against keeping the vouch, over 90 days.
      const c = geo.chart;
      p.chartG = S('g', null, g);
      p.zero = S('line', {class: 'rule-strong', x1: c.x0, x2: c.x1}, p.chartG);
      p.yTop = S('text', {class: 't-mut', x: c.x0 - 6, 'text-anchor': 'end'}, p.chartG);
      p.yBot = S('text', {class: 't-mut', x: c.x0 - 6, 'text-anchor': 'end'}, p.chartG);
      p.yZero = S('text', {class: 't-mut', x: c.x0 - 6, 'text-anchor': 'end'}, p.chartG, '0');
      p.xLbl = S('text', {class: 't-mut', x: c.x1, y: c.y1 + 16, 'text-anchor': 'end'}, p.chartG, '90 days');
      p.x0Lbl = S('text', {class: 't-mut', x: c.x0, y: c.y1 + 16}, p.chartG, 'day 0');
      p.cTitle = S('text', {class: 't-lbl', x: c.x0, y: c.y0 - 8}, p.chartG);
      p.avg = S('path', {class: 'line-avg', d: 'M0 0'}, p.chartG);
      p.avgTxt = S('text', {class: 't-att t-halo', 'text-anchor': 'end'}, p.chartG);
      p.line = S('path', {class: 'line-you', d: 'M0 0'}, p.chartG);
      p.caught = S('circle', {class: 'flash', r: 0}, p.chartG);
      p.caughtTxt = S('text', {class: 't-att t-halo'}, p.chartG);
      p.endTxt = S('text', {class: 't-lbl t-halo'}, p.chartG);
      draw();
    };
    const yOf = v => { const c = geo.chart, [Y0, Y1] = RANGE[st.rules]; return c.y1 - (clamp(v, Y0, Y1) - Y0) / (Y1 - Y0) * (c.y1 - c.y0); };
    const xOf = d => { const c = geo.chart; return c.x0 + clamp(d / DAYS, 0, 1) * (c.x1 - c.x0); };
    // A seller's run: paid each day until the ring is caught, then the loss.
    const newRun = rules => {
      const pts = [0];
      let v = 0, caught = -1;
      for (let d = 1; d <= DAYS; d++) {
        if (caught < 0) { v += OFFER[rules]; if (rules === 'flow' && Math.random() < P_DAY) { v -= LOSS; caught = d; } }
        pts.push(v);
      }
      return {pts, caught, rules};
    };
    const path = (pts, upto) => {
      let d = '';
      const last = Math.floor(clamp(upto, 0, DAYS));
      for (let i = 0; i <= last; i++) d += (i ? 'L' : 'M') + xOf(i).toFixed(1) + ' ' + yOf(pts[i]).toFixed(1);
      return d || 'M0 0';
    };
    const draw = () => {
      if (!p) return;
      const sponsor = st.mode === 'sponsor', c = geo.chart;
      const {cx, cy, W} = geo;
      p.ringG.style.display = sponsor ? 'none' : '';
      p.sponsorG.style.display = sponsor ? '' : 'none';
      p.offer.style.display = p.offerTxt.style.display = sponsor ? 'none' : '';
      p.you.at(cx, cy, 15, 1);
      set(p.youTxt, {x: cx + 22, y: cy - 22}); set(p.youSub, {x: cx + 22, y: cy - 8});
      txt(p.youSub, 'standing 100 a day');
      if (!sponsor) {
        const sold = st.choice === 'sell';
        set(p.offer, {x1: cx + 16, y1: cy, x2: W - 96, y2: cy, class: sold ? 'edge-sold' : 'edge'});
        set(p.offerTxt, {x: (cx + W - 96) / 2 + 10, y: cy + 18});
        txt(p.offerTxt, sold ? 'sold: ' + OFFER[st.rules] + ' a day to you' : 'offer: ' + OFFER[st.rules] + ' a day');
        const inflow = sold ? (st.rules === 'count' ? 1 : 0.35) : 0;
        p.ring.forEach(r => r.gl.at(r.x, r.y, 5, inflow));
        // The chart.
        p.chartG.style.display = '';
        set(p.zero, {y1: yOf(0), y2: yOf(0)});
        set(p.yZero, {y: yOf(0) + 4});
        const [Y0, Y1] = RANGE[st.rules];
        txt(p.yTop, '+' + count(Y1)); set(p.yTop, {y: yOf(Y1) + 10});
        txt(p.yBot, '−' + count(-Y0)); set(p.yBot, {y: yOf(Y0)});
        txt(p.cTitle, 'What selling earns you, against keeping the vouch');
        const run = st.run;
        if (st.choice === 'sell' && run) {
          set(p.line, {d: path(run.pts, progress * DAYS)});
          const showAvg = run.rules === 'flow';
          const avgPts = []; for (let d = 0; d <= DAYS; d++) avgPts.push(expected(d));
          set(p.avg, {d: showAvg ? path(avgPts, progress * DAYS) : 'M0 0'});
          txt(p.avgTxt, showAvg && progress >= 0.6 ? 'average of 1,000 sellers' : '');
          set(p.avgTxt, {x: xOf(52), y: yOf(expected(52)) - 8, 'text-anchor': 'middle'});
          const caughtNow = run.caught > 0 && progress * DAYS >= run.caught;
          set(p.caught, {cx: caughtNow ? xOf(run.caught) : 0, cy: caughtNow ? yOf(run.pts[run.caught]) : 0, r: caughtNow ? 4 : 0});
          txt(p.caughtTxt, caughtNow ? 'caught on day ' + run.caught + ': −' + count(LOSS) : '');
          set(p.caughtTxt, {x: caughtNow ? clamp(xOf(run.caught) + 8, c.x0, c.x1 - 150) : 0, y: caughtNow ? yOf(run.pts[run.caught]) + 16 : 0});
          const end = run.pts[DAYS];
          txt(p.endTxt, progress >= 1 ? 'you: ' + signed(end) : '');
          const above = !showAvg || end >= expected(DAYS);
          set(p.endTxt, {x: c.x1, y: yOf(end) + (above ? -8 : 16), 'text-anchor': 'end'});
        } else if (st.choice === 'keep') {
          set(p.line, {d: 'M' + xOf(0) + ' ' + yOf(0) + 'L' + xOf(progress * DAYS) + ' ' + yOf(0)});
          set(p.avg, {d: 'M0 0'}); txt(p.avgTxt, ''); set(p.caught, {r: 0}); txt(p.caughtTxt, '');
          txt(p.endTxt, progress >= 1 ? 'you keep your standing: 0' : ''); set(p.endTxt, {x: c.x1 - 4, y: yOf(0) - 8, 'text-anchor': 'end'});
        } else {
          set(p.line, {d: 'M0 0'}); set(p.avg, {d: 'M0 0'}); txt(p.avgTxt, ''); set(p.caught, {r: 0}); txt(p.caughtTxt, ''); txt(p.endTxt, '');
        }
      } else {
        // The sponsor dividend: independent inflow to your newcomer earns you a dividend.
        const others = st.who === 'others';
        p.newcomer.at(p.newX, p.newY, 9, others ? clamp(progress, 0.15, 1) : 0.4);
        p.sEdges.forEach(e => { e.setAttribute('class', others ? 'edge-hot' : 'edge'); });
        p.fEdges.forEach(e => { e.setAttribute('class', others ? 'edge' : 'edge-hot'); });
        p.chartG.style.display = '';
        txt(p.cTitle, 'Your sponsor dividend');
        const div = d => others ? 60 * (1 - Math.exp(-d / 25)) : 0;
        const pts = []; for (let d = 0; d <= DAYS; d++) pts.push(div(d));
        const yS = v => c.y1 - clamp(v / 70, 0, 1) * (c.y1 - c.y0);
        let dd = ''; for (let i = 0; i <= Math.floor(progress * DAYS); i++) dd += (i ? 'L' : 'M') + xOf(i).toFixed(1) + ' ' + yS(pts[i]).toFixed(1);
        set(p.line, {d: dd || 'M0 0'}); set(p.avg, {d: 'M0 0'}); txt(p.avgTxt, ''); set(p.caught, {r: 0}); txt(p.caughtTxt, '');
        set(p.zero, {y1: c.y1, y2: c.y1}); set(p.yZero, {y: c.y1 + 4});
        txt(p.yTop, ''); txt(p.yBot, '');
        txt(p.endTxt, progress >= 1 ? (others ? 'earned: ' + count(div(DAYS)) : 'earned: 0, the inflow is not independent') : '');
        set(p.endTxt, {x: c.x1 - 4, y: yS(pts[DAYS]) - 8, 'text-anchor': 'end'});
      }
    };
    const readout = () => {
      let s;
      if (st.mode === 'sponsor') s = st.who === 'others'
        ? 'Agents outside your circle endorse your newcomer. Each time that independent inflow sets a new high, you earn a dividend.'
        : 'Only your friends endorse your newcomer. That inflow is not independent of you, so you earn nothing.';
      else if (!st.choice) s = st.rules === 'count'
        ? 'The ring offers 10 a day for one vouch. Under counted trust your vouch costs you nothing to give.'
        : 'The ring offers 50 a day: half your standing, the most your vouch can carry. If the ring is caught, you lose 30 days of standing, 3,000.';
      else if (st.choice === 'keep') s = 'You keep your vouch and your standing.';
      else if (st.rules === 'count') s = 'You earn 10 a day and risk nothing. Your vouch carries 260 a day of standing to the ring, 26 times what it pays you. Everyone who is asked sells.';
      else {
        const r = st.run;
        s = progress < 1 ? 'Ninety days of selling…' : (r && r.caught > 0 ? 'The ring was caught on day ' + r.caught + ': you lost 3,000 and ended ' + signed(r.pts[DAYS]) + '.' : 'The ring was not caught this time: you ended ' + signed(r ? r.pts[DAYS] : 0) + '. Sell again.') +
          ' On average a seller ends ' + signed(expected(DAYS)) + ': each day costs a 3.1% chance of losing 3,000, about 94, and pays 50.';
      }
      txt(read, s);
      describe(svg, 'You, with a standing of 100 a day. ' + s);
    };
    const play = () => {
      stopTween(); progress = 0;
      stopTween = tween(2600, k => { progress = k; draw(); if (k === 1) readout(); });
      readout();
    };
    const choose = c => { st.mode = 'sell'; st.choice = c; st.run = c === 'sell' ? newRun(st.rules) : null; txt(bSell, 'Sell again'); play(); };
    bSell.addEventListener('click', () => choose('sell'));
    bKeep.addEventListener('click', () => choose('keep'));
    radios(fig, 'rules').forEach(r => r.addEventListener('change', () => { stopTween(); st.rules = radioValue(fig, 'rules') === 'flow' ? 'flow' : 'count'; st.choice = ''; st.run = null; progress = 0; txt(bSell, 'Sell'); draw(); readout(); }));
    radios(fig, 'who').forEach(r => r.addEventListener('change', () => { st.who = radioValue(fig, 'who') === 'friends' ? 'friends' : 'others'; sponsorFlow(); play(); }));
    const sponsorFlow = () => {
      if (!p) return;
      if (st.mode !== 'sponsor') { p.parts.clear(); particles.stop(); return; }
      const from = st.who === 'others' ? p.strangers : p.friends;
      const pts = from.concat([{x: p.newX, y: p.newY}, {x: geo.cx, y: geo.cy}]);
      const list = from.map((_, i) => ({u: i, v: from.length})).concat([{u: from.length, v: from.length + 1}]);
      const flows = list.map((e, i) => (i < from.length ? 1 : st.who === 'others' ? 1.5 : 0));
      p.parts.set(list, flows, i => pts[i]);
      if (api.onScreen) particles.start();
    };
    const api = {
      resize() { layout(); sponsorFlow(); },
      step(i, instant) {
        stopTween();
        if (i <= 3) {
          st.mode = 'sell';
          const rules = i <= 1 ? 'count' : 'flow';
          if (st.rules !== rules) { st.rules = rules; st.choice = ''; st.run = null; }
          setRadio(fig, 'rules', st.rules);
          if (i === 0 || i === 2) { st.choice = ''; st.run = null; progress = 0; txt(bSell, 'Sell'); }
          if ((i === 1 || i === 3) && st.choice !== 'sell') { st.choice = 'sell'; st.run = newRun(st.rules); progress = instant ? 1 : 0; if (!instant) { play(); sponsorFlow(); return; } }
          sponsorFlow(); draw(); readout();
          return;
        }
        st.mode = 'sponsor'; setRadio(fig, 'who', st.who);
        sponsorFlow();
        if (instant) { progress = 1; draw(); readout(); } else play();
      },
      init() { layout(); readout(); },
      visible(on) { if (on && st.mode === 'sponsor') particles.start(); else particles.stop(); },
    };
    return api;
  });

  // ======================= 7. Rings =======================
  register('rings', fig => {
    const svg = fig.querySelector('svg'), read = out(fig, 'read');
    const tradeIn = inp(fig, 'trade'), boughtIn = inp(fig, 'bought'), keysIn = inp(fig, 'keys');
    const rnd = prng(777);
    const kinds = [];
    const add = k => { kinds.push(k); return kinds.length - 1; };
    const A = [0, 1].map(() => add('a')), Bs = [0, 1].map(() => add('b'));
    const Hn = []; for (let i = 0; i < 26; i++) Hn.push(add('h'));
    const G = []; for (let i = 0; i < 10; i++) G.push(add('g')); // the group that trades
    const RMAX = 24, R = []; for (let i = 0; i < RMAX; i++) R.push(add('r'));
    const n = kinds.length;
    const base = [], seen = new Set();
    const edge = (list, u, v) => { const k = u + '>' + v; if (u === v || seen.has(k)) return; seen.add(k); list.push({u, v}); };
    const honest = Hn.concat(G);
    const pick = l => l[Math.floor(rnd() * l.length)];
    A.concat(Bs).forEach(s => { for (let k = 0; k < 8; k++) edge(base, s, pick(Hn)); edge(base, s, pick(G)); });
    honest.forEach(u => { for (let k = 0; k < 3; k++) edge(base, u, pick(honest)); });
    const trades = []; G.forEach((u, i) => G.forEach((v, j) => { if (i !== j && (j - i + G.length) % G.length <= 3) edge(trades, u, v); }));
    // Positions: seeds left, community centre-left with the group in a cluster, ring right.
    const pos = new Array(n).fill(null);
    A.forEach((s, i) => { pos[s] = {x: 0.03, y: 0.18 + 0.16 * i}; });
    Bs.forEach((s, i) => { pos[s] = {x: 0.03, y: 0.66 + 0.16 * i}; });
    G.forEach((g, i) => { const a = 2 * Math.PI * i / G.length; pos[g] = {x: 0.43 + 0.07 * Math.cos(a), y: 0.5 + 0.2 * Math.sin(a)}; });
    Hn.forEach(h => { pos[h] = {x: 0.12 + 0.46 * rnd(), y: 0.06 + 0.88 * rnd()}; });
    forceLayout(pos, Hn, base.map(e => ({u: e.u, v: e.v})), {x0: 0.1, x1: 0.6, y0: 0.06, y1: 0.94}, 0.6, 200);
    const ringPos = (i, m) => { const a = 2 * Math.PI * i / m - Math.PI / 2; return {x: 0.86 + 0.1 * Math.cos(a), y: 0.5 + 0.34 * Math.sin(a)}; };
    // Phase A scores: the group's share, trading or not.
    const groupShare = (score, pool) => G.reduce((a, i) => a + score[i], 0) / (pool || score.reduce((a, b) => a + b, 0) || 1);
    const transitAll = kinds.map(k => (k === 'r' ? 0 : U));
    // The pool is one fair share (U units) for every honest agent.
    const poolUnits = U * honest.length, shares = poolUnits / U;
    const cache = {};
    const phaseA = trade => cache['a' + trade] || (cache['a' + trade] = (() => {
      const list = trade ? base.concat(trades) : base;
      const pr = pagerank(n, list, A.concat(Bs)), fl = flowScores(n, list, A, Bs, transitAll, poolUnits);
      return {list, pr: groupShare(pr.score), fl: groupShare(fl.score), flow: fl};
    })());
    // Phase B: k endorsements bought from the agents with the most standing, into a ring.
    const own = phaseA(false).flow.score;
    const sellers = Hn.slice().sort((a, b) => own[b] - own[a] || a - b);
    const transit = kinds.map((k, i) => (k === 'r' ? 0 : Math.floor(0.5 * own[i])));
    // (Flow from both seed sets at once here: the two-set minimum of chapter 5
    // would only lower what the ring gets.)
    const phaseB = k => cache['b' + k] || (cache['b' + k] = (() => {
      const bought = sellers.slice(0, k).map((s, j) => ({u: s, v: R[(j * 5) % RMAX], sold: true}));
      const ringInner = R.map((u, i) => ({u, v: R[(i + 1) % RMAX]}));
      const list = base.concat(bought, ringInner);
      const f = maxflow(n, list, A.concat(Bs), transit, poolUnits, U);
      const fl = {score: f.absorbed, edgeFlow: f.edgeFlow};
      const pr = pagerank(n, list, A.concat(Bs));
      const ringFlow = R.reduce((a, i) => a + fl.score[i], 0);
      const ceiling = bought.reduce((a, e) => a + Math.min(U, transit[e.u]), 0);
      return {list, bought, ringFlow, ceiling, prShares: R.reduce((a, i) => a + pr.score[i], 0) * shares, flow: fl};
    })());
    const st = {phase: 'a', trade: false, k: 0, keys: 100};
    let p = null, geo = null, shown = null, stopTween = () => {};
    const particles = loop(dt => { if (p) p.parts.step(dt); });
    const target = () => ({ringIn: st.phase === 'b' ? 1 : 0, trade: st.trade ? 1 : 0, k: st.phase === 'b' ? st.k : 0});
    const layout = () => {
      const W = widthOf(fig), Hh = clamp(W * 0.62, 230, maxFigH() - 20);
      resetSvg(svg); sizeSvg(svg, W, Hh);
      geo = {W, Hh};
      const g = S('g', null, svg);
      p = {};
      p.groupBg = S('ellipse', {class: 'region-gold'}, g);
      p.groupLbl = S('text', {class: 't-gold t-halo', 'text-anchor': 'middle'}, g, 'the group');
      p.ringBg = S('rect', {class: 'region', rx: 10}, g);
      p.ringLbl = S('text', {class: 't-att', 'text-anchor': 'middle'}, g);
      p.edgesG = S('g', null, g);
      p.base = base.map(() => S('line', {class: 'edge'}, p.edgesG));
      p.trades = trades.map(() => S('line', {class: 'edge-hot'}, p.edgesG));
      p.pipes = [];
      for (let j = 0; j < 20; j++) p.pipes.push(S('line', {class: 'pipe'}, p.edgesG));
      p.partG = S('g', {'aria-hidden': 'true'}, g);
      p.parts = Particles(p.partG);
      p.nodes = kinds.map(k => Glyph(g, k === 'a' || k === 'b' ? 'ag-seed' : k === 'r' ? 'ag-fake-ring' : 'ag', 'ag-gold'));
      p.letters = kinds.map(k => (k === 'a' || k === 'b' ? S('text', {class: 'ag-letter'}, g, k.toUpperCase()) : null));
      // The gauge: what the ring captured, against the ceiling (the capture bound).
      p.gauge = S('g', null, g);
      p.gTrack = S('rect', {class: 'basin-bg', height: 10, rx: 2}, p.gauge);
      p.gFill = S('rect', {class: 'gold', height: 10, rx: 2}, p.gauge);
      p.gPr = S('rect', {class: 'pr-outline', height: 10, rx: 2}, p.gauge);
      p.gPrLbl = S('text', {class: 't-att t-halo', 'text-anchor': 'end'}, p.gauge);
      p.gCeil = S('line', {class: 'ceiling'}, p.gauge);
      p.gLbl = S('text', {class: 't-lbl'}, p.gauge);
      p.gCeilLbl = S('text', {class: 't-gold t-halo'}, p.gauge);
      if (shown) draw(shown);
      particlesFor();
    };
    const P = i => {
      let q = pos[i];
      if (kinds[i] === 'r') q = ringPos(R.indexOf(i), Math.min(RMAX, st.keys));
      return {x: 12 + q.x * (geo.W - 24), y: 8 + q.y * (geo.Hh - 50)};
    };
    const draw = v => {
      shown = v;
      if (!p) return;
      const {W, Hh} = geo;
      const aT = phaseA(v.trade > 0.5), kk = Math.round(v.k), b = phaseB(kk);
      const sc = v.ringIn > 0.5 ? b.flow.score : aT.flow.score;
      const max = Math.max(1, ...sc.filter((_, i) => kinds[i] !== 'r'));
      kinds.forEach((k, i) => {
        const gl = p.nodes[i], q = P(i);
        if (k === 'r') {
          const j = R.indexOf(i), on = j < Math.min(RMAX, st.keys);
          gl.g.style.display = on ? '' : 'none';
          gl.g.style.opacity = String(v.ringIn);
          // What each key holds: the ring's capture split among all its keys.
          gl.at(q.x, q.y, 5, v.ringIn * clamp(b.ringFlow / st.keys / U * 4, 0, 1));
          return;
        }
        const seed = k === 'a' || k === 'b';
        gl.at(q.x, q.y, seed ? 8 : 5.5, seed ? 1 : sc[i] / max);
        if (p.letters[i]) set(p.letters[i], {x: q.x, y: q.y + 0.5});
      });
      base.forEach((e, j) => { const a = P(e.u), c = P(e.v); set(p.base[j], {x1: a.x, y1: a.y, x2: c.x, y2: c.y}); });
      trades.forEach((e, j) => { const a = P(e.u), c = P(e.v); set(p.trades[j], {x1: a.x, y1: a.y, x2: c.x, y2: c.y}); p.trades[j].style.opacity = String(v.trade * (1 - v.ringIn)); });
      p.pipes.forEach((pipe, j) => {
        const e = b.bought[j];
        if (!e || v.ringIn < 0.01) { pipe.style.display = 'none'; return; }
        pipe.style.display = '';
        const a = P(e.u), c = P(e.v);
        set(pipe, {x1: a.x, y1: a.y, x2: c.x, y2: c.y});
      });
      const gc = P(G[0]);
      set(p.groupBg, {cx: 12 + 0.43 * (W - 24), cy: 8 + 0.5 * (Hh - 50), rx: 0.1 * (W - 24), ry: 0.27 * (Hh - 50)});
      p.groupBg.style.opacity = p.groupLbl.style.opacity = String(1 - v.ringIn);
      set(p.groupLbl, {x: 12 + 0.43 * (W - 24), y: 8 + 0.2 * (Hh - 50)});
      const rx = 12 + 0.75 * (W - 24), rw = 0.22 * (W - 24);
      set(p.ringBg, {x: rx, y: 8 + 0.1 * (Hh - 50), width: rw, height: 0.8 * (Hh - 50)});
      p.ringBg.style.opacity = p.ringLbl.style.opacity = String(v.ringIn);
      txt(p.ringLbl, count(st.keys) + ' fake keys');
      set(p.ringLbl, {x: rx + rw / 2, y: 8 + 0.1 * (Hh - 50) - 5});
      // Gauge, in fair shares: captured, and the ceiling k × half a share.
      const gy = Hh - 26, gx = 0, gw = W - 2, maxShares = 24;
      p.gauge.style.opacity = String(v.ringIn);
      const got = b.ringFlow / U, ceil = b.ceiling / U;
      set(p.gTrack, {x: gx, y: gy, width: gw});
      set(p.gFill, {x: gx, y: gy, width: gw * clamp(got / maxShares, 0, 1)});
      set(p.gPr, {x: gx + 0.5, y: gy + 0.5, width: Math.max(0, gw * clamp(b.prShares / maxShares, 0, 1) - 1), height: 9});
      txt(p.gPrLbl, kk > 0 ? 'PageRank-style: ' + fmtShares(b.prShares) + (b.prShares > maxShares ? ' →' : '') : '');
      set(p.gPrLbl, {x: gw, y: gy - 8});
      const cxl = gx + gw * clamp(ceil / maxShares, 0, 1);
      set(p.gCeil, {x1: cxl, x2: cxl, y1: gy - 6, y2: gy + 16});
      txt(p.gLbl, 'Flow: the ring captured ' + fmtShares(got) + (got === 1 ? ' fair share' : ' fair shares'));
      set(p.gLbl, {x: gx, y: gy - 8});
      txt(p.gCeilLbl, kk > 0 ? 'ceiling ' + fmtShares(ceil) : 'ceiling 0');
      set(p.gCeilLbl, {x: clamp(cxl + 4, 0, W - 80), y: gy + 22});
    };
    const fmtShares = x => (Math.round(Math.max(0, fin(x)) * 10) / 10).toFixed(1).replace(/\.0$/, '');
    const particlesFor = () => {
      if (!p) return;
      const list = st.phase === 'b' ? phaseB(st.k).list : phaseA(st.trade).list;
      const fl = st.phase === 'b' ? phaseB(st.k).flow.edgeFlow : phaseA(st.trade).flow.edgeFlow;
      p.parts.set(list, fl, P, e => kinds[e.v] === 'r');
      if (api.onScreen) particles.start();
    };
    const readout = () => {
      let s;
      if (st.phase === 'a') {
        const a0 = phaseA(false), a1 = phaseA(true), a = st.trade ? a1 : a0;
        txt(out(fig, 'pr'), pct(a.pr)); setWidth(out(fig, 'pr-bar'), a.pr);
        txt(out(fig, 'fl'), pct(a.fl)); setWidth(out(fig, 'fl-bar'), a.fl);
        s = st.trade ? 'Trading moved the group from ' + pct(a0.pr) + ' to ' + pct(a1.pr) + ' of all trust under PageRank-style scoring, and from ' + pct(a0.fl) + ' to ' + pct(a1.fl) + ' under flow.'
          : 'The group\'s share of all trust, before trading: ' + pct(a0.pr) + ' under PageRank-style scoring, ' + pct(a0.fl) + ' under flow.';
      } else {
        const b = phaseB(st.k);
        s = st.k === 0 ? 'No endorsement reaches the ring: it captures nothing.'
          : count(st.k) + (st.k === 1 ? ' bought endorsement lets' : ' bought endorsements let') + ' in ' + fmtShares(b.ringFlow / U) + ' fair shares, against a ceiling of ' + fmtShares(b.ceiling / U) +
            ', split among ' + count(st.keys) + ' keys. Under PageRank-style scoring the same ring would hold ' + fmtShares(b.prShares) + ' fair shares.';
      }
      txt(read, s);
      describe(svg, s);
      txt(out(fig, 'bought'), count(st.k)); txt(out(fig, 'keys'), count(st.keys));
    };
    const go = instant => {
      stopTween();
      const to = target(), from = shown || to;
      stopTween = tween(instant ? 0 : 900, k => { const e = ease(k), m = {}; for (const key in to) m[key] = lerp(from[key], to[key], e); m.k = to.k; draw(m); }, particlesFor);
      readout();
    };
    tradeIn.addEventListener('change', () => { st.trade = tradeIn.checked; go(); });
    boughtIn.addEventListener('input', () => { st.k = clamp(Math.round(Number(boughtIn.value)), 0, 20); stopTween(); draw(target()); particlesFor(); readout(); });
    keysIn.addEventListener('input', () => { stopTween(); st.keys = logCount(keysIn.value); draw(target()); readout(); });
    const api = {
      resize: layout,
      step(i, instant) {
        stopTween();
        if (i <= 1) { st.phase = 'a'; if (i === 1 && !st.trade) st.trade = true; if (i === 0) st.trade = false; tradeIn.checked = st.trade; }
        if (i >= 2) {
          st.phase = 'b';
          if (st.k === 0) { st.k = 5; boughtIn.value = '5'; }
          if (i === 2) { st.keys = 100; keysIn.value = '20'; }
        }
        if (i === 3 && st.keys < 1000000) {
          go(instant);
          const from = Number(keysIn.value);
          stopTween = tween(instant ? 0 : 2200, k => { keysIn.value = String(Math.round(lerp(from, 60, ease(k)))); st.keys = logCount(keysIn.value); readout(); if (shown) draw(target()); });
          return;
        }
        go(instant);
      },
      init() { layout(); draw(target()); readout(); },
      visible(on) { if (on) particles.start(); else particles.stop(); },
    };
    return api;
  });

  // ======================= 8. Bad days =======================
  register('levers', fig => {
    const svg = fig.querySelector('svg'), read = out(fig, 'read');
    const budgetIn = inp(fig, 'budget'), floodIn = inp(fig, 'flood');
    const leverIns = [...fig.querySelectorAll('[data-in="lever"]')];
    const cas = Cascade(svg, 'levers');
    const BUDGETS = [16, 32, 64, 128, 256].map(x => x * MiB);
    let shown = null, stopTween = () => {};
    const model = () => simulateDay({
      B: BUDGETS[clamp(Math.round(Number(budgetIn.value)), 0, 4)],
      flood: Number(floodIn.value) > 0 ? logCount(floodIn.value) : 0,
      floodTier: Number(radioValue(fig, 'kind')) === 4 ? 4 : 3,
      levers: new Set(leverIns.filter(x => x.checked).map(x => x.value)),
    });
    const NOON = 12;
    const view = m => {
      const fr = frameAt(m, NOON);
      const extra = {};
      if (m.floodBlocked) { extra.blocked = m.floodBlocked === 'paused' ? 'new keys paused: the flood gets nothing' : 'the flood is refused'; extra.blockedRow = m.ft - 1; }
      return cascadeState(m, fr, extra);
    };
    const mix = (a, b, k) => {
      const o = Object.assign({}, b);
      for (const key of ['cap', 'level', 'fill', 'flood']) o[key] = b[key].map((v, i) => lerp(a[key][i] || 0, v || 0, k));
      o.flood = o.flood.map(Math.round);
      return o;
    };
    const draw = v => { shown = v; cas.draw(v); };
    const update = instant => {
      const m = model(), to = view(m), from = shown || to;
      stopTween();
      stopTween = tween(instant ? 0 : 500, k => draw(mix(from, to, ease(k))));
      const fr = frameAt(m, NOON);
      txt(out(fig, 'budget'), bytes(BUDGETS[clamp(Math.round(Number(budgetIn.value)), 0, 4)]));
      const nf = Number(floodIn.value) > 0 ? logCount(floodIn.value) : 0;
      txt(out(fig, 'flood'), nf ? count(nf) + (m.ft === 4 ? ' networks' : ' keys') : 'none');
      const served = [1, 2, 3, 4].map(t => TIER[t].toLowerCase() + ' ' + pct(servedShare(m, fr, t)));
      let s = 'By noon: ' + served.join(', ') + ' of what each tier asked for so far.';
      if (nf && !m.floodBlocked) s += ' The flood took ' + bytes(fr.floodGot) + ', ' + pct(fr.floodGot / m.B) + ' of the day.';
      if (m.floodBlocked) s += m.floodBlocked === 'paused' ? ' The new keys get nothing while the lever is pulled.' : ' The flood is refused.';
      if (m.unallocated > 0) s += ' ' + bytes(m.unallocated) + ' is not poured at all.';
      txt(read, s);
      describe(svg, 'The four basins at noon. ' + s);
    };
    [budgetIn, floodIn].forEach(x => x.addEventListener('input', () => update(true)));
    radios(fig, 'kind').concat(leverIns).forEach(x => x.addEventListener('change', () => update()));
    const layout = () => { cas.layout(widthOf(fig), narrow() ? 250 : clamp(widthOf(fig) * 0.56, 220, maxFigH() - 120)); if (shown) cas.draw(shown); };
    return {
      resize: layout,
      step(i, instant) {
        if (i === 0) { leverIns.forEach(x => { x.checked = false; }); if (Number(floodIn.value) === 0) floodIn.value = '60'; }
        if (i === 1 && !leverIns.some(x => x.checked)) { const pk = leverIns.find(x => x.value === 'pause-new-keys'); if (pk) pk.checked = true; setRadio(fig, 'kind', 3); if (Number(floodIn.value) === 0) floodIn.value = '60'; }
        update(instant);
      },
      init() { layout(); update(true); },
      hidden() { cas.stop(); },
    };
  });

  // ======================= 9. Today's live basins =======================
  register('live', fig => {
    const svg = fig.querySelector('svg'), block = document.getElementById('waterfall-live');
    if (!block) return null;
    const cas = Cascade(svg, 'live');
    const num = n => { const v = Number(n); return isFinite(v) && v > 0 ? v : 0; };
    const b = block.querySelector('[data-role="budget"]');
    const B = b ? num(b.dataset.value) : 0;
    const tiers = [1, 2, 3, 4].map(t => {
      const row = block.querySelector('tr[data-tier="' + t + '"]'), v = {};
      if (row) row.querySelectorAll('[data-role]').forEach(c => { v[c.dataset.role] = num(c.dataset.value); });
      return v;
    });
    const drawn = t => (t.claimed || 0) + (t.lent || 0);
    const state = () => ({
      B, cap: tiers.map(t => Math.max(t.water || 0, 1)), level: tiers.map(t => Math.max(0, (t.water || 0) - drawn(t))),
      fill: tiers.map(() => null), agents: tiers.map(t => t.claimants || 0), flood: [0, 0, 0, 0],
      num: tiers.map(t => bytes(Math.max(0, (t.water || 0) - drawn(t))) + ' of ' + bytes(t.water || 0) + ' left'),
      src: 'Budget ' + bytes(B), srcNote: '', pourTo: -1, spill: [0, 0, 0],
    });
    const layout = () => { cas.layout(widthOf(fig), clamp(widthOf(fig) * 0.5, 210, 320)); const s = state(); cas.draw(s); describe(svg, 'Today on this server: ' + s.num.map((x, i) => TIER[i + 1] + ' ' + x).join('; ') + '. The table below has the same numbers.'); };
    return {resize: layout, init: layout, step() {}};
  });

  // ======================= 9. Look up an agent =======================
  (() => {
    const form = root.querySelector('[data-fig="lookup"]');
    if (!form) return;
    const input = form.querySelector('input'), button = form.querySelector('button[type="submit"]'), parts = out(form, 'parts');
    const tier = t => ['grant pool', 'trusted', 'proven', 'signed', 'anonymous'][t] || 'tier ' + t;
    const dl = (parent, rows) => { const list = H('dl', '', parent); rows.forEach(([k, v]) => { const d = H('div', '', list); H('dt', '', d, k); H('dd', '', d, v); }); return list; };
    const render = (data, url) => {
      clear(parts);
      const head = H('p', 'tx-small', parts);
      head.append('Agent ');
      const link = H('a', '', head, String(data.agent || '').slice(0, 16) + '…');
      link.href = '/agent/' + encodeURIComponent(data.agent || '');
      head.append(', run ' + count(data.run || 0) + ', ' + String(data.mode || '') + ' mode' + (data.stale ? ', stale: the last run did not finish' : '') + '. ');
      const json = H('a', '', head, 'Read as JSON'); json.href = url;
      const total = Number(data.collateral && data.collateral.total) || 0;
      H('p', 'tx-total', parts, count(total));
      H('p', 'tx-small', parts, 'Collateral: what it would cost to rebuild this identity, in the unit named at /api/params/trust.');
      const proofs = Array.isArray(data.proofs) ? data.proofs : [];
      const proofSum = proofs.reduce((a, q) => a + Math.max(0, Number(q.contribution) || 0), 0);
      if (total > 0) {
        const stack = H('div', 'tx-stack', parts); stack.setAttribute('aria-hidden', 'true');
        const legend = H('ul', 'tx-legend', parts);
        const part = (cls, label, v) => { setWidth(H('span', cls, stack), v / total); H('li', '', legend, label + ' ' + count(v)); };
        proofs.forEach(q => { if (Number(q.contribution) > 0) part(q.kind === 'domain' ? 's-domain' : q.kind === 'history' ? 's-history' : 's-other', String(q.kind), Number(q.contribution)); });
        if (total - proofSum > 0) part('s-endorse', 'endorsements', total - proofSum);
      }
      const e = data.endorsements || {}, f = e.flow || {}, t = data.tier || {};
      H('h4', '', parts, 'Endorsement flow, in twentieths of a fair share');
      dl(parts, [['From set A', count(f.a || 0)], ['From set B', count(f.b || 0)], ['Counted', count(f.effective || 0)], ['Endorsers', count(e.endorsers_total || 0)], ['Down votes', count(e.down_votes || 0)]]);
      H('h4', '', parts, 'Proofs');
      if (!proofs.length) H('p', 'tx-small', parts, 'No linked proofs.');
      else {
        const ul = H('ul', '', parts);
        proofs.forEach(q => {
          const li = H('li', '', ul);
          li.append(String(q.kind) + ' ');
          H('code', '', li, String(q.value || q.root || ''));
          li.append(', ' + String(q.state || '') + ': ' + count(q.contribution || 0) + (q.saturated_by ? ', counted once under ' + String(q.saturated_by) : ''));
        });
      }
      H('h4', '', parts, 'Tier and liability');
      const pen = data.liability && Array.isArray(data.liability.penalties) ? data.liability.penalties.length : 0;
      dl(parts, [['By trust', tier(t.would_be)], ['Used now', tier(t.effective)], ['Penalties', count(pen)], ['Breaker', data.breaker && data.breaker.active ? 'active' : 'not active']]);
      if (t.reason) H('p', 'tx-small', parts, String(t.reason));
      if (Array.isArray(data.caveats) && data.caveats.length) { const ul = H('ul', 'tx-small', parts); data.caveats.forEach(c => H('li', '', ul, String(c))); }
    };
    const fail = s => { clear(parts); H('p', 'tx-error', parts, s); };
    form.addEventListener('submit', async event => {
      event.preventDefault();
      const id = input.value.trim();
      if (!input.checkValidity() || !id) { fail('Enter a 64-character fingerprint or a handle: letters, digits, dot, dash or underscore.'); input.focus(); return; }
      const url = '/api/agent/' + encodeURIComponent(id) + '/trust';
      button.disabled = true; button.setAttribute('aria-busy', 'true');
      try {
        const res = await fetch(url, {headers: {Accept: 'application/json'}, credentials: 'omit'});
        const body = await res.json().catch(() => null);
        if (res.ok && body && body.ok && body.data) render(body.data, url);
        else if (res.status === 404) fail('No public agent with that fingerprint or handle.');
        else fail(body && body.error && body.error.message ? String(body.error.message) : 'The trust estimate could not be read right now. Please try again shortly.');
      } catch (_) {
        fail('The trust estimate could not be read: the request did not complete. Check your connection and try again.');
      } finally {
        button.disabled = false; button.removeAttribute('aria-busy');
      }
    });
  })();

  // Sliders speak their value as the page writes it ("1,000,000", "08:00 UTC").
  root.querySelectorAll('input[type="range"][id]').forEach(input => {
    const o = root.querySelector('output[for="' + input.id + '"]');
    if (!o) return;
    const sync = () => input.setAttribute('aria-valuetext', o.textContent);
    sync();
    if (window.MutationObserver) new MutationObserver(sync).observe(o, {childList: true, characterData: true, subtree: true});
  });

  // ---------- start: size every figure, then let the scroll drive the steps ----------
  figs.forEach(api => { if (api.init) api.init(); api.lastW = api.el.clientWidth; activate(api, 0, true); });
  // Steps: one observer, a thin band across the viewport (the middle on wide
  // screens, below the sticky figure on narrow ones).
  const stepOf = new Map();
  figs.forEach(api => api.steps.forEach((s, i) => stepOf.set(s, [api, i])));
  let stepIO = null;
  const observeSteps = () => {
    if (!window.IntersectionObserver) return;
    if (stepIO) stepIO.disconnect();
    stepIO = new IntersectionObserver(entries => entries.forEach(e => {
      if (!e.isIntersecting) return;
      const hit = stepOf.get(e.target);
      if (hit) activate(hit[0], hit[1], false);
    }), {rootMargin: narrow() ? '-70% 0px -29% 0px' : '-50% 0px -49% 0px'});
    stepOf.forEach((_, s) => stepIO.observe(s));
  };
  observeSteps();
  if (narrowQ && narrowQ.addEventListener) narrowQ.addEventListener('change', () => { observeSteps(); figs.forEach(api => api.resize && api.resize()); });
  // Figures: loops run only on screen.
  if (window.IntersectionObserver) {
    const figIO = new IntersectionObserver(entries => entries.forEach(e => {
      const api = figs.find(x => x.el === e.target);
      if (!api) return;
      api.onScreen = e.isIntersecting;
      if (api.visible) api.visible(e.isIntersecting);
      if (!e.isIntersecting && api.hidden) api.hidden();
    }), {rootMargin: '80px'});
    figs.forEach(api => figIO.observe(api.el));
  } else figs.forEach(api => { api.onScreen = true; if (api.visible) api.visible(true); });
  // Resize: redraw a figure whose width changed, once per frame.
  if (window.ResizeObserver) {
    let queued = new Set(), frame = 0;
    const ro = new ResizeObserver(entries => {
      entries.forEach(e => { const api = figs.find(x => x.el === e.target); if (api && Math.abs(api.el.clientWidth - api.lastW) > 1) queued.add(api); });
      if (!frame && queued.size) frame = requestAnimationFrame(() => { frame = 0; queued.forEach(api => { api.lastW = api.el.clientWidth; if (api.resize) api.resize(); }); queued = new Set(); });
    });
    figs.forEach(api => ro.observe(api.el));
  }
  if (motionQ && motionQ.addEventListener) motionQ.addEventListener('change', () => figs.forEach(api => api.visible && api.visible(api.onScreen)));
})();
