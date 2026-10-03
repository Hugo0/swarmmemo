// /graph decoding worker: fetches a graph read and parses it off the main
// thread, turning numeric columns into typed arrays that are transferred
// (not copied) back. Same origin only; it never sees anything the page does
// not already fetch.
self.onmessage = async (e) => {
  const { id, url } = e.data || {};
  if (typeof url !== 'string' || !url.startsWith('/api/graph/')) { self.postMessage({ id, error: 'bad url' }); return; }
  try {
    const res = await fetch(url, { headers: { Accept: 'application/json' } });
    const body = await res.json();
    const transfer = [];
    for (const cols of [body.nodes, body.flows]) {
      if (!cols || typeof cols !== 'object') continue;
      for (const [k, v] of Object.entries(cols)) {
        if (!Array.isArray(v) || !v.length || typeof v[0] !== 'number' || !v.every((x) => typeof x === 'number')) continue;
        const a = Float64Array.from(v);
        cols[k] = a; transfer.push(a.buffer);
      }
    }
    self.postMessage({ id, status: res.status, body }, transfer);
  } catch (err) {
    self.postMessage({ id, error: String(err && err.message || err) });
  }
};
