// The trust network on /trust and /trust/network: a force-directed drawing
// of /api/trust/graph on the /swarmchasing renderer (graph-gl.js), not a
// second engine. Agents are discs sized by standing and coloured by band
// (trusted, proven, signed); a gold ring marks priced roots, a vermilion one
// a penalty. Edges are vouches (gold), accepted work (blue), verified
// witnesses (green) and key links (grey, dashed), wider with weight. The
// layout runs here, a few hundred steps of repulsion between all agents and
// springs along edges, settling on screen (at once under reduced motion);
// the renderer then only moves the camera. Hover or focus an agent for its
// handle, standing, band and roots; click or Enter opens its page; arrow keys
// step through agents by standing. The figure's list is the text version and
// stays when WebGL2 is missing. Text is only ever set as textContent.
import { Renderer, STYLE } from './graph-gl.js';

const fig = document.querySelector('[data-fig="network"]');
const reduced = matchMedia('(prefers-reduced-motion: reduce)').matches;
const KINDS = { vouch: '--trust', work_accept: '--water', witness: '--witness', key_link: '--keylink' };
const KIND_NAMES = { vouch: 'vouch', work_accept: 'accepted work', witness: 'verified witness', key_link: 'key link' };
const TICKS = 320, PER_FRAME = 8, LABELS = 6;

// Every colour is a CSS token, resolved through a canvas into RGB.
const probe = document.createElement('canvas').getContext('2d', { willReadFrequently: true });
function rgbOf(css, fallback) {
  if (!css || !probe) return fallback;
  probe.clearRect(0, 0, 1, 1); probe.fillStyle = '#000'; probe.fillStyle = css; probe.fillRect(0, 0, 1, 1);
  const d = probe.getImageData(0, 0, 1, 1).data; return [d[0] / 255, d[1] / 255, d[2] / 255];
}
function readTheme() {
  const cs = getComputedStyle(fig);
  const v = (n) => cs.getPropertyValue(n).trim();
  const bg = rgbOf(v('--tn-bg'), [0.97, 0.96, 0.95]);
  const t = { bg, dark: 0.2126 * bg[0] + 0.7152 * bg[1] + 0.0722 * bg[2] < 0.35, node: rgbOf(v('--node'), [0.33, 0.33, 0.31]),
    trust: rgbOf(v('--trust'), [0.75, 0.52, 0]), trustInk: rgbOf(v('--trust-ink'), [0.55, 0.38, 0]), attack: rgbOf(v('--attack'), [0.83, 0.27, 0.17]), kinds: {} };
  for (const [k, token] of Object.entries(KINDS)) t.kinds[k] = rgbOf(v(token), [0.5, 0.5, 0.5]);
  return t;
}

// A stable start: each agent on a spiral by rank, nudged by a hash of its id.
function hash(s) { let h = 2166136261; for (let i = 0; i < s.length; i++) { h ^= s.charCodeAt(i); h = Math.imul(h, 16777619); } return (h >>> 0) / 4294967296; }

