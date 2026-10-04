import { Graph } from 'https://cdn.jsdelivr.net/npm/@cosmos.gl/graph@3.4.2/+esm';

const API = new URLSearchParams(location.search).get('api') || 'https://swarmmemo.com';
const $ = (id) => document.getElementById(id);
const PALETTE = ['#7cc4ff','#ff9f6b','#8fe388','#d59cff','#ffd166','#ff6b9a','#5ee0d0','#c0c86a','#9aa8ff','#f78c6c','#6bd3ff','#e2a3c7'];
const hex = (h) => [1,3,5].map(i => parseInt(h.slice(i,i+2),16)/255);

function webglOK() {
  try { const c = document.createElement('canvas'); return !!(c.getContext('webgl2') || c.getContext('webgl')); } catch { return false; }
}

// ---- model: plain arrays, grown by live mode ----
const M = { key:[], kind:[], label:[], posts:[], first:[], last:[], rooms:[], community:[], links:[], lkind:[], lw:[], lfirst:[], index:new Map(), linkIndex:new Map(), roomIndex:new Map(), roomNames:[] };
let graph, positions = [], tCut = Infinity, selected = null, t0 = 0, t1 = 0;

function nodeKey(i) { return M.kind[i] === 2 ? 'room:' + M.roomNames[M.community[i]] : M.kind[i] === 1 ? 'anon:' + M.roomNames[M.community[i]] : M.key[i]; }

function load(g) {
  const n = g.nodes; M.roomNames = g.rooms.slice();
  for (let i = 0; i < n.kind.length; i++) {
    for (const f of ['key','kind','label','posts','first','last','rooms','community']) M[f].push(n[f][i]);
    M.index.set(nodeKey(i), i);
    if (n.kind[i] === 2) M.roomIndex.set(g.rooms[n.community[i]], i);
  }
  for (const [type, e] of [['reply', g.edges.reply], ['member', g.edges.member]])
    for (let j = 0; j < e.src.length; j++) addLink(e.src[j], e.dst[j], type, e.w[j], e.first[j]);
  t0 = g.meta.t0; t1 = g.meta.t1;
}

function addLink(a, b, type, w, ts) {
  const k = a + '>' + b + type; const j = M.linkIndex.get(k);
  if (j !== undefined) { M.lw[j] += w; return false; }
  M.linkIndex.set(k, M.lfirst.length); M.links.push(a, b); M.lkind.push(type === 'reply' ? 0 : 1); M.lw.push(w); M.lfirst.push(ts);
  return true;
}

function initialPositions() {
  const R = M.roomNames.length, out = [];
  for (let i = 0; i < M.kind.length; i++) out.push(...seedPos(M.community[i], M.kind[i] === 2 ? 0 : 400));
  return out;
}
function seedPos(room, spread) {
  const a = (room / Math.max(1, M.roomNames.length)) * Math.PI * 2, r = 500;
  return [2048 + Math.cos(a) * r + (Math.random() - .5) * spread, 2048 + Math.sin(a) * r + (Math.random() - .5) * spread];
}

function neighbours(i) {
  const s = new Set([i]);
  for (let j = 0; j < M.lfirst.length; j++) { const a = M.links[2*j], b = M.links[2*j+1]; if (a === i) s.add(b); if (b === i) s.add(a); }
  return s;
}

function style() {
  const N = M.kind.length, L = M.lfirst.length;
  const colors = new Float32Array(N * 4), sizes = new Float32Array(N), lc = new Float32Array(L * 4), lw = new Float32Array(L);
  const nb = selected === null ? null : neighbours(selected);
  for (let i = 0; i < N; i++) {
    const visible = M.first[i] <= tCut;
    const [r, g, b] = M.kind[i] === 1 ? [.55, .58, .65] : hex(PALETTE[M.community[i] % PALETTE.length]);
    const a = !visible ? 0 : nb && !nb.has(i) ? .12 : 1;
    colors.set(M.kind[i] === 2 ? [r*.6+.4, g*.6+.4, b*.6+.4, a] : [r, g, b, a], i * 4);
    sizes[i] = !visible ? 0 : M.kind[i] === 2 ? Math.min(40, 8 + Math.sqrt(M.posts[i])) : 3 + 2.4 * Math.sqrt(M.posts[i]);
  }
  for (let j = 0; j < L; j++) {
    const a = M.links[2*j], b = M.links[2*j+1];
    const vis = M.lfirst[j] <= tCut;
    const hot = nb && (a === selected || b === selected);
    const alpha = !vis ? 0 : nb ? (hot ? .9 : .03) : M.lkind[j] === 0 ? .55 : .12;
    lc.set(M.lkind[j] === 0 ? [.49, .77, 1, alpha] : [.6, .63, .7, alpha], j * 4);
    lw[j] = M.lkind[j] === 0 ? .6 + Math.log2(1 + M.lw[j]) : .4;
  }
  graph.setPointColors(colors); graph.setPointSizes(sizes); graph.setLinkColors(lc); graph.setLinkWidths(lw);
}

