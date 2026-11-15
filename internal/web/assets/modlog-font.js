/* Load only visible, accessible style fonts; ordinary text is the fallback. */
(() => {
  'use strict';
  if (!('IntersectionObserver' in window) || !('FontFace' in window) || !document.fonts) return;
  let next = 0;
  const fonts = new Map();
  // Bound simultaneous fetches/decodes; loaded faces remain until page unload.
  const pending = [];
  let active = 0;
  function pump() {
    while (active < 3 && pending.length) {
      const job = pending.shift();
      active++;
      Promise.resolve().then(job.run).then(job.resolve, job.reject).finally(() => {
        active--;
        pump();
      });
    }
  }
  function loadFont(url, name) {
    return new Promise((resolve, reject) => {
      pending.push({resolve, reject, run: () => {
        const face = new FontFace(name, `url("${url}")`);
        return face.load().then(loaded => {
          document.fonts.add(loaded);
          return name;
        });
      }});
      pump();
    });
  }
  const observer = new IntersectionObserver(entries => {
    for (const entry of entries) {
      if (!entry.isIntersecting) continue;
      const sample = entry.target;
      observer.unobserve(sample);
      const url = sample.dataset.fontUrl;
      // Only local attachment IDs; never interpret markup as a CSS URL.
      if (!/^\/a\/[A-Za-z0-9_-]+$/.test(url)) continue;
      if (!fonts.has(url)) {
        const name = `modlog-font-${next++}`;
        fonts.set(url, loadFont(url, name));
      }
      fonts.get(url).then(name => { sample.style.fontFamily = `"${name}"`; }).catch(() => {});
    }
  });
  document.querySelectorAll('.modlog-font[data-font-url]').forEach(sample => observer.observe(sample));
})();
