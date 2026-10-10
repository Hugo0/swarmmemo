// Node only, no browser or server: runs embed-v1.js against a minimal DOM and
// a stubbed /api/messages. Any commenter can build a reply chain, so a long
// chain must stay a shallow DOM and render in linear time on the host page.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

class Node {
  constructor(tag) { this.tagName = tag; this.children = []; this.parent = null; this.attrs = {}; this.dataset = {}; this.style = {setProperty() {}}; this.hidden = false; this._text = ''; this.className = ''; }
  set textContent(v) { this._text = String(v); this.children = []; }
  get textContent() { return this._text + this.children.map(c => c.textContent).join(''); }
  get classList() { const n = this; return {add: c => { n.className += ' ' + c; }, remove: () => {}}; }
  setAttribute(k, v) { this.attrs[k] = String(v); }
  getAttribute(k) { return this.attrs[k]; }
  append(...nodes) { for (let n of nodes) { if (typeof n === 'string') { const t = new Node('#text'); t._text = n; n = t; } if (n.parent) n.parent.children.splice(n.parent.children.indexOf(n), 1); n.parent = this; this.children.push(n); } }
  after(...nodes) { const p = this.parent; let i = p.children.indexOf(this) + 1; for (const n of nodes) { if (n.parent) n.parent.children.splice(n.parent.children.indexOf(n), 1); if (n.parent === p && p.children.indexOf(this) + 1 < i) i--; n.parent = p; p.children.splice(i++, 0, n); } }
  replaceChildren(...nodes) { this.children = []; this.append(...nodes); }
  attachShadow() { this.shadowRoot = new Node('#shadow'); return this.shadowRoot; }
  *walk() { for (const c of this.children) { yield c; yield* c.walk(); } }
  querySelectorAll(tag) { return [...this.walk()].filter(n => n.tagName === tag); }
  getElementById(id) { return [...this.walk()].find(n => n.id === id) || null; }
  focus() {}
  scrollIntoView() {}
}

function mount(messages) {
  const body = new Node('body');
  const script = new Node('script');
  script.src = 'http://127.0.0.1:9/embed/v1.js';
  Object.assign(script.dataset, {room: 'blog', page: 'post'});
  body.append(script);
  let requests = 0;
  const sandbox = {
    document: {currentScript: script, createElement: t => new Node(t), createElementNS: (_, t) => new Node(t), querySelector: () => null},
    CSS: {supports: () => true},
    localStorage: {getItem: () => null, setItem() {}},
    fetch: async () => { requests++; return {ok: true, status: 200, headers: {get: () => null}, json: async () => ({ok: true, messages, data: {has_more: false}})}; },
    crypto: globalThis.crypto, TextEncoder, btoa, atob, URL, URLSearchParams, setTimeout, Date, Math, Error, Map, Set, JSON, Promise, Array, Object, String, Uint8Array,
    navigator: {},
  };
  sandbox.globalThis = sandbox;
  // The same bundle embed.go serves: memo-core.js and embed-v1.js in one closure.
  const read = name => fs.readFileSync(path.join(__dirname, 'assets', name), 'utf8');
  vm.runInNewContext('(() => {\n' + read('memo-core.js') + '\n' + read('embed-v1.js') + '})();\n', sandbox);
  return {body, done: async () => { for (let i = 0; i < 2000 && !(requests && sandbox.document.currentScript.parent.children[1]?.shadowRoot?.querySelectorAll('article').length); i++) await new Promise(r => setTimeout(r, 5)); }};
}

function depthOf(node) { let max = 0; for (const c of node.children) max = Math.max(max, depthOf(c)); return max + 1; }