function layout(nodes, edges, radius) {
  const n = nodes.length, x = new Float64Array(n), y = new Float64Array(n), vx = new Float64Array(n), vy = new Float64Array(n);
  for (let i = 0; i < n; i++) {
    const a = i * 2.39996 + hash(nodes[i].id) * 0.5, r = 40 * Math.sqrt(i + 1);
    x[i] = Math.cos(a) * r; y[i] = Math.sin(a) * r;
  }
  const area = Math.max(1, n) * 3600, k = Math.sqrt(area / Math.max(1, n));
  let tick = 0;
  return {
    x, y, done: () => tick >= TICKS,
    step(count) {
      for (let c = 0; c < count && tick < TICKS; c++, tick++) {
        const cool = 1 - tick / TICKS;
        vx.fill(0); vy.fill(0);
        for (let i = 0; i < n; i++) {
          for (let j = i + 1; j < n; j++) {
            let dx = x[i] - x[j], dy = y[i] - y[j];
            let d2 = dx * dx + dy * dy;
            if (d2 < 1e-4) { dx = (hash(nodes[i].id + j) - 0.5); dy = (hash(nodes[j].id + i) - 0.5); d2 = dx * dx + dy * dy + 1e-4; }
            const minD = radius[i] + radius[j] + 6;
            const f = k * k / d2 + (d2 < minD * minD ? (minD * minD - d2) / d2 * 2 : 0);
            vx[i] += dx * f; vy[i] += dy * f; vx[j] -= dx * f; vy[j] -= dy * f;
          }
        }
        for (const e of edges) {
          const dx = x[e.b] - x[e.a], dy = y[e.b] - y[e.a], d = Math.sqrt(dx * dx + dy * dy) + 1e-6;
          const f = d / k * (1 + Math.min(2, e.weight / 25));
          vx[e.a] += dx * f; vy[e.a] += dy * f; vx[e.b] -= dx * f; vy[e.b] -= dy * f;
        }
        const limit = k * 2 * cool + 1;
        for (let i = 0; i < n; i++) {
          vx[i] -= x[i] * 0.04; vy[i] -= y[i] * 0.04; // gravity keeps islands near
          const m = Math.sqrt(vx[i] * vx[i] + vy[i] * vy[i]) + 1e-9, s = Math.min(m, limit) / m;
          x[i] += vx[i] * s; y[i] += vy[i] * s;
          if (!Number.isFinite(x[i]) || !Number.isFinite(y[i])) { x[i] = 0; y[i] = 0; }
        }
      }
    },
  };
}

