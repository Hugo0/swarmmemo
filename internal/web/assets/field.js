// A slow halftone field behind the page: ink dots whose size follows drifting
// value noise, so the paper feels alive without moving the content. Adapted
// from the voiceio site's dither field (itself after KOLlateral's 1-bit
// grammar). It draws at most 10 frames a second at CSS-pixel resolution,
// stops while the tab is hidden, draws one still frame under reduced motion,
// and is skipped on styled rooms and Save-Data connections.
(() => {
  if (document.documentElement.classList.contains('room-styled') || navigator.connection?.saveData) return;
  const still = matchMedia('(prefers-reduced-motion: reduce)').matches;
  const box = document.createElement('div');
  box.className = 'page-field'; box.setAttribute('aria-hidden', 'true');
  const canvas = document.createElement('canvas');
  box.append(canvas); document.body.prepend(box);
  const ctx = canvas.getContext('2d');
  const hash = (x, y) => {const s = Math.sin(x * 127.1 + y * 311.7) * 43758.5453; return s - Math.floor(s);};
  const noise = (x, y) => {
    const ix = Math.floor(x), iy = Math.floor(y), fx = x - ix, fy = y - iy;
    const ux = fx * fx * (3 - 2 * fx), uy = fy * fy * (3 - 2 * fy);
    return hash(ix, iy) * (1 - ux) * (1 - uy) + hash(ix + 1, iy) * ux * (1 - uy) + hash(ix, iy + 1) * (1 - ux) * uy + hash(ix + 1, iy + 1) * ux * uy;
  };
  const smooth = (a, b, x) => {const t = Math.max(0, Math.min(1, (x - a) / (b - a))); return t * t * (3 - 2 * t);};
  const cell = 7;
  // Interaction: a soft brightening under the pointer, and on click or tap a
  // brighter ring that expands and fades. The field runs at 10 fps at rest and
  // 30 while someone is interacting, then settles back.
  let px = -1e4, py = -1e4, lastInput = -1e9;
  const ripples = [];
  addEventListener('pointermove', e => {if (still) return; px = e.clientX; py = e.clientY; lastInput = performance.now(); wake();}, {passive: true});
  addEventListener('pointerdown', e => {
    if (still) return;
    ripples.push({x: e.clientX, y: e.clientY, at: performance.now()});
    if (ripples.length > 6) ripples.shift();
    lastInput = performance.now(); wake();
  }, {passive: true});
  document.documentElement.addEventListener('pointerleave', () => {px = py = -1e4;});
  // Wall-clock time, so the pattern continues from page to page instead of restarting.
  const clock = () => (Date.now() / 1000) % 86400;
  function draw(t) {
    const w = Math.max(1, canvas.clientWidth), h = Math.max(1, canvas.clientHeight);
    if (canvas.width !== w || canvas.height !== h) {canvas.width = w; canvas.height = h;}
    ctx.clearRect(0, 0, w, h);
    const style = getComputedStyle(box);
    ctx.fillStyle = style.color;
    const base = parseFloat(style.getPropertyValue('--field-alpha')) || 0.07;
    const now = performance.now();
    for (let i = ripples.length - 1; i >= 0; i--) if (now - ripples[i].at > 1800) ripples.splice(i, 1);
    const asp = w / h, glowR = 150;
    for (let gy = cell / 2; gy < h; gy += cell) {
      for (let gx = cell / 2; gx < w; gx += cell) {
        const cx = (gx / w - 0.5) * asp + 0.5, cy = gy / h;
        const c = smooth(0.5, 0.95, noise(cx * 3.2 + t * 0.06, cy * 3.2 - t * 0.045)) * 0.7;
        // Excitement from the pointer and from ripples, 0..1.
        let e = 0;
        const dx = gx - px, dy = gy - py;
        if (dx > -glowR && dx < glowR && dy > -glowR && dy < glowR) e = Math.max(e, 0.45 * Math.exp(-(dx * dx + dy * dy) / (2 * 55 * 55)));
        for (const r of ripples) {
          const age = (now - r.at) / 1000, radius = age * 420, d = Math.hypot(gx - r.x, gy - r.y) - radius;
          if (d > -40 && d < 40) e = Math.max(e, (1 - age / 1.8) * Math.exp(-(d * d) / (2 * 14 * 14)));
        }
        const v = Math.max(c, e * 0.9);
        if (v <= 0.04) continue;
        const j = (hash(gx, gy) - 0.5) * cell * 0.35;
        ctx.globalAlpha = Math.min(1, (0.35 + v) * (base + e * 0.4));
        ctx.beginPath();
        ctx.arc(gx + j, gy + j, Math.min(cell * 0.52, cell * 0.16 + v * cell * 0.42), 0, 6.2832);
        ctx.fill();
      }
    }
    ctx.globalAlpha = 1;
  }
  draw(clock());
  addEventListener('resize', () => draw(clock()), {passive: true});
  let raf = 0, last = 0;
  const loop = now => {
    const active = now - lastInput < 2000 || ripples.length > 0;
    if (still && !active) {raf = 0; draw(clock()); return;}
    if (now - last > 1000 / (active ? 30 : 10)) {last = now; draw(still ? 2 : clock());}
    raf = document.hidden ? 0 : requestAnimationFrame(loop);
  };
  function wake() {if (!raf && !document.hidden) raf = requestAnimationFrame(loop);}
  document.addEventListener('visibilitychange', wake);
  if (!still) wake();
})();
