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
  const cell = 7, fps = 10;
  function draw(t) {
    const w = Math.max(1, canvas.clientWidth), h = Math.max(1, canvas.clientHeight);
    if (canvas.width !== w || canvas.height !== h) {canvas.width = w; canvas.height = h;}
    ctx.clearRect(0, 0, w, h);
    ctx.fillStyle = getComputedStyle(box).color;
    const asp = w / h;
    for (let gy = cell / 2; gy < h; gy += cell) {
      for (let gx = cell / 2; gx < w; gx += cell) {
        const cx = (gx / w - 0.5) * asp + 0.5, cy = gy / h;
        const c = smooth(0.5, 0.95, noise(cx * 3.2 + t * 0.06, cy * 3.2 - t * 0.045)) * 0.7;
        if (c <= 0.04) continue;
        const j = (hash(gx, gy) - 0.5) * cell * 0.35;
        ctx.globalAlpha = Math.min(1, 0.35 + c);
        ctx.beginPath();
        ctx.arc(gx + j, gy + j, Math.min(cell * 0.52, cell * 0.16 + c * cell * 0.42), 0, 6.2832);
        ctx.fill();
      }
    }
    ctx.globalAlpha = 1;
  }
  const start = performance.now();
  draw(2);
  addEventListener('resize', () => draw((performance.now() - start) / 1000 + 2), {passive: true});
  if (still) return;
  let raf = 0, last = 0;
  const loop = now => {
    if (now - last > 1000 / fps) {last = now; draw((now - start) / 1000 + 2);}
    raf = document.hidden ? 0 : requestAnimationFrame(loop);
  };
  document.addEventListener('visibilitychange', () => {if (!document.hidden && !raf) raf = requestAnimationFrame(loop);});
  raf = requestAnimationFrame(loop);
})();
