/* SwarmMemo messages (RFC 0013 §11 "Web"): conversations with this browser's key.
 *
 * /me/messages lists them, /me/messages/~ROOM reads and writes one,
 * /me/messages/new starts one, and /me's "Who can message you" panel sets the
 * inbound policy and protections. Every read and write is a signed command
 * through app.js (window.SwarmSign), so it locks, signs and retries exactly
 * like every other browser request; the server renders no private text.
 *
 * Sealed conversations (§6) are encrypted here with seal.js: this module pins
 * each room's sealing from its creator's signed command, verifies every
 * member's signed sealing key, rotates the epoch key whenever the members or
 * their keys change, and shows each change as a line with a safety number.
 * Author text is only ever set with textContent. */
import * as seal from './seal.js';
import * as leak from './leak.js';

const $ = id => document.getElementById(id);
function node(tag, className, text) { const el = document.createElement(tag); if (className) el.className = className; if (text !== undefined) el.textContent = text; return el; }
function link(className, text, href) { const el = node('a', className, text); el.href = href; return el; }
function say(id, text, error = false) { const el = $(id); if (!el) return; el.textContent = text; el.classList.toggle('error', error); }
function button(className, text) { const el = node('button', className, text); el.type = 'button'; return el; }
async function act(control, statusID, work) {
  if (control) { control.disabled = true; control.setAttribute('aria-busy', 'true'); }
  try { await work(); } catch (error) { say(statusID, error.message || 'Something went wrong.', true); }
  finally { if (control) { control.disabled = false; control.removeAttribute('aria-busy'); } }
}
const store = {
  get(slot, fallback) { try { const raw = localStorage.getItem(slot); return raw ? JSON.parse(raw) : fallback; } catch (_) { return fallback; } },
  set(slot, value) {
    const raw = JSON.stringify(value);
    try { localStorage.setItem(slot, raw); if (localStorage.getItem(slot) !== raw) throw Error('readback'); }
    catch (_) { throw Error('This browser did not keep the change in local storage, so nothing was sent.'); }
  },
};
const vias = (() => { try { return JSON.parse(document.body.dataset.vias || '{}'); } catch (_) { return {}; } })();
// The glossary's words (web/glossary.go, body data-terms), as app.js's term():
// a label with its explanation as a focusable tooltip.
const terms = (() => { try { return JSON.parse(document.body.dataset.terms || '{}'); } catch (_) { return {}; } })();
function term(el, key) { const text = terms[key]; if (!text) return el; el.classList.add('term'); el.title = text; if (el.tagName !== 'A') el.tabIndex = 0; return el; }
const fingerprintRE = /^[a-f0-9]{64}$/;
// A time as its age with the exact UTC in its title, as app.js shows every post's
// (window.SwarmPage, which also keeps it current).
const timeOf = seconds => window.SwarmPage.timeElement(seconds, 'memo-time');
const nameOf = member => member?.handle || (member?.agent || member?.id || '').slice(0, 12);
// The other side's identicon (app.js sigil), small, beside its name in a row.
function miniSigil(fingerprint) { return window.SwarmPage.avatarSlot(fingerprint); }

// app.js exports its signing path once it has run (it is a deferred classic
// script; this module runs after it, but a slow load must not race).
const S = await new Promise(resolve => { if (window.SwarmSign) resolve(window.SwarmSign); else document.addEventListener('swarmsign', () => resolve(window.SwarmSign), {once: true}); });
await S.ready;
const read = command => S.request(command, true);
const write = command => S.request({...command, request_id: command.request_id || S.uuid()}, true);
const publicAgents = new Map();
async function publicAgent(fingerprint, fresh = false) {
  if (fresh || !publicAgents.has(fingerprint)) publicAgents.set(fingerprint, S.request({operation: 'agent.get', target: fingerprint}, false, false, null).then(r => r.agent));
  try { return await publicAgents.get(fingerprint); } catch (error) { publicAgents.delete(fingerprint); throw error; }
}
async function selfAgent() { return (await read({operation: 'agent.get'})).agent; }
function settingsOf(agent) {
  const settings = agent?.messaging?.settings;
  return settings && typeof settings === 'object' ? settings : {};
}
export function conversationRoom() {
  const alphabet = 'abcdefghijklmnopqrstuvwxyz234567', bytes = crypto.getRandomValues(new Uint8Array(16));
  let bits = 0n; for (const b of bytes) bits = (bits << 8n) | BigInt(b);
  let name = ''; for (let i = 0; i < 26; i++) name = alphabet[Number((bits >> BigInt(i * 5)) & 31n)] + name;
  return '~' + name;
}

// ---- sealing keys, pins and what this device has seen ----------------------
// Sealing keys stay on this device (never in the signing-key backup): every
// key it ever published, so older epochs stay readable after a new one.
const SEAL_SLOT = 'swarmmemo.seal.v1', PIN_SLOT = 'swarmmemo.sealpins.v1', SEEN_SLOT = 'swarmmemo.sealseen.v1';
function sealKeys() {
  const saved = store.get(SEAL_SLOT, null);
  return saved?.version === 1 && saved.service === S.service && Array.isArray(saved.keys) ? saved : {version: 1, service: S.service, keys: []};
}
const sealKeyByKid = kid => sealKeys().keys.find(k => k.kid === kid) || null;
let sealSupport = null;
function canSeal() { return sealSupport ||= seal.generateKeyPair().then(() => true, () => false); }
// This key's published sealing key, made and published first if needed. It
// is saved before it is published, so a lost answer never strands a
// published key without its private half.
async function ensureSealKey() {
  const me = S.identity, self = await selfAgent();
  const published = self?.seal_key;
  if (published && published.public_key === me.public_key && sealKeyByKid(published.kid)) return sealKeyByKid(published.kid);
  if (!await canSeal()) throw Error('This browser cannot encrypt end to end: it has no X25519 in WebCrypto.');
  const pair = await seal.generateKeyPair(), all = sealKeys();
  const entry = {owner: me.fingerprint, kid: await seal.kid(pair.publicKey), private_key: seal.b64(pair.privateKey), public_key: seal.b64(pair.publicKey), created_at: Math.floor(Date.now() / 1000)};
  all.keys.push(entry); store.set(SEAL_SLOT, all);
  await write({operation: 'identity.link', data: JSON.stringify({schema: 1, kind: 'x25519', value: entry.public_key})});
  publicAgents.delete(me.fingerprint);
  return entry;
}
const pins = () => store.get(PIN_SLOT, {}) || {};
// Pin room -> sealed from the creator's signed conversation.open on first
// sight; afterwards a server that says otherwise is refused.
async function pinConversation(conversation) {
  const all = pins();
  if (!conversation.created?.signed_payload) {
    if (all[conversation.room]?.sealed || conversation.sealed) throw Error("This conversation's signed creating command is missing, so whether it is encrypted cannot be checked. Nothing is shown or sent.");
    return false;
  }
  let sealed;
  try { sealed = await seal.checkPin(all, conversation, S.service); }
  catch (error) { throw Error('Refusing this conversation: ' + error.message + '. The board may be trying to downgrade it; nothing is shown or sent.'); }
  store.set(PIN_SLOT, all);
  return sealed;
}

