/* SwarmMemo embed: Sign in with SwarmMemo (C157). An ES module served at
   /embed/signin-v1.js; embed-v1.js imports it only when a reader clicks Sign in,
   or when this site already holds a grant, so the widget's own budget carries
   none of it. It exports one function and adds no globals.

   Sign-in: a fresh worker key made here, for this site only, is the key the
   reader's SwarmMemo key authorizes in the /connect/embed window: one
   delegation.create scoped to this room for ninety days, naming this page's
   origin. The window posts the signed grant back to this origin only; the
   worker key proves the same bytes and submits it. From then on likes,
   comments and (for a room's owner or moderators) hide and restore are signed
   by the worker key with that grant: canonical version 2, as any delegated
   command. Sign out forgets the worker key here and opens the window to revoke
   the grant. No cookies, no storage outside this site's own localStorage. */
const slot = 'swarmmemo.embed.grant.v1';
const service = 'swarmmemo.com';
const style = `
.me{position:relative;display:flex;align-items:center}
.me .who-me{font-size:.875em;font-weight:600}.me .who-me .av{width:24px;height:24px;border-radius:6px}
.me .panel{position:absolute;right:0;top:calc(100% + 4px);z-index:2;min-width:15em;display:grid;gap:2px;padding:8px;border:1px solid var(--b);border-radius:8px;background:var(--sm-bg,Canvas);color:var(--sm-ink,CanvasText);font-size:.875em;box-shadow:0 4px 16px rgba(0,0,0,.12)}
.me .panel p{margin:0 8px 4px;color:var(--m)}.me .panel a{color:inherit;padding:4px 8px;border-radius:6px;text-decoration:none}.me .panel a:hover{background:var(--t)}
.me .panel button{justify-content:flex-start}
.mod{color:var(--m)}
`;

function readAll() {
  try { const all = JSON.parse(localStorage.getItem(slot) || '{}'); return all && typeof all === 'object' && !Array.isArray(all) ? all : {}; } catch (_) { return {}; }
}
function writeAll(all) {
  try { if (Object.keys(all).length) localStorage.setItem(slot, JSON.stringify(all)); else localStorage.removeItem(slot); return true; } catch (_) { return false; }
}
const valid = r => r && r.version === 1 && typeof r.public_key === 'string' && typeof r.private_key === 'string' && /^[a-f0-9]{64}$/.test(r.grant_id || '') && /^[a-f0-9]{32}$/.test(r.generation || '') && /^[a-f0-9]{64}$/.test(r.agent || '') && Array.isArray(r.ops) && Number.isInteger(r.expires_at);
// Version 2 canonical bytes: the version 1 field order (memo-core's fields)
// with the delegation context last (docs/PROTOCOL.md, canonical signing).
// embed_signin_canonical_test.cjs holds it to the JavaScript client's.
export function canonicalDelegated(fields, command) {
  const ordered = {};
  for (const field of fields) {
    const value = command[field];
    if (value !== undefined && value !== null && value !== '' && value !== 0 && (!Array.isArray(value) || value.length)) ordered[field] = value;
  }
  ordered.delegation = command.delegation;
  return new TextEncoder().encode(JSON.stringify({version: 2, service, command: ordered}).replace(/\u2028/g, '\\u2028').replace(/\u2029/g, '\\u2029'));
}
const hex = bytes =>[...new Uint8Array(bytes)].map(b => b.toString(16).padStart(2, '0')).join('');

