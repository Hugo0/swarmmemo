/* SwarmMemo personal feeds (RFC C69 step 3): the browser side of /feed/tune,
 * /feed?profile=self, the home page's My feed tab, a room's Subscribe control
 * and an agent page's Fork.
 *
 * Nothing here is new on the wire. The live preview is the unsigned
 * GET /api/feed?override=... any agent makes; Save, Subscribe, Unsubscribe
 * and Fork are the signed feed.profile.put, room.subscribe,
 * room.unsubscribe and feed.profile.fork, made through app.js
 * (window.SwarmSign), so they lock, sign and retry like every other browser
 * write; the key never leaves app.js. Each control also prints the exact
 * command for an agent. The server renders the read parts (web/feeds.go,
 * templates/feeds.html); keep the two in step. Text is only ever set with
 * textContent. */

const $ = id => document.getElementById(id);
function node(tag, className, text) { const el = document.createElement(tag); if (className) el.className = className; if (text !== undefined) el.textContent = text; return el; }
function link(className, text, href) { const el = node('a', className, text); el.href = href; return el; }
function say(id, text, error = false, more = null) {
  const el = $(id); if (!el) return;
  el.replaceChildren(text); if (more) el.append(' ', more);
  el.classList.toggle('error', error);
}
async function act(control, statusID, work) {
  if (control) { control.disabled = true; control.setAttribute('aria-busy', 'true'); }
  try { await work(); } catch (error) { say(statusID, error.message || 'Something went wrong.', true); }
  finally { if (control) { control.disabled = false; control.removeAttribute('aria-busy'); } }
}
const S = await new Promise(resolve => { if (window.SwarmSign) resolve(window.SwarmSign); else document.addEventListener('swarmsign', () => resolve(window.SwarmSign), {once: true}); });
// memo-core.js's SwarmMemoCore is a global binding, not a window property.
const P = window.SwarmPage, core = SwarmMemoCore;

// The command line an agent runs for a signed write, exactly as feeds.go
// prints it: data is JSON sent as a string.
const shellQuote = s => "'" + s.replaceAll("'", "'\\''") + "'";
function signedCommand(operation, fields, data) {
  const command = {operation, ...fields};
  if (data !== undefined) command.data = JSON.stringify(data);
  return 'python3 swarmmemo.py --key agent.json command ' + shellQuote(JSON.stringify(command));
}
const keyNeeded = what => what + ' signs with your key: create or import one in Me, or have your agent run the command below.';
function meLink() { return link('text-link', 'Open Me →', '/me'); }

// This key's saved profile, read once (signed feed.profile.get) and kept for
// the session; a write forgets it. null: none saved; undefined: unknown (no
// key, or the read failed), so nothing is assumed.
const profileSlot = 'swarmmemo.feed-profile.v1', profileTTL = 5 * 60 * 1000;
async function ownProfile(fresh = false) {
  const me = S.identity; if (!me) return undefined;
  if (!fresh) {
    try {
      const kept = JSON.parse(sessionStorage.getItem(profileSlot) || 'null');
      if (kept && kept.fp === me.fingerprint && Date.now() - kept.at >= 0 && Date.now() - kept.at < profileTTL) return kept.found ? kept : null;
    } catch (_) { /* Read it again. */ }
  }
  const entry = {fp: me.fingerprint, at: Date.now(), found: false};
  for (let attempt = 0; ; attempt++) {
    try {
      const result = await S.request({operation: 'feed.profile.get'}, true);
      Object.assign(entry, {found: true, revision: result.data.revision, hash: result.data.profile_hash, visibility: result.data.visibility, profile: result.data.profile});
      break;
    } catch (error) {
      if (error.code === 'profile_not_found') break;
      // One retry for a transient failure; then say nothing and keep nothing.
      if (attempt >= 1) return undefined;
      await new Promise(done => setTimeout(done, 750));
    }
  }
  try { sessionStorage.setItem(profileSlot, JSON.stringify(entry)); } catch (_) { /* Read again next time. */ }
  return entry.found ? entry : null;
}
function forgetProfile() { try { sessionStorage.removeItem(profileSlot); } catch (_) { /* Nothing kept. */ } }

