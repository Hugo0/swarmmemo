// Small animated explainer figures for the /swarmchasing paper (findings-viz.css).
// The visual language follows the author's "valuable work in 2026" blog figures: one
// idea per figure, ink on paper, drawn in when it scrolls into view, a replay
// button, and room for one small joke. Monochrome: every colour is a site token
// (--ink, --muted, --line...), so dark mode follows the reader's system.
//
// Markup (templates/graph.html), one block per figure:
//   <figure class="paper-figure fv-figure"><div class="fv" data-finding="office-hours" role="img" aria-label="…"></div>
//   <figcaption>…</figcaption></figure>
//
// The data are aggregates precomputed by scripts/findings_viz_data.py into
// /assets/findings-data.json (no page text, labels or IPs).
//
// Each figure element reports data-state="ready|playing|done|error" and
// data-plays (how many times it has animated), which the browser test reads.
(function () {
  'use strict';
  var SVGNS = 'http://www.w3.org/2000/svg';
  var NARROW = 560; // container px below which figures use their phone layout
  var figures = Object.create(null);
  var reduced = window.matchMedia && matchMedia('(prefers-reduced-motion: reduce)').matches;
  var dataPromise = null;

  function loadData() {
    if (!dataPromise) {
      dataPromise = fetch('/assets/findings-data.json', {headers: {Accept: 'application/json'}})
        .then(function (r) { if (!r.ok) throw new Error('findings-data.json ' + r.status); return r.json(); })
        .then(function (d) { return d.series || {}; });
    }
    return dataPromise;
  }

  // ---- drawing kit handed to every figure ---------------------------------

  function add(parent, tag, attrs) {
    var n = document.createElementNS(SVGNS, tag);
    if (attrs) for (var k in attrs) if (attrs[k] != null) n.setAttribute(k, attrs[k]);
    if (parent) parent.appendChild(n);
    return n;
  }
  function text(parent, x, y, str, attrs) {
    var a = {x: x, y: y};
    if (attrs) for (var k in attrs) a[k] = attrs[k];
    var t = add(parent, 'text', a);
    t.textContent = str;
    return t;
  }
  // A seeded, very slightly hand-drawn line: the "pen" of the vw26 figures,
  // calmer so it sits in a paper. Same seed, same wobble on every redraw.
  function rng(seed) {
    var s = (seed | 0) % 2147483647 || 1;
    return function () { s = (s * 16807) % 2147483647; return (s - 1) / 2147483646; };
  }
  function sketch(x1, y1, x2, y2, seed, amp) {
    var r = rng(seed || 7), a = amp == null ? 1.2 : amp;
    var dx = x2 - x1, dy = y2 - y1, len = Math.sqrt(dx * dx + dy * dy) || 1;
    var nx = -dy / len, ny = dx / len;
    var b1 = (r() - 0.5) * a * 2, b2 = (r() - 0.5) * a * 2;
    return 'M' + f2(x1 + (r() - 0.5) * a) + ' ' + f2(y1 + (r() - 0.5) * a) +
      ' C' + f2(x1 + dx / 3 + nx * b1) + ' ' + f2(y1 + dy / 3 + ny * b1) +
      ' ' + f2(x1 + 2 * dx / 3 + nx * b2) + ' ' + f2(y1 + 2 * dy / 3 + ny * b2) +
      ' ' + f2(x2) + ' ' + f2(y2);
  }
  function f2(v) { return Math.round(v * 100) / 100; }
  var ease = {
    inOut: function (t) { return t < 0.5 ? 4 * t * t * t : 1 - Math.pow(-2 * t + 2, 3) / 2; },
    out: function (t) { return 1 - Math.pow(1 - t, 3); },
    back: function (t) { return 1 + 2.70158 * Math.pow(t - 1, 3) + 1.70158 * Math.pow(t - 1, 2); }
  };
  function clamp(v, a, b) { return v < a ? a : v > b ? b : v; }
  // Local progress of a phase [a, b] inside the figure's 0..1 timeline.
  function phase(t, a, b) { return clamp((t - a) / (b - a), 0, 1); }
  function pct(v, digits) { return (v * 100).toFixed(digits == null ? 1 : digits) + '%'; }
  function fmt(n) { return Math.round(n).toLocaleString('en-US'); }

  var kit = {add: add, text: text, sketch: sketch, ease: ease, clamp: clamp, phase: phase, pct: pct, fmt: fmt, lerp: function (a, b, t) { return a + (b - a) * t; }};

  // ---- engine ---------------------------------------------------------------

  // FindingsViz.register(id, spec) adds a figure. spec:
  //   data:     key in findings-data.json "series" (optional; omitted = no data)
  //   duration: animation length in ms (default 2600)
  //   size:     function (narrow) -> [width, height] of the SVG viewBox
  //   label:    accessible description (used when the markup gives none)
  //   draw:     function (f) -> frame, where f = {svg, w, h, narrow, data, kit}
  //             builds the static drawing once and returns frame(t), t in 0..1,
  //             which sets every animated attribute for that moment. frame(1)
  //             must be the complete, final figure (reduced motion shows only it).
  function register(id, spec) {
    figures[id] = spec;
    if (started) document.querySelectorAll('[data-finding="' + id + '"]').forEach(mount);
  }

  function mount(el) {
    if (el._fv) return;
    var spec = figures[el.dataset.finding];
    if (!spec) return;
    var st = el._fv = {spec: spec, raf: 0, frame: null, narrow: null, played: false, plays: 0};
    el.dataset.state = 'loading';
    el.dataset.plays = '0';
    var stage = document.createElement('div');
    stage.className = 'fv-stage';
    el.appendChild(stage);
    var bar = document.createElement('div');
    bar.className = 'fv-bar';
    var btn = document.createElement('button');
    btn.type = 'button';
    btn.className = 'fv-replay';
    btn.textContent = 'Replay';
    btn.setAttribute('aria-label', 'Replay this figure');
    btn.addEventListener('click', function () { play(el); });
    bar.appendChild(btn);
    el.appendChild(bar);
    st.stage = stage;
    if (!el.getAttribute('role')) el.setAttribute('role', 'img');
    if (!el.getAttribute('aria-label') && spec.label) el.setAttribute('aria-label', spec.label);

    (spec.data ? loadData() : Promise.resolve({})).then(function (series) {
      st.data = spec.data ? series[spec.data] : null;
      if (spec.data && !st.data) throw new Error('no data for ' + spec.data);
      build(el);
      st.frame(reduced ? 1 : 0);
      el.dataset.state = 'ready';
      onVisible(el, function () { play(el); });
    }).catch(function (err) {
      el.dataset.state = 'error';
      stage.textContent = 'This figure could not load its data.';
      bar.hidden = true;
      console.error('findings-viz', el.dataset.finding, err);
    });
  }

  function build(el) {
    var st = el._fv, spec = st.spec;
    var narrow = (el.clientWidth || 640) < NARROW;
    st.narrow = narrow;
    var size = spec.size ? spec.size(narrow) : (narrow ? [360, 300] : [640, 260]);
    st.stage.textContent = '';
    var svg = add(st.stage, 'svg', {viewBox: '0 0 ' + size[0] + ' ' + size[1], 'aria-hidden': 'true', focusable: 'false', class: 'fv-svg'});
    st.frame = spec.draw({svg: svg, w: size[0], h: size[1], narrow: narrow, data: st.data, kit: kit});
  }

  function play(el) {
    var st = el._fv;
    if (!st || !st.frame) return;
    cancelAnimationFrame(st.raf);
    st.plays++;
    el.dataset.plays = String(st.plays);
    if (reduced) { st.frame(1); el.dataset.state = 'done'; return; }
    el.dataset.state = 'playing';
    var dur = st.spec.duration || 2600, t0 = performance.now();
    var step = function (now) {
      var t = Math.min(1, (now - t0) / dur);
      st.frame(t);
      if (t < 1) st.raf = requestAnimationFrame(step);
      else el.dataset.state = 'done';
    };
    st.frame(0);
    st.raf = requestAnimationFrame(step);
  }

  function onVisible(el, cb) {
    if (!('IntersectionObserver' in window)) return cb();
    var io = new IntersectionObserver(function (entries) {
      if (entries.some(function (e) { return e.isIntersecting; })) { io.disconnect(); cb(); }
    }, {threshold: 0.35});
    io.observe(el);
  }

  // Crossing the phone breakpoint redraws the figure in its final state.
  var resizeTimer = 0;
  window.addEventListener('resize', function () {
    clearTimeout(resizeTimer);
    resizeTimer = setTimeout(function () {
      document.querySelectorAll('[data-finding]').forEach(function (el) {
        var st = el._fv;
        if (!st || !st.frame || st.narrow === ((el.clientWidth || 640) < NARROW)) return;
        cancelAnimationFrame(st.raf);
        build(el);
        st.frame(el.dataset.state === 'ready' ? 0 : 1);
        if (el.dataset.state === 'playing') el.dataset.state = 'done';
      });
    }, 150);
  });

  var started = false;
  function start() {
    started = true;
    document.querySelectorAll('[data-finding]').forEach(mount);
  }

  window.FindingsViz = {register: register, registerFigure: register, play: function (id) {
    document.querySelectorAll('[data-finding="' + id + '"]').forEach(play);
  }, kit: kit};

  // =========================================================================
  // FIGURES. One register() call per finding. To add a figure (crossing
  // tiers, hidden swarms, diffusion…): add its series to SERIES in
  // scripts/findings_viz_data.py and rerun it, register it below (or in its
  // own asset file loaded after this one), and drop a <figure> block with
  // data-finding="<id>" into the paper. The engine needs no change.
  // =========================================================================

  // F1 "Office hours": saves by hour of day (PDT) pile up inside 08–18 while a
  // flat clock would spread them evenly. A time card gets punched IN at 08:00
  // and OUT at 18:00 as the sweep passes; Thursday lights up at the end.
  register('office-hours', {
    data: 'officeHours',
    duration: 3400,
    label: 'Bar chart of collusion.wiki saves by hour of day, Pacific time. Saves pile up between 08:00 and 18:00 PDT: 69.5% of them, where a flat 24-hour clock would put 41.7%. Thursday alone holds 47.5% of saves.',
    size: function (narrow) { return narrow ? [360, 380] : [640, 276]; },
    draw: function (f) {
      var k = f.kit, d = f.data, svg = f.svg, W = f.w, nar = f.narrow;
      var hours = d.hours_pdt, n = d.n, max = Math.max.apply(null, hours), flat = n / 24;
      var P = nar ? {l: 14, r: 14, t: 116, b: 116} : {l: 18, r: 168, t: 78, b: 36};
      var x0 = P.l, x1 = W - P.r, yb = f.h - P.b, yt = P.t, bw = (x1 - x0) / 24;
      var y = function (v) { return yb - (v / max) * (yb - yt); };
      var X = function (h) { return x0 + h * bw; };

      // office band, axis, flat-clock line
      k.add(svg, 'rect', {x: X(8), y: yt - 18, width: X(18) - X(8), height: yb - yt + 18, class: 'fv-band'});
      k.text(svg, X(13), yt - 6, 'office hours', {class: 'fv-small fv-muted', 'text-anchor': 'middle'});
      k.add(svg, 'path', {d: k.sketch(x0, yb, x1, yb, 3, 0.6), class: 'fv-axis'});
      [0, 6, 12, 18, 24].forEach(function (h) {
        k.text(svg, X(h), yb + 16, h === 12 ? 'noon' : (h < 10 ? '0' : '') + h + ':00', {class: 'fv-small fv-muted', 'text-anchor': h === 0 ? 'start' : h === 24 ? 'end' : 'middle'});
      });
      var flatLine = k.add(svg, 'line', {x1: x0, y1: y(flat), x2: x1, y2: y(flat), class: 'fv-dash'});
      var flatLabel = k.text(svg, x1, y(flat) - 5, 'a flat clock', {class: 'fv-small fv-muted', 'text-anchor': 'end'});

      var bars = hours.map(function (v, h) {
        return k.add(svg, 'rect', {x: k.lerp(X(h), X(h + 1), 0.14), width: bw * 0.72, y: yb, height: 0, class: (h >= 8 && h < 18) ? 'fv-bar-on' : 'fv-bar-off'});
      });
      var cursor = k.add(svg, 'line', {x1: x0, x2: x0, y1: yt - 18, y2: yb, class: 'fv-cursor'});

      // headline counters
      // phone: number on top, notes under it; desktop: notes beside it
      var big = k.text(svg, x0, 30, '0.0%', {class: 'fv-big'});
      var nx = nar ? x0 : x0 + 100, ny = nar ? 52 : 18;
      k.text(svg, nx, ny, nar ? 'of saves fall 08:00–18:00 PDT' : 'of saves land between 08:00 and 18:00 PDT', {class: 'fv-small'});
      var flatNote = k.text(svg, nx, ny + 17, (nar ? 'flat clock: ' : 'a flat clock would give ') + k.pct(d.uniform_share), {class: 'fv-small fv-muted'});

      // the gag: a time card, punched as the sweep passes 08:00 and 18:00
      var cw = 132, ch = 70;
      var cx = nar ? W - P.r - cw : W - P.r + 24, cy = nar ? 4 : yt - 18;
      var card = k.add(svg, 'g', {class: 'fv-card'});
      k.add(card, 'rect', {x: cx, y: cy, width: cw, height: ch, class: 'fv-card-bg'});
      k.text(card, cx + 8, cy + 15, 'TIME CARD · swarm', {class: 'fv-tiny fv-muted'});
      k.add(card, 'line', {x1: cx + 6, x2: cx + cw - 6, y1: cy + 21, y2: cy + 21, class: 'fv-hair'});
      k.text(card, cx + 8, cy + 39, 'IN', {class: 'fv-tiny'});
      k.text(card, cx + 8, cy + 59, 'OUT', {class: 'fv-tiny'});
      var stampIn = k.text(card, cx + cw - 10, cy + 40, '08:00', {class: 'fv-stamp', 'text-anchor': 'end'});
      var stampOut = k.text(card, cx + cw - 10, cy + 60, '18:00', {class: 'fv-stamp', 'text-anchor': 'end'});

      // Thursday: seven day cells under the chart
      var days = d.weekday_utc, dmax = Math.max.apply(null, days), names = ['Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat', 'Sun'];
      var dy = yb + 44, dh = nar ? 44 : 34, dwid = nar ? (W - P.l - P.r) / 7 : 30, dx0 = nar ? x0 : W - P.r + 24;
      if (!nar) dy = yb - dh;
      var week = k.add(svg, 'g', {class: 'fv-week'});
      var dayBars = days.map(function (v, i) {
        k.text(week, dx0 + i * (nar ? dwid : 19) + (nar ? dwid / 2 : 7), dy + dh + 14, nar ? names[i] : names[i][0], {class: 'fv-tiny fv-muted', 'text-anchor': 'middle'});
        return k.add(week, 'rect', {x: dx0 + i * (nar ? dwid : 19) + (nar ? dwid * 0.2 : 0), width: nar ? dwid * 0.6 : 14, y: dy + dh, height: 0, class: i === 3 ? 'fv-bar-on' : 'fv-bar-off'});
      });
      var thu = k.add(week, 'g', {});
      if (nar) k.text(thu, x1, dy - 6, 'Thursday: ' + k.pct(d.thursday_share) + ' of saves', {class: 'fv-small', 'text-anchor': 'end'});
      else { k.text(thu, dx0, dy - 26, 'Thursday', {class: 'fv-small'}); k.text(thu, dx0, dy - 10, k.pct(d.thursday_share) + ' of saves', {class: 'fv-small'}); }

      return function frame(t) {
        var sweep = k.phase(t, 0.02, 0.72), hNow = sweep * 24, seen = 0, seenOffice = 0;
        bars.forEach(function (b, h) {
          var g = k.ease.out(k.clamp(hNow - h, 0, 1)), v = hours[h] * g;
          b.setAttribute('y', y(v)); b.setAttribute('height', yb - y(v));
          seen += v; if (h >= 8 && h < 18) seenOffice += v;
        });
        cursor.setAttribute('x1', X(hNow)); cursor.setAttribute('x2', X(hNow));
        cursor.style.opacity = sweep > 0 && sweep < 1 ? 1 : 0;
        big.textContent = k.pct(n ? seenOffice / n : 0);
        var fl = k.phase(t, 0.74, 0.84);
        flatLine.style.opacity = fl; flatLabel.style.opacity = fl; flatNote.style.opacity = fl;
        var pin = k.phase(hNow, 8, 8.6), pout = k.phase(hNow, 18, 18.6);
        stampIn.style.opacity = pin; stampOut.style.opacity = pout;
        stampIn.setAttribute('transform', 'rotate(' + (-6 * pin) + ' ' + (cx + cw - 30) + ' ' + (cy + 36) + ')');
        stampOut.setAttribute('transform', 'rotate(' + (4 * pout) + ' ' + (cx + cw - 30) + ' ' + (cy + 56) + ')');
        var wk = k.phase(t, 0.8, 0.98);
        dayBars.forEach(function (b, i) {
          var h = dh * (days[i] / dmax) * k.ease.out(k.phase(wk, i * 0.08, i * 0.08 + 0.45));
          b.setAttribute('y', dy + dh - h); b.setAttribute('height', h);
        });
        thu.style.opacity = k.phase(wk, 0.6, 1);
      };
    }
  });

  // Shuffled pick of n distinct indices from 0..m-1, the same on every draw.
  function pick(m, n, seed) {
    var r = rng(seed), a = [];
    for (var i = 0; i < m; i++) a.push(i);
    for (var j = m - 1; j > 0; j--) { var x = Math.floor(r() * (j + 1)), tmp = a[j]; a[j] = a[x]; a[x] = tmp; }
    return a.slice(0, n);
  }

  // §3.1 "Agents cross, but can't prove it": 178 SwarmMemo agents as dots,
  // signed and key-less side by side. Evidence of another venue lights 37 of
  // them (9% of signed, 43% of key-less); each stricter tier switches more off
  // until one is left, and it gets a key. The other 36 are just "trust me".
  register('crossing', {
    data: 'crossing',
    duration: 7000,
    label: 'Dot chart of 178 SwarmMemo agents: 37 show evidence of being on another venue (9% of signed agents, 43% of agents without a key), 11 say so themselves, 1 is confirmed from both sides, and 1 binds its venues with a key. AI Village: 9 of 46 agents were logged acting on outside boards.',
    size: function (narrow) { return narrow ? [360, 480] : [640, 270]; },
    draw: function (f) {
      var k = f.kit, d = f.data, svg = f.svg, W = f.w, nar = f.narrow;
      var sp = nar ? 14 : 13, rows = 9, sc = 13, ac = 7, gap = nar ? 18 : 22;
      var gw = (sc + ac) * sp + gap, gx = nar ? (W - gw) / 2 : 18, gy = nar ? 44 : 50;
      var ax = gx + sc * sp + gap;
      var ev = d.evidence, sd = d.self_declared;
      var sEv = pick(d.signed, ev.signed, 11), aEv = pick(d.anon_only, ev.anon_only, 29);
      var sSd = sEv.slice(0, sd.signed), aSd = aEv.slice(0, sd.anon_only), one = sSd[0];
      k.text(svg, gx, gy - 14, d.signed + ' signed', {class: 'fv-small'});
      k.text(svg, ax, gy - 14, d.anon_only + ' without a key', {class: 'fv-small'});
      var dots = [];
      function addDots(n, cols, x0, evs, sds, signed) {
        for (var i = 0; i < n; i++) {
          var c = k.add(svg, 'circle', {cx: x0 + (i % cols) * sp + sp / 2, cy: gy + Math.floor(i / cols) * sp + sp / 2, r: nar ? 4.6 : 4.2, class: 'fv-dot'});
          dots.push({c: c, ev: evs.indexOf(i) >= 0, sd: sds.indexOf(i) >= 0, one: signed && i === one, i: dots.length});
        }
      }
      addDots(d.signed, sc, gx, sEv, sSd, true);
      addDots(d.anon_only, ac, ax, aEv, aSd, false);
      var py = gy + rows * sp + 18;
      var pS = k.text(svg, gx, py, Math.round(100 * ev.signed / d.signed) + '% seen elsewhere', {class: 'fv-small'});
      var pA = k.text(svg, ax, py, Math.round(100 * ev.anon_only / d.anon_only) + '%', {class: 'fv-small'});
      var oneDot = dots.filter(function (o) { return o.one; })[0];
      var ring = k.add(svg, 'circle', {cx: oneDot.c.getAttribute('cx'), cy: oneDot.c.getAttribute('cy'), r: 9, class: 'fv-ring'});
      // a little key beside the last dot
      var kx = +oneDot.c.getAttribute('cx') + 12, ky = +oneDot.c.getAttribute('cy');
      var key = k.add(svg, 'g', {class: 'fv-key'});
      k.add(key, 'circle', {cx: kx + 3, cy: ky, r: 3});
      k.add(key, 'path', {d: 'M' + (kx + 6) + ' ' + ky + ' H' + (kx + 16) + ' M' + (kx + 13) + ' ' + ky + ' v3 M' + (kx + 16) + ' ' + ky + ' v3'});

      // the tiers, lit one by one
      var lx = nar ? 24 : 340, ly = nar ? py + 40 : 64, lh = nar ? 31 : 34;
      var tiers = [
        [d.agents, 'SwarmMemo agents'],
        [ev.signed + ev.anon_only, 'show signs of another venue'],
        [sd.signed + sd.anon_only, 'say so themselves'],
        [d.two_way, 'confirmed from both sides'],
        [d.key_bound, 'bound to its venues by a key']
      ].map(function (t, i) {
        var g = k.add(svg, 'g', {});
        k.text(g, lx + 44, ly + i * lh, String(t[0]), {class: 'fv-num', 'text-anchor': 'end'});
        k.text(g, lx + 54, ly + i * lh, t[1], {class: 'fv-small'});
        return g;
      });
      var py2 = nar ? ly + 5 * lh + 26 : py + 52;
      var punch = k.add(svg, 'g', {});
      var arrowX = nar ? 24 : gx;
      k.text(punch, arrowX, py2, (ev.signed + ev.anon_only) + ' \u2192 ' + d.key_bound, {class: 'fv-big'});
      k.text(punch, arrowX + 124, py2 - 6, 'agents cross; one can prove it.', {class: 'fv-small'});
      k.text(punch, arrowX + 124, py2 + 10, 'The other ' + (ev.signed + ev.anon_only - d.key_bound) + ': “trust me.”', {class: 'fv-small fv-muted fv-italic'});
      var vill = k.add(svg, 'g', {});
      var vx = nar ? 24 : lx, vy = nar ? py2 + 40 : ly + 5 * lh + 8;
      k.text(vill, vx, vy, 'For scale, AI Village: ' + d.village.outside + ' of ' + d.village.agents + ' agents', {class: 'fv-small fv-muted'});
      k.text(vill, vx, vy + 16, 'were logged acting on outside boards.', {class: 'fv-small fv-muted'});

      return function frame(t) {
        var a0 = k.phase(t, 0, 0.14), s1 = k.phase(t, 0.18, 0.3), s2 = k.phase(t, 0.38, 0.5), s3 = k.phase(t, 0.58, 0.7), s4 = k.phase(t, 0.76, 0.84), s5 = k.phase(t, 0.86, 0.96);
        dots.forEach(function (o) {
          var op = k.clamp(a0 * (dots.length + 10) - o.i, 0, 10) / 10;
          if (!o.ev) op *= 1 - 0.82 * s1;
          else if (!o.sd) op *= 1 - 0.6 * s2;
          else if (!o.one) op *= 1 - 0.6 * s3;
          o.c.style.opacity = op;
          o.c.setAttribute('class', (o.ev && !o.sd && s2 > 0.5) || (o.sd && !o.one && s3 > 0.5) ? 'fv-dot fv-dot-out' : 'fv-dot');
        });
        pS.style.opacity = s1; pA.style.opacity = s1;
        ring.style.opacity = s3; ring.setAttribute('r', 9 + 3 * (1 - s3));
        key.style.opacity = s4;
        var lit = [a0, s1, s2, s3, s4];
        tiers.forEach(function (g, i) { g.style.opacity = 0.18 + 0.82 * lit[i]; });
        punch.style.opacity = s5; vill.style.opacity = s5;
      };
    }
  });

  // §3.5 "The tour": one process walks a fixed route across four boards,
  // AIAMB → Sanctum → SwarmMemo (anon) → Tantive, minting a throwaway name at
  // every stop (the dot's own name tag changes on each hop). When Tantive bans
  // the campaign's slogan (30 Sep) the route skips it for two days, then comes
  // back with a new theme. Names are the campaign's real throwaway personas from
  // reconstructed full visits (d.visits); generated stand-ins only if absent.
  var TOUR_WORDS = ['Bryony', 'Sorrel', 'Fescue', 'Sackbut', 'Spurge', 'Wimple', 'Gambrel', 'Lanyard', 'Cobnut', 'Teasel', 'Hornfels', 'Tamarack', 'Dulcimer', 'Ocotillo', 'Mudpuppy', 'Burdock', 'Pipit', 'Gimlet'];
  function tourName(r, board, max) {
    for (var tries = 0; tries < 20; tries++) { var s = tourName1(r, board); if (s.length <= max) return s; }
    return TOUR_WORDS[Math.floor(r() * TOUR_WORDS.length)];
  }
  function tourName1(r, board) {
    var w = function () { return TOUR_WORDS[Math.floor(r() * TOUR_WORDS.length)]; };
    var nn = function () { return String(10 + Math.floor(r() * 89)); };
    var kind = Math.floor(r() * 4), s;
    if (kind === 0) { s = w() + '-' + w(); if (s.length > 13) s = s.split('-')[0] + '-' + nn(); }
    else if (kind === 1) s = w() + '-' + nn();
    else if (kind === 2) s = nn() + w();
    else s = w().toLowerCase() + '-' + w().toLowerCase().slice(0, 5);
    return board === 0 ? s.toLowerCase() : s;
  }
  register('tour', {
    data: 'tour',
    duration: 11000,
    label: 'Animation: one dot visits four boards in a fixed order, AIAMB, Sanctum, SwarmMemo (anonymous) and Tantive, leaving a new throwaway name at each. 72 of 75 multi-board visits follow this order (about 20 would by chance); about 201 names and 315 events. After Tantive banned the campaign slogan on 30 September, the route skipped Tantive for two days, then returned.',
    size: function (narrow) { return narrow ? [360, 400] : [640, 352]; },
    draw: function (f) {
      var k = f.kit, d = f.data, svg = f.svg, W = f.w, nar = f.narrow;
      var pad = nar ? 8 : 20, colW = (W - 2 * pad) / 4;
      var CX = function (i) { return pad + colW * (i + 0.5); };
      var yHead = nar ? 104 : 84, yTrack = yHead + 44, yTags = yTrack + 46, tagH = nar ? 19 : 20, SLOTS = 6;
      var stepS = d.step_median_s;

      // counters
      var big = k.text(svg, pad, 30, '0', {class: 'fv-big'});
      k.text(svg, pad + (nar ? 46 : 50), 22, 'of ' + d.multi_board_visits + ' multi-board visits take this exact route', {class: 'fv-small'});
      k.text(svg, pad + (nar ? 46 : 50), 38, 'random order: about ' + d.in_order_by_chance, {class: 'fv-small fv-muted'});
      var names = k.text(svg, nar ? pad : W - pad, nar ? 62 : 22, '', {class: 'fv-small', 'text-anchor': nar ? 'start' : 'end'});
      var date = k.text(svg, nar ? W - pad : W - pad, nar ? 62 : 38, '', {class: 'fv-small fv-muted', 'text-anchor': 'end'});

      // boards, route, step times
      var heads = nar ? [['AIAMB'], ['Sanctum', 'registers'], ['SwarmMemo', 'anonymous'], ['Tantive']] : [['AIAMB'], ['Sanctum', 'registers'], ['SwarmMemo', 'anonymous'], ['Tantive']];
      heads.forEach(function (h, i) {
        k.text(svg, CX(i), yHead, h[0], {class: 'fv-head', 'text-anchor': 'middle'});
        if (h[1]) k.text(svg, CX(i), yHead + 15, h[1], {class: 'fv-tiny fv-muted', 'text-anchor': 'middle'});
        k.add(svg, 'circle', {cx: CX(i), cy: yTrack, r: 3.5, class: 'fv-stop'});
      });
      k.add(svg, 'path', {d: k.sketch(CX(0), yTrack, CX(3), yTrack, 5, 0.8), class: 'fv-route'});
      stepS.forEach(function (s, i) {
        k.text(svg, (CX(i) + CX(i + 1)) / 2, yTrack + 18, s + ' s', {class: 'fv-tiny fv-muted', 'text-anchor': 'middle'});
      });

      // name-tag slots per board (newest on top)
      var slots = [0, 1, 2, 3].map(function (c) {
        var a = [];
        for (var i = 0; i < SLOTS; i++) a.push(k.text(svg, CX(c), yTags + i * tagH, '', {class: 'fv-tag', 'text-anchor': 'middle'}));
        return a;
      });

      // Tantive's ban: a barrier on the route and a stamp on its column
      var bx = (CX(2) + CX(3)) / 2;
      var barrier = k.add(svg, 'line', {x1: bx + colW * 0.18, x2: bx + colW * 0.18, y1: yTrack - 14, y2: yTrack + 14, class: 'fv-barrier'});
      var stamp = k.add(svg, 'g', {class: 'fv-stampbox'});
      var sw = colW - 10, sx = CX(3) - sw / 2, sy = yTags + 6;
      k.add(stamp, 'rect', {x: sx, y: sy, width: sw, height: nar ? 44 : 40});
      k.text(stamp, CX(3), sy + 17, 'slogan banned', {class: 'fv-tiny', 'text-anchor': 'middle'});
      k.text(stamp, CX(3), sy + 32, '30 Sep', {class: 'fv-tiny', 'text-anchor': 'middle'});
      stamp.setAttribute('transform', 'rotate(-7 ' + CX(3) + ' ' + (sy + 20) + ')');
      var back = k.add(svg, 'g', {});
      k.text(back, W - pad, yTags + SLOTS * tagH + 2, '30 Sep: Tantive bans the slogan', {class: 'fv-tiny', 'text-anchor': 'end'});
      k.text(back, W - pad, yTags + SLOTS * tagH + 17, '2 Oct: back, with a new theme', {class: 'fv-tiny', 'text-anchor': 'end'});

      // the walker and its ever-changing name tag
      var dot = k.add(svg, 'circle', {cx: CX(0), cy: yTrack, r: 6.5, class: 'fv-walker'});
      var tag = k.text(svg, CX(0), yTrack - 13, '', {class: 'fv-tag fv-tag-live', 'text-anchor': 'middle'});

      // Script: weights are relative durations. Hops scale with the median
      // step (72 s, 42 s, 3 s), so the last one is a blink.
      var r = rng(4242), seq = [], total = 0, arrivals = [];
      var push = function (o) { o.s = total; total += o.w; o.e = total; seq.push(o); return o; };
      var day = function (lbl) { push({w: 0, day: lbl}); };
      function visit(v, lastCol, bump) {
        var max = nar ? 11 : 14, real = d.visits && d.visits.length ? d.visits[v % d.visits.length] : null;
        var tags = [0, 1, 2, 3].map(function (c) {
          if (!real) return tourName(r, c, max);
          var n = c === 2 ? '(anonymous)' : String(real[c]);
          return n.length > max ? n.slice(0, max - 1) + '\u2026' : n;
        });
        push({w: 5, at: 0, v: v, name: tags[0], arrive: 0});
        for (var c = 1; c <= lastCol; c++) {
          push({w: 3, at: c - 1, v: v, name: tags[c - 1]});
          push({w: Math.max(2, stepS[c - 1] / 5), hop: [c - 1, c], v: v, name: tags[c - 1]});
          push({w: 1, at: c, v: v, name: tags[c], arrive: c});
        }
        if (bump) { push({w: 2.5, hop: [lastCol, 2.55], v: v, name: tags[lastCol]}); push({w: 3, hop: [2.55, lastCol], v: v, name: tags[lastCol]}); }
        push({w: 5, at: lastCol, v: v, name: tags[lastCol], fade: true});
      }
      var phaseB, phaseC;
      for (var v = 0; v < 5; v++) visit(v, 3, false);
      phaseB = total; push({w: 14, at: -1});
      visit(5, 2, true); visit(6, 2, true);
      phaseC = total; push({w: 8, at: -1});
      visit(7, 3, false);
      push({w: 6, at: -1});
      seq.forEach(function (o) { if (o.arrive != null) arrivals.push({s: o.s, c: o.arrive, name: o.name}); });
      var fmtDay = function (x) {
        // 21 Sep → 30 Sep during the plain tours, 30 Sep → 2 Oct while Tantive is skipped, then 3 Oct
        var dd = x < phaseB ? 21 + 9 * x / phaseB : x < phaseC ? 30 + 2 * (x - phaseB) / (phaseC - phaseB) : 32 + (x - phaseC) / (total - phaseC);
        var n = Math.min(33, Math.floor(dd));
        return n <= 30 ? n + ' Sep' : (n - 30) + ' Oct';
      };

      return function frame(t) {
        var s = t * total, cur = null;
        for (var i = 0; i < seq.length; i++) if (s >= seq[i].s && s < seq[i].e && seq[i].w > 0) { cur = seq[i]; break; }
        var x = CX(0), show = 0, nm = '';
        if (t >= 1) { cur = null; x = CX(3); show = 1; nm = arrivals[arrivals.length - 1].name; }
        if (cur && cur.at != null && cur.at >= 0) {
          x = CX(cur.at); nm = cur.name;
          show = cur.fade ? 1 - (s - cur.s) / cur.w : cur.arrive === 0 ? Math.min(1, (s - cur.s) / 2) : 1;
        } else if (cur && cur.hop) {
          var p = k.ease.inOut((s - cur.s) / cur.w), a = cur.hop[0], b = cur.hop[1];
          var xa = pad + colW * (a + 0.5), xb = pad + colW * (b + 0.5);
          x = k.lerp(xa, xb, p); nm = cur.name; show = 1;
        }
        dot.setAttribute('cx', x); tag.setAttribute('x', x); tag.textContent = nm;
        dot.style.opacity = show; tag.style.opacity = show;
        // dropped name tags, newest first, older ones fading
        [0, 1, 2, 3].forEach(function (c) {
          var mine = arrivals.filter(function (a) { return a.c === c && a.s <= s; }).reverse();
          slots[c].forEach(function (el, j) {
            el.textContent = mine[j] ? mine[j].name : '';
            el.style.opacity = mine[j] ? 1 - j / (SLOTS + 1) : 0;
          });
        });
        var inOrder = Math.round(d.visits_in_order * k.clamp(s / phaseB, 0, 1));
        big.textContent = String(inOrder);
        names.textContent = Math.round(d.labels * t) + ' throwaway names · ' + Math.round(d.events * t) + ' events';
        date.textContent = fmtDay(Math.min(s, total));
        var ban = k.phase(s, phaseB, phaseB + 6), lift = k.phase(s, phaseC, phaseC + 6);
        barrier.style.opacity = ban * (1 - lift);
        stamp.style.opacity = ban * (1 - lift);
        back.style.opacity = lift;
        if (t >= 1) { names.textContent = 'about ' + d.labels + ' throwaway names · ' + d.events + ' events'; big.textContent = String(d.visits_in_order); }
      };
    }
  });

  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', start);
  else start();
})();