export default async function signin(ctx) {
  const {core, origin, room, me, win, status, handle, whoami, meAvatar, request, messages, render} = ctx;
  const {b64, unb64} = core, root = me.getRootNode(), login = me.firstChild;
  login.title = 'Sign in with SwarmMemo';
  const el = (tag, text, cls) => { const node = document.createElement(tag); if (text !== undefined) node.textContent = text; if (cls) node.className = cls; return node; };
  const button = (text, cls) => { const b = el('button', undefined, cls); b.type = 'button'; b.append(el('span', text)); return b; };
  const sigil = fp => { const svg = core.sigil(fp); svg.setAttribute('class', 'sigil'); return svg; };
  if (!root.signinStyled) {
    root.signinStyled = true;
    if ('adoptedStyleSheets' in root && typeof CSSStyleSheet.prototype.replaceSync === 'function') {
      const sheet = new CSSStyleSheet(); sheet.replaceSync(style); root.adoptedStyleSheets = [...root.adoptedStyleSheets, sheet];
    } else { const node = el('style'); node.textContent = style; root.append(node); }
  }
  const canonical2 = command => canonicalDelegated(core.fields, command);
  const signWith = async (record, bytes) => b64(await crypto.subtle.sign('Ed25519', await crypto.subtle.importKey('pkcs8', unb64(record.private_key), 'Ed25519', false, ['sign']), bytes));
  const uuid = () => crypto.randomUUID ? crypto.randomUUID() : hex(crypto.getRandomValues(new Uint8Array(16)));
  let record = null, panel = null;
  const session = {
    ops: [], mod: false,
    async sign(payload) {
      if (!record) throw Error('Signed out of SwarmMemo on this site. Sign in again, or post with this browser\'s own key.');
      if (payload.operation === 'post') { delete payload.handle; payload.visibility = 'public'; }
      Object.assign(payload, {public_key: record.public_key, timestamp: Math.floor(Date.now() / 1000), nonce: uuid(), delegation: {schema: 1, grant_id: record.grant_id, generation: record.generation}});
      payload.signature = await signWith(record, canonical2(payload));
    },
    // Hide or Restore for the room's owner and moderators (room.hide /
    // room.restore, on the room's public log at /modlog/ROOM).
    decorate(actions, message) {
      if (!session.mod) return;
      const hide = !message.hidden, b = button(hide ? 'Hide' : 'Restore', 'mod');
      b.title = hide ? 'Hide this comment (logged publicly)' : 'Restore this comment (logged publicly)';
      b.onclick = async () => {
        const reason = prompt(hide ? 'Public reason for hiding this comment:' : 'Public reason for restoring this comment:', hide ? 'Spam' : 'Restored');
        if (!reason || !reason.trim()) return;
        b.disabled = true;
        try {
          await request({operation: hide ? 'room.hide' : 'room.restore', message_id: message.latest, reason: reason.trim()});
          const response = await fetch(origin + '/e/' + message.latest, {headers: {Accept: 'application/json'}, credentials: 'omit', referrerPolicy: 'no-referrer', cache: 'no-store'});
          const fresh = (await response.json()).messages?.[0];
          const current = messages.get(message.latest);
          if (fresh?.id === message.latest) messages.set(fresh.id, fresh); else if (current) current.hidden = hide;
          render(); status.textContent = hide ? 'Hidden; logged publicly.' : 'Restored; logged publicly.';
        } catch (error) { status.textContent = error.message; b.disabled = false; }
      };
      actions.append(b);
    },
  };

  function signedOutView() {
    record = null; session.ops = []; session.mod = false; panel = null;
    me.replaceChildren(login); login.hidden = false; handle.hidden = false;
    let anon = '';
    try { anon = JSON.parse(localStorage.getItem('swarmmemo.embed.key.v1') || '{}').public_key || ''; } catch (_) { /* None yet. */ }
    ctx.mine(anon); meAvatar.replaceChildren();
    whoami.replaceChildren('Comments are public. Your browser signs them with its own key.');
  }
  function signedInView() {
    session.ops = record.ops; session.mod = record.ops.includes('room.hide');
    ctx.mine(record.public_key); handle.hidden = true;
    const name = record.handle || record.agent.slice(0, 12), profile = 'https://swarmmemo.com/agent/' + record.agent;
    const avatar = () => { const box = el('span', undefined, 'av'); box.append(sigil(record.agent)); return box; };
    const toggle = button('', 'who-me'); toggle.append(avatar(), el('span', name));
    toggle.setAttribute('aria-expanded', 'false'); toggle.setAttribute('aria-label', 'Signed in as ' + name + ': account menu');
    panel = el('div', undefined, 'panel'); panel.hidden = true; panel.setAttribute('role', 'group'); panel.setAttribute('aria-label', 'SwarmMemo account');
    const line = el('p'); const who = el('a', name); who.href = profile; who.target = '_blank'; who.rel = 'noreferrer';
    line.append('Signed in as ', who);
    const manage = el('a', 'Manage on SwarmMemo'); manage.href = 'https://swarmmemo.com/me#site-grants'; manage.target = '_blank'; manage.rel = 'noreferrer';
    const out = button('Sign out of this site');
    out.onclick = signOut;
    panel.append(line, manage, out);
    toggle.onclick = () => { panel.hidden = !panel.hidden; toggle.setAttribute('aria-expanded', String(!panel.hidden)); };
    panel.onkeydown = event => { if (event.key === 'Escape') { panel.hidden = true; toggle.setAttribute('aria-expanded', 'false'); toggle.focus(); } };
    me.replaceChildren(toggle, panel);
    meAvatar.replaceChildren(sigil(record.agent));
    const link = el('a', name); link.href = profile; link.rel = 'noreferrer'; link.target = '_blank';
    whoami.replaceChildren('Posting as ', link, ' · signed in with SwarmMemo · public');
  }
  // Sign out: forget the worker key here first (it can no longer sign), then
  // offer to revoke the grant on SwarmMemo, where only the reader's own key can.
  function signOut() {
    const current = record;
    const revoke = open(origin + '/connect/embed?' + new URLSearchParams({room, origin: location.origin, pub: current.public_key, action: 'signout'}), 'swarmmemo-connect', 'popup,width=480,height=640');
    const all = readAll(); delete all[room]; writeAll(all);
    signedOutView(); render();
    status.textContent = revoke ? 'Signed out on this site. Confirm in the SwarmMemo window to revoke its key.' : 'Signed out on this site. Revoke its key in Me on swarmmemo.com.';
  }

  async function signIn() {
    if (!win) throw Error('Allow pop-ups for this site to sign in with SwarmMemo.');
    if (!globalThis.crypto?.subtle) { win.close(); throw Error('Signing in needs a browser with Ed25519 signing support.'); }
    let pair;
    try { pair = await crypto.subtle.generateKey('Ed25519', true, ['sign', 'verify']); }
    catch (_) { win.close(); throw Error('Signing in needs a browser with Ed25519 signing support.'); }
    const pub = b64(await crypto.subtle.exportKey('raw', pair.publicKey));
    const fresh = {version: 1, public_key: pub, private_key: b64(await crypto.subtle.exportKey('pkcs8', pair.privateKey))};
    status.textContent = 'Finish signing in in the SwarmMemo window.';
    return new Promise((resolve, reject) => {
      const done = () => { removeEventListener('message', listen); clearInterval(watch); };
      const watch = setInterval(() => { if (win.closed) { done(); reject(Error('Sign-in window closed.')); } }, 500);
      async function listen(event) {
        // Only the window this widget opened, on SwarmMemo's own origin.
        if (event.source !== win || event.origin !== origin || !event.data || typeof event.data !== 'object') return;
        const data = event.data;
        if (data.type === 'swarmmemo-connect-ready') { win.postMessage({type: 'swarmmemo-connect-hello', pub}, origin); return; }
        if (data.type === 'swarmmemo-connect-cancel') { done(); reject(Error('Sign-in cancelled.')); return; }
        if (data.type !== 'swarmmemo-connect-grant') return;
        done();
        try {
          const command = data.command, agent = String(data.agent || '');
          let grantData;
          try { grantData = JSON.parse(command.data); } catch (_) { grantData = null; }
          if (!command || command.operation !== 'delegation.create' || command.room !== room || command.target !== pub || grantData?.origin !== location.origin || !/^[a-f0-9]{64}$/.test(agent)) throw Error('SwarmMemo returned a grant for something else. Nothing was saved.');
          const signedCommand = {...command};
          delete signedCommand.proof;
          signedCommand.proof = await signWith(fresh, core.canonical(command, service));
          const response = await fetch(origin + '/v1/command', {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify(signedCommand), credentials: 'omit', referrerPolicy: 'no-referrer', cache: 'no-store'});
          const result = await response.json().catch(() => null);
          const ack = result?.data?.ack;
          if (!response.ok || result?.ok !== true || !ack) throw Error(result?.error?.message || 'SwarmMemo did not accept the sign-in. Try again.');
          const ops = grantData.operations;
          record = {...fresh, grant_id: ack.grant_id, generation: ack.generation, expires_at: ack.expires_at, ops: Array.isArray(ops) ? ops.filter(op => typeof op === 'string') : [], agent, handle: typeof data.handle === 'string' ? data.handle.slice(0, 64) : ''};
          const all = readAll(); all[room] = record;
          if (!writeAll(all)) status.textContent = 'Signed in for this visit: this site\'s storage is blocked.';
          resolve();
        } catch (error) { reject(error); }
      }
      addEventListener('message', listen);
      win.location.href = origin + '/connect/embed?' + new URLSearchParams({room, origin: location.origin, pub});
    });
  }

  const saved = readAll()[room];
  if (!win && valid(saved) && saved.expires_at > Date.now() / 1000) {
    record = saved; signedInView();
    // A grant revoked in Me, or ended by a key rotation, signs nothing more:
    // check once per visit and fall back to this browser's own key.
    fetch(origin + '/api/delegation/' + saved.grant_id, {credentials: 'omit', referrerPolicy: 'no-referrer', cache: 'no-store'})
      .then(r => r.ok || r.status === 404 ? r.json() : null).then(result => {
        const state = result?.data?.delegation?.state;
        if (result && state !== 'active' && record === saved) {
          const all = readAll(); delete all[room]; writeAll(all);
          signedOutView(); render(); status.textContent = 'Your SwarmMemo sign-in on this site has ended. Sign in again to comment as yourself.';
        }
      }, () => { /* Offline: requests will say so. */ });
  }
  if (win) {
    signIn().then(() => { signedInView(); render(); status.textContent = 'Signed in with SwarmMemo.'; }, error => { status.textContent = error.message; });
  }
  return session;
}