(async () => {
  const n = 1500;
  const messages = Array.from({length: n}, (_, i) => ({id: 'm' + i, sequence: i + 1, created_at: 1e9 + i, text: 'reply ' + i, handle: 'h' + i, reply_to: i ? 'm' + (i - 1) : '', votes: {up: 0}}));
  const started = Date.now();
  const {body, done} = mount(messages);
  await done();
  const elapsed = Date.now() - started;
  const root = body.children[1].shadowRoot;
  const articles = root.querySelectorAll('article');
  assert.equal(articles.length, n, 'every comment renders');
  assert.ok(depthOf(root) < 40, 'a reply chain stays a shallow DOM: depth ' + depthOf(root));
  assert.ok(elapsed < 5000, 'a ' + n + '-reply chain renders in linear time: ' + elapsed + 'ms');
  // Order is chronological and deep replies say whom they answer.
  assert.deepEqual(articles.slice(0, 7).map(a => a.dataset.id), ['m0', 'm1', 'm2', 'm3', 'm4', 'm5', 'm6']);
  const deep = articles.find(a => a.dataset.id === 'm900');
  assert.equal(deep.querySelectorAll('a').find(a => a.className === 'to').textContent, '↳ replying to h899');
  const flat = articles.find(a => a.dataset.id === 'm4').children.find(c => c.className === 'flat');
  assert.equal(flat.children.length, n - 5, 'replies past depth four share one flat list');
  assert.equal(articles.find(a => a.dataset.id === 'm3').querySelectorAll('button').find(b => b.className === 'fold').textContent, 'Hide replies');
  // Header count, heart state and sorting of top-level comments (replies stay in order).
  const small = mount([
    {id: 'a', sequence: 1, created_at: 1e9, text: 'first', handle: 'ha', votes: {up: 40, weight: 0}},
    {id: 'b', sequence: 2, created_at: 1e9 + 1, text: 'second', handle: 'hb', votes: {up: 5, weight: 2}},
    {id: 'c', sequence: 3, created_at: 1e9 + 2, text: 'reply', handle: 'hc', reply_to: 'a', votes: {up: 9, weight: 9}},
    {id: 'd', sequence: 4, created_at: 1e9 + 3, text: 'gone', hidden: true},
  ]);
  await small.done();
  const sroot = small.body.children[1].shadowRoot;
  const tops = () => sroot.querySelectorAll('article').filter(a => a.parent.parent?.tagName === 'section').map(a => a.dataset.id);
  assert.equal(sroot.querySelectorAll('h2')[0].textContent, '3 comments', 'removed comments are not counted');
  const heart = sroot.querySelectorAll('article').find(a => a.dataset.id === 'b').querySelectorAll('button').find(b => b.className === 'heart');
  assert.equal(heart.getAttribute('aria-label'), 'Like (5)'); assert.equal(heart.getAttribute('aria-pressed'), 'false');
  assert.match(sroot.querySelectorAll('time')[0].title, /UTC$/, 'exact time on hover');
  assert.deepEqual(tops(), ['a', 'b']);
  const sortButton = name => sroot.querySelectorAll('button').find(b => b.textContent === name);
  sortButton('Newest').onclick(); assert.deepEqual(tops(), ['b', 'a']);
  sortButton('Top').onclick(); assert.deepEqual(tops(), ['b', 'a'], 'top-level by ranking weight: forty new-key likes (weight 0) do not lift a; a reply\'s weight does not lift its parent');
  assert.equal(sortButton('Top').getAttribute('aria-pressed'), 'true');
  sortButton('Oldest').onclick(); assert.deepEqual(tops(), ['a', 'b']);
  // A work request and its accepted result carry the shared work line (memo-core.js
  // workLine), linking to the board in a new tab; a removed result shows none.
  const task = 'f'.repeat(32);
  const worked = mount([
    {id: task, sequence: 1, created_at: 1e9, text: 'task', handle: 'hr', kind: 'request', work: {id: task, title: 'Fix it', state: 'open', deadline: 1791979200, eligibility: 'linked', claimable: true, url: '/work/' + task, reward: {amount: 2500, unit: 'credit'}}},
    {id: 'r', sequence: 2, created_at: 1e9 + 1, text: 'done', handle: 'hw', reply_to: task, work: {result_of: task, title: 'Fix it', state: 'accepted', url: '/work/' + task}},
    {id: 'x', sequence: 3, created_at: 1e9 + 2, text: '', reply_to: task, hidden: true, work: {result_of: task, title: 'Fix it', state: 'rejected', url: '/work/' + task}},
  ]);
  await worked.done();
  const lines = worked.body.children[1].shadowRoot.querySelectorAll('p').filter(p => p.className.startsWith('work-line'));
  assert.deepEqual(lines.map(p => p.textContent), ['Paid task · 2,500 credits · open · due Oct 14 · eligible: linked agentsHow to claim →', 'Accepted ✓ for Fix it']);
  assert.deepEqual(lines.map(p => p.className), ['work-line work-state-open', 'work-line work-result work-state-accepted']);
  const links = lines.flatMap(p => p.querySelectorAll('a'));
  assert.deepEqual(links.map(a => a.href), ['http://127.0.0.1:9/work/' + task, 'http://127.0.0.1:9/tools/work', 'http://127.0.0.1:9/work/' + task]);
  assert.ok(links.every(a => a.target === '_blank'));
  // Who wrote it (memo-core.js authorNodes, C67/C68): a claimed handle is a name, a
  // generated name says so beside its key, and an unsigned post shows its daily tag.
  const fp = '9eb0e947' + 'z'.repeat(56); // not hex, so no avatar fetch
  const named = mount([
    {id: 'h', sequence: 1, created_at: 1e9, text: 'a', author: 'z'.repeat(64), public_key: 'k1', author_handle: 'atlas', display_name_source: 'handle'},
    {id: 'g', sequence: 2, created_at: 1e9 + 1, text: 'b', author: fp, public_key: 'k2', nickname: 'sable-bellows', display_name_source: 'generated'},
    {id: 'n', sequence: 3, created_at: 1e9 + 2, text: 'c', author: 'anonymous', anon_tag: 'd092'},
  ]);
  await named.done();
  const byline = id => named.body.children[1].shadowRoot.querySelectorAll('article').find(a => a.dataset.id === id).querySelectorAll('strong')[0];
  assert.equal(byline('h').textContent, 'atlas');
  const generated = byline('g');
  assert.equal(generated.textContent, 'sable-bellows key 9eb0e947 (generated name; no handle claimed)');
  assert.equal(generated.children[0].className, 'generated-name');
  assert.match(generated.children[0].title, /^Name generated from this key; no handle claimed\./);
  assert.equal(byline('n').textContent, 'Anonymous · net d092');
  assert.match(byline('n').children.find(c => c.className === 'name-tag').title, /resets daily/);
  console.log('PASS: ' + n + '-reply chain rendered in ' + elapsed + 'ms at DOM depth ' + depthOf(root) + '; count, heart state, sort, work lines and bylines.');
})().catch(error => { console.error(error); process.exitCode = 1; });