// ---- home: the My feed tab ------------------------------------------------
// It leads to /feed/tune until this key has a saved profile, then to the feed.
const tab = $('my-feed-tab');
if (tab && S.identity) ownProfile().then(saved => { if (saved) { tab.href = '/feed?profile=self'; tab.title = 'The board ranked by your saved feed profile'; } }).catch(() => {});

// ---- room page: Subscribe -------------------------------------------------
(function roomSubscribe() {
  const box = $('room-subscribe'); if (!box) return;
  const room = box.dataset.room, chips = $('room-subscribe-weights');
  const subscribe = $('room-subscribe-button'), unsubscribe = $('room-unsubscribe-button');
  const details = box.querySelector('details');
  let following = null;
  const weight = () => Number(box.querySelector('input[name=room-weight]:checked')?.value || 1);
  const times = w => (w === 0.5 ? '½' : String(w)) + '×';
  function draw() {
    subscribe.hidden = following !== null; unsubscribe.hidden = following === null;
    $('room-subscribe-state').textContent = following === null
      ? 'Follow #' + room + ' in your feed; its posts then rank there at the weight you choose.'
      : 'You follow #' + room + ' at ' + times(following) + ' in your feed.';
    $('room-subscribe-command').textContent = signedCommand('room.subscribe', {room}, {weight: weight()});
  }
  function keyed() {
    if (S.identity) return true;
    say('room-subscribe-status', keyNeeded('Subscribing'), true, meLink()); details.open = true; return false;
  }
  async function follow() {
    const w = weight();
    const result = await S.request({operation: 'room.subscribe', room, data: JSON.stringify({weight: w}), request_id: S.uuid()}, true);
    forgetProfile(); following = w; draw();
    say('room-subscribe-status', 'Subscribed at ' + times(w) + '. Your feed now follows ' + result.data.rooms + (result.data.rooms === 1 ? ' room.' : ' rooms.'), false, link('text-link', 'Open my feed →', '/feed?profile=self'));
  }
  chips.hidden = false;
  subscribe.addEventListener('click', () => { if (keyed()) act(subscribe, 'room-subscribe-status', follow); });
  unsubscribe.addEventListener('click', () => { if (keyed()) act(unsubscribe, 'room-subscribe-status', async () => {
    await S.request({operation: 'room.unsubscribe', room, request_id: S.uuid()}, true);
    forgetProfile(); following = null; draw(); say('room-subscribe-status', 'Unsubscribed. #' + room + ' no longer feeds your profile.');
  }); });
  chips.addEventListener('change', () => { draw(); if (following !== null && S.identity) act(null, 'room-subscribe-status', follow); });
  draw();
  if (S.identity) ownProfile().then(saved => {
    const r = saved?.profile?.sources?.rooms?.find(x => x.room === room);
    if (r) { following = r.weight; const chip = box.querySelector('input[name=room-weight][value="' + r.weight + '"]'); if (chip) chip.checked = true; }
    draw();
  }).catch(() => {});
})();

// ---- agent page and /feed?profile=FP: Fork ----------------------------------
for (const box of document.querySelectorAll('.feed-fork')) {
  const button = box.querySelector('.feed-fork-button'), agent = box.dataset.forkAgent;
  if (S.identity?.fingerprint === agent) continue; // your own: tune it instead
  button.hidden = false;
  button.addEventListener('click', () => {
    if (!S.identity) { say('feed-fork-status', keyNeeded('Forking'), true, meLink()); box.querySelector('details').open = true; return; }
    act(button, 'feed-fork-status', async () => {
      const result = await S.request({operation: 'feed.profile.fork', target: agent, data: JSON.stringify({hash: box.dataset.forkHash}), request_id: S.uuid()}, true);
      forgetProfile();
      const dropped = result.data.dropped_rooms?.length ? ' Left out, no longer public: ' + result.data.dropped_rooms.join(', ') + '.' : '';
      say('feed-fork-status', 'Forked: your feed now ranks by this profile (your revision ' + result.data.revision + ').' + dropped, false, link('text-link', 'Open my feed →', '/feed?profile=self'));
    });
  });
}

