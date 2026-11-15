'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const vm = require('node:vm');
const fs = require('node:fs');
const path = require('node:path');
const app = fs.readFileSync(path.join(__dirname, 'assets/app.js'), 'utf8');
const start = app.indexOf('// BEGIN modlog font previews');
const end = app.indexOf('// END modlog font previews');
assert.ok(start >= 0 && end > start, 'font preview section exists in the published app asset');
const code = app.slice(start, end);

test('font samples load only on intersection, deduplicate and reject unsafe URLs', async () => {
  const samples = ['/a/abc123', '/a/abc123', '/a/evil")'].map(url => ({dataset: {fontUrl: url}, style: {}}));
  const loaded = [], added = [], observed = [], unobserved = [];
  let callback;
  class IntersectionObserver {
    constructor(cb) { callback = cb; }
    observe(el) { observed.push(el); }
    unobserve(el) { unobserved.push(el); }
  }
  class FontFace {
    constructor(name, source) { this.name = name; this.source = source; }
    load() { loaded.push(this.source); return Promise.resolve(this); }
  }
  const document = {fonts: {add(face) { added.push(face); }}, querySelectorAll: () => samples};
  vm.runInNewContext(code, {window: {IntersectionObserver, FontFace}, IntersectionObserver, FontFace, document});
  assert.equal(observed.length, 3);
  assert.equal(loaded.length, 0);
  callback(samples.map(target => ({target, isIntersecting: false})));
  assert.equal(loaded.length, 0);
  callback(samples.map(target => ({target, isIntersecting: true})));
  await new Promise(resolve => setImmediate(resolve));
  assert.deepEqual(loaded, ['url("/a/abc123")']);
  assert.equal(added.length, 1);
  assert.equal(unobserved.length, 3);
  assert.equal(samples[0].style.fontFamily, '"modlog-font-0"');
  assert.equal(samples[1].style.fontFamily, '"modlog-font-0"');
  assert.equal(samples[2].style.fontFamily, undefined);
});

test('missing browser support leaves ordinary text', () => {
  vm.runInNewContext(code, {window: {}, document: {}});
});

test('font fetches are limited to three concurrent loads and failures release a slot', async () => {
  const samples = Array.from({length: 5}, (_, i) => ({dataset: {fontUrl: `/a/f${i}`}, style: {}}));
  let callback, inFlight = 0, peak = 0;
  const finish = [];
  class IntersectionObserver {
    constructor(cb) { callback = cb; }
    observe() {}
    unobserve() {}
  }
  class FontFace {
    constructor(name) { this.name = name; }
    load() {
      inFlight++; peak = Math.max(peak, inFlight);
      return new Promise((resolve, reject) => finish.push(ok => {
        inFlight--; ok ? resolve(this) : reject(new Error('temporary failure'));
      }));
    }
  }
  const document = {fonts: {add() {}}, querySelectorAll: () => samples};
  vm.runInNewContext(code, {window: {IntersectionObserver, FontFace}, IntersectionObserver, FontFace, document});
  callback(samples.map(target => ({target, isIntersecting: true})));
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(inFlight, 3); assert.equal(finish.length, 3);
  finish[0](false);
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(inFlight, 3); assert.equal(finish.length, 4);
  finish[1](true);
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(inFlight, 3); assert.equal(finish.length, 5);
  finish.slice(2).forEach(done => done(true));
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(inFlight, 0); assert.equal(peak, 3);
  assert.equal(samples[0].style.fontFamily, undefined);
});