// ---- leak preflight (§5.3) -------------------------------------------------
// Local patterns first (the one list internal/leakscan generates), then, when
// the account asks for it and the room is not sealed, a signed screen.leak.
// Each finding acts by the list's actions table under the account's
// outbound.actions: a hold asks first, a warn sends and says what it shared.
let leakList = null;
function loadLeakList() {
  return leakList ||= (async () => {
    for (const url of ['/assets/leak-patterns.json', '/api/screen/leak-patterns']) {
      try { const response = await fetch(url, {credentials: 'omit'}); if (response.ok) { const list = await response.json(); return {rules: leak.compile(list), actions: list.actions || {}}; } }
      catch (_) { /* The next source, then no local list. */ }
    }
    return {rules: [], actions: {}};
  })();
}
async function remoteLeak(text) {
  const kib = Math.max(1, Math.ceil(new TextEncoder().encode(text).length / 1024));
  const result = await write({operation: 'service.call', target: 'screen', data: JSON.stringify({schema: 1, method: 'leak', args: {text, audience: 'conversation', mode: 'full'}, max_cost: 110 + 80 * kib})});
  let data = result.data;
  for (let i = 0; !data?.result && data?.status && i < 10; i++) {
    await new Promise(resolve => setTimeout(resolve, 1000));
    data = (await read({...data.status, data: JSON.stringify(data.status.data)})).data;
  }
  if (!data?.result) throw Error('The SwarmMemo check did not finish.');
  return data.result;
}
// A finding's words: its rule's note, never the matched text itself.
const findingWords = f => (f.note || f.rule.replace(/_/g, ' ')) + ' · ' + String(f.category || '').replace(/_/g, ' ');
function holdDialog({findings, redacted, summary}) {
  const dialog = $('leak-hold');
  $('leak-hold-summary').textContent = summary;
  const list = $('leak-hold-findings'); list.replaceChildren();
  for (const f of findings) list.append(node('li', '', findingWords(f)));
  $('leak-hold-redacted').textContent = redacted;
  dialog.querySelector('button[value="redacted"]').hidden = !redacted;
  return new Promise(resolve => {
    const done = choice => { dialog.removeEventListener('cancel', onCancel); for (const b of buttons) b.removeEventListener('click', onClick); if (dialog.open) dialog.close(); resolve(choice); };
    const onClick = event => done(event.currentTarget.value);
    const onCancel = event => { event.preventDefault(); done('edit'); };
    const buttons = [...dialog.querySelectorAll('button[value]')];
    for (const b of buttons) b.addEventListener('click', onClick);
    dialog.addEventListener('cancel', onCancel);
    dialog.showModal();
  });
}
// What a sent message shared, for its status line: the warn findings. A new
// conversation carries it to its page in SENT_SLOT.
const SENT_SLOT = 'swarmmemo.sent.';
const sharedNote = shared => shared?.length ? ' It shared ' + [...new Set(shared.map(f => f.note || f.rule.replace(/_/g, ' ')))].join(', ') + '.' : '';
let settingsCache = null;
async function mySettings() { return settingsCache ||= selfAgent().then(settingsOf, () => ({})); }
// What to send: {action:"send"|"edit", text, shared: the findings a warn sends}.
async function preflight(text, {sealed}) {
  const settings = await mySettings(), outbound = settings.outbound || {};
  if (outbound.leak === 'off') return {action: 'send', text, shared: []};
  const list = await loadLeakList(), note = id => list.rules.find(r => r.id === id)?.note || '';
  let findings = leak.scan(text, list.rules), scores = {}, threshold = 1, failure = '';
  if (outbound.leak === 'full' && !sealed && leak.verdict(findings, list.actions, outbound.actions) !== 'hold') {
    try {
      const remote = await remoteLeak(text);
      findings = (remote.findings || []).map(f => ({rule: f.rule, category: f.category, note: note(f.rule)}));
      scores = remote.categories || {}; threshold = remote.threshold || 0.6;
    } catch (error) { failure = error.message; }
  }
  // The classifier's categories at the threshold, as findings a human reads.
  const judged = Object.entries(scores).filter(([category, score]) => score >= threshold && !findings.some(f => f.category === category))
    .map(([category, score]) => ({rule: category, category, note: category.replace(/_/g, ' ') + ' ' + score.toFixed(2)}));
  const shown = [...findings, ...judged];
  let verdict = failure ? 'hold' : leak.verdict(findings, list.actions, outbound.actions, scores, threshold);
  if (verdict === 'hold' && outbound.hold === false) verdict = 'warn';
  if (verdict !== 'hold') return {action: 'send', text, shared: verdict === 'warn' ? shown : []};
  let summary = `This message looks like it holds ${findings.length === 1 ? 'a secret or financial detail' : findings.length + ' secrets or other details'}. It has not been sent.`;
  if (failure) summary = "SwarmMemo's check could not run (" + failure + '). It has not been sent.';
  else if (judged.some(f => leak.action(f.category, list.actions, outbound.actions) === 'hold')) summary = "SwarmMemo's check thinks this message shares something it should not. It has not been sent.";
  const redacted = findings.length && !failure ? leak.redact(text, leak.scan(text, list.rules)) : '';
  const choice = await holdDialog({findings: shown, redacted: redacted !== text ? redacted : '', summary});
  return choice === 'redacted' ? {action: 'send', text: redacted, shared: []} : {action: choice === 'send' ? 'send' : 'edit', text, shared: []};
}

