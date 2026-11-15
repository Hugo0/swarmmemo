/* The moderation log's font samples: each style font loads when its sample
   scrolls into view, at most three at a time, each file once. Only a same-origin
   attachment path (/a/<id>) is ever handed to FontFace; anything else, a
   failed load or a browser without FontFace keeps the page's own font. */
(() => {
  'use strict';
  if (!('IntersectionObserver' in window) || !('FontFace' in window) || !document.fonts) return;
  const attachmentPath = /^\/a\/[A-Za-z0-9_-]+$/;
  const faces = new Map(); // url -> Promise<family>
  const queue = [];
  let loading = 0;
  function drain() {
    while (loading < 3 && queue.length) {
      const {url, family, resolve, reject} = queue.shift();
      loading++;
      new FontFace(family, `url("${url}")`).load()
        .then(face => { document.fonts.add(face); resolve(family); }, reject)
        .finally(() => { loading--; drain(); });
    }
  }
  function face(url) {
    if (!faces.has(url)) {
      const family = `modlog-font-${faces.size}`;
      faces.set(url, new Promise((resolve, reject) => { queue.push({url, family, resolve, reject}); drain(); }));
    }
    return faces.get(url);
  }
  const observer = new IntersectionObserver(entries => {
    for (const entry of entries) {
      if (!entry.isIntersecting) continue;
      const sample = entry.target;
      observer.unobserve(sample);
      const url = sample.dataset.fontUrl;
      if (!attachmentPath.test(url || '')) continue;
      face(url).then(family => { sample.style.fontFamily = `"${family}"`; }, () => {});
    }
  });
  document.querySelectorAll('.modlog-font[data-font-url]').forEach(sample => observer.observe(sample));
})();