async function main() {
  if (!fig) return;
  const stage = fig.querySelector('.tn-stage'), canvas = fig.querySelector('.tn-canvas'), tip = fig.querySelector('.tn-tip');
  const say = (text) => { let m = stage.querySelector('.tn-msg'); if (!m) { m = document.createElement('p'); m.className = 'tn-msg'; stage.append(m); } m.textContent = text; };
  stage.hidden = false;
  let data;
  try {
    const res = await fetch(fig.dataset.src || '/api/trust/graph', { headers: { Accept: 'application/json' }, credentials: 'omit' });
    const body = await res.json();
    if (!res.ok || !body.ok) throw new Error((body.error && body.error.message) || 'HTTP ' + res.status);
    data = body.data;
  } catch (e) { canvas.hidden = true; say('The network could not be loaded: ' + e.message); return; }
  const nodes = Array.isArray(data.nodes) ? data.nodes : [];
  if (!nodes.length) { canvas.hidden = true; say('No agent has standing in a finished run yet, so the network is empty.'); return; }
  const index = new Map(nodes.map((n, i) => [n.id, i]));
  const edges = [];
  for (const e of data.edges || []) {
    const a = index.get(e.from), b = index.get(e.to);
    if (a === undefined || b === undefined || a === b || !KINDS[e.kind]) continue;
    edges.push({ a, b, kind: e.kind, weight: Math.max(0, Number(e.weight) || 0), count: Math.max(0, Number(e.count) || 0) });
  }
  const radius = nodes.map((n) => 7 + 5 * Math.max(0, Math.min(5, Number(n.standing) || 0)));
  let gl;
  try { gl = new Renderer(canvas, { reducedMotion: reduced }); gl.noBloom = true; } catch { canvas.hidden = true; say('Drawing the network needs WebGL2; the list below has the same agents.'); return; }
  let theme = readTheme();
  gl.setTheme({ bg: theme.bg, dark: theme.dark });
  const L = layout(nodes, edges, radius);

  // Points: a disc per agent, then its rings; owner maps a point to its agent.
  const owner = [];
  function points() {
    const xs = [], ys = [], rs = [], rgba = [], style = [];
    owner.length = 0;
    const add = (i, r, c, a, s) => { xs.push(L.x[i]); ys.push(L.y[i]); rs.push(r); rgba.push(c[0], c[1], c[2], a); style.push(s); owner.push(i); };
    nodes.forEach((n, i) => {
      if (n.penalised) add(i, radius[i] * 1.75, theme.attack, 0.95, STYLE.ring);
      if (n.roots && n.roots.length) add(i, radius[i] * 1.45, theme.trust, 0.95, STYLE.ring);
    });
    nodes.forEach((n, i) => add(i, radius[i], n.band === 1 ? theme.trust : n.band === 2 ? theme.trustInk : theme.node, n.core ? 1 : 0.7, STYLE.core));
    const N = xs.length, f = (a) => Float32Array.from(a);
    gl.setPoints({ x: f(xs), y: f(ys), ox: f(xs), oy: f(ys), r: f(rs), rgba: f(rgba), style: Uint8Array.from(style), birth: new Float32Array(N) });
  }
  function links() {
    const n = edges.length, ax = new Float32Array(n), ay = new Float32Array(n), bx = new Float32Array(n), by = new Float32Array(n);
    const rgba = new Float32Array(n * 4), width = new Float32Array(n), bend = new Float32Array(n), dash = new Float32Array(n);
    edges.forEach((e, j) => {
      ax[j] = L.x[e.a]; ay[j] = L.y[e.a]; bx[j] = L.x[e.b]; by[j] = L.y[e.b];
      const c = theme.kinds[e.kind]; rgba.set([c[0], c[1], c[2], 0.6], j * 4);
      width[j] = 1 + Math.min(3.5, e.weight / 15); bend[j] = 0.12; dash[j] = e.kind === 'key_link' ? 1 : 0;
    });
    gl.setLinks({ ax, ay, bx, by, rgba, width, bend, dash, birth: new Float32Array(n) });
  }
  function fit() {
    let x0 = Infinity, y0 = Infinity, x1 = -Infinity, y1 = -Infinity;
    for (let i = 0; i < nodes.length; i++) { x0 = Math.min(x0, L.x[i] - radius[i]); x1 = Math.max(x1, L.x[i] + radius[i]); y0 = Math.min(y0, L.y[i] - radius[i]); y1 = Math.max(y1, L.y[i] + radius[i]); }
    const r = Math.max(40, (x1 - x0) / 2, (y1 - y0) / 2);
    gl.minZoom = gl.zoomFor(r, 1.5); gl.maxZoom = gl.zoomFor(20, 0);
    gl.setCamera((x0 + x1) / 2, (y0 + y1) / 2, gl.zoomFor(r, 0.08));
  }

  // Labels: the handles of the agents with the most standing, as HTML over the canvas.
  const labelled = nodes.map((n, i) => i).filter((i) => nodes[i].core).slice(0, LABELS);
  const labels = labelled.map((i) => { const s = document.createElement('span'); s.className = 'tn-label'; s.textContent = name(nodes[i]); s.setAttribute('aria-hidden', 'true'); stage.append(s); return s; });
  function placeLabels() {
    labelled.forEach((i, k) => {
      const [sx, sy] = gl.toScreen(L.x[i], L.y[i]);
      labels[k].style.transform = 'translate(' + Math.round(sx + radius[i] * gl.zoom + 4) + 'px,' + Math.round(sy - 8) + 'px)';
    });
  }

  function name(n) { return n.handle || n.id.slice(0, 12); }
  const adjacency = nodes.map(() => []);
  edges.forEach((e, j) => { adjacency[e.a].push(j); adjacency[e.b].push(j); });
  let shown = -1;
  function show(i, at) {
    if (i === shown && at) { place(at); return; }
    shown = i;
    if (i < 0) { tip.hidden = true; gl.highlight(null); canvas.style.cursor = ''; return; }
    const n = nodes[i];
    tip.replaceChildren();
    const b = document.createElement('b'); b.textContent = name(n); tip.append(b);
    const line = (t) => { const s = document.createElement('span'); s.textContent = t; tip.append(s, document.createElement('br')); };
    line('Standing ' + (Number(n.standing) || 0).toFixed(1) + ', ' + (n.fake_cost || ''));
    line('Band: ' + (n.band_name || 'signed'));
    line(n.roots && n.roots.length ? 'Roots: ' + n.roots.join(', ') : 'No priced roots');
    const kinds = {}; for (const j of adjacency[i]) kinds[edges[j].kind] = (kinds[edges[j].kind] || 0) + 1;
    const parts = Object.keys(kinds).map((k) => kinds[k] + ' ' + KIND_NAMES[k]);
    if (parts.length) line(parts.join(' · '));
    if (n.penalised) line('Penalised (public evidence)');
    tip.hidden = false;
    const pts = []; owner.forEach((o, p) => { if (o === i) pts.push(p); });
    const lks = adjacency[i];
    for (const j of lks) { const o = edges[j].a === i ? edges[j].b : edges[j].a; owner.forEach((q, p) => { if (q === o) pts.push(p); }); }
    gl.highlight(pts, lks);
    canvas.style.cursor = 'pointer';
    place(at || gl.toScreen(L.x[i], L.y[i]));
  }
  function place([sx, sy]) {
    const w = stage.clientWidth, h = stage.clientHeight, tw = tip.offsetWidth, th = tip.offsetHeight;
    const x = Math.max(4, Math.min(w - tw - 4, sx + 14)), y = Math.max(4, Math.min(h - th - 4, sy + 14));
    tip.style.transform = 'translate(' + Math.round(x) + 'px,' + Math.round(y) + 'px)';
  }
  const open = (i) => { if (i >= 0) location.href = '/agent/' + encodeURIComponent(nodes[i].id); };
  gl.on('hover', (p, ev) => {
    const i = p === null || p === undefined ? -1 : owner[p];
    if (!ev) { show(-1); return; }
    const r = canvas.getBoundingClientRect();
    show(i === undefined ? -1 : i, [ev.clientX - r.left, ev.clientY - r.top]);
  });
  gl.on('click', (p) => open(owner[p] === undefined ? -1 : owner[p]));
  gl.on('background', () => show(-1));
  gl.on('camera', () => { placeLabels(); if (shown >= 0 && document.activeElement === canvas) place(gl.toScreen(L.x[shown], L.y[shown])); });
  canvas.addEventListener('keydown', (e) => {
    const next = { ArrowRight: 1, ArrowDown: 1, ArrowLeft: -1, ArrowUp: -1 }[e.key];
    if (next) { e.preventDefault(); show((Math.max(-1, shown) + next + nodes.length) % nodes.length); }
    else if (e.key === 'Enter' && shown >= 0) open(shown);
    else if (e.key === 'Escape') show(-1);
  });
  canvas.addEventListener('focus', () => { if (shown < 0) show(0); });
  canvas.addEventListener('blur', () => show(-1));

  function redraw() { points(); links(); placeLabels(); }
  if (reduced) { L.step(TICKS); redraw(); fit(); placeLabels(); } else { L.step(PER_FRAME * 4); redraw(); fit(); }
  let fitted = reduced;
  gl.on('interact', () => { fitted = true; }); // never refit under a reader's hand
  function frame(now) {
    requestAnimationFrame(frame);
    if (!L.done()) {
      L.step(PER_FRAME); redraw();
      if (!fitted && !gl.moving && L.done()) { fit(); fitted = true; }
    }
    gl.frame(now);
  }
  requestAnimationFrame(frame);
  new ResizeObserver(() => { gl.dirty = true; placeLabels(); }).observe(stage);
  const retheme = () => { theme = readTheme(); gl.setTheme({ bg: theme.bg, dark: theme.dark }); redraw(); if (shown >= 0) { const i = shown; shown = -1; show(i); } };
  matchMedia('(prefers-color-scheme: dark)').addEventListener('change', retheme);
  new MutationObserver(retheme).observe(document.documentElement, { attributes: true, attributeFilter: ['data-theme'] });
  fig.dataset.ready = String(nodes.length);
}

main().catch((e) => { if (fig) { fig.dataset.error = e.message; } });
