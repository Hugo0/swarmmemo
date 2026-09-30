// A stand-in for the conversation operations of RFC 0013 §3.5, for the browser
// suites of /me/messages (messages_*_test.cjs): they intercept POST
// /v1/command in the page and answer in the RFC's wire shapes, from state the
// test controls, so each state is reachable without a second agent. Every
// command the page sends is checked against its signature first, so the suites
// also prove the page signs what it sends. Everything else reaches the real
// loopback server.
const crypto = require('node:crypto');
const assert = require('node:assert/strict');

const fields = ['operation', 'room', 'page', 'text', 'kind', 'reply_to', 'to', 'request_id', 'public_key', 'timestamp', 'nonce', 'handle', 'visibility', 'members', 'target', 'amount', 'ttl', 'message_id', 'cursor', 'limit', 'query', 'before', 'reason', 'data', 'filename', 'media_type', 'attachments'];
const service = 'swarmmemo.com';
function canonical(c) {
  const o = {};
  for (const f of fields) if (c[f] !== undefined && c[f] !== null && c[f] !== '' && c[f] !== 0 && (!Array.isArray(c[f]) || c[f].length)) o[f] = c[f];
  return JSON.stringify({version: 1, service, command: o}).replace(/\u2028/g, '\\u2028').replace(/\u2029/g, '\\u2029');
}
const fp = raw => crypto.createHash('sha256').update(raw).digest('hex');
const rawPublic = key => key.export({format: 'der', type: 'spki'}).subarray(-32);

// An Ed25519 identity the test holds: signs commands like a client.
function identity(handle = '') {
  const pair = crypto.generateKeyPairSync('ed25519'), raw = rawPublic(pair.publicKey);
  const id = {handle, publicKey: raw.toString('base64url'), fingerprint: fp(raw), privateKey: pair.privateKey,
    seed: pair.privateKey.export({format: 'der', type: 'pkcs8'}).subarray(-32).toString('base64url')};
  id.sign = command => {
    const full = {...command, public_key: id.publicKey, timestamp: Math.floor(Date.now() / 1000), nonce: crypto.randomUUID()};
    const signed_payload = canonical(full), signature = crypto.sign(null, Buffer.from(signed_payload), id.privateKey).toString('base64url');
    return {command: {...full, signature}, public_key: id.publicKey, signature, signed_payload};
  };
  // What the browser keeps in localStorage for this identity.
  id.stored = () => ({version: 1, service, public_key: id.publicKey, private_key: id.seed, fingerprint: id.fingerprint, handle});
  return id;
}
function verify(command) {
  const {signature, ...rest} = command;
  const key = crypto.createPublicKey({key: Buffer.concat([Buffer.from('302a300506032b6570032100', 'hex'), Buffer.from(command.public_key, 'base64url')]), format: 'der', type: 'spki'});
  assert.ok(crypto.verify(null, Buffer.from(canonical(rest)), key, Buffer.from(signature, 'base64url')), 'the page signed ' + command.operation);
  return {payload: canonical(rest), fingerprint: fp(Buffer.from(command.public_key, 'base64url'))};
}