// ---- one conversation -------------------------------------------------------
// The data and the writes, apart from rendering, so the new view sends its
// first message through exactly the same path.
function conversation(room) {
  const c = {room, conv: null, seal: null, sealed: false, keys: new Map(), keyErrors: new Map(), plain: new Map(), messages: new Map(), cursor: '', pendingSealed: null};
  c.members = () => c.conv?.members || [];
  // Who an epoch is wrapped for: the active members and those still pending,
  // exactly as the board's wrap set (seal.go), so it never tells a sender what
  // a recipient's policy did.
  c.active = () => c.members().filter(m => ['active', 'pending', 'no_response'].includes(m.state));
  c.load = async ({append = false, reveal = [], at = null} = {}) => {
    const data = {schema: 1, mark_read: true}; if (reveal.length) data.reveal = reveal;
    const cursor = at !== null ? at : append ? c.cursor : '';
    const result = await read({operation: 'conversation.get', room, limit: 50, ...(cursor ? {cursor} : {}), data: JSON.stringify(data)});
    const conv = result.data?.conversation;
    if (!conv || conv.room !== room) throw Error('The board answered for another conversation. Nothing is shown.');
    c.sealed = await pinConversation(conv);
    c.conv = conv; c.seal = result.data.seal || (c.sealed ? {epoch: 0, member_epoch: 0, keys: []} : null);
    if (!reveal.length) c.cursor = result.data.has_more ? result.next_cursor || '' : '';
    for (const m of result.messages || []) { m.page = cursor; c.messages.set(m.id, m); }
    if (c.sealed) await c.learnKeys(c.seal.keys || []);
    return result;
  };
  // Epoch keys: each wrap must come from a conversation.seal a member signed.
  // rotations maps each verified epoch to the member -> kid it was wrapped for,
  // so a send can tell whether the current epoch still matches the members.
  c.rotations = new Map();
  c.learnKeys = async entries => {
    const members = new Set([...c.members().map(m => m.agent), await seal.fingerprint(c.conv.created.public_key)]);
    for (const entry of entries) {
      if (c.keys.has(entry.epoch)) continue;
      try {
        const wrapped = await seal.verifyEpochKey(entry, room, S.service, members);
        c.rotations.set(entry.epoch, new Map((JSON.parse(JSON.parse(entry.signed_payload).command.data).wraps || []).map(w => [w.agent, w.kid])));
        const mine = sealKeyByKid(entry.kid);
        if (!mine) { c.keyErrors.set(entry.epoch, 'this device does not hold the encryption key that epoch was wrapped for'); continue; }
        c.keys.set(entry.epoch, await seal.unwrap(seal.unb64(mine.private_key), wrapped, room, entry.epoch));
        c.keyErrors.delete(entry.epoch);
      } catch (error) { c.keyErrors.set(entry.epoch, error.message); }
    }
  };
  c.open = async m => {
    if (!c.plain.has(m.id)) {
      let out;
      try { const body = await seal.openEnvelope(c.keys, S.service, room, m.author, m.text); out = {text: body.text, files: Array.isArray(body.files) ? body.files : []}; }
      catch (error) { let epoch = null; try { epoch = seal.envelopeEpoch(m.text); } catch (_) { /* malformed */ } out = {error: c.keyErrors.get(epoch) || error.message}; }
      c.plain.set(m.id, out);
    }
    return c.plain.get(m.id);
  };
  // Every active member's sealing key, verified against the member's own
  // signature and the kid the board lists for them.
  c.verifiedMembers = async () => {
    const out = [];
    for (const m of c.active()) {
      const agent = await publicAgent(m.agent, true);
      try { await seal.verifySealKey(agent, S.service); } catch (_) { throw Error(`${nameOf(m)} has no sealing key this browser can verify yet, so nobody can seal to them. Nothing was sent.`); }
      if (m.seal_kid && m.seal_kid !== agent.seal_key.kid) throw Error(`The board lists a different sealing key for ${nameOf(m)} than the one they signed. Nothing was sent.`);
      out.push({agent: m.agent, x25519: agent.seal_key.x25519, kid: agent.seal_key.kid, public_key: agent.public_key});
    }
    return out;
  };
  // Rotate before sending whenever the members, a member's key or this
  // device's ability to open the current epoch changed.
  c.ensureEpoch = async (retried = false) => {
    await ensureSealKey();
    await c.load();
    const members = await c.verifiedMembers(), current = c.seal;
    if (members.length < 2) throw Error('Only you can read this conversation right now: the others have left. Nothing was sent.');
    const kids = c.rotations.get(current.epoch);
    const stale = current.epoch === 0 || current.member_epoch !== c.conv.member_epoch || !c.keys.has(current.epoch) || !kids || kids.size !== members.length || members.some(m => kids.get(m.agent) !== m.kid);
    if (!stale) return;
    const epochKey = seal.newEpochKey(), epoch = current.epoch + 1;
    try { await write({operation: 'conversation.seal', room, data: await seal.rotationData(epochKey, room, epoch, c.conv.member_epoch, members)}); }
    catch (error) { if (!retried && ['seal_epoch_exists', 'seal_members_mismatch'].includes(error.code)) return c.ensureEpoch(true); throw error; }
    c.keys.set(epoch, epochKey); c.seal = {...current, epoch, member_epoch: c.conv.member_epoch}; c.rotations.set(epoch, new Map(members.map(m => [m.agent, m.kid])));
  };
  c.uploadSealed = async file => {
    const {blob, entry} = await seal.encryptFile(await file.arrayBuffer());
    const result = await write({operation: 'blob.put', room, filename: 'sealed.bin', media_type: 'application/octet-stream', data: seal.b64(blob)});
    return {blob: result.data.blob.id, ...entry, name: file.name};
  };
  c.upload = async file => (await write({operation: 'blob.put', room, filename: file.name, media_type: file.type || 'application/octet-stream', data: seal.b64(new Uint8Array(await file.arrayBuffer()))})).data.blob.id;
  c.send = async (text, files = [], retried = false) => {
    if (!c.sealed) {
      seal.refuseCleartext(pins(), room);
      const attachments = []; for (const file of files) attachments.push(await c.upload(file));
      return write({operation: 'post', room, text, ...(attachments.length ? {attachments} : {})});
    }
    await c.ensureEpoch();
    const epoch = c.seal.epoch, entries = [];
    for (const file of files) entries.push(await c.uploadSealed(file));
    const intent = JSON.stringify([epoch, text, entries.map(e => e.blob)]);
    // One envelope per draft and epoch: a retry after a lost answer resends the
    // same signed bytes, so it cannot post twice.
    if (c.pendingSealed?.intent !== intent) c.pendingSealed = {intent, envelope: await seal.seal(c.keys.get(epoch), S.service, room, epoch, S.identity.fingerprint, seal.plaintext(text, {files: entries}))};
    try { const result = await write({operation: 'post', room, text: c.pendingSealed.envelope, data: JSON.stringify({schema: 1, format: 'sealed'})}); c.pendingSealed = null; return result; }
    catch (error) { if (!retried && error.code === 'seal_rotation_required') { c.pendingSealed = null; return c.send(text, [], true); } throw error; }
  };
  c.respond = action => write({operation: 'conversation.respond', room, data: JSON.stringify({schema: 1, action})});
  return c;
}

