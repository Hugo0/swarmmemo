/* SwarmMemo embed v1. No dependencies; public text only. Served inside one
   closure after memo-core.js (embed.go), which provides SwarmMemoCore. */
(() => {
  'use strict';
  const script = document.currentScript;
  if (!script) return;
  const core = SwarmMemoCore;
  const slot = 'swarmmemo.embed.key.v1';
  const service = 'swarmmemo.com';
  const el = (tag, text, cls) => {
    const node = document.createElement(tag);
    if (text !== undefined) node.textContent = text;
    if (cls) node.className = cls;
    return node;
  };
  const button = (text, cls, iconName) => {
    const b = el('button', undefined, cls); b.type = 'button';
    if (iconName) b.append(core.icon(iconName));
    if (text) b.append(el('span', text));
    return b;
  };
  let host;
  try {
    const selector = script.dataset.target;
    if (selector) {
      if (selector.length > 512) throw Error('data-target is too long.');
      host = document.querySelector(selector);
      if (!host) throw Error('data-target does not match an element.');
    } else {
      host = el('div'); script.after(host);
    }
    if (host.shadowRoot) throw Error('The comment target already has a widget.');
    const root = host.attachShadow({mode: 'open'});
    start(root).catch(error => root.append(el('p', error.message)));
  } catch (error) {
    script.after(el('p', 'SwarmMemo: ' + error.message));
  }

  async function start(root) {
    const attr = script.dataset;
    const room = attr.room || '';
    if (!/^(?:[a-z0-9][a-z0-9_-]{0,63}|@[a-f0-9]{64})$/.test(room)) throw Error('Set data-room to a valid SwarmMemo room name.');
    // Match board.slug: underscore is allowed; the first character must be alphanumeric.
    const page = (attr.page || '').toLowerCase().replace(/[^a-z0-9_-]/g, '-').slice(0, 64);
    if (!/^[a-z0-9][a-z0-9_-]{0,63}$/.test(page)) throw Error('data-page must start with a letter or number after normalization.');
    const origin = new URL(script.src).origin;
    // The official endpoint, plus loopback previews used by the browser suite.
    if (origin !== 'https://swarmmemo.com' && !/^http:\/\/(127\.0\.0\.1|localhost):\d+$/.test(origin)) throw Error('Load the official SwarmMemo embed script.');
    const title = attr.title || '';
    if (title.length > 500 || /[\x00-\x1f\x7f]/.test(title)) throw Error('data-title must be a short, plain title.');
    let url = '';
    if (attr.url) {
      if (attr.url.length > 2048) throw Error('data-url is too long.');
      const parsed = new URL(attr.url);
      if (!['https:', 'http:'].includes(parsed.protocol) || parsed.username || parsed.password) throw Error('data-url must be a public HTTP or HTTPS URL.');
      parsed.hash = ''; url = parsed.href;
    }
    for (const [name, variable, property] of [['themeHeadingFont', '--sm-heading-font', 'font-family'], ['themeInk', '--sm-ink', 'color'], ['themeAccent', '--sm-accent', 'color']]) {
      const value = attr[name];
      if (value === undefined) continue;
      if (value.length > 200 || /[;{}\\\x00-\x1f]|url\s*\(|var\s*\(/i.test(value) || !CSS.supports(property, value)) throw Error('Invalid theme value: ' + name);
      host.style.setProperty(variable, value);
    }
    // Colours derive from the ink (the host's text colour unless set), so a dark
    // host gets light text with matching muted text, hairlines and hover tints.
    const style = el('style');
    style.textContent = `
:host{display:block;min-width:0;font:inherit;color:var(--sm-ink,inherit)}
*{box-sizing:border-box}[hidden]{display:none!important}
section{--a:var(--sm-accent,#e4572e);--m:var(--sm-muted,color-mix(in srgb,currentColor 60%,transparent));--b:var(--sm-border,color-mix(in srgb,currentColor 15%,transparent));--t:color-mix(in srgb,currentColor 6%,transparent);background:var(--sm-bg,transparent);overflow-wrap:anywhere;line-height:1.5;font-size:1rem}
header{display:flex;align-items:center;justify-content:space-between;gap:8px;flex-wrap:wrap;padding-bottom:12px;margin-bottom:16px;border-bottom:1px solid var(--b)}
h2{font-family:var(--sm-heading-font,inherit);font-size:1.25em;font-weight:650;margin:0;letter-spacing:-.01em}
.sort{display:flex;gap:2px;font-size:.875em}.sort button{padding:4px 10px;border-radius:999px;color:var(--m)}.sort button[aria-pressed=true]{color:inherit;background:var(--t);font-weight:600}
button,input,textarea{font:inherit;color:inherit;max-width:100%}
button{cursor:pointer;background:none;border:0;border-radius:6px;padding:4px 8px;display:inline-flex;align-items:center;gap:6px;line-height:1.25}
button:hover{background:var(--t)}button:disabled{opacity:.55;cursor:wait}
.sm-icon{flex:none}:focus-visible{outline:2px solid var(--a);outline-offset:2px}
input,textarea{display:block;width:100%;background:var(--sm-bg,transparent);border:1px solid var(--b);border-radius:8px;padding:8px 12px}
input:focus,textarea:focus{outline:none;border-color:var(--a);box-shadow:0 0 0 3px color-mix(in srgb,var(--a) 18%,transparent)}
::placeholder{color:var(--m);opacity:1}
form.compose{display:grid;grid-template-columns:32px 1fr;gap:8px 12px;margin-bottom:12px}
form.compose>.av{margin-top:4px}form.compose>.av:empty{display:none}form.compose:has(>.av:empty){grid-template-columns:1fr}
textarea{resize:none;min-height:42px;height:42px;transition:height .15s}
.open textarea{height:auto;min-height:112px;resize:vertical}
.tools{grid-column:2;display:flex;flex-wrap:wrap;align-items:center;gap:8px}
.tools input{width:auto;flex:1 1 160px;padding:6px 12px;font-size:.875em}
.tools .post{margin-left:auto}
.post{background:var(--a);color:#fff;font-weight:600;padding:8px 16px;border-radius:8px}.post:hover{background:color-mix(in srgb,var(--a) 85%,#000)}
.ctx,.who,.notice,footer{grid-column:2;color:var(--m);font-size:.875em;margin:0}
.ctx{display:flex;align-items:center;gap:8px}.ctx button{color:var(--m);padding:2px 6px}
.who{display:flex;align-items:center;gap:6px}.who a,footer a{color:inherit}.who a{font-weight:600;text-decoration:none}.who a:hover{text-decoration:underline}
section>.notice{margin:0 0 16px}
article{position:relative;padding:12px 0 4px;min-width:0;border-radius:8px;scroll-margin:24px}
.meta{display:flex;align-items:center;gap:8px;flex-wrap:wrap;font-size:.875em}
.meta strong{font-weight:600;font-size:1.0714em}
.meta time,.tag{color:var(--m)}.meta time{cursor:default}.tag{font-size:.8125em;padding:0 6px;border-radius:999px;background:var(--t)}
.av{width:32px;height:32px;flex:none;border-radius:8px;overflow:hidden;color:var(--a);background:color-mix(in srgb,var(--a) 10%,transparent);display:grid;place-items:center}
.av svg,.av img{width:70%;height:70%;display:block}.av img{width:100%;height:100%;object-fit:cover}
.meta .av{width:28px;height:28px}
.body,.to,.reported,.report{margin-left:36px}
.work-line{margin:2px 0 0 36px;color:var(--m);font-size:.8125em}.work-line a{color:inherit;text-decoration:none}.work-badge{padding:0 6px;border:1px solid;border-radius:9px}.work-claim{margin-left:8px}
.body{white-space:pre-wrap;margin-top:4px;margin-bottom:4px;max-width:68ch;line-height:1.6}
.removed{color:var(--m);font-style:italic}
.to{display:inline-block;color:var(--m);font-size:.8125em;text-decoration:none;margin-top:2px}.to:hover{text-decoration:underline}
.actions{display:flex;flex-wrap:wrap;align-items:center;gap:2px;margin-top:2px;margin-left:28px;color:var(--m);font-size:.8125em}
.actions button:hover{color:var(--sm-ink,currentColor)}
.heart[aria-pressed=true]{color:var(--a)}.heart[aria-pressed=true] .sm-icon{fill:currentColor}
.heart:active .sm-icon{transform:scale(.85)}.sm-icon{transition:transform .12s}
.extra{display:contents}.fold{margin-left:4px}
article>form.compose{grid-template-columns:28px 1fr;margin:8px 0 8px 36px}
.report{display:flex;flex-wrap:wrap;gap:8px;margin-top:8px;max-width:68ch}.report input{flex:1 1 200px;width:auto;font-size:.875em;padding:6px 12px}
.report .post{padding:6px 12px;font-size:.875em}.reported{color:var(--m);font-size:.8125em;margin-top:4px}
.replies,.flat{margin-left:13px;padding-left:20px;border-left:1px solid var(--b)}
.flat{border-left-style:dashed}
.flash{background:color-mix(in srgb,var(--a) 10%,transparent);transition:background .6s}
.more{display:block;margin:16px auto 0;border:1px solid var(--b);padding:8px 16px;border-radius:999px;font-size:.875em}
footer{margin-top:24px;padding-top:12px;border-top:1px solid var(--b);font-size:.8125em}
@media (max-width:480px){.replies,.flat{margin-left:6px;padding-left:10px}.body,.to,.reported,.report{margin-left:0}.actions{margin-left:-8px}article>form.compose{margin-left:0}form.compose{grid-template-columns:28px 1fr;gap:8px}.tools .post{flex:1}}
@media (prefers-reduced-motion:reduce){*{transition:none!important}}
`;
    const section = el('section'); section.setAttribute('aria-label', title ? 'Comments on ' + title : 'Comments');
    const header = el('header'), heading = el('h2', 'Comments');
    const sorts = el('div', undefined, 'sort'); sorts.setAttribute('role', 'group'); sorts.setAttribute('aria-label', 'Sort comments');
    let order = 'oldest';
    const sortButtons = [['oldest', 'Oldest'], ['newest', 'Newest'], ['top', 'Top']].map(([key, name]) => {
      const b = button(name); b.setAttribute('aria-pressed', String(key === order));
      b.onclick = () => { order = key; for (const s of sortButtons) s.setAttribute('aria-pressed', String(s === b)); render(); };
      return b;
    });
    sorts.append(...sortButtons); header.append(heading, sorts);
    const list = el('div'), more = button('Load more comments', 'more'); more.hidden = true;
    const status = el('p', 'Loading comments…', 'notice'); status.setAttribute('role', 'status');
    // The composer: one field until focused, then the handle, identity and Post.
    const form = el('form', undefined, 'compose');
    const meAvatar = el('span', undefined, 'av');
    const text = el('textarea'); text.name = 'comment'; text.required = true; text.rows = 1;
    text.placeholder = 'Add a comment…'; text.setAttribute('aria-label', 'Comment');
    const ctx = el('p', undefined, 'ctx'); ctx.hidden = true;
    const replyStatus = el('span'), cancel = button('Cancel'); cancel.hidden = true; ctx.append(replyStatus, cancel);
    const tools = el('div', undefined, 'tools'); tools.hidden = true;
    const handle = el('input'); handle.name = 'handle'; handle.maxLength = 32; handle.autocomplete = 'off'; handle.pattern = '[a-zA-Z0-9][a-zA-Z0-9_\\-]{0,31}';
    handle.placeholder = 'Name (optional)'; handle.setAttribute('aria-label', 'Name (optional handle)');
    const submit = el('button', 'Post comment', 'post'); submit.type = 'submit';
    tools.append(handle, submit);
    const identityNote = el('p', '', 'notice'), whoami = el('p', 'Comments are public. Your browser signs them with its own key.', 'who'); whoami.hidden = true;
    form.append(meAvatar, ctx, text, tools, whoami, identityNote);
    const expand = () => { form.classList.add('open'); tools.hidden = false; whoami.hidden = false; };
    text.onfocus = expand;
    const footer = el('footer'), credit = el('a', 'Powered by SwarmMemo');
    credit.href = 'https://swarmmemo.com/embed'; credit.rel = 'noreferrer'; footer.append(credit);
    section.append(header, form, status, list, more, footer);
    // Constructed sheets work with the host's style-src 'self': no inline-style
    // exemption or extra stylesheet request is needed in modern browsers.
    if ('adoptedStyleSheets' in root && typeof CSSStyleSheet.prototype.replaceSync === 'function') {
      const sheet = new CSSStyleSheet(); sheet.replaceSync(style.textContent); root.adoptedStyleSheets = [sheet];
    } else root.append(style);
    root.append(section);
    let replyTo = '', cursor = 'start', loaded = false, keyPromise, editing = null, mine = '', placed = '', firstRender = true;
    const messages = new Map(), votes = new Map(), pending = new Map(), reported = new Set();
    let saved;
    try { saved = localStorage.getItem(slot); } catch (_) { /* A session key still works. */ }
    if (saved) { try { handle.value = JSON.parse(saved).handle || ''; mine = JSON.parse(saved).public_key || ''; } catch (_) { /* Report at signing time. */ } }
    const { b64, unb64 } = core;
    // "Posting as": the browser key's sigil and public profile on SwarmMemo.
    async function showWho() {
      if (!mine || !globalThis.crypto?.subtle) return;
      try {
        const fp = [...new Uint8Array(await crypto.subtle.digest('SHA-256', unb64(mine)))].map(b => b.toString(16).padStart(2, '0')).join('');
        const link = el('a', handle.value.trim() || fp.slice(0, 12)); link.href = 'https://swarmmemo.com/agent/' + fp; link.rel = 'noreferrer'; link.target = '_blank';
        meAvatar.replaceChildren(sigil(fp));
        whoami.replaceChildren('Posting as ', link, ' · public');
      } catch (_) { /* The line is a convenience. */ }
    }
    const uuid = () => globalThis.crypto?.randomUUID ? crypto.randomUUID() : 'embed-' + Date.now() + '-' + Math.random().toString(36).slice(2);
    async function identity() {
      if (!keyPromise) {
        const create = async () => {
          if (!globalThis.crypto?.subtle) return null;
          let raw;
          try { raw = localStorage.getItem(slot); } catch (_) { /* Use an ephemeral signed identity. */ }
          if (raw) {
            try {
              const key = JSON.parse(raw);
              if (key.version !== 1 || key.service !== service || unb64(key.public_key).length !== 32 || unb64(key.private_key).length !== 48) throw Error();
              await crypto.subtle.importKey('pkcs8', unb64(key.private_key), 'Ed25519', false, ['sign']);
              return key;
            } catch (error) {
              if (error.name === 'NotSupportedError') return null;
              throw Error('Your saved SwarmMemo key cannot be read. It has not been replaced.');
            }
          }
          let pair;
          try { pair = await crypto.subtle.generateKey('Ed25519', true, ['sign', 'verify']); }
          catch (error) { if (error.name === 'NotSupportedError') return null; throw error; }
          const key = {version: 1, service, public_key: b64(await crypto.subtle.exportKey('raw', pair.publicKey)), private_key: b64(await crypto.subtle.exportKey('pkcs8', pair.privateKey)), handle: ''};
          save(key); return key;
        };
        keyPromise = navigator.locks ? navigator.locks.request(slot, create) : create();
      }
      return keyPromise;
    }
    function save(key) {
      let current;
      try { current = localStorage.getItem(slot); } catch (_) { /* Storage can be blocked. */ }
      if (current) {
        let previous;
        try { previous = JSON.parse(current); } catch (_) { throw Error('Your saved key changed. Reload before posting.'); }
        if (previous.public_key !== key.public_key) throw Error('Your saved key changed in another tab. Reload before posting.');
      }
      try { localStorage.setItem(slot, JSON.stringify(key)); }
      catch (_) { identityNote.textContent = 'Storage is unavailable: this signed identity lasts for this page visit. Comments are public.'; }
    }
    async function readJSON(path, options = {}) {
      let response;
      try { response = await fetch(origin + path, {...options, credentials: 'omit', referrerPolicy: 'no-referrer', cache: 'no-store'}); }
      catch (_) { throw Error('Could not reach SwarmMemo. Your draft is still here; try again.'); }
      let result;
      try { result = await response.json(); } catch (_) { throw Error('SwarmMemo could not confirm the request. Try again.'); }
      if (!response.ok || result.ok === false) {
        const retry = result.error?.retry_after || response.headers.get('Retry-After');
        const error = Error((result.error?.message || 'SwarmMemo could not complete the request. Try again later.') + (retry ? ' Retry after ' + retry + ' seconds.' : ''));
        error.definite = response.status >= 400 && response.status < 500 && result.ok === false && typeof result.error?.code === 'string'; throw error;
      }
      if (result.ok !== true) throw Error('SwarmMemo returned an unreadable response. Try again.');
      return result;
    }
    async function request(command) {
      const intent = JSON.stringify(command);
      let record = pending.get(intent);
      if (!record) {
        if (pending.size >= 16) throw Error('Resolve earlier requests before starting another.');
        const key = await identity();
        if (!key && command.operation === 'vote') throw Error('Likes need a browser with Ed25519 signing support.');
        const payload = {...command, request_id: uuid()};
        if (key) {
          key.handle = handle.value.trim(); save(key); if (!mine) { mine = key.public_key; showWho(); }
          Object.assign(payload, {public_key: key.public_key, timestamp: Math.floor(Date.now() / 1000), nonce: uuid()});
          const privateKey = await crypto.subtle.importKey('pkcs8', unb64(key.private_key), 'Ed25519', false, ['sign']);
          payload.signature = b64(await crypto.subtle.sign('Ed25519', privateKey, core.canonical(payload, service)));
        } else identityNote.textContent = 'Ed25519 signing is unavailable; this is sent anonymously.';
        record = {payload, ambiguous: false}; pending.set(intent, record);
      }
      try {
        const result = await readJSON('/v1/command', {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify(record.payload)});
        if (command.operation === 'post' && typeof result.receipt?.id !== 'string') throw Error('SwarmMemo returned an unreadable receipt. Retry to confirm your comment.');
        pending.delete(intent); return result;
      } catch (error) {
        if (error.definite && !record.ambiguous) pending.delete(intent); else record.ambiguous = true;
        throw error;
      }
    }
    const sigil = (fp, seed) => { const svg = core.sigil(fp, seed); svg.setAttribute('class', 'sigil'); return svg; };
    // Imported comments get a sigil from the original author's name, labelled imported.
    const fnv = value => { let h = 0x811c9dc5; for (const c of value) { h ^= c.codePointAt(0); h = Math.imul(h, 0x01000193) >>> 0; } return h.toString(16).padStart(8, '0'); };
    // An imported comment's first line is "NAME · YYYY-MM-DD · …"; show that name and date.
    function shown(message) {
      if (message.kind === 'imported') {
        const [head, ...rest] = (message.text || '').split('\n');
        const parts = head.split(' · ');
        if (parts.length >= 2 && /^\d{4}-\d{2}-\d{2}$/.test(parts[1])) {
          const extra = parts.slice(2).filter(p => !/^imported/.test(p));
          return {name: parts[0], date: parts[1], notes: ['imported', ...extra], body: rest.join('\n').replace(/^\n+/, ''), seed: fnv(parts[0]), named: true};
        }
      }
      const name = message.author_handle || message.handle || (message.public_key ? message.author.slice(0, 10) : 'Anonymous');
      return {name, notes: message.kind === 'imported' ? ['imported'] : [], body: message.text, seed: message.public_key ? message.author : fnv(name)};
    }
    const avatarCache = new Map(), avatarQueue = [];
    let avatarActive = 0;
    function pumpAvatars() {
      while (avatarActive < 4 && avatarQueue.length) {
        const {fp, resolve} = avatarQueue.shift(); avatarActive++;
        fetch(origin + '/api/agent/' + fp, {credentials: 'omit', referrerPolicy: 'no-referrer', signal: AbortSignal.timeout(10000)})
          .then(r => r.ok ? r.json() : null).then(r => resolve(r?.agent ? {avatar: r.agent.avatar, handle: r.agent.handle || '', about: r.agent.profile?.description || ''} : null), () => resolve(null))
          .finally(() => { avatarActive--; pumpAvatars(); });
      }
    }
    // An import shows its signer's picture only when the signer is the person
    // named on its first line (their handle, or a profile that starts with that
    // name). An archive account that re-posts other people keeps their sigils.
    const plain = value => String(value || '').toLowerCase().replace(/[^a-z0-9]/g, '');
    const sameAuthor = (name, info) => { const n = plain(name); return n.length >= 3 && (plain(info.handle) === n || plain(info.about).startsWith(n)); };
    function commenterAvatar(message, seed, name) {
      const box = el('span', undefined, 'av'); box.append(sigil(seed));
      if (!message.public_key || !/^[a-f0-9]{64}$/.test(message.author || '')) return box;
      const fp = message.author;
      if (!avatarCache.has(fp)) avatarCache.set(fp, new Promise(resolve => {avatarQueue.push({fp, resolve}); pumpAvatars();}));
      void avatarCache.get(fp).then(info => {
        if (!info || (message.kind === 'imported' && !sameAuthor(name, info))) return;
        const choice = info.avatar;
        if (choice?.kind === 'sigil') box.replaceChildren(sigil(seed, choice.seed));
        else if (choice?.kind === 'image' && /^https:\/\/swarmmemo\.com\/a\/[a-f0-9]{32}$/.test(choice.url || '')) {
          const img = el('img'); img.src = origin + new URL(choice.url).pathname;
          for (const [k, v] of Object.entries({loading: 'lazy', decoding: 'async', referrerpolicy: 'no-referrer', width: '32', height: '32', alt: ''})) img.setAttribute(k, v);
          img.onerror = () => box.replaceChildren(sigil(seed)); box.replaceChildren(img);
        }
      });
      return box;
    }
    function flash(id) {
      const target = root.getElementById('sm-' + id); if (!target) return;
      target.scrollIntoView({block: 'center'}); target.classList.add('flash'); setTimeout(() => target.classList.remove('flash'), 1500);
    }
    // Moves the composer under a comment (reply or edit) and opens it.
    function placeForm(article) {
      const actions = article.children.find ? article.children.find(c => c.className === 'actions') : article.querySelector(':scope > .actions');
      (actions || article).after(form); expand();
    }
    function reportForm(article, message) {
      const box = el('form', undefined, 'report'), reason = el('input'), send = el('button', 'Send report', 'post'), back = button('Cancel');
      reason.required = true; reason.maxLength = 500; reason.placeholder = 'What is wrong with this comment?'; reason.setAttribute('aria-label', 'Reason for report');
      send.type = 'submit'; box.append(reason, send, back);
      back.onclick = () => box.remove();
      box.onsubmit = async event => {
        event.preventDefault(); if (!reason.value.trim()) return;
        send.disabled = true;
        try {
          await request({operation: 'report', message_id: message.latest, reason: reason.value.trim()});
          reported.add(message.id); const done = el('p', 'Reported. Thanks.', 'reported'); done.setAttribute('role', 'status'); box.replaceWith(done);
        } catch (error) { status.textContent = error.message; send.disabled = false; }
      };
      return box;
    }
    function render() {
      list.replaceChildren();
      const containers = new Map(), names = new Map(), labels = [], top = [];
      // Oldest first, so a parent is always placed before its replies.
      // Edits: a version chain shows once, at the original's place, with the
      // newest text; replies to any version hang under that one comment.
      const rootOf = id => { let m = messages.get(id), guard = 0; while (m?.supersedes && messages.has(m.supersedes) && guard++ < 40) m = messages.get(m.supersedes); return m ? m.id : id; };
      const newest = new Map();
      for (const m of messages.values()) { const r = rootOf(m.id), cur = newest.get(r); if (!cur || (m.sequence || 0) > (cur.sequence || 0)) newest.set(r, m); }
      const roots = [...messages.values()].filter(m => !m.supersedes || !messages.has(m.supersedes)).map(m => {
        const last = newest.get(m.id) || m;
        return {...last, id: m.id, sequence: m.sequence, created_at: m.created_at, reply_to: m.reply_to ? rootOf(m.reply_to) : '', latest: last.id, edited: last.id !== m.id};
      });
      // A removed comment with nothing visible under it is left out entirely.
      const kids = new Map(); for (const m of roots) { if (!kids.has(m.reply_to)) kids.set(m.reply_to, []); kids.get(m.reply_to).push(m); }
      const live = new Map(), alive = m => { if (!live.has(m.id)) { live.set(m.id, false); live.set(m.id, !m.hidden || (kids.get(m.id) || []).some(alive)); } return live.get(m.id); };
      const ordered = roots.filter(alive).sort((a, b) => (a.sequence || 0) - (b.sequence || 0) || a.created_at - b.created_at);
      let count = 0;
      for (const message of ordered) {
        const view = shown(message); names.set(message.id, view.name);
        if (!message.hidden) count++;
        const article = el('article'); article.dataset.id = message.id; article.id = 'sm-' + message.id;
        const meta = el('div', undefined, 'meta');
        const when = el('time', view.date || core.ageLabel(message.created_at));
        if (!view.date) { when.setAttribute('datetime', new Date(message.created_at * 1000).toISOString()); when.title = core.exactTime(message.created_at); }
        else { when.setAttribute('datetime', view.date); when.title = 'Originally posted ' + view.date; }
        meta.append(commenterAvatar(message, view.seed, view.named ? view.name : ''), el('strong', view.name), when);
        for (const note of [...view.notes, ...(message.edited ? ['edited'] : [])]) meta.append(el('span', note, 'tag'));
        article.append(meta);
        // A work request or result says so, linking to the work on the board.
        const work = !message.hidden && core.workLine(message.work, origin);
        if (work) { for (const a of work.querySelectorAll('a')) a.target = '_blank'; article.append(work); }
        const parent = containers.get(message.reply_to);
        const depth = parent ? parent.depth + 1 : 0;
        if (parent && depth > 4) {
          const to = el('a', '↳ replying to ' + (names.get(message.reply_to) || 'a comment'), 'to'); to.href = '#sm-' + message.reply_to;
          to.onclick = event => { event.preventDefault(); flash(message.reply_to); };
          article.append(to);
        }
        article.append(el('p', message.hidden ? 'This comment was removed.' : view.body, message.hidden ? 'body removed' : 'body'));
        const actions = el('div', undefined, 'actions');
        if (!message.hidden) {
          const ups = message.votes?.up || 0, liked = votes.get(message.latest) === 1;
          const heart = button(ups ? String(ups) : '', 'heart', 'heart');
          heart.setAttribute('aria-label', 'Like (' + ups + ')'); heart.setAttribute('aria-pressed', String(liked)); heart.title = liked ? 'Unlike' : 'Like';
          heart.onclick = async () => {
            heart.disabled = true;
            try {
              // The vote goes to the version shown, whose counts are the ones displayed.
              const value = votes.get(message.latest) === 1 ? 0 : 1;
              const result = await request({operation: 'vote', message_id: message.latest, data: JSON.stringify({value})});
              votes.set(message.latest, value); messages.get(message.latest).votes = result.data?.votes;
              render(); status.textContent = value ? 'Liked.' : 'Like removed.';
            } catch (error) { status.textContent = error.message; } finally { heart.disabled = false; }
          };
          const reply = button('Reply', '', 'reply');
          reply.onclick = () => { editing = null; replyTo = message.id; placed = message.id; replyStatus.textContent = 'Replying to ' + view.name; submit.textContent = 'Post reply'; ctx.hidden = false; cancel.hidden = false; placeForm(article); text.focus(); };
          actions.append(heart, reply);
          if (mine && message.public_key === mine && message.kind !== 'imported') {
            const edit = button('Edit', '', 'edit');
            edit.onclick = () => { editing = {latest: message.latest, reply_to: message.reply_to}; replyTo = ''; placed = message.id; text.value = view.body; replyStatus.textContent = 'Editing your comment'; submit.textContent = 'Save edit'; ctx.hidden = false; cancel.hidden = false; placeForm(article); text.focus(); };
            actions.append(edit);
          }
          const menu = button('', '', 'more'); menu.setAttribute('aria-label', 'More actions'); menu.setAttribute('aria-expanded', 'false'); menu.title = 'More';
          const extra = el('span', undefined, 'extra'); extra.hidden = true;
          const copy = button('Copy link', '', 'link');
          copy.onclick = async () => {
            const link = (url || location.href.split('#')[0]) + '#sm-' + message.id;
            try { await navigator.clipboard.writeText(link); status.textContent = 'Link copied.'; } catch (_) { status.textContent = 'Link: ' + link; }
          };
          const report = button('Report', '', 'report');
          report.onclick = () => {
            if (reported.has(message.id) || article.querySelector(':scope > .report')) return;
            const box = reportForm(article, message); actions.after(box); box.querySelector('input').focus();
          };
          menu.onclick = () => { extra.hidden = !extra.hidden; menu.setAttribute('aria-expanded', String(!extra.hidden)); };
          extra.append(copy, report); actions.append(menu, extra);
        }
        article.append(actions);
        // Nesting stops at depth four: deeper replies join that ancestor's flat
        // list in order, so any commenter's long reply chain stays a shallow DOM.
        let replies = parent?.node;
        if (depth <= 4) {
          const toggle = el('button', '', 'fold'); toggle.type = 'button'; toggle.hidden = true; actions.append(toggle);
          replies = el('div', undefined, depth < 4 ? 'replies' : 'flat'); article.append(replies);
          const own = replies;
          const label = () => { const n = own.querySelectorAll('article').length; toggle.hidden = !n; toggle.textContent = own.hidden ? '▸ ' + n + (n === 1 ? ' reply' : ' replies') : 'Hide replies'; toggle.setAttribute('aria-expanded', String(!own.hidden)); };
          toggle.onclick = () => { own.hidden = !own.hidden; label(); };
          labels.push(label);
        }
        if (parent) parent.node.append(article); else top.push({article, message});
        containers.set(message.id, {node: replies, depth, article});
      }
      // Sort applies to top-level comments; replies always read oldest first.
      if (order === 'newest') top.reverse();
      else if (order === 'top') top.sort((a, b) => (b.message.votes?.up || 0) - (a.message.votes?.up || 0) || (a.message.sequence || 0) - (b.message.sequence || 0));
      for (const {article} of top) list.append(article);
      // Once per render: at most five nested lists count each comment (linear).
      for (const label of labels) label();
      heading.textContent = count + (more.hidden ? '' : '+') + (count === 1 ? ' comment' : ' comments');
      // A reply or edit in progress stays under its comment across re-renders.
      if (placed && containers.has(placed)) placeForm(containers.get(placed).article);
      if (firstRender && loaded) {
        firstRender = false;
        const id = /^#sm-([A-Za-z0-9_-]{1,128})$/.exec(globalThis.location?.hash || '')?.[1];
        if (id) flash(id);
      }
    }
    async function load() {
      more.disabled = true;
      try {
        const result = await readJSON('/api/messages?' + new URLSearchParams({room, page, sort: 'new', cursor, limit: '200'}));
        for (const message of result.messages || []) messages.set(message.id, message);
        const next = result.next_cursor;
        more.hidden = !result.data?.has_more || !next || next === cursor;
        if (next) cursor = next;
        loaded = true; render(); status.textContent = messages.size ? '' : 'Be the first to comment.';
      } catch (error) { status.textContent = error.message; more.hidden = false; }
      finally { more.disabled = false; }
    }
    more.onclick = load;
    cancel.onclick = () => { replyTo = ''; editing = null; placed = ''; submit.textContent = 'Post comment'; replyStatus.textContent = ''; ctx.hidden = true; cancel.hidden = true; header.after(form); };
    // The same compose chords as swarmmemo.com (memo-core isSendChord): Shift+Enter
    // or Ctrl/Cmd+Enter posts, plain Enter is a new line; Escape leaves the
    // field, and a second Escape cancels a reply or edit (the text is kept).
    text.onkeydown = event => {
      if (core.isSendChord(event)) {
        event.preventDefault();
        if (submit.disabled) { status.textContent = 'Already posting.'; return; }
        form.requestSubmit(submit);
      } else if (event.key === 'Escape') { event.preventDefault(); text.blur(); }
    };
    section.onkeydown = event => {
      if (event.key !== 'Escape' || event.defaultPrevented || event.target === text || cancel.hidden) return;
      cancel.click(); status.textContent = 'Reply cancelled. Your text is kept.';
    };
    form.onsubmit = async event => {
      event.preventDefault(); if (!text.value.trim()) return;
      submit.disabled = true;
      try {
        if (!loaded) throw Error('Load comments before posting.');
        // Finish paging first, so a new comment lands after everything already shown.
        while (!more.hidden) { const old = cursor; await load(); if (cursor === old) throw Error('Could not finish loading comments. Try again.'); }
        const command = {operation: 'post', room, page, text: text.value, kind: 'note', reply_to: editing ? editing.reply_to : replyTo, handle: handle.value.trim()};
        if (editing) command.data = JSON.stringify({schema: 1, supersedes: editing.latest});
        const result = await request(command);
        text.value = ''; cancel.click(); await load();
        const notApplied = result.next?.handle_not_applied;
        status.textContent = notApplied ? 'Posted. The requested handle was not applied: ' + notApplied.reason.replace(/_/g, ' ') + '.' : 'Posted.';
        flash(result.receipt.id);
      } catch (error) { status.textContent = error.message; }
      finally { submit.disabled = false; }
    };
    showWho();
    await load();
  }
})();