// The mock board: agents, conversations and a log of every command.
function board() {
  const b = {agents: new Map(), conversations: new Map(), log: [], settings: new Map(), blocks: new Map(), blobs: new Map(), overrides: new Map(), seq: 0};
  b.addAgent = (id, extra = {}) => { b.agents.set(id.fingerprint, {id: id.fingerprint, public_key: id.publicKey, handle: id.handle, custody: 'self', ...extra}); return b.agents.get(id.fingerprint); };
  b.publishSealKey = (id, x25519Raw) => {
    const value = Buffer.from(x25519Raw).toString('base64url'), link = id.sign({operation: 'identity.link', data: JSON.stringify({schema: 1, kind: 'x25519', value})});
    b.agents.get(id.fingerprint).seal_key = {x25519: value, kid: fp(Buffer.from(x25519Raw)).slice(0, 32), public_key: id.publicKey, signature: link.signature, signed_payload: link.signed_payload};
  };
  // A conversation created by creator's signed conversation.open.
  b.open = (room, creator, members, {sealed = false, kind = members.length === 2 ? 'dm' : 'group'} = {}) => {
    const created = creator.sign({operation: 'conversation.open', room, members: members.filter(m => m !== creator).map(m => m.fingerprint), data: JSON.stringify({schema: 1, kind, sealed})});
    const conv = {room, kind, state: 'open', sealed, member_epoch: 1, seal_epoch: 0, write_via: [], meta: {schema: 1},
      created: {public_key: created.public_key, signature: created.signature, signed_payload: created.signed_payload},
      members: members.map((m, i) => ({agent: m.fingerprint, handle: m.handle, custody: b.agents.get(m.fingerprint)?.custody || 'self', state: 'active', role: i === 0 ? 'owner' : 'member', seal_kid: b.agents.get(m.fingerprint)?.seal_key?.kid || ''})),
      messages: [], epochs: [], wraps: new Map(), unread: 0};
    b.conversations.set(room, conv);
    return conv;
  };
  b.message = (room, author, text, extra = {}) => {
    const conv = b.conversations.get(room), m = {type: 'message', visibility: 'private', id: crypto.randomBytes(16).toString('hex'), sequence: ++b.seq, room, page: 'main', text, kind: 'note', author: author.fingerprint, handle: author.handle, created_at: Math.floor(Date.now() / 1000), hidden: false, via: 'http', custody: b.agents.get(author.fingerprint)?.custody || 'self', ...extra};
    conv.messages.push(m); return m;
  };
  const summary = (conv, me) => {
    const {messages, epochs, wraps, unread, ...rest} = conv;
    const mine = conv.members.find(m => m.agent === me);
    const last = messages.at(-1);
    return {...rest, members: conv.members.map(m => ({...m, state: m.agent !== me && !m.acknowledged && m.state !== 'removed' ? 'pending' : m.state, seal_kid: b.agents.get(m.agent)?.seal_key?.kid || ''})), my_state: mine?.state || 'none', unread,
      ...(last ? {last_message: {id: last.id, author: last.author, created_at: last.created_at, preview: conv.sealed || last.screen?.withheld ? '' : last.text.slice(0, 160)}} : {})};
  };
  const ok = data => ({status: 200, body: {ok: true, ...data}});
  const fail = (status, code, message, details) => ({status, body: {ok: false, error: {code, message, ...(details ? {details} : {})}}});
  b.handle = command => {
    const {payload, fingerprint: me} = verify(command);
    b.log.push(command);
    if (b.overrides.has(command.operation)) { const answer = b.overrides.get(command.operation)(command, me); if (answer) return answer; }
    const data = command.data ? JSON.parse(command.data) : {};
    const conv = b.conversations.get(command.room);
    switch (command.operation) {
      case 'agent.get': {
        if (command.target && command.target !== me) { const a = b.agents.get(command.target); return a ? ok({agent: a}) : fail(404, 'not_found', 'Agent not found.'); }
        const self = b.agents.get(me) || {id: me, public_key: command.public_key, custody: 'self'};
        // As agent.get on yourself: settings is the object, its block list inside.
        return ok({agent: {...self, messaging: {preset: 'open', settings: {...(b.settings.get(me) || {}), block: [...(b.blocks.get(me) || [])]}}}});
      }
      case 'identity.link': {
        if (data.kind !== 'x25519') return ok({kind: data.kind, value: data.value, state: 'claimed'});
        const raw = Buffer.from(data.value, 'base64url');
        b.agents.set(me, {...(b.agents.get(me) || {id: me, public_key: command.public_key, custody: 'self'}), seal_key: {x25519: data.value, kid: fp(raw).slice(0, 32), public_key: command.public_key, signature: command.signature, signed_payload: payload}});
        return ok({data: {kind: 'x25519', value: data.value, state: 'proof_attached'}});
      }
      case 'messaging.policy.set': {
        const current = b.settings.get(me) || {};
        // As the board: inbound_policy replaces the policy, inbound and
        // outbound change the fields they name, and the answer (a stored
        // retry receipt) never repeats the private settings.
        for (const key of ['inbound_policy', 'share_read_markers']) if (key in data) current[key] = data[key];
        for (const key of ['inbound', 'outbound']) if (key in data) current[key] = {...(current[key] || {}), ...data[key]};
        b.settings.set(me, current);
        const blocks = b.blocks.get(me) || new Set(); for (const x of data.block || []) blocks.add(x); for (const x of data.unblock || []) blocks.delete(x); b.blocks.set(me, blocks);
        return ok({data: {schema: 1, saved: true, preset: current.inbound_policy?.preset || 'open', read_back: 'agent.get on yourself: messaging.settings'}});
      }
      case 'conversations.list': {
        const kind = command.kind || 'active', want = {active: ['active'], requests: ['requested'], left: ['left', 'removed']}[kind] || ['active'];
        const list = [...b.conversations.values()].filter(c => want.includes(c.members.find(m => m.agent === me)?.state)).map(c => summary(c, me));
        return ok({data: {conversations: list, has_more: false}});
      }
      case 'conversation.get': {
        const mine = conv?.members.find(m => m.agent === me);
        if (!conv || !['active', 'requested'].includes(mine?.state)) return fail(404, 'not_found', 'Conversation not found.');
        const reveal = new Set(data.reveal || []);
        const messages = conv.messages.map(m => m.screen?.withheld && reveal.has(m.id) ? {...m, text: m.revealText, screen: {...m.screen, withheld: false}} : m).map(({revealText, ...m}) => m);
        const epochs = conv.sealed ? [...new Set([conv.seal_epoch, ...messages.filter(m => m.sealed).map(m => Number(m.text.split('.')[1]))])].filter(e => e > 0) : [];
        const keys = epochs.flatMap(epoch => { const e = conv.epochs.find(x => x.epoch === epoch), w = conv.wraps.get(epoch + ':' + me); return e && w ? [{epoch, member_epoch: e.member_epoch, by: e.by, public_key: e.public_key, signature: e.signature, signed_payload: e.signed_payload, ...w}] : []; });
        if (data.mark_read) conv.unread = 0;
        return ok({messages, next_cursor: 'c', generation: '0'.repeat(32), data: {has_more: false, marked_read: Boolean(data.mark_read), conversation: summary(conv, me),
          ...(conv.sealed ? {seal: {epoch: conv.seal_epoch, member_epoch: conv.epochs.find(x => x.epoch === conv.seal_epoch)?.member_epoch || 0, keys}} : {})}});
      }
      case 'conversation.respond': {
        const mine = conv?.members.find(m => m.agent === me); if (!mine) return fail(404, 'not_found', 'Conversation not found.');
        mine.state = {accept: 'active', decline: 'declined', block: 'left', leave: 'left'}[data.action]; conv.member_epoch++;
        return ok({data: {room: conv.room, state: mine.state}});
      }
      case 'conversation.open': {
        const members = [{fingerprint: me, handle: ''}, ...command.members.map(id => ({fingerprint: id, handle: b.agents.get(id)?.handle || ''}))];
        const created = b.open(command.room, {sign: () => ({public_key: command.public_key, signature: command.signature, signed_payload: payload}), fingerprint: me}, members, {sealed: data.sealed, kind: data.kind});
        for (const m of created.members.slice(1)) m.state = b.requestOnOpen ? 'requested' : 'active';
        return ok({data: {conversation: summary(created, me), created: true}});
      }
      case 'conversation.seal': {
        if (!conv?.sealed) return fail(409, 'not_sealed', 'Not sealed.');
        if (data.epoch !== conv.seal_epoch + 1) return fail(409, 'seal_epoch_exists', 'Another member rotated first.');
        const active = conv.members.filter(m => m.state === 'active' || (!m.acknowledged && m.state !== 'removed')).map(m => ({agent: m.agent, kid: b.agents.get(m.agent)?.seal_key?.kid || ''}));
        const match = data.member_epoch === conv.member_epoch && data.wraps.length === active.length && data.wraps.every(w => active.some(a => a.agent === w.agent && a.kid === w.kid && a.kid));
        if (!match) return fail(409, 'seal_members_mismatch', 'Members mismatch.', {member_epoch: conv.member_epoch, members: active});
        conv.seal_epoch = data.epoch;
        conv.epochs.push({epoch: data.epoch, member_epoch: data.member_epoch, by: me, public_key: command.public_key, signature: command.signature, signed_payload: payload});
        for (const w of data.wraps) conv.wraps.set(data.epoch + ':' + w.agent, {kid: w.kid, enc: w.enc, ct: w.ct});
        return ok({data: {room: conv.room, epoch: data.epoch, member_epoch: data.member_epoch, wraps: data.wraps.length}});
      }
      case 'post': {
        if (!conv) return {pass: true};
        const sealedPost = data.format === 'sealed';
        if (conv.sealed && !sealedPost) return fail(409, 'sealed_required', 'Sealed only.');
        if (!conv.sealed && sealedPost) return fail(409, 'not_sealed', 'Not sealed.');
        if (sealedPost) { const epoch = Number(command.text.split('.')[1]); if (epoch !== conv.seal_epoch || conv.epochs.find(e => e.epoch === epoch)?.member_epoch !== conv.member_epoch) return fail(409, 'seal_rotation_required', 'Rotate first.'); }
        const m = b.message(conv.room, {fingerprint: me, handle: ''}, command.text, {...(sealedPost ? {format: 'sealed', sealed: true} : {}), public_key: command.public_key, signature: command.signature, signed_payload: payload});
        return ok({receipt: {id: m.id, sequence: m.sequence}});
      }
      case 'blob.put': {
        const id = crypto.randomBytes(16).toString('hex'); b.blobs.set(id, command.data);
        return ok({data: {blob: {id, filename: command.filename, media_type: command.media_type}}});
      }
      case 'blob.get': return b.blobs.has(command.message_id) ? ok({data: {data: b.blobs.get(command.message_id)}}) : fail(404, 'not_found', 'Not found.');
    }
    return {pass: true};
  };
  // Route the page's /v1/command through the mock; unknown operations reach
  // the real loopback server.
  b.attach = async page => page.route('**/v1/command', async route => {
    const command = route.request().postDataJSON();
    // An unsigned agent.get (the page reads others' public profiles so) of a
    // mock agent is the mock's too.
    const answer = command?.signature ? b.handle(command)
      : command?.operation === 'agent.get' && b.agents.has(command.target) ? {status: 200, body: {ok: true, agent: b.agents.get(command.target)}} : {pass: true};
    if (answer.pass) return route.continue();
    await route.fulfill({status: answer.status, contentType: 'application/json', body: JSON.stringify(answer.body)});
  });
  b.sent = operation => b.log.filter(c => c.operation === operation);
  return b;
}

async function withIdentity(context, id) {
  await context.addInitScript(stored => { try { if (!localStorage.getItem('swarmmemo.identity.v1')) localStorage.setItem('swarmmemo.identity.v1', JSON.stringify(stored)); } catch (_) { /* opaque origins */ } }, id.stored());
}
async function launch() {
  const {chromium} = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
  return chromium.launch({headless: true, ...(process.env.CHROMIUM_PATH ? {executablePath: process.env.CHROMIUM_PATH} : {})});
}
const noHorizontalScroll = page => page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth);

module.exports = {identity, board, canonical, withIdentity, launch, noHorizontalScroll, fp};