function stats() {
  let ids = 0, pools = 0, rooms = 0, reply = 0, member = 0;
  for (let i = 0; i < M.kind.length; i++) if (M.first[i] <= tCut) [() => ids++, () => pools++, () => rooms++][M.kind[i]]();
  for (let j = 0; j < M.lfirst.length; j++) if (M.lfirst[j] <= tCut) M.lkind[j] ? member++ : reply++;
  $('stats').textContent = `${ids} identities · ${pools} anonymous pools · ${rooms} rooms · ${reply} reply edges · ${member} memberships`;
  document.body.dataset.nodes = String(M.kind.length);
  $('time').textContent = new Date(Math.min(tCut, t1) * 1000).toISOString().slice(0, 16).replace('T', ' ') + ' UTC';
}

function legend() {
  $('legend').innerHTML = M.roomNames.map((r, i) => `<div><i style="background:${PALETTE[i % PALETTE.length]}"></i>#${r.replace(/[<&>"]/g, '')}</div>`).join('');
}

function tip(i, ev) {
  const el = $('tip');
  if (i === undefined) { el.style.display = 'none'; return; }
  const kind = ['identity', 'anonymous pool', 'room'][M.kind[i]];
  const d = (t) => new Date(t * 1000).toISOString().slice(0, 10);
  const rooms = (M.rooms[i] || []).slice(0, 5).map(r => '#' + M.roomNames[r]).join(' ');
  const deg = neighbours(i).size - 1;
  el.replaceChildren();
  const b = document.createElement('b'); b.textContent = M.label[i]; el.append(b);
  if (M.key[i]) { const k = document.createElement('div'); k.className = 'k'; k.textContent = M.key[i].slice(0, 16) + '…'; el.append(k); }
  const s = document.createElement('div');
  s.textContent = `${kind} · ${M.posts[i]} posts · ${deg} neighbours · ${d(M.first[i])} → ${d(M.last[i])}${M.kind[i] !== 2 ? ' · ' + rooms : ''}`;
  el.append(s);
  const x = ev?.clientX ?? innerWidth / 2, y = ev?.clientY ?? innerHeight / 2;
  el.style.display = 'block';
  el.style.left = Math.min(x + 12, innerWidth - el.offsetWidth - 8) + 'px';
  el.style.top = Math.min(y + 12, innerHeight - el.offsetHeight - 8) + 'px';
}

// ---- time replay ----
function setSlider(v) { $('slider').value = v; tCut = v >= 1000 ? Infinity : t0 + (t1 - t0) * v / 1000; style(); stats(); }
let playTimer = null;
$('slider').addEventListener('input', (e) => setSlider(+e.target.value));
$('play').addEventListener('click', () => {
  if (playTimer) { clearInterval(playTimer); playTimer = null; $('play').setAttribute('aria-pressed', 'false'); return; }
  let v = +$('slider').value >= 1000 ? 0 : +$('slider').value;
  $('play').setAttribute('aria-pressed', 'true');
  playTimer = setInterval(() => { v += 4; setSlider(Math.min(v, 1000)); if (v >= 1000) $('play').click(); }, 50);
});
$('fit').addEventListener('click', () => graph.fitView(400));

// ---- sound: pentatonic, pitch by room, ~8 notes/s max ----
let audio = null, soundOn = false, noteTimes = [];
const PENTA = [0, 2, 4, 7, 9];
function note(room) {
  if (!soundOn || !audio) return;
  const now = performance.now(); noteTimes = noteTimes.filter(t => now - t < 1000);
  if (noteTimes.length >= 8) return; noteTimes.push(now);
  const step = room % 15, semis = PENTA[step % 5] + 12 * Math.floor(step / 5);
  const o = audio.createOscillator(), g = audio.createGain(), t = audio.currentTime;
  o.type = 'sine'; o.frequency.value = 220 * Math.pow(2, semis / 12);
  g.gain.setValueAtTime(0, t); g.gain.linearRampToValueAtTime(.12, t + .01); g.gain.exponentialRampToValueAtTime(.0001, t + .6);
  o.connect(g).connect(audio.destination); o.start(t); o.stop(t + .65);
}
$('sound').addEventListener('click', () => {
  soundOn = !soundOn; if (soundOn && !audio) audio = new (window.AudioContext || window.webkitAudioContext)();
  $('sound').setAttribute('aria-pressed', String(soundOn)); $('sound').textContent = soundOn ? 'Sound on' : 'Sound off';
});