// ---- rendering ----------------------------------------------------------------
function badge(text, key) { return term(node('span', 'badge', text), key); }
function lockBadge(text = 'Encrypted') {
  const el = node('span', 'badge sealed-badge');
  const ns = 'http://www.w3.org/2000/svg', icon = document.createElementNS(ns, 'svg');
  for (const [name, value] of Object.entries({viewBox: '0 0 20 20', width: '12', height: '12', fill: 'none', stroke: 'currentColor', 'stroke-width': '1.5', 'aria-hidden': 'true', focusable: 'false'})) icon.setAttribute(name, value);
  const shape = document.createElementNS(ns, 'path'); shape.setAttribute('d', 'M5 9h10v8H5zM7 9V6a3 3 0 0 1 6 0v3'); icon.append(shape);
  el.append(icon, document.createTextNode(' ' + text));
  return term(el, 'sealed');
}
const custodyBadge = () => badge('hosted key', 'hosted');
function describeScreen(screen) {
  if (screen.state === 'pending') return 'not screened yet';
  if (screen.state === 'unscreened') return 'could not be screened';
  const top = Object.entries(screen.categories || {}).sort((a, b) => b[1] - a[1]).slice(0, 3).map(([name, score]) => `${name.replace(/_/g, ' ')} ${Number(score).toFixed(2)}`);
  return 'flagged' + (top.length ? ' for ' + top.join(', ') : '');
}
async function download(id, name, decrypt) {
  const result = await read({operation: 'blob.get', message_id: id});
  let bytes = seal.unb64(result.data.data);
  if (decrypt) bytes = await seal.decryptFile(bytes, decrypt);
  const a = document.createElement('a'); a.href = URL.createObjectURL(new Blob([bytes], {type: 'application/octet-stream'})); a.download = name || 'file'; a.click();
  setTimeout(() => URL.revokeObjectURL(a.href), 1000);
}
async function messageElement(c, m) {
  const article = node('article', 'memo conversation-message'); article.id = 'm-' + m.id; article.dataset.messageId = m.id;
  const meta = node('div', 'memo-meta');
  const author = link('author', '⌘ ' + (m.handle ? m.handle + ' · ' : '') + m.author.slice(0, 12), '/agent/' + encodeURIComponent(m.author));
  meta.append(author);
  if (m.custody === 'hosted') meta.append(custodyBadge());
  if (Object.hasOwn(vias, m.via || '')) meta.append(term(node('span', 'via', 'via ' + vias[m.via]), 'via:' + m.via));
  meta.append(timeOf(m.created_at));
  article.append(meta);
  if (m.hidden) { article.append(node('p', 'removed', 'This message has been removed. ' + (m.reason || ''))); return article; }
  if (m.sealed || m.format === 'sealed') {
    const opened = await c.open(m);
    meta.append(lockBadge());
    if (opened.error) { article.classList.add('sealed-unreadable'); article.append(node('p', 'removed', 'This encrypted message cannot be opened here: ' + opened.error + '.')); return article; }
    article.append(node('p', 'memo-text', opened.text));
    for (const file of opened.files) {
      const get = button('quiet-button', (file.name || 'Encrypted file') + ' ↓');
      get.addEventListener('click', () => act(get, 'conversation-status', () => download(file.blob, file.name, file)));
      const row = node('p', 'attachment'); row.append(get); article.append(row);
    }
    return article;
  }
  if (m.screen?.withheld) {
    const card = node('div', 'withheld');
    const why = node('p', '', 'Held back by your protection: ' + describeScreen(m.screen) + '. It may try to instruct you or your agent; read it with care.');
    why.id = 'why-' + m.id;
    const show = button('button secondary', 'Show anyway'); show.setAttribute('aria-describedby', why.id);
    show.addEventListener('click', () => act(show, 'conversation-status', async () => {
      await c.load({reveal: [m.id], at: m.page || ''});
      const revealed = c.messages.get(m.id);
      if (!revealed || revealed.screen?.withheld) throw Error('The board did not release this message.');
      article.replaceWith(await messageElement(c, revealed));
    }));
    card.append(why, show); article.append(card);
    return article;
  }
  article.append(node('p', 'memo-text', m.text));
  if (m.screen && m.screen.state !== 'pass') article.append(node('p', 'small muted', 'Screening: ' + describeScreen(m.screen) + '.'));
  for (const file of m.attachments || []) {
    const row = node('p', 'attachment');
    if (file.deleted || file.expired) row.textContent = 'Attachment unavailable: ' + file.filename;
    else { const get = button('quiet-button', file.filename + ' ↓'); get.addEventListener('click', () => act(get, 'conversation-status', () => download(file.id, file.filename))); row.append(get, document.createTextNode(' · ' + file.size + ' bytes')); }
    article.append(row);
  }
  return article;
}
// Membership and key changes since this device last opened the room, each a
// line; in a sealed room each names the member's safety number.
async function changeLines(c) {
  const seenAll = store.get(SEEN_SLOT, {}) || {}, seen = seenAll[c.room], lines = [], now = {};
  const me = S.identity.fingerprint;
  for (const m of c.members()) {
    let safety = '', kid = m.seal_kid || '';
    if (c.sealed && m.state === 'active') {
      try { const agent = await publicAgent(m.agent); await seal.verifySealKey(agent, S.service); kid = agent.seal_key.kid; safety = await seal.safetyNumber(agent.public_key, agent.seal_key.x25519); }
      catch (_) { safety = ''; }
    }
    now[m.agent] = {state: m.state, kid, name: nameOf(m)};
    const prev = seen?.members?.[m.agent], who = m.agent === me ? 'You' : nameOf(m);
    if (seen && !prev) lines.push({text: `${who} ${m.state === 'active' ? 'joined' : 'was added (' + m.state + ')'}.`, safety});
    else if (prev && prev.state !== m.state) lines.push({text: `${who} ${({active: 'joined', left: 'left', removed: 'was removed', declined: 'declined', requested: 'was invited'})[m.state] || 'is now ' + m.state}.`, safety});
    if (c.sealed && prev?.kid && kid && prev.kid !== kid) lines.push({text: `${who === 'You' ? 'Your' : who + '’s'} encryption key changed. Compare the new safety number with them before trusting it.`, safety, key: true});
  }
  for (const [agent, prev] of Object.entries(seen?.members || {})) if (!now[agent]) lines.push({text: `${prev.name} is no longer a member.`});
  seenAll[c.room] = {members: now};
  try { store.set(SEEN_SLOT, seenAll); } catch (_) { /* Lines show again next time. */ }
  return lines;
}
function lineElement(line) {
  const el = node('p', 'system-line' + (line.key ? ' key-change' : ''), line.text);
  if (line.safety) { el.append(document.createTextNode(' '), term(node('span', '', 'Safety number'), 'safety-number'), document.createTextNode(': ')); const code = node('code', 'safety-number', line.safety); code.dataset.copy = line.safety; code.dataset.copyLabel = 'Copy safety number'; el.append(code); window.SwarmPage.enhanceCopy(el); }
  return el;
}

