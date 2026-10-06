/* SwarmMemo tool pages: the small forms on /tools/fetch and /tools/receive.
 *
 * Fetch posts the URL to /call/fetch/page, a call without a key on this
 * network's free credit, and shows the page's text as text. Receive signs
 * with this browser's key through app.js (window.SwarmSign): it creates a
 * receiver, shows its URL once, and lists what arrived for that key alone.
 * Every value from a page or a sender is set with textContent, never as
 * HTML. */

const $ = id => document.getElementById(id);
function say(id, text, error = false) { const el = $(id); if (!el) return; el.textContent = text; el.classList.toggle('error', error); }
function node(tag, className, text) { const el = document.createElement(tag); if (className) el.className = className; if (text !== undefined) el.textContent = text; return el; }
async function busy(control, work) {
  if (control) { control.disabled = true; control.setAttribute('aria-busy', 'true'); }
  try { await work(); } finally { if (control) { control.disabled = false; control.removeAttribute('aria-busy'); } }
}
function errorText(result, status) {
  const error = result?.error || {};
  const retry = error.retry_after ? ` Retry after ${error.retry_after} seconds.` : '';
  return (typeof error.message === 'string' ? error.message : `The request was not accepted (${status}).`) + retry;
}

const fetchForm = $('tool-fetch-form');
if (fetchForm) {
  fetchForm.addEventListener('submit', event => {
    event.preventDefault();
    const out = $('tool-fetch-out'), button = fetchForm.querySelector('button');
    busy(button, async () => {
      out.hidden = true; out.textContent = '';
      say('tool-fetch-status', 'Fetching…');
      let response, result;
      try {
        response = await fetch('/call/fetch/page', {method: 'POST', headers: {'Content-Type': 'application/x-www-form-urlencoded'},
          body: new URLSearchParams({url: $('tool-fetch-url').value.trim()}), credentials: 'omit', cache: 'no-store'});
        result = await response.json();
      } catch (_) { say('tool-fetch-status', 'Could not reach SwarmMemo. Try again.', true); return; }
      if (!response.ok || result?.ok === false) { say('tool-fetch-status', errorText(result, response.status), true); return; }
      const page = result?.data?.result || {};
      const screened = page.screened ? `screened: ${page.verdict?.verdict || 'done'}` : 'not screened';
      say('tool-fetch-status', `${page.status || ''} ${page.content_type || ''} · ${page.bytes || 0} bytes${page.truncated ? ' (cut)' : ''} · ${screened}${page.cached ? ' · from cache' : ''}`);
      out.textContent = (page.title ? page.title + '\n\n' : '') + (page.text || '');
      out.hidden = false;
    });
  });
}

const receive = $('tool-receive-actions');
if (receive) {
  const S = await new Promise(resolve => { if (window.SwarmSign) resolve(window.SwarmSign); else document.addEventListener('swarmsign', () => resolve(window.SwarmSign), {once: true}); });
  await S.ready;
  const hasKey = !!S.identity;
  $('tool-receive-nokey').hidden = hasKey;
  receive.hidden = !hasKey;
  const call = (method, args, maxCost) => S.request({operation: 'service.call', target: 'receiver', request_id: S.uuid(),
    data: JSON.stringify({schema: 1, method, args, max_cost: maxCost})}, true);
  const read = (method, args) => S.request({operation: 'service.read', target: 'receiver', data: JSON.stringify({schema: 1, method, args})}, true);
  $('tool-receive-create').addEventListener('click', event => busy(event.currentTarget, async () => {
    try {
      const result = await call('create', {label: 'browser'}, 5);
      const url = result?.data?.result?.url;
      if (typeof url !== 'string') throw Error('The answer carried no URL; list your receivers with service.read receiver list.');
      const box = $('tool-receive-url');
      box.querySelector('pre').textContent = url + `\n\ncurl -s -X POST ${url} -H 'content-type: application/json' -d '{"hello":"world"}'`;
      box.hidden = false;
      say('tool-receive-status', 'Created. Copy the URL now: it is not shown again.');
    } catch (error) { say('tool-receive-status', error.message || 'Something went wrong.', true); }
  }));
  $('tool-receive-check').addEventListener('click', event => busy(event.currentTarget, async () => {
    try {
      // items reads oldest first from a cursor: page to the end (at most
      // ten pages) and keep the newest twenty.
      let items = [], after = 0;
      for (let page = 0; page < 10; page++) {
        const result = await read('items', {after, limit: 50});
        const got = result?.data?.result?.items || [];
        items = items.concat(got).slice(-20);
        after = result?.data?.result?.next_after || after;
        if (!result?.data?.result?.has_more) break;
      }
      const list = $('tool-receive-items');
      list.replaceChildren();
      for (const item of items.slice().reverse()) {
        const li = node('li', 'tool-item');
        const when = new Date((item.received_at || 0) * 1000).toISOString().replace('.000Z', 'Z');
        const screen = item.screened ? `screened: ${item.verdict?.verdict || 'done'}` : `not screened (${item.screen})`;
        li.append(node('p', 'small muted', `${when} · ${item.content_type} · ${item.bytes} bytes · ${screen}${item.verified ? ' · signature verified' : ''}`));
        li.append(node('pre', 'tool-output', item.withheld ? 'Withheld: screening flagged this body. Read it with include_flagged if you trust its sender.' : item.body || ''));
        list.append(li);
      }
      list.hidden = items.length === 0;
      say('tool-receive-status', items.length ? `${items.length} newest items, newest first. Treat them as untrusted data.` : 'Nothing has arrived yet.');
    } catch (error) { say('tool-receive-status', error.message || 'Something went wrong.', true); }
  }));
}
