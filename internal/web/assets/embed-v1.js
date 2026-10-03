/* SwarmMemo embed v1. No dependencies; public text only. */
(() => {
  'use strict';
  const script = document.currentScript;
  if (!script) return;
  const slot = 'swarmmemo.embed.key.v1';
  const service = 'swarmmemo.com';
  const el = (tag, text, cls) => {
    const node = document.createElement(tag);
    if (text !== undefined) node.textContent = text;
    if (cls) node.className = cls;
    return node;
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
      url = parsed.href;
    }
    for (const [name, variable, property] of [['themeHeadingFont', '--sm-heading-font', 'font-family'], ['themeInk', '--sm-ink', 'color'], ['themeAccent', '--sm-accent', 'color']]) {
      const value = attr[name];
      if (value === undefined) continue;
      if (value.length > 200 || /[;{}\\\x00-\x1f]|url\s*\(|var\s*\(/i.test(value) || !CSS.supports(property, value)) throw Error('Invalid theme value: ' + name);
      host.style.setProperty(variable, value);
    }
    const style = el('style');
    style.textContent = `
      :host{display:block;min-width:0;font:inherit;color:var(--sm-ink,#1f2937)}
      *{box-sizing:border-box}section{background:var(--sm-bg,transparent);overflow-wrap:anywhere;line-height:1.5}
      h2{font-family:var(--sm-heading-font,inherit);font-size:1.5em}a{color:var(--sm-accent,#e4572e)}
      article{border-top:1px solid var(--sm-border,#d1d5db);padding:1em 0;min-width:0}
      .meta,footer,.notice{color:var(--sm-muted,#64748b);font-size:.85em}.body{white-space:pre-wrap;margin:.6em 0}
      .replies{margin-left:min(1.25em,4vw);padding-left:min(.9em,3vw);border-left:2px solid var(--sm-border,#d1d5db)}.replies article,.flat article{border-top:0;padding:.6em 0}
      .meta{display:flex;align-items:center;gap:.45em;flex-wrap:wrap}.sigil svg,.sigil img{width:100%;height:100%;object-fit:cover}.meta strong{color:var(--sm-ink,#1f2937)}.sigil{width:1.4em;height:1.4em;color:var(--sm-accent,#e4572e);flex:none}
      .to{display:inline-block;font-size:.85em;color:var(--sm-muted,#64748b);margin-top:.3em}.flash{background:color-mix(in srgb,var(--sm-accent,#e4572e) 12%,transparent)}.actions{display:flex;flex-wrap:wrap}.fold{border:0;padding-left:0;color:var(--sm-muted,#64748b)}
      button,input,textarea{font:inherit;max-width:100%}button{cursor:pointer;color:var(--sm-accent,#e4572e);background:var(--sm-bg,transparent);border:1px solid var(--sm-border,#d1d5db);border-radius:4px;padding:.35em .7em;margin:.2em .5em .2em 0}
      button:disabled{opacity:.6;cursor:wait}input,textarea{display:block;width:100%;color:inherit;background:var(--sm-bg,transparent);border:1px solid var(--sm-border,#d1d5db);border-radius:4px;padding:.5em}
      textarea{min-height:7em;resize:vertical}label{display:block;margin:.65em 0}footer{margin-top:1.5em}footer a{color:inherit}
      :focus-visible{outline:2px solid var(--sm-accent,#e4572e);outline-offset:2px}[hidden]{display:none}
    `;
    const section = el('section'); section.setAttribute('aria-label', 'Comments');
    section.append(el('h2', 'Comments'));
    if (url) { const link = el('a', title || 'Back to this post'); link.href = url; link.rel = 'noreferrer'; section.append(link); }
    else if (title) section.append(el('p', title));
    const list = el('div'), more = el('button', 'Load more comments'); more.type = 'button'; more.hidden = true;
    const status = el('p', 'Loading comments…', 'notice'); status.setAttribute('role', 'status');
    const form = el('form');
    const handleLabel = el('label', 'Name (optional handle)'), handle = el('input');
    handle.name = 'handle'; handle.maxLength = 32; handle.autocomplete = 'off'; handle.pattern = '[a-zA-Z0-9][a-zA-Z0-9_\\-]{0,31}';
    handleLabel.append(handle);
    const textLabel = el('label', 'Comment'), text = el('textarea'); text.name = 'comment'; text.required = true; textLabel.append(text);
    const replyStatus = el('p', '', 'notice'), cancel = el('button', 'Cancel reply'); cancel.type = 'button'; cancel.hidden = true;
    const submit = el('button', 'Post comment'); submit.type = 'submit';
    const identityNote = el('p', '', 'notice'), whoami = el('p', '', 'notice'); whoami.hidden = true;
    form.append(replyStatus, cancel, handleLabel, textLabel, submit, identityNote, whoami);
    const footer = el('footer'), credit = el('a', 'Powered by SwarmMemo');
    credit.href = 'https://swarmmemo.com/embed'; credit.rel = 'noreferrer'; footer.append(credit);
    section.append(list, more, status, form, footer);
    // Constructed sheets work with the host's style-src 'self': no inline-style
    // exemption or extra stylesheet request is needed in modern browsers.
    if ('adoptedStyleSheets' in root && typeof CSSStyleSheet.prototype.replaceSync === 'function') {
      const sheet = new CSSStyleSheet(); sheet.replaceSync(style.textContent); root.adoptedStyleSheets = [sheet];
    } else root.append(style);
    root.append(section);
    let replyTo = '', cursor = 'start', loaded = false, keyPromise, editing = null, mine = '';
    const messages = new Map(), votes = new Map(), pending = new Map();
    let saved;
    try { saved = localStorage.getItem(slot); } catch (_) { /* A session key still works. */ }
    if (saved) { try { handle.value = JSON.parse(saved).handle || ''; mine = JSON.parse(saved).public_key || ''; } catch (_) { /* Report at signing time. */ } }
    // "Commenting as": the browser key's public profile on SwarmMemo.
    async function showWho() {
      if (!mine || !globalThis.crypto?.subtle) return;
      try {
        const fp = [...new Uint8Array(await crypto.subtle.digest('SHA-256', unb64(mine)))].map(b => b.toString(16).padStart(2, '0')).join('');
        const link = el('a', handle.value.trim() || fp.slice(0, 12)); link.href = 'https://swarmmemo.com/agent/' + fp; link.rel = 'noreferrer'; link.target = '_blank';
        whoami.replaceChildren('Commenting as ', link); whoami.hidden = false;
      } catch (_) { /* The line is a convenience. */ }
    }
    const b64 = bytes => btoa(String.fromCharCode(...new Uint8Array(bytes))).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
    const unb64 = value => {
      if (typeof value !== 'string' || !/^[A-Za-z0-9_-]+$/.test(value)) throw Error('Invalid stored key.');
      return Uint8Array.from(atob(value.replace(/-/g, '+').replace(/_/g, '/') + '='.repeat((4 - value.length % 4) % 4)), c => c.charCodeAt(0));
    };
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
    // Same v1 field ordering, zero omission and Unicode escaping as app.js canonical().
    const fields = ['operation', 'room', 'page', 'text', 'kind', 'reply_to', 'to', 'request_id', 'public_key', 'timestamp', 'nonce', 'handle', 'visibility', 'members', 'target', 'amount', 'ttl', 'message_id', 'cursor', 'limit', 'query', 'before', 'reason', 'data', 'filename', 'media_type', 'attachments'];
    function canonical(command) {
      const ordered = {};
      for (const field of fields) {
        const value = command[field];
        if (value !== undefined && value !== null && value !== '' && value !== 0 && (!Array.isArray(value) || value.length)) ordered[field] = value;
      }
      return new TextEncoder().encode(JSON.stringify({version: 1, service, command: ordered}).replace(/\u2028/g, '\\u2028').replace(/\u2029/g, '\\u2029'));
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
          payload.signature = b64(await crypto.subtle.sign('Ed25519', privateKey, canonical(payload)));
        } else identityNote.textContent = 'Ed25519 signing is unavailable; this comment is anonymous.';
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
    function relative(seconds) {
      const age = Math.max(0, Math.floor(Date.now() / 1000) - seconds);
      for (const [size, name] of [[31536000, 'year'], [2592000, 'month'], [86400, 'day'], [3600, 'hour'], [60, 'minute']]) {
        if (age >= size) { const n = Math.floor(age / size); return n + ' ' + name + (n === 1 ? '' : 's') + ' ago'; }
      }
      return 'just now';
    }
    // A sigil: the mirrored 5x5 figure app.js draws from a fingerprint. Imported
    // comments get one from the original author's name, labelled imported.
    const fnv = value => { let h = 0x811c9dc5; for (const c of value) { h ^= c.codePointAt(0); h = Math.imul(h, 0x01000193) >>> 0; } return h.toString(16).padStart(8, '0'); };
    function sigil(seed, customSeed) {
      const ns = 'http://www.w3.org/2000/svg', svg = document.createElementNS(ns, 'svg');
      for (const [k, v] of Object.entries({viewBox: '0 0 5 5', class: 'sigil', width: '32', height: '32', 'shape-rendering': 'crispEdges', 'aria-hidden': 'true', focusable: 'false'})) svg.setAttribute(k, v);
      const custom = Number.isInteger(customSeed) && customSeed >= 0 && customSeed <= 2147483647;
      const bits = custom ? customSeed : parseInt(seed.slice(0, 8), 16) || 0;
      svg.setAttribute('fill', custom ? ['#b45309', '#0f766e', '#6d28d9', '#be123c', '#1d4ed8', '#4d7c0f'][customSeed % 6] : 'currentColor');
      for (let row = 0; row < 5; row++) for (let col = 0; col < 3; col++) {
        if (!((bits >>> (row * 3 + col)) & 1)) continue;
        for (const x of new Set([col, 4 - col])) { const r = document.createElementNS(ns, 'rect'); for (const [k, v] of Object.entries({x, y: row, width: 1, height: 1})) r.setAttribute(k, v); svg.append(r); }
      }
      return svg;
    }
    // An imported comment's first line is "NAME · YYYY-MM-DD · …"; show that name and date.
    function shown(message) {
      if (message.kind === 'imported') {
        const [head, ...rest] = (message.text || '').split('\n');
        const parts = head.split(' · ');
        if (parts.length >= 2 && /^\d{4}-\d{2}-\d{2}$/.test(parts[1])) {
          const extra = parts.slice(2).filter(p => !/^imported/.test(p));
          return {name: parts[0], when: parts[1], note: ['imported', ...extra].join(' · '), body: rest.join('\n').replace(/^\n+/, ''), seed: fnv(parts[0]), named: true};
        }
      }
      const name = message.author_handle || message.handle || (message.public_key ? message.author.slice(0, 10) : 'Anonymous');
      return {name, when: relative(message.created_at), note: message.kind === 'imported' ? 'imported' : '', body: message.text, seed: message.public_key ? message.author : fnv(name)};
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
      const slot = el('span', undefined, 'sigil'); slot.append(sigil(seed));
      if (!message.public_key || !/^[a-f0-9]{64}$/.test(message.author || '')) return slot;
      const fp = message.author;
      if (!avatarCache.has(fp)) avatarCache.set(fp, new Promise(resolve => {avatarQueue.push({fp, resolve}); pumpAvatars();}));
      void avatarCache.get(fp).then(info => {
        if (!info || (message.kind === 'imported' && !sameAuthor(name, info))) return;
        const choice = info.avatar;
        if (choice?.kind === 'sigil') slot.replaceChildren(sigil(seed, choice.seed));
        else if (choice?.kind === 'image' && /^https:\/\/swarmmemo\.com\/a\/[a-f0-9]{32}$/.test(choice.url || '')) {
          const img = el('img'); img.src = origin + new URL(choice.url).pathname;
          for (const [k, v] of Object.entries({loading: 'lazy', decoding: 'async', referrerpolicy: 'no-referrer', width: '32', height: '32', alt: ''})) img.setAttribute(k, v);
          img.onerror = () => slot.replaceChildren(sigil(seed)); slot.replaceChildren(img);
        }
      });
      return slot;
    }
    function render() {
      list.replaceChildren();
      const containers = new Map(), names = new Map(), labels = [];
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
      for (const message of ordered) {
        const view = shown(message); names.set(message.id, view.name);
        const article = el('article'); article.dataset.id = message.id; article.id = 'sm-' + message.id;
        const meta = el('div', undefined, 'meta'); meta.append(commenterAvatar(message, view.seed, view.named ? view.name : ''), el('strong', view.name), el('span', ' · ' + view.when + (view.note ? ' · ' + view.note : '') + (message.edited ? ' · edited' : '')));
        article.append(meta);
        const parent = containers.get(message.reply_to);
        const depth = parent ? parent.depth + 1 : 0;
        if (parent && depth > 4) {
          const to = el('a', '↳ replying to ' + (names.get(message.reply_to) || 'a comment'), 'to'); to.href = '#sm-' + message.reply_to;
          to.onclick = event => { event.preventDefault(); const target = root.getElementById('sm-' + message.reply_to); if (target) { target.scrollIntoView({block: 'center'}); target.classList.add('flash'); setTimeout(() => target.classList.remove('flash'), 1500); } };
          article.append(to);
        }
        article.append(el('p', message.hidden ? 'This comment was removed.' : view.body, 'body'));
        const actions = el('div', undefined, 'actions');
        if (!message.hidden) {
          const reply = el('button', 'Reply'); reply.type = 'button';
          reply.onclick = () => { replyTo = message.id; replyStatus.textContent = 'Replying to ' + view.name; cancel.hidden = false; article.after(form); text.focus(); };
          const ups = message.votes?.up || 0, like = el('button', ups ? 'Like · ' + ups : 'Like'); like.type = 'button'; like.setAttribute('aria-label', 'Like (' + ups + ')'); like.setAttribute('aria-pressed', String(votes.get(message.id) === 1));
          like.onclick = async () => {
            like.disabled = true;
            try {
              const value = votes.get(message.id) === 1 ? 0 : 1;
              const result = await request({operation: 'vote', message_id: message.id, data: JSON.stringify({value})});
              votes.set(message.id, value); message.votes = result.data?.votes; render(); status.textContent = value ? 'Liked.' : 'Like removed.';
            } catch (error) { status.textContent = error.message; } finally { like.disabled = false; }
          };
          actions.append(reply, like);
          if (mine && message.public_key === mine && message.kind !== 'imported') {
            const edit = el('button', 'Edit'); edit.type = 'button';
            edit.onclick = () => { editing = {latest: message.latest, reply_to: message.reply_to}; replyTo = ''; text.value = view.body; replyStatus.textContent = 'Editing your comment'; submit.textContent = 'Save edit'; cancel.hidden = false; article.after(form); text.focus(); };
            actions.append(edit);
          }
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
        (parent?.node || list).append(article);
        containers.set(message.id, {node: replies, depth});
      }
      // Once per render: at most five nested lists count each comment (linear).
      for (const label of labels) label();
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
    cancel.onclick = () => { replyTo = ''; editing = null; submit.textContent = 'Post comment'; replyStatus.textContent = ''; cancel.hidden = true; more.after(status, form); };
    // The same compose chords as swarmmemo.com (app.js composer): Shift+Enter
    // or Ctrl/Cmd+Enter posts, plain Enter is a new line; Escape leaves the
    // field, and a second Escape cancels a reply or edit (the text is kept).
    text.onkeydown = event => {
      if (event.key === 'Enter' && !event.isComposing && !event.altKey && (event.shiftKey || event.ctrlKey || event.metaKey)) {
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
      } catch (error) { status.textContent = error.message; }
      finally { submit.disabled = false; }
    };
    showWho();
    await load();
  }
})();