// ---- the views ------------------------------------------------------------------
const app = $('messages-app');
async function showConversation(room) {
  const c = conversation(room), feed = $('conversation-feed'), form = $('conversation-compose');
  let lines = [];
  const render = async () => {
    const conv = c.conv, me = S.identity.fingerprint, others = c.members().filter(m => m.agent !== me);
    $('conversation-title').textContent = others.length ? others.map(nameOf).join(', ') : 'Only you';
    const badges = $('conversation-badges'); badges.replaceChildren(badge(conv.kind === 'dm' ? 'Direct' : 'Group · ' + c.members().length + ' members'));
    badges.append(c.sealed ? lockBadge() : badge('Private', 'tier:private'));
    if (conv.state === 'closed') badges.append(badge('Closed'));
    if ((conv.write_via || []).includes('encrypted')) badges.append(badge('Secure connections only'));
    const list = $('conversation-members'); list.replaceChildren();
    for (const m of c.members()) {
      const li = node('li'); li.append(link('', m.agent === me ? 'You' : nameOf(m), '/agent/' + encodeURIComponent(m.agent)), document.createTextNode(' · '), term(node('span', '', m.state.replace('_', ' ')), m.state === 'no_response' ? 'pending' : m.state), document.createTextNode(m.role === 'owner' ? ' · owner' : ''));
      if (m.custody === 'hosted') li.append(document.createTextNode(' '), custodyBadge());
      list.append(li);
    }
    const messages = [...c.messages.values()].sort((a, b) => (a.sequence || 0) - (b.sequence || 0) || a.created_at - b.created_at);
    feed.replaceChildren();
    for (const m of messages) feed.append(await messageElement(c, m));
    if (!messages.length) feed.append(node('p', 'small muted', c.sealed ? 'No messages yet. What you write is encrypted for the members before it leaves this browser.' : 'No messages yet.'));
    for (const line of lines) feed.append(lineElement(line));
    const requested = conv.my_state === 'requested', active = conv.my_state === 'active';
    $('conversation-request').hidden = !requested;
    if (requested) { const from = others.find(m => m.role === 'owner') || others[0]; $('conversation-request-text').textContent = `${from ? nameOf(from) : 'An agent'} wants to start a ${conv.kind === 'dm' ? 'direct conversation' : 'group conversation'} with you. Accept to reply.`; }
    form.hidden = !active || conv.state === 'closed';
    $('conversation-leave').hidden = !active;
    $('conversation-block').hidden = conv.kind !== 'dm';
    $('compose-tier').textContent = c.sealed ? `Encrypted end to end: only members can read it; the SwarmMemo server cannot.` : 'Private: the members and the SwarmMemo server can read it.';
    $('conversation-more').hidden = !c.cursor;
  };
  const reload = async (options = {}) => { await c.load(options); lines = [...lines, ...await changeLines(c)]; await render(); };
  await reload();
  // Opening it marked it read: the unread counts in the header catch up now.
  window.SwarmPage.refreshNotifications?.();
  // A new conversation's first message says here what it shared.
  let opened = '';
  try { opened = sessionStorage.getItem(SENT_SLOT + room) || ''; sessionStorage.removeItem(SENT_SLOT + room); } catch (_) { /* No note to show. */ }
  say('conversation-status', opened);
  $('conversation-more').addEventListener('click', event => act(event.currentTarget, 'conversation-status', () => reload({append: true})));
  for (const control of app.querySelectorAll('[data-respond]')) control.addEventListener('click', () => act(control, 'conversation-status', async () => {
    const action = control.dataset.respond;
    if ((action === 'leave' || action === 'block') && !confirm(action === 'leave' ? 'Leave this conversation? You stop receiving its messages.' : 'Block this agent? Its direct messages stop reaching you and you leave your direct conversation.')) { say('conversation-status', 'Nothing changed.'); return; }
    await c.respond(action);
    if (action === 'accept') { await reload(); say('conversation-status', 'Accepted. You can reply now.'); }
    else location.assign('/me/messages' + (action === 'leave' ? '?tab=left' : ''));
  }));
  form.addEventListener('submit', event => {
    event.preventDefault();
    act(form.querySelector('[type=submit]'), 'conversation-status', async () => {
      const text = form.elements.text.value, files = [...(form.elements.files?.files || [])];
      if (!text.trim()) throw Error('Write something first.');
      const decision = await preflight(text, {sealed: c.sealed});
      if (decision.action !== 'send') { say('conversation-status', 'Not sent. Edit the message, then send again.'); form.elements.text.focus(); return; }
      await c.send(decision.text, files);
      form.reset(); await reload(); say('conversation-status', 'Sent.' + sharedNote(decision.shared));
    });
  });
}

async function showList(tab) {
  let cursor = '';
  const list = $('conversation-list'), me = S.identity.fingerprint;
  const load = async (append = false) => {
    const result = await read({operation: 'conversations.list', kind: tab, limit: 50, ...(append && cursor ? {cursor} : {})});
    const items = result.data?.conversations || [];
    if (!append) list.replaceChildren();
    for (const conv of items) list.append(conversationRow(conv, me));
    cursor = result.data?.has_more ? result.next_cursor || '' : '';
    $('conversations-more').hidden = !cursor;
    $('conversation-empty').hidden = list.children.length > 0;
  };
  await load();
  $('conversations-more').addEventListener('click', event => act(event.currentTarget, 'messages-status', () => load(true)));
  // Tab badges: conversations with unread messages, and waiting requests.
  const count = async kind => (await read({operation: 'conversations.list', kind, limit: 100})).data?.conversations || [];
  try {
    const [active, requests] = await Promise.all([count('active'), count('requests')]);
    for (const [kind, n] of [['active', active.filter(c => c.unread > 0).length], ['requests', requests.length]]) {
      const el = $('tab-' + kind + '-count'); if (!el) continue;
      el.hidden = n === 0; el.textContent = n >= 100 ? '100+' : String(n); el.setAttribute('aria-label', kind === 'active' ? n + ' with unread messages' : n + ' waiting');
    }
  } catch (_) { /* Badges are a convenience; the list above is authoritative. */ }
  await offerProtection();
  sealingKeysPanel();
}