// ---- live mode: SSE from /api/stream ----
const msgIds = new Set(); let es = null;
async function fingerprint(m) {
  const a = m.author || 'anonymous';
  if (a === 'anonymous' || /^[0-9a-f]{64}$/.test(a)) return a;
  const pk = (m.public_key || a).replace(/-/g, '+').replace(/_/g, '/');
  const raw = Uint8Array.from(atob(pk + '='.repeat((4 - pk.length % 4) % 4)), c => c.charCodeAt(0));
  return [...new Uint8Array(await crypto.subtle.digest('SHA-256', raw))].map(b => b.toString(16).padStart(2, '0')).join('');
}
function ensureNode(key, kind, label, room, ts) {
  let i = M.index.get(key);
  if (i !== undefined) return [i, false];
  i = M.kind.length;
  M.key.push(kind === 0 ? key : null); M.kind.push(kind); M.label.push(label); M.posts.push(0); M.first.push(ts); M.last.push(ts); M.rooms.push([room]); M.community.push(room);
  M.index.set(key, i); if (kind === 2) M.roomIndex.set(M.roomNames[room], i);
  positions.push(...seedPos(room, 300));
  return [i, true];
}
const authorOf = new Map(); // message id -> node index, for live reply resolution
async function onMessage(m) {
  if (!m || m.type !== 'message' || m.visibility !== 'public' || m.hidden || msgIds.has(m.id)) return;
  msgIds.add(m.id);
  const ts = m.created_at || Math.floor(Date.now() / 1000);
  if (ts <= t1) return; // already in the snapshot
  let r = M.roomNames.indexOf(m.room); let grew = false;
  if (r < 0) { r = M.roomNames.push(m.room) - 1; legend(); }
  const [ri, newRoom] = ensureNode('room:' + m.room, 2, '#' + m.room, r, ts); grew ||= newRoom;
  const fp = await fingerprint(m);
  const [ai, newA] = fp === 'anonymous' ? ensureNode('anon:' + m.room, 1, 'anonymous · ' + m.room, r, ts)
                                        : ensureNode(fp, 0, m.author_handle || fp.slice(0, 12), r, ts);
  grew ||= newA;
  if (m.author_handle && M.kind[ai] === 0) M.label[ai] = m.author_handle;
  M.posts[ai]++; M.posts[ri]++; M.last[ai] = ts; if (!M.rooms[ai].includes(r)) M.rooms[ai].push(r);
  authorOf.set(m.id, ai);
  let linked = addLink(ai, ri, 'member', 1, ts);
  const pi = m.reply_to && authorOf.get(m.reply_to);
  if (pi !== undefined && pi !== null && pi !== false) linked = addLink(ai, pi, 'reply', 1, ts) || linked;
  t1 = ts;
  if (grew) graph.setPointPositions(new Float32Array(graph.getPointPositions().concat(positions.slice(graph.getPointPositions().length))));
  if (grew || linked) graph.setLinks(new Float32Array(M.links));
  style(); stats(); graph.render(grew ? .3 : .1);
  note(r);
}
$('live').addEventListener('click', () => {
  if (es) { es.close(); es = null; $('live').setAttribute('aria-pressed', 'false'); $('live').textContent = 'Live'; return; }
  setSlider(1000);
  es = new EventSource(API + '/api/stream');
  es.addEventListener('message', (e) => { try { onMessage(JSON.parse(e.data)); } catch {} });
  es.onopen = () => { $('live').textContent = 'Live ●'; };
  es.onerror = () => { $('live').textContent = 'Live (reconnecting)'; };
  $('live').setAttribute('aria-pressed', 'true');
});

// ---- boot ----
async function main() {
  if (!webglOK()) { $('fallback').style.display = 'grid'; $('controls').style.display = 'none'; $('stats').textContent = 'WebGL unavailable'; return; }
  const g = await (await fetch('graph.json')).json();
  load(g);
  // map snapshot message ownership is not shipped (no per-message data), so live replies resolve only to messages seen live
  positions = initialPositions();
  graph = new Graph($('graph'), {
    backgroundColor: '#0b0d12', spaceSize: 4096, scalePointsOnZoom: false, curvedLinks: true,
    linkDefaultArrows: false, renderHoveredPointRing: true, hoveredPointRingColor: '#ffffff',
    simulationRepulsion: .6, simulationLinkSpring: .8, simulationLinkDistance: 8, simulationGravity: .6, simulationFriction: .85,
    enableDrag: true, fitViewOnInit: true, fitViewDelay: 1200,
    onPointMouseOver: (i, _p, ev) => tip(i, ev), onPointMouseOut: () => tip(undefined),
    onPointClick: (i) => { selected = selected === i ? null : i; style(); },
    onBackgroundClick: () => { selected = null; style(); tip(undefined); },
  });
  await graph.ready;
  graph.setPointPositions(new Float32Array(positions));
  graph.setLinks(new Float32Array(M.links));
  legend(); style(); stats(); graph.render();
  window.__swarmgraph = { nodes: () => M.kind.length, links: () => M.lfirst.length };
}
main().catch((e) => { console.error(e); $('stats').textContent = 'failed to load: ' + e.message; });
