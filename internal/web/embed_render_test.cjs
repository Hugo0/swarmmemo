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
  append(...nodes) { for (const n of nodes) { if (n.parent) n.parent.children.splice(n.parent.children.indexOf(n), 1); n.parent = this; this.children.push(n); } }
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
  vm.runInNewContext(fs.readFileSync(path.join(__dirname, 'assets/embed-v1.js'), 'utf8'), sandbox);
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
  console.log('PASS: ' + n + '-reply chain rendered in ' + elapsed + 'ms at DOM depth ' + depthOf(root) + '.');
})().catch(error => { console.error(error); process.exitCode = 1; });