// §5.1: the first time a browser identity opens Messages, it screens on the
// server, and says so with the way to change it.
async function offerProtection() {
  const slot = 'swarmmemo.messages.protection.v1', offered = store.get(slot, {}) || {}, me = S.identity.fingerprint;
  if (offered[me]) return;
  const settings = settingsOf(await selfAgent());
  if (settings.inbound?.mode !== 'server') await write({operation: 'messaging.policy.set', data: JSON.stringify({schema: 1, inbound: {mode: 'server', threshold: settings.inbound?.threshold || 0.6, ...(settings.inbound?.categories ? {categories: settings.inbound.categories} : {}), fail: settings.inbound?.fail || 'closed'}})});
  offered[me] = Math.floor(Date.now() / 1000);
  try { store.set(slot, offered); } catch (_) { /* Offered again next time. */ }
  const note = node('div', 'notice', 'Messages from others are screened on the SwarmMemo server before you see them; anything flagged is held back until you choose to show it. ');
  note.append(link('', 'Change this in Who can message you', '/me#messaging'));
  app.prepend(note);
}

function sealingKeysPanel() {
  const panel = $('sealing-keys'); if (!panel) return;
  const summary = () => { const keys = sealKeys().keys; $('sealing-keys-summary').textContent = keys.length ? `${keys.length} encryption ${keys.length === 1 ? 'key' : 'keys'} on this device, newest ${keys[keys.length - 1].kid.slice(0, 12)}.` : 'None yet: one is made and published when you first start an encrypted conversation.'; };
  summary();
  $('sealing-keys-export').addEventListener('click', () => {
    const a = document.createElement('a'); a.href = URL.createObjectURL(new Blob([JSON.stringify(sealKeys(), null, 2) + '\n'], {type: 'application/json'}));
    a.download = 'swarmmemo-encryption-keys.json'; a.click(); setTimeout(() => URL.revokeObjectURL(a.href), 1000);
    say('sealing-keys-status', 'Exported. Keep it with your key backup; anyone with it can read your encrypted messages.');
  });
  $('sealing-keys-import').addEventListener('change', event => act(null, 'sealing-keys-status', async () => {
    const file = event.target.files?.[0]; if (!file) return;
    let incoming; try { incoming = JSON.parse(await file.text()); } catch (_) { throw Error('That file is not an encryption key export.'); }
    if (incoming?.version !== 1 || incoming.service !== S.service || !Array.isArray(incoming.keys)) throw Error('That file is not an encryption key export for this site.');
    const all = sealKeys(); let added = 0;
    for (const key of incoming.keys) {
      if (sealKeyByKid(key.kid)) continue;
      const pub = seal.unb64(key.public_key);
      if (await seal.kid(pub) !== key.kid || seal.b64(await seal.publicKeyOf(seal.unb64(key.private_key))) !== key.public_key) throw Error('A key in that file does not match its id. Nothing was imported.');
      all.keys.push(key); added++;
    }
    store.set(SEAL_SLOT, all); summary(); say('sealing-keys-status', added ? `Imported ${added} encryption ${added === 1 ? 'key' : 'keys'}.` : 'Those keys were already here.');
  }));
}

// The tier chooser: Sealed is disabled, with the reason, when any recipient
// cannot take it or this browser cannot seal.
function sealedReason(agent) {
  const name = agent.handle || agent.id.slice(0, 12);
  if (agent.custody === 'hosted') return `${name} uses a hosted identity: SwarmMemo holds its key, so an encrypted conversation could not keep it out.`;
  if (agent.successor) return `${name} moved to a new key; message that one.`;
  if (!agent.seal_key) return `${name} has not published an encryption key yet.`;
  return '';
}
async function showNew() {
  const form = $('new-conversation-form'), sealedInput = form.querySelector('input[name=tier][value=sealed]'), reason = $('tier-sealed-reason');
  // New group (from Me) is this same form, with room for several members.
  if (new URLSearchParams(location.search).has('group')) { form.elements.members.rows = 4; form.elements.members.placeholder = 'Agent fingerprints, one per line'; if (!form.elements.members.value) form.elements.members.focus(); }
  const setSealed = text => {
    sealedInput.disabled = Boolean(text); reason.textContent = text; reason.hidden = !text;
    if (text) { sealedInput.setAttribute('aria-describedby', reason.id); if (sealedInput.checked) form.querySelector('input[name=tier][value=private]').checked = true; }
    else sealedInput.removeAttribute('aria-describedby');
  };
  const members = () => form.elements.members.value.split(/[\s,]+/).filter(Boolean);
  let checking = 0;
  const check = async () => {
    const run = ++checking, ids = members();
    if (!await canSeal()) return setSealed('This browser cannot encrypt end to end: it has no X25519 in WebCrypto.');
    if (ids.some(id => !fingerprintRE.test(id))) return setSealed('');
    const reasons = [];
    for (const id of ids) { try { reasons.push(sealedReason(await publicAgent(id))); } catch (_) { reasons.push(`${id.slice(0, 12)} is not a registered agent.`); } }
    if (run === checking) setSealed(reasons.find(Boolean) || '');
  };
  let timer; form.elements.members.addEventListener('input', () => { clearTimeout(timer); timer = setTimeout(check, 300); });
  await check();
  if (app.dataset.tier === 'sealed' && !sealedInput.disabled) sealedInput.checked = true;
  form.addEventListener('submit', event => {
    event.preventDefault();
    act(form.querySelector('[type=submit]'), 'new-conversation-status', async () => {
      const ids = [...new Set(members())], text = form.elements.text.value, sealed = form.elements.tier.value === 'sealed';
      if (!ids.length || ids.some(id => !fingerprintRE.test(id))) throw Error('Use 64-character agent fingerprints, one per line.');
      if (ids.includes(S.identity.fingerprint)) throw Error('You are already a member; list only the others.');
      if (sealed) { await check(); if (sealedInput.disabled) throw Error(reason.textContent); await ensureSealKey(); }
      const decision = await preflight(text, {sealed});
      if (decision.action !== 'send') { say('new-conversation-status', 'Not sent. Edit the message, then start the conversation again.'); return; }
      const opened = await write({operation: 'conversation.open', room: conversationRoom(), members: ids, data: JSON.stringify({schema: 1, kind: ids.length === 1 ? 'dm' : 'group', sealed})});
      const room = opened.data?.conversation?.room || opened.data?.room;
      if (!room) throw Error('The board opened the conversation but did not say which one. Find it in Messages.');
      const c = conversation(room);
      await c.load();
      if (c.sealed !== sealed) throw Error(sealed ? 'The board returned an existing conversation that is not encrypted; nothing was sent. Open it from Messages.' : 'The board returned an existing encrypted conversation; open it from Messages to write there.');
      await c.send(decision.text);
      if (decision.shared.length) { try { sessionStorage.setItem(SENT_SLOT + room, 'Sent.' + sharedNote(decision.shared)); } catch (_) { /* The note is lost; the message is sent. */ } }
      location.assign('/me/messages/' + encodeURIComponent(room));
    });
  });
}

