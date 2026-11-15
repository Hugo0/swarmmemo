// assets/modlog-font.js in a stub DOM (no server, no browser): a font sample
// loads only once it is in view, each file once, at most three at a time; a
// path other than /a/<id> never reaches FontFace; no FontFace, no work.
'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const code = fs.readFileSync(path.join(__dirname, 'assets/modlog-font.js'), 'utf8');
const tick = () => new Promise(resolve => setImmediate(resolve));

function page(urls, load) {
  const samples = urls.map(fontUrl => ({dataset: {fontUrl}, style: {}}));
  const env = {sources: [], added: [], observed: [], unobserved: [], samples};
  class IntersectionObserver {
    constructor(cb) { env.see = targets => cb(targets.map(target => ({target, isIntersecting: true}))); env.miss = targets => cb(targets.map(target => ({target, isIntersecting: false}))); }
    observe(el) { env.observed.push(el); }
    unobserve(el) { env.unobserved.push(el); }
  }
  class FontFace {
    constructor(family, source) { this.family = family; env.sources.push(source); }
    load() { return load ? load(this) : Promise.resolve(this); }
  }
  const document = {fonts: {add(face) { env.added.push(face); }}, querySelectorAll: () => samples};
  vm.runInNewContext(code, {window: {IntersectionObserver, FontFace}, IntersectionObserver, FontFace, document});
  return env;
}

test('loads on view, once per file, and refuses other paths', async () => {
  const env = page(['/a/abc123', '/a/abc123', '/a/evil")', 'https://elsewhere.example/a/x', '/a/../x']);
  assert.equal(env.observed.length, 5);
  env.miss(env.samples);
  assert.equal(env.sources.length, 0);
  env.see(env.samples);
  await tick();
  assert.deepEqual(env.sources, ['url("/a/abc123")']);
  assert.equal(env.added.length, 1);
  assert.equal(env.unobserved.length, 5);
  assert.equal(env.samples[0].style.fontFamily, '"modlog-font-0"');
  assert.equal(env.samples[1].style.fontFamily, '"modlog-font-0"');
  for (const s of env.samples.slice(2)) assert.equal(s.style.fontFamily, undefined);
});

test('three loads at a time; a failure frees its slot and keeps the page font', async () => {
  let inFlight = 0, peak = 0;
  const finish = [];
  const env = page(Array.from({length: 5}, (_, i) => `/a/f${i}`), face => {
    inFlight++; peak = Math.max(peak, inFlight);
    return new Promise((resolve, reject) => finish.push(ok => { inFlight--; ok ? resolve(face) : reject(new Error('bad font')); }));
  });
  env.see(env.samples);
  await tick();
  assert.equal(finish.length, 3);
  finish[0](false);
  await tick();
  assert.equal(finish.length, 4);
  finish.slice(1).forEach(done => done(true));
  await tick();
  finish[4](true);
  await tick();
  assert.equal(inFlight, 0);
  assert.equal(peak, 3);
  assert.equal(env.samples[0].style.fontFamily, undefined);
  assert.equal(env.samples[4].style.fontFamily, '"modlog-font-4"');
});

test('without FontFace the script does nothing', () => {
  vm.runInNewContext(code, {window: {}, document: {}});
});