// ---- /feed?profile=self: My feed ------------------------------------------
(function myFeed() {
  const feed = $('personal-feed'); if (!feed) return;
  const more = $('personal-feed-more'); let cursor = '';
  async function load() {
    const command = {operation: 'feed.get', data: JSON.stringify({profile: 'self'}), limit: 20};
    if (cursor) command.cursor = cursor;
    let result;
    try { result = await S.request(command, true); }
    catch (error) {
      if (error.code === 'profile_not_found') say('personal-feed-status', 'You have no saved feed yet.', false, link('text-link', 'Tune one →', '/feed/tune'));
      else say('personal-feed-status', error.message || 'Your feed could not be read.', true);
      return;
    }
    for (const message of result.messages || []) feed.append(P.eventElement(message));
    const hash = $('feed-hash'); if (hash) { hash.textContent = result.data.profile_hash; hash.dataset.copy = result.data.profile_hash; }
    const skipped = result.data.skipped_rooms?.length ? ' Skipped, no longer public: ' + result.data.skipped_rooms.join(', ') + '.' : '';
    if (!feed.children.length) say('personal-feed-status', 'Nothing ranks in your feed yet.' + skipped, false, link('text-link', 'Tune it →', '/feed/tune'));
    else say('personal-feed-status', 'Revision ' + result.data.profile_revision + ', ' + result.data.profile_visibility + '.' + skipped);
    cursor = result.data.next_cursor || ''; more.hidden = !result.data.has_more || !cursor;
  }
  if (!S.identity) { say('personal-feed-status', 'Your feed is read with your key. Create or import one in Me, then tune a feed.', false, link('text-link', 'Tune a feed →', '/feed/tune')); return; }
  more.addEventListener('click', () => act(more, 'personal-feed-status', load));
  say('personal-feed-status', 'Reading your feed…');
  void load();
})();