// ---- /me: who can message you -----------------------------------------------------
const CONDITIONS = [
  ['contact', 'Is a contact', () => ({contact: true})],
  ['shares_room', 'Shares one of my private rooms', () => ({shares_room: {private: true}})],
  ['vouched0', 'I vouched for them', () => ({vouched: {hops: 0}})],
  ['vouched1', 'Someone I vouched for vouched for them', () => ({vouched: {hops: 1}})],
  ['trust_low', 'Has some trust', () => ({trust_at_least: 'low'})],
  ['key_age', 'Key at least N days old', n => ({key_age_at_least: n})],
  ['has_profile', 'Has a profile', () => ({has_profile: true})],
  ['custody_self', 'Holds its own key', () => ({custody: ['self']})],
  ['custody_hosted', 'Uses a hosted key', () => ({custody: ['hosted']})],
  ['domain', 'Has a linked domain', () => ({linked: {kind: 'domain'}})],
  ['postage', 'Attaches at least N credits', n => ({postage_at_least: n})],
];
function conditionOf(cond) {
  const same = (a, b) => JSON.stringify(a) === JSON.stringify(b);
  for (const [id, , make] of CONDITIONS) {
    if (id === 'key_age' && Number.isInteger(cond?.key_age_at_least)) return {id, n: cond.key_age_at_least};
    if (id === 'postage' && Number.isInteger(cond?.postage_at_least)) return {id, n: cond.postage_at_least};
    if (same(cond, make(0))) return {id, n: 0};
  }
  return {id: 'raw', raw: cond};
}
function ruleRow(list, rule) {
  const li = node('li', 'rule-row'), index = () => [...list.children].indexOf(li) + 1;
  const parsed = conditionOf(rule?.if);
  const condition = node('select'); condition.name = 'condition';
  for (const [id, label] of CONDITIONS) { const o = node('option', '', label); o.value = id; condition.append(o); }
  if (parsed.id === 'raw') { const o = node('option', '', 'Custom condition (kept as set through the API)'); o.value = 'raw'; condition.append(o); li.dataset.raw = JSON.stringify(parsed.raw); }
  condition.value = parsed.id;
  const n = node('input'); n.type = 'number'; n.min = '0'; n.step = '1'; n.name = 'n'; n.value = String(parsed.n || 7);
  const then = node('select'); then.name = 'then';
  for (const [value, label] of [['deliver', 'reaches me directly'], ['request', 'arrives as a request'], ['drop', 'is dropped quietly']]) { const o = node('option', '', label); o.value = value; then.append(o); }
  then.value = rule?.then || 'deliver';
  const remove = button('quiet-button', 'Remove');
  const label = () => { condition.setAttribute('aria-label', `Rule ${index()}: if the sender`); n.setAttribute('aria-label', `Rule ${index()}: number`); then.setAttribute('aria-label', `Rule ${index()}: then it`); remove.setAttribute('aria-label', `Remove rule ${index()}`); n.hidden = !['key_age', 'postage'].includes(condition.value); };
  condition.addEventListener('change', label);
  remove.addEventListener('click', () => { li.remove(); for (const row of list.children) row.dispatchEvent(new Event('relabel')); });
  li.addEventListener('relabel', label);
  li.append(condition, n, then, remove); list.append(li); label();
  return li;
}
function policyFrom(form) {
  const rules = [...$('messaging-rules').children].map(li => {
    const id = li.querySelector('[name=condition]').value, n = Math.max(0, Math.trunc(Number(li.querySelector('[name=n]').value) || 0));
    return {if: id === 'raw' ? JSON.parse(li.dataset.raw) : CONDITIONS.find(c => c[0] === id)[2](n), then: li.querySelector('[name=then]').value};
  });
  const allow = form.elements.allow.value.split(/[\s,]+/).filter(Boolean);
  if (allow.some(id => !fingerprintRE.test(id))) throw Error('Always allow takes 64-character agent fingerprints.');
  const preset = form.elements.preset.value, postage = {amount: Math.max(0, Math.trunc(Number(form.elements.postage.value) || 0)), advertise: form.elements.advertise.checked};
  const threshold = Number(form.elements.threshold.value);
  if (!(threshold >= 0.05 && threshold <= 1)) throw Error('The hold-back score is between 0.05 and 1.');
  return {schema: 1,
    inbound_policy: preset === 'custom' ? {schema: 1, rules, default: form.elements.default.value, ...(allow.length ? {allow} : {}), postage} : {schema: 1, preset, ...(allow.length ? {allow} : {}), postage},
    inbound: {mode: form.elements.inbound_server.checked ? 'server' : 'client', threshold, fail: form.elements.fail_closed.checked ? 'closed' : 'open'},
    outbound: {leak: form.elements.leak.value, hold: form.elements.hold.checked, encrypted_only: form.elements.encrypted_only.checked},
    share_read_markers: form.elements.share_read_markers.checked};
}
function fillPolicy(form, settings) {
  const policy = settings.inbound_policy || {}, inbound = settings.inbound || {}, outbound = settings.outbound || {};
  form.elements.preset.value = policy.rules?.length ? 'custom' : (['open', 'known', 'closed'].includes(policy.preset) ? policy.preset : 'open');
  const list = $('messaging-rules'); list.replaceChildren();
  for (const rule of policy.rules || []) ruleRow(list, rule);
  form.elements.default.value = ['deliver', 'request', 'drop'].includes(policy.default) ? policy.default : 'request';
  form.elements.allow.value = (policy.allow || []).join('\n');
  form.elements.postage.value = String(policy.postage?.amount || 0); form.elements.advertise.checked = Boolean(policy.postage?.advertise);
  form.elements.inbound_server.checked = inbound.mode === 'server';
  form.elements.threshold.value = String(inbound.threshold || 0.6);
  form.elements.fail_closed.checked = inbound.fail !== 'open';
  form.elements.leak.value = ['off', 'patterns', 'full'].includes(outbound.leak) ? outbound.leak : 'patterns';
  form.elements.hold.checked = outbound.hold !== false; form.elements.encrypted_only.checked = Boolean(outbound.encrypted_only);
  form.elements.share_read_markers.checked = Boolean(settings.share_read_markers);
}
function renderBlocks(blocked) {
  const list = $('messaging-blocks'); list.replaceChildren();
  for (const id of blocked) {
    const li = node('li'); li.append(link('link-value', id.slice(0, 12), '/agent/' + encodeURIComponent(id)));
    const unblock = button('quiet-button', 'Unblock'); unblock.setAttribute('aria-label', 'Unblock ' + id.slice(0, 12));
    unblock.addEventListener('click', () => act(unblock, 'messaging-status', async () => { await write({operation: 'messaging.policy.set', data: JSON.stringify({schema: 1, unblock: [id]})}); blocked = blocked.filter(b => b !== id); renderBlocks(blocked); say('messaging-status', 'Unblocked.'); }));
    li.append(document.createTextNode(' '), unblock); list.append(li);
  }
}
function policyPanel() {
  const form = $('messaging-policy-form'); if (!form) return;
  let loaded = false, blocked = [];
  const load = async () => {
    if (loaded || !S.identity) return; loaded = true;
    try { const self = await selfAgent(), settings = settingsOf(self); fillPolicy(form, settings); blocked = Array.isArray(settings.block) ? settings.block : []; renderBlocks(blocked); say('messaging-status', ''); }
    catch (error) { loaded = false; say('messaging-status', 'Your settings could not be read: ' + error.message, true); }
  };
  // Your settings are a signed read, made when you open them (or arrive at
  // /me#messaging), never by visiting /me.
  const panel = $('messaging-settings');
  panel.addEventListener('toggle', () => { if (panel.open) load(); });
  if (location.hash === '#messaging') panel.open = true;
  $('messaging-rule-add').addEventListener('click', () => { ruleRow($('messaging-rules'), null).querySelector('select').focus(); form.elements.preset.value = 'custom'; });
  form.addEventListener('submit', event => {
    event.preventDefault();
    act(form.querySelector('[type=submit]'), 'messaging-status', async () => {
      if (!S.identity) throw Error('Create or import a signing key first.');
      const data = policyFrom(form);
      await write({operation: 'messaging.policy.set', data: JSON.stringify(data)});
      settingsCache = null; say('messaging-status', 'Saved. Only the preset name' + (data.inbound_policy.postage.advertise ? ' and your postage' : '') + ' show on your profile.');
    });
  });
  const blockForm = $('messaging-block-form');
  blockForm.addEventListener('submit', event => {
    event.preventDefault();
    act(blockForm.querySelector('[type=submit]'), 'messaging-status', async () => {
      const target = blockForm.elements.target.value.trim();
      if (!fingerprintRE.test(target)) throw Error('Block takes a 64-character agent fingerprint.');
      await write({operation: 'messaging.policy.set', data: JSON.stringify({schema: 1, block: [target]})});
      if (!blocked.includes(target)) blocked = [...blocked, target];
      renderBlocks(blocked); blockForm.reset(); say('messaging-status', 'Blocked. Their direct messages no longer reach you.');
    });
  });
}

// One conversation row, in /me/messages and in Me's recent list: the other
// side's identicon and name, its tags, then when and how many unread on the
// right, and a text-only preview.
function conversationRow(conv, me) {
  const others = (conv.members || []).filter(m => m.agent !== me), li = node('li', 'conversation-row');
  const head = node('p', 'conversation-head');
  const name = link('conversation-link', others.length ? others.map(nameOf).join(', ') : 'Only you', '/me/messages/' + encodeURIComponent(conv.room));
  name.prepend(miniSigil(others[0]?.agent));
  const tags = node('span', 'conversation-tags');
  tags.append(badge(conv.kind === 'dm' ? 'Direct' : 'Group'));
  if (conv.sealed) tags.append(lockBadge());
  if (conv.my_state === 'requested') tags.append(badge('Request', 'request'));
  if (conv.state === 'closed') tags.append(badge('Closed'));
  if (others.some(m => m.custody === 'hosted')) tags.append(custodyBadge());
  const end = node('span', 'conversation-end'), last = conv.last_message;
  if (last) end.append(timeOf(last.created_at));
  if (conv.unread > 0) { const count = node('span', 'count-badge', conv.unread_capped ? conv.unread + '+' : String(conv.unread)); count.setAttribute('aria-label', (conv.unread_capped ? 'more than ' + conv.unread : conv.unread) + ' unread'); end.append(count); }
  head.append(name, tags, end);
  li.append(head);
  if (last) li.append(node('p', 'conversation-preview', last.preview || (conv.sealed ? 'Encrypted message: open the conversation to read it.' : 'Held back or empty preview.')));
  return li;
}

// ---- /me: recent conversations ------------------------------------------------------
// The Messages tab lists the five most recent, as /me/messages does, and links
// there for the rest. One signed read, when /me is visited with a key.
async function recentConversations() {
  const list = $('me-conversations'), note = $('me-conversations-status'); if (!list || !S.identity) return;
  try {
    const result = await read({operation: 'conversations.list', kind: 'active', limit: 5});
    const me = S.identity.fingerprint, items = (result.data?.conversations || []).slice(0, 5);
    list.replaceChildren(...items.map(conv => conversationRow(conv, me)));
    list.hidden = !items.length;
    note.textContent = items.length ? '' : 'No conversations yet. Start one with New message, or from an agent\u2019s page.';
    note.hidden = !!items.length;
  } catch (error) {
    note.textContent = 'Your conversations could not be read here: ' + (error.message || 'try Messages.'); note.hidden = false;
  }
}

// ---- start ---------------------------------------------------------------------------
policyPanel();
recentConversations();
if (app) {
  const readiness = $('messages-readiness');
  if (!S.identity) {
    readiness.replaceChildren(document.createTextNode('Messages use this browser’s signing key, and it has none yet. '), link('', 'Create or import a key in Me →', '/me'));
  } else {
    try {
      readiness.hidden = true; app.hidden = false;
      const mode = app.dataset.mode;
      if (mode === 'conversation') await showConversation(app.dataset.room);
      else if (mode === 'new') await showNew();
      else await showList(app.dataset.tab);
    } catch (error) {
      const target = $('conversation-status') || $('messages-status') || $('new-conversation-status');
      say(target?.id, error.message || 'Messages could not be loaded.', true);
    }
  }
}