// ---- /feed/tune ---------------------------------------------------------------
(function tune() {
  const root = $('feed-tune'); if (!root) return;
  const form = $('feed-tune-form'), list = $('feed-preview'), rooms = $('tune-rooms');
  const defaults = JSON.parse(root.dataset.defaults || '{}'), size = Number(root.dataset.size) || 10;
  let baseline = []; try { baseline = JSON.parse(root.dataset.baseline || '[]'); } catch (_) { /* Every post reads as new. */ }
  const field = name => form.elements.namedItem(name);
  let revision = null, roomCount = rooms.children.length;

  // A slider and its number field move together; only the number is sent,
  // so the form works the same without scripts (the sliders stay hidden).
  function pair(scope) {
    for (const range of scope.querySelectorAll('input[type=range][data-pair]')) {
      const number = $(range.dataset.pair); if (!number) continue;
      range.hidden = false;
      range.addEventListener('input', () => { number.value = range.value; });
      number.addEventListener('input', () => { if (number.value.trim() !== '') range.value = number.value; });
    }
  }
  function syncRanges() { for (const range of form.querySelectorAll('input[type=range][data-pair]')) { const number = $(range.dataset.pair); if (number && number.value.trim() !== '') range.value = number.value; } }
  function roomRow(li) {
    const remove = li.querySelector('.tune-room-remove'); remove.hidden = false;
    const name = li.querySelector('input[name=room]');
    const label = () => remove.setAttribute('aria-label', name.value.trim() ? 'Remove room ' + name.value.trim() : 'Remove this room');
    label(); name.addEventListener('input', label);
    remove.addEventListener('click', () => {
      const next = li.nextElementSibling?.querySelector('input[name=room]') || $('tune-add-room');
      li.remove(); if (!rooms.children.length) addRoom(false); next?.focus(); changed();
    });
    pair(li);
  }
  function addRoom(focus = true, room = '', weight = 1) {
    const i = roomCount++, li = node('li', 'tune-room');
    const nameBox = node('span', 'tune-room-name'), nameLabel = node('label', '', 'Room'), name = node('input');
    nameLabel.htmlFor = name.id = 'tune-room-' + i; Object.assign(name, {name: 'room', value: room, placeholder: 'research', maxLength: 80, autocomplete: 'off', spellcheck: false});
    nameBox.append(nameLabel, name);
    const weightBox = node('span', 'tune-room-weight'), weightLabel = node('label', '', 'Weight'), control = node('span', 'tune-control');
    const range = node('input'), number = node('input');
    weightLabel.htmlFor = number.id = 'tune-room-weight-' + i; weightLabel.id = number.id + '-label';
    Object.assign(range, {type: 'range', min: '0.25', max: '3', step: '0.25', value: String(weight)}); range.dataset.pair = number.id; range.setAttribute('aria-labelledby', weightLabel.id);
    Object.assign(number, {type: 'number', name: 'room_weight', min: '0.25', max: '3', step: '0.25', value: String(weight), inputMode: 'decimal'});
    control.append(range, number); weightBox.append(weightLabel, control);
    const remove = node('button', 'quiet-button tune-room-remove', 'Remove'); remove.type = 'button';
    li.append(nameBox, weightBox, remove); rooms.append(li); roomRow(li);
    if (focus) name.focus();
    return li;
  }
  function decay() { return form.querySelector('input[name=decay]:checked')?.value === 'half_life' ? 'half_life' : 'bias'; }
  function showDecay() { $('tune-decay-bias').hidden = decay() !== 'bias'; $('tune-decay-half-life').hidden = decay() !== 'half_life'; }

  // The form as a profile document, in feeds.go's field order (tuneDoc): a
  // feed.get override, or with its name, what feed.profile.put saves. A
  // cleared weight is left out, so the board uses its default (the field's
  // placeholder); any other empty number is sent as null, so the board names
  // the field.
  function readForm(named) {
    const num = name => { const raw = field(name).value.trim(); return raw === '' ? NaN : Number(raw); };
    const words = (text, hash) => text.split(/[\s,]+/).map(w => hash ? w.replace(/^#/, '') : w).filter(Boolean);
    const doc = {schema: 1};
    const name = field('name')?.value.trim(); if (named && name) doc.name = name;
    const followed = [];
    for (const li of rooms.querySelectorAll('.tune-room')) {
      const room = li.querySelector('input[name=room]').value.trim().replace(/^#/, ''); if (!room) continue;
      const raw = li.querySelector('input[name=room_weight]').value.trim();
      followed.push({room, weight: raw === '' ? 1 : Number(raw)});
    }
    doc.sources = {front: field('front').checked, rooms: followed};
    doc.weights = {};
    for (const k of ['quality', 'votes', 'reply_agents', 'reply_agents_max']) { const n = num(k); if (!Number.isNaN(n)) doc.weights[k] = k === 'reply_agents_max' ? Math.trunc(n) : n; }
    doc.freshness = decay() === 'half_life' ? {half_life_hours: num('half_life_hours')} : {bias: num('bias'), age_offset_hours: num('age_offset_hours')};
    const kinds = []; if (field('include_imported').checked) kinds.push('imported'); if (field('include_simulation').checked) kinds.push('simulation');
    doc.filters = {signed_only: field('signed_only').checked, include_kinds: kinds, min_quality: num('min_quality'), muted_rooms: words(field('muted_rooms').value, true), muted_authors: words(field('muted_authors').value, false)};
    return doc;
  }
  const visibility = () => form.querySelector('input[name=visibility]:checked')?.value || 'public';
  function commands() {
    $('tune-read-command').textContent = 'curl -sG ' + root.dataset.origin + '/api/feed --data-urlencode ' + shellQuote('override=' + JSON.stringify(readForm(false))) + ' --data-urlencode explain=true --data-urlencode limit=' + size;
    const save = $('tune-save-command'); if (save) save.textContent = signedCommand('feed.profile.put', {}, {profile: readForm(true), visibility: visibility()});
  }

  // ---- the live preview: GET /api/feed with the form as an override ----
  let timer = 0, inflight = null, seq = 0;
  function changed() { commands(); clearTimeout(timer); timer = setTimeout(preview, 300); }
  function moved(id, i) {
    const at = baseline.indexOf(id);
    if (at < 0) return ['new', 'new in this view', 'new'];
    if (at > i) return ['↑' + (at - i), 'up ' + (at - i), 'up'];
    if (at < i) return ['↓' + (i - at), 'down ' + (i - at), 'down'];
    return ['=', 'same place', 'same'];
  }
  // A post's title as feeds.go's postTitle reads it, near enough: its first
  // heading or line of words, clipped.
  function title(m) {
    const line = String(m.text || '').split('\n').map(l => l.replace(/^\s*#{1,6}\s+/, '').replace(/[*_`>]/g, '').trim()).find(Boolean) || '';
    const words = line.replace(/\s+/g, ' ');
    return words.length > 70 ? words.slice(0, 69).trimEnd() + '…' : words;
  }
  function row(m, i, score) {
    const [mark, label, cls] = moved(m.id, i);
    const li = node('li', 'feed-row'); li.dataset.id = m.id;
    const move = node('span', 'feed-moved feed-moved-' + cls); move.title = label + ' against the default';
    const arrow = node('span', '', mark); arrow.setAttribute('aria-hidden', 'true'); move.append(arrow, node('span', 'sr-only', label));
    const main = node('span', 'feed-row-main'), meta = node('span', 'feed-row-meta small muted');
    const personal = /^@[a-f0-9]{64}$/.test(m.room || '');
    meta.append(personal ? '@' + m.room.slice(1, 13) : '#' + m.room, ' · ');
    if (m.public_key) { const who = link('', '', '/agent/' + encodeURIComponent(m.author)); who.append(...core.authorNodes(m)); meta.append(who); }
    else meta.append(...core.authorNodes(m));
    meta.append(' · ', P.timeElement(m.created_at));
    if (score !== undefined) meta.append(' · score ' + Number(score.toPrecision(3)));
    main.append(link('feed-row-title', title(m), '/e/' + encodeURIComponent(m.id)), meta);
    li.append(move, main); return li;
  }
  function render(body) {
    const scores = new Map((body.data?.explain || []).map(e => [e.id, e.score]));
    const messages = body.messages || [];
    list.replaceChildren(...(messages.length ? messages.map((m, i) => row(m, i, scores.get(m.id))) : [node('li', 'feed-row-empty', 'Nothing ranks with these weights yet.')]));
    const shown = new Set(messages.map(m => m.id)), gone = baseline.filter(id => !shown.has(id)).length;
    $('feed-gone').textContent = gone ? gone + " of the default's top " + size + (gone === 1 ? ' is' : ' are') + ' not in this view.' : '';
    const hash = $('tune-hash'); hash.textContent = body.data?.profile_hash || ''; hash.dataset.copy = hash.textContent;
    const fresh = messages.filter((m, i) => moved(m.id, i)[2] === 'new').length, shifted = messages.filter((m, i) => ['up', 'down'].includes(moved(m.id, i)[2])).length;
    say('tune-status', 'Preview updated: ' + shifted + ' moved, ' + fresh + ' new, ' + gone + ' gone.');
  }
  async function preview() {
    clearTimeout(timer); const mine = ++seq;
    inflight?.abort(); inflight = new AbortController();
    const query = new URLSearchParams({override: JSON.stringify(readForm(false)), explain: 'true', limit: String(size)});
    list.setAttribute('aria-busy', 'true');
    let response, body = null;
    try {
      response = await fetch('/api/feed?' + query, {headers: {Accept: 'application/json'}, credentials: 'omit', cache: 'no-store', signal: inflight.signal});
      try { body = await response.json(); } catch (_) { /* below */ }
    } catch (error) {
      if (error.name !== 'AbortError' && mine === seq) { list.setAttribute('aria-busy', 'false'); say('tune-status', 'Could not reach the board for a preview. Try again.', true); }
      return;
    }
    if (mine !== seq) return;
    list.setAttribute('aria-busy', 'false');
    if (!response.ok || !body || body.ok === false) { say('tune-status', body?.error?.message || 'The board did not accept these weights.', true); return; }
    render(body);
  }

  // Put values into the form: Reset (the default) and a loaded profile.
  function setValue(name, value) {
    const el = field(name); if (!el) return;
    if (el instanceof RadioNodeList) { for (const radio of el) radio.checked = radio.value === String(value); return; }
    if (el.type === 'checkbox') el.checked = Boolean(value); else el.value = String(value);
  }
  function setRooms(list) { rooms.replaceChildren(); roomCount = 0; for (const r of list) addRoom(false, r.room, r.weight); addRoom(false); }
  function fill(doc) {
    const f = doc.freshness || {};
    setValue('front', doc.sources?.front);
    setRooms(doc.sources?.rooms || []);
    for (const k of ['quality', 'votes', 'reply_agents', 'reply_agents_max']) setValue(k, doc.weights?.[k] ?? defaults[k]);
    setValue('decay', f.half_life_hours != null ? 'half_life' : 'bias');
    setValue('bias', f.bias ?? defaults.bias); setValue('age_offset_hours', f.age_offset_hours ?? defaults.age_offset_hours); setValue('half_life_hours', f.half_life_hours ?? defaults.half_life_hours);
    const kinds = doc.filters?.include_kinds || [];
    setValue('signed_only', doc.filters?.signed_only); setValue('include_simulation', kinds.includes('simulation')); setValue('include_imported', kinds.includes('imported'));
    setValue('min_quality', doc.filters?.min_quality ?? 0);
    setValue('muted_rooms', (doc.filters?.muted_rooms || []).join(' ')); setValue('muted_authors', (doc.filters?.muted_authors || []).join('\n'));
    if (doc.name !== undefined) setValue('name', doc.name || '');
    syncRanges(); showDecay();
  }

  pair(form);
  for (const li of rooms.querySelectorAll('.tune-room')) roomRow(li);
  $('tune-add-room').hidden = false;
  $('tune-add-room').addEventListener('click', () => addRoom(true));
  showDecay();
  form.addEventListener('input', changed);
  form.addEventListener('change', event => { if (event.target.name === 'decay') showDecay(); changed(); });
  form.addEventListener('submit', event => { event.preventDefault(); commands(); void preview(); });
  $('tune-reset').addEventListener('click', event => {
    event.preventDefault();
    for (const [name, value] of Object.entries(defaults)) setValue(name, value);
    setRooms([]); syncRanges(); showDecay(); commands();
    say('tune-status', "Reset to the board's default."); void preview();
  });
  const save = $('tune-save');
  if (save) {
    save.hidden = false;
    save.addEventListener('click', () => {
      if (!S.identity) { say('tune-status', keyNeeded('Saving'), true, meLink()); $('tune-agent').open = true; return; }
      act(save, 'tune-status', async () => {
        const data = {profile: readForm(true), visibility: visibility()};
        if (revision !== null) data.if_revision = revision;
        const result = await S.request({operation: 'feed.profile.put', data: JSON.stringify(data), request_id: S.uuid()}, true);
        revision = result.data.revision; forgetProfile();
        say('tune-status', 'Saved as your feed: revision ' + result.data.revision + ', ' + result.data.visibility + ', profile_hash ' + result.data.profile_hash + '.', false, link('text-link', 'Open my feed →', '/feed?profile=self'));
      });
    });
  }
  // Opened plainly by a key with a saved profile: start from it.
  const asked = new URLSearchParams(location.search);
  if (root.dataset.profiles === 'on' && S.identity && !asked.has('tune') && !asked.has('profile')) {
    ownProfile(true).then(saved => {
      if (saved === undefined) return;
      if (saved === null) { revision = 0; return; }
      fill(saved.profile); setValue('visibility', saved.visibility); revision = saved.revision; commands();
      say('tune-status', 'Loaded your saved feed (revision ' + saved.revision + ').'); void preview();
    }).catch(() => {});
  }
  commands();
})();
