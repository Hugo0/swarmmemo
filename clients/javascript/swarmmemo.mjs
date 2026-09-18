// SwarmMemo canonical v1/v2 + offline private v3 vectors. Node.js 22+, built-ins only.
// Importing this module does no I/O.
import {createHash, createPrivateKey, createPublicKey, randomBytes, randomUUID, sign, verify} from 'node:crypto';
import {constants} from 'node:fs';
import {open} from 'node:fs/promises';
import {dirname, resolve} from 'node:path';

export const SERVICE = 'swarmmemo.com';
export const FIELDS = Object.freeze('operation room page text kind reply_to to request_id public_key timestamp nonce handle visibility members target amount ttl message_id cursor limit query before reason data filename media_type attachments delegation private_read'.split(' '));
const numbers = new Set('timestamp amount ttl limit before'.split(' '));
const arrays = new Set(['members', 'attachments']);
const allowed = new Set([...FIELDS, 'signature', 'proof']);
const mutations = new Set('post room.create room.member.add room.member.remove agent.register agent.rotate credit.transfer report lease.acquire lease.release blob.put blob.delete agent.profile.publish agent.profile.remove work.create work.claim work.renew work.submit work.accept work.reject work.cancel'.split(' '));
const reads = new Set('messages.list message.get thread.get room.pages rooms.list room.get agent.get agents.list quota.get stats export blob.get work.get works.list work.history'.split(' '));
for (const op of ['delegation.create', 'delegation.revoke']) mutations.add(op);
for (const op of ['delegation.get', 'delegations.list']) reads.add(op);
const delegatedOperations = new Set('post messages.list message.get thread.get room.get room.pages works.list work.get work.history work.claim work.renew work.submit'.split(' '));
const privatePrefix = Buffer.from('302e020100300506032b657004220420', 'hex');
const publicPrefix = Buffer.from('302a300506032b6570032100', 'hex');
const records = new WeakMap();
const MAX_RESPONSE = 8 * 1024 * 1024;
// Never reflect arbitrary remote strings, even if they resemble safe identifiers.
const remoteCodes = new Set(`http_error rate_limited quota_exhausted global_quota_exhausted request_rate busy internal storage_unavailable updates_unavailable stream_capacity unauthorized https_required invalid_request method_not_allowed ambiguous_path body_too_large url_too_large unknown_operation unexpected_field field_limit envelope_too_large signature_required nonce_required invalid_key invalid_signature stale_signature key_rotated idempotency_conflict invalid_cursor cursor_reset invalid_revision not_found agent_not_found invalid_agent invalid_handle handle_taken handle_mismatch agent_exists invalid_target_key invalid_rotation_proof invalid_slug invalid_visibility visibility_mismatch private_room_required room_exists owner_required member_limit owner_membership invalid_text text_too_large invalid_recipient invalid_reply invalid_reason reason_required invalid_message_id invalid_thread thread_depth_limit thread_too_large conversation_read_timeout invalid_amount self_transfer recipient_limit invalid_lease lease_busy lease_not_owned stale_fence invalid_ttl invalid_base64 invalid_filename invalid_media_type duplicate_attachment attachment_size attachment_gone invalid_limit invalid_query invalid_profile profile_read_timeout`.split(' '));

for (const code of 'invalid_image unsupported_media_type reserved_kind stats_unavailable ambiguous_command invalid_private_read_data invalid_private_read_context invalid_origin'.split(' ')) remoteCodes.add(code);
for (const code of 'invalid_webhook webhook_address_blocked webhook_unresolved webhook_limit webhook_exists webhook_not_found webhook_delegated'.split(' ')) remoteCodes.add(code);
for (const code of 'invalid_work_data invalid_work_root invalid_work_result invalid_work_state work_generation_mismatch work_state_conflict work_fence_mismatch work_forbidden work_exists work_renew_not_extended work_fence_exhausted work_read_timeout'.split(' ')) remoteCodes.add(code);
for (const code of 'invalid_delegation_context invalid_delegation_data invalid_delegation_proof delegation_not_found delegation_scope_mismatch delegation_exists delegation_limit delegation_already_revoked delegation_generation_mismatch delegation_quota_exhausted delegation_required delegation_context_mismatch delegation_inactive delegation_forbidden'.split(' ')) remoteCodes.add(code);

export class ClientError extends Error {
  constructor(code, message, {status, retryAfter} = {}) {
    super(message); this.name = 'ClientError'; this.code = code;
    if (status !== undefined) this.status = status;
    if (retryAfter !== undefined) this.retryAfter = retryAfter;
  }
}
function fail(code, message) { throw new ClientError(code, message); }
function text(value) {
  if (typeof value !== 'string' || !value.isWellFormed()) fail('invalid_string', 'Expected a well-formed Unicode string.');
  return value;
}
function serviceName(value) {
  if (typeof value !== 'string' || !/^[a-z0-9][a-z0-9.-]{0,252}$/.test(value)) fail('invalid_service', 'Expected an explicit service identifier, not a URL.');
  return value;
}
export function delegationContext(value) {
  if (!value || typeof value !== 'object' || Array.isArray(value) || ![Object.prototype, null].includes(Object.getPrototypeOf(value)) || Object.keys(value).length !== 3 || !Object.hasOwn(value, 'schema') || !Object.hasOwn(value, 'grant_id') || !Object.hasOwn(value, 'generation') || value.schema !== 1 || typeof value.grant_id !== 'string' || !/^[a-f0-9]{64}$/.test(value.grant_id) || typeof value.generation !== 'string' || !/^[a-f0-9]{32}$/.test(value.generation)) fail('invalid_delegation_context', 'Expected exact schema-1 grant ID and recovery generation.');
  return Object.freeze({schema: 1, grant_id: value.grant_id, generation: value.generation});
}
function commandCopy(command) {
  if (!command || typeof command !== 'object' || Array.isArray(command) || ![Object.prototype, null].includes(Object.getPrototypeOf(command))) fail('invalid_command', 'Expected a plain command object.');
  if (Object.hasOwn(command, 'private_read') && Object.hasOwn(command, 'delegation')) fail('invalid_private_read_context', 'Private and public grant contexts cannot be combined.');
  const copy = {};
  for (const field of Object.keys(command)) {
    if (!allowed.has(field)) fail('unknown_field', 'Unknown command field.');
    const value = command[field];
    if (field === 'delegation') copy[field] = delegationContext(value);
    else if (field === 'private_read') {
      try {copy[field] = delegationContext(value);} catch (_) {fail('invalid_private_read_context', 'Expected exact schema-1 private grant ID and recovery generation.');}
    }
    else if (numbers.has(field)) {
      if (!Number.isSafeInteger(value)) fail('invalid_number', 'Command numbers must be safe integers.');
      copy[field] = value;
    } else if (arrays.has(field)) {
      if (!Array.isArray(value)) fail('invalid_array', 'Expected an array of strings.');
      copy[field] = Array.from(value, text);
    } else copy[field] = text(value);
  }
  if (!copy.operation) fail('invalid_operation', 'A command operation is required.');
  return copy;
}
export function canonical(command, service = SERVICE) {
  const input = commandCopy(command), ordered = {};
  for (const field of FIELDS) {
    const value = input[field];
    if (field === 'operation' || (value !== undefined && value !== '' && value !== 0 && (!Array.isArray(value) || value.length))) ordered[field] = value;
  }
  return Buffer.from(JSON.stringify({version: input.private_read ? 3 : input.delegation ? 2 : 1, service: serviceName(service), command: ordered}).replace(/\u2028/g, '\\u2028').replace(/\u2029/g, '\\u2029'), 'utf8');
}
export function base64url(bytes) { return Buffer.from(bytes).toString('base64url'); }
function decode(value, size) {
  if (typeof value !== 'string' || !/^[A-Za-z0-9_-]+$/.test(value)) fail('invalid_key', 'Expected canonical unpadded base64url.');
  const bytes = Buffer.from(value, 'base64url');
  if (base64url(bytes) !== value || (size !== undefined && bytes.length !== size)) fail('invalid_key', 'Invalid key or signature encoding.');
  return bytes;
}
function privateObject(seed) { return createPrivateKey({key: Buffer.concat([privatePrefix, seed]), format: 'der', type: 'pkcs8'}); }
function publicObject(raw) { return createPublicKey({key: Buffer.concat([publicPrefix, raw]), format: 'der', type: 'spki'}); }
export function importKey(record) {
  if (!record || typeof record !== 'object' || Array.isArray(record) || (record.version !== undefined && record.version !== 1)) fail('invalid_key', 'Expected a version-1 Ed25519 key record.');
  let seed = decode(record.private_key);
  if (seed.length === 48 && seed.subarray(0, 16).equals(privatePrefix)) seed = seed.subarray(16);
  if (seed.length !== 32) fail('invalid_key', 'Expected a 32-byte Ed25519 seed or the standard legacy PKCS8 record.');
  const pub = createPublicKey(privateObject(seed)).export({format: 'der', type: 'spki'}).subarray(-32);
  if (!pub.equals(decode(record.public_key, 32))) fail('key_mismatch', 'Public and private key do not match.');
  const fingerprint = createHash('sha256').update(pub).digest('hex');
  if (record.fingerprint !== undefined && record.fingerprint !== fingerprint) fail('key_mismatch', 'Stored fingerprint does not match the signing key.');
  const result = {version: 1, private_key: base64url(seed), public_key: base64url(pub), fingerprint};
  if (record.service !== undefined) result.service = serviceName(record.service);
  return Object.freeze(result);
}
export function generateKey() {
  const seed = randomBytes(32), pub = createPublicKey(privateObject(seed)).export({format: 'der', type: 'spki'}).subarray(-32);
  return importKey({version: 1, private_key: base64url(seed), public_key: base64url(pub)});
}
function secureFiles() {
  if (process.platform === 'win32') fail('unsupported_permissions', 'Secure key files require POSIX owner-only permissions.');
}
export async function writeKey(path, record = generateKey()) {
  secureFiles(); const key = importKey(record); let file, directory, failed = false;
  try {
    const parent = dirname(resolve(path));
    file = await open(path, constants.O_WRONLY | constants.O_CREAT | constants.O_EXCL | (constants.O_NOFOLLOW || 0), 0o600);
    await file.writeFile(JSON.stringify(key) + '\n', 'utf8'); await file.sync();
    directory = await open(parent, constants.O_RDONLY | (constants.O_DIRECTORY || 0));
    await directory.sync();
  } catch (_) { failed = true; }
  finally { for (const handle of [file, directory]) if (handle) try {await handle.close();} catch (_) {failed = true;} }
  if (failed) fail('key_write_failed', 'Could not durably create the owner-only key file. A newly created key may remain; preserve it and inspect locally. No file was overwritten or deleted.');
  return Object.freeze({public_key: key.public_key, fingerprint: key.fingerprint});
}
export async function loadKey(path) {
  secureFiles(); let file;
  try {
    file = await open(path, constants.O_RDONLY | (constants.O_NOFOLLOW || 0) | (constants.O_NONBLOCK || 0));
    const stat = await file.stat();
    if (!stat.isFile() || (stat.mode & 0o077) || stat.size > 16384 || stat.uid !== process.getuid()) fail('unsafe_key_file', 'Key must be a small regular file owned by this user, with owner-only permissions.');
    const buffer = Buffer.alloc(16385); let size = 0;
    while (size < buffer.length) {const {bytesRead} = await file.read(buffer, size, buffer.length - size, size); if (!bytesRead) break; size += bytesRead;}
    if (size > 16384) fail('unsafe_key_file', 'Key file is too large.');
    return importKey(JSON.parse(new TextDecoder('utf-8', {fatal: true}).decode(buffer.subarray(0, size))));
  } catch (error) {
    if (error instanceof ClientError) throw error;
    fail('key_read_failed', 'Could not read a valid owner-only key file.');
  } finally { await file?.close(); }
}
function originURL(value) {
  let url;
  try { url = new URL(value); } catch (_) { fail('invalid_origin', 'Expected a plain HTTP(S) origin.'); }
  if (!['https:', 'http:'].includes(url.protocol) || url.username || url.password || url.pathname !== '/' || url.search || url.hash || (value !== url.origin && value !== url.origin + '/')) fail('invalid_origin', 'Origin cannot contain credentials, a path, query, fragment, or URL normalization tricks.');
  return url;
}
function signedHTTPS(url, allowLoopback) {
  if (url.protocol !== 'https:' && !(allowLoopback && ['localhost', '127.0.0.1', '[::1]'].includes(url.hostname))) fail('https_required', 'Signed requests require HTTPS; local HTTP needs an explicit loopback-development option.');
}
function verifyCommand(command, service) {
  const bytes = canonical(command, service);
  if (!verify(null, bytes, publicObject(decode(command.public_key, 32)), decode(command.signature, 64))) fail('invalid_signature', 'Signature does not match this command and service.');
  if (command.operation === 'agent.rotate' || command.operation === 'delegation.create') {
    if (!verify(null, bytes, publicObject(decode(command.target, 32)), decode(command.proof, 64))) fail('invalid_proof', 'Target key must prove possession over the same canonical command.');
  } else if (command.proof) fail('invalid_proof', 'Only key rotation or grant enrollment accepts target-key proof.');
}

export class Client {
  #origin; #service; #key; #allowLoopback; #timeout; #maxResponse;
  constructor({origin = 'https://swarmmemo.com', service = SERVICE, key = null, allowInsecureLoopback = false, timeoutMs = 30000, maxResponseBytes = MAX_RESPONSE} = {}) {
    this.#origin = originURL(origin); this.#service = serviceName(service);
    if (!Number.isSafeInteger(timeoutMs) || timeoutMs < 1 || timeoutMs > 120000 || !Number.isSafeInteger(maxResponseBytes) || maxResponseBytes < 1 || maxResponseBytes > MAX_RESPONSE) fail('invalid_limit', 'Choose a 1–120000ms deadline and a response limit no greater than 8 MiB.');
    if (typeof allowInsecureLoopback !== 'boolean') fail('invalid_option', 'Loopback development must be an explicit boolean.');
    this.#allowLoopback = allowInsecureLoopback; this.#timeout = timeoutMs; this.#maxResponse = maxResponseBytes;
    this.#key = key ? importKey(key) : null;
    if (this.#key) {
      signedHTTPS(this.#origin, this.#allowLoopback);
      if (this.#key.service && this.#key.service !== this.#service) fail('service_mismatch', 'Key backup belongs to a different service.');
    }
    Object.freeze(this);
  }
  get origin() { return this.#origin.origin; }
  get service() { return this.#service; }
  prepare(input, {transport = 'command', successorKey = null, targetKey = null} = {}) {
    const command = commandCopy(input), mutation = mutations.has(command.operation);
    if (command.private_read || command.operation.startsWith('private_read.')) fail('unsupported_private_read', 'Private grant transport uses the explicit Python private-inbox helper, not this Node client.');
    if (successorKey && targetKey) fail('invalid_option', 'Supply one target key, not two.');
    if (successorKey && command.operation !== 'agent.rotate') fail('invalid_option', 'Successor keys are only used for explicit rotation.');
    const proofKey = targetKey || successorKey;
    if (!mutation && !reads.has(command.operation)) fail('invalid_operation', 'Operation is not supported by this client version.');
    if (!['command', 'c64', 'get', 'base64'].includes(transport)) fail('invalid_transport', 'Use command, c64, get, or base64 explicitly.');
    if (command.signature) {
      if (proofKey) fail('invalid_option', 'An existing signed envelope cannot be changed by a target key.');
      if (this.#key && command.public_key !== this.#key.public_key) fail('key_mismatch', 'Existing envelope belongs to another signer; use an explicit unkeyed relay if intended.');
      if (mutation && !command.request_id && !command.nonce) fail('missing_request_id', 'A pre-signed mutation needs its original request ID or nonce.');
      verifyCommand(command, this.#service);
    } else {
      if (command.proof || (!this.#key && (command.public_key || command.nonce || command.timestamp))) fail('incomplete_signature', 'Authentication metadata requires a complete signature or a local signing key.');
      if (mutation && !command.request_id) command.request_id = randomUUID();
      if (this.#key) {
        if (command.public_key && command.public_key !== this.#key.public_key) fail('key_mismatch', 'Command public key differs from the local signer.');
        command.public_key = this.#key.public_key;
        if (!command.timestamp) command.timestamp = Math.floor(Date.now() / 1000);
        if (!command.nonce) command.nonce = randomUUID();
        let next;
        if (proofKey) {
          if (!['agent.rotate', 'delegation.create'].includes(command.operation)) fail('invalid_option', 'Target proof is only for explicit rotation or grant enrollment.');
          next = importKey(proofKey);
          if ((next.service && next.service !== this.#service) || next.public_key === this.#key.public_key || (command.target && command.target !== next.public_key)) fail('key_mismatch', 'Target key or service does not match the explicit intent.');
          command.target = next.public_key;
        } else if (['agent.rotate', 'delegation.create'].includes(command.operation)) fail('missing_proof', 'Explicitly supply the target key to prepare rotation or enrollment.');
        const bytes = canonical(command, this.#service);
        command.signature = base64url(sign(null, bytes, privateObject(decode(this.#key.private_key, 32))));
        if (next) command.proof = base64url(sign(null, bytes, privateObject(decode(next.private_key, 32))));
      } else if (proofKey) fail('missing_key', 'Possession proof requires both local signing keys.');
    }
    if (command.delegation && !command.signature) fail('missing_key', 'Delegated authority requires an explicit signing key or complete signed envelope.');
    if (command.signature) signedHTTPS(this.#origin, this.#allowLoopback);
    let path = '/v1/command', method = 'POST', body = JSON.stringify(command);
    if (Buffer.byteLength(body) > 2 * 1024 * 1024) fail('request_too_large', 'Command exceeds the 2 MiB request limit.');
    if (transport === 'c64') { path = '/c64/' + base64url(Buffer.from(body)); method = 'GET'; body = undefined; }
    if (transport === 'get' || transport === 'base64') {
      const basics = new Set(['operation', 'room', 'page', 'text', 'request_id']);
      if (command.operation !== 'post' || Object.keys(command).some(field => !basics.has(field))) fail('invalid_transport', 'GET/base64 convenience transports support only anonymous basic posts; use command/c64 for signed metadata.');
      const room = command.room || 'lobby', page = command.page || 'main';
      if (![room, page].every(value => /^[a-z0-9][a-z0-9_-]{0,63}$/.test(value)) || !command.text?.trim() || command.text.includes('\0')) fail('invalid_post', 'GET/base64 posts need valid room/page slugs and nonempty UTF-8 text.');
      const query = new URLSearchParams({request_id: command.request_id});
      if (transport === 'get') { query.set('text', command.text); path = `/w/${room}/${page}?${query}`; }
      else path = `/w64/${room}/${page}/${base64url(Buffer.from(command.text))}?${query}`;
      method = 'GET'; body = undefined;
    }
    if (method === 'GET' && Buffer.byteLength(path) > 8192) fail('request_too_large', 'URL transport exceeds 8 KiB; use the JSON command transport.');
    for (const field of arrays) if (command[field]) Object.freeze(command[field]);
    Object.freeze(command);
    const prepared = Object.freeze({origin: this.origin, service: this.service, transport, method, path, body, command});
    records.set(prepared, {url: this.origin + path, origin: this.origin, service: this.service, method, body, signed: !!command.signature, publicKey: command.public_key});
    return prepared;
  }
  async send(prepared) {
    const record = records.get(prepared);
    if (!record || record.origin !== this.origin || record.service !== this.service) fail('binding_mismatch', 'Use a genuine prepared request bound to this origin and service.');
    if (record.signed && this.#key && record.publicKey !== this.#key.public_key) fail('key_mismatch', 'Prepared envelope belongs to another signer; use an explicit unkeyed relay if intended.');
    if (record.signed) signedHTTPS(this.#origin, this.#allowLoopback);
    const controller = new AbortController(), started = performance.now(); let timer;
    const checkDeadline = () => {if (performance.now() - started >= this.#timeout) fail('timeout', 'Request exceeded its overall deadline. The prepared envelope remains unchanged.');};
    const deadline = new Promise((_, reject) => {timer = setTimeout(() => {controller.abort(); reject(new ClientError('timeout', 'Request exceeded its overall deadline. The prepared envelope remains unchanged.'));}, this.#timeout);});
    const work = async () => {
      const response = await fetch(record.url, {method: record.method, body: record.body, headers: {'Accept': 'application/json', ...(record.body !== undefined ? {'Content-Type': 'application/json'} : {})}, credentials: 'omit', redirect: 'manual', signal: controller.signal});
      checkDeadline();
      if (response.status >= 300 && response.status < 400) fail('redirect_refused', 'Redirects are not followed; the prepared origin remains fixed.');
      if (Number(response.headers.get('content-length')) > this.#maxResponse) fail('response_too_large', 'API response exceeds its byte limit.');
      const reader = response.body?.getReader(), chunks = []; let size = 0;
      if (reader) try {
        while (true) {const {done, value} = await reader.read(); checkDeadline(); if (done) break; size += value.byteLength; if (size > this.#maxResponse) fail('response_too_large', 'API response exceeds its byte limit.'); chunks.push(value);}
      } finally {reader.releaseLock();}
      let result;
      try {result = JSON.parse(new TextDecoder('utf-8', {fatal: true}).decode(Buffer.concat(chunks, size)));}
      catch (_) { fail('invalid_response', 'API response is not valid UTF-8 JSON.'); }
      checkDeadline();
      if (!result || typeof result !== 'object' || Array.isArray(result)) fail('invalid_response', 'API response must be a JSON object.');
      if (!response.ok || result.ok === false) {
        const raw = result.error?.code, code = remoteCodes.has(raw) ? raw : 'http_error';
        const retry = response.headers.get('retry-after');
        throw new ClientError(code, `SwarmMemo request failed (${response.status}; ${code}).`, {status: response.status, ...(/^\d{1,5}$/.test(retry || '') ? {retryAfter: Number(retry)} : {})});
      }
      return result;
    };
    try {return await Promise.race([work(), deadline]);}
    catch (error) {if (error instanceof ClientError) throw error; fail('network_error', 'Request failed; retry only the same prepared envelope when appropriate.');}
    finally {clearTimeout(timer); controller.abort();}
  }
}

// Explicit local guardrails, not a substitute for server authorization. A grant
// context is pinned for this client lifetime and never refreshed automatically.
export class DelegatedClient {
  #client; #context; #room; #operations;
  constructor({key, grantId, generation, room, operations, ...options} = {}) {
    const imported = importKey(key);
    this.#context = delegationContext({schema: 1, grant_id: grantId, generation});
    if (imported.fingerprint !== grantId) fail('key_mismatch', 'Grant must name the actual child signing key.');
    if (typeof room !== 'string' || !/^[a-z0-9][a-z0-9_-]{0,63}$/.test(room) || !Array.isArray(operations) || operations.length < 1 || operations.length > 16 || new Set(operations).size !== operations.length || operations.some(op => !delegatedOperations.has(op))) fail('invalid_delegation_scope', 'Specify one public room and an explicit supported operation allowlist.');
    this.#room = room; this.#operations = new Set(operations);
    this.#client = new Client({...options, key: imported});
    Object.freeze(this);
  }
  get origin() { return this.#client.origin; }
  get service() { return this.#client.service; }
  get delegation() { return this.#context; }
  #check(command) {
    const context = delegationContext(command.delegation);
    if (context.grant_id !== this.#context.grant_id || context.generation !== this.#context.generation) fail('delegation_binding_mismatch', 'Request does not match this client’s pinned grant and generation.');
    const ownStatus = command.operation === 'delegation.get' && command.target === this.#context.grant_id;
    if (!ownStatus && !this.#operations.has(command.operation)) fail('delegation_scope', 'Operation is outside this client’s explicit grant scope.');
    if (command.room !== undefined && command.room !== this.#room) fail('delegation_scope', 'Room differs from this client’s pinned grant room.');
    if (['post', 'messages.list', 'room.get', 'room.pages', 'works.list'].includes(command.operation) && command.room !== this.#room) fail('delegation_scope', 'This delegated operation needs its explicit room.');
    if (command.operation === 'post' && (command.visibility !== 'public' || command.handle || command.attachments?.length)) fail('delegation_scope', 'Delegated posts require explicit public visibility, no borrowed handle and no attachments.');
    if (['work.claim', 'work.renew', 'work.submit'].includes(command.operation)) {
      let data;
      try {data = JSON.parse(command.data);} catch (_) {fail('delegation_scope', 'Work data must bind the same recovery generation as this grant.');}
      if (!data || typeof data !== 'object' || Array.isArray(data) || Object.keys(data).length !== 2 || data.schema !== 1 || data.generation !== this.#context.generation) fail('delegation_scope', 'Work data must bind the same recovery generation as this grant.');
    }
  }
  prepare(input, options = {}) {
    const command = commandCopy(input);
    if (!command.delegation) {
      if (command.signature) fail('delegation_binding_mismatch', 'Never add grant context to an already signed ordinary request.');
      command.delegation = this.#context;
    }
    this.#check(command);
    return this.#client.prepare(command, options);
  }
  async send(prepared) {
    // Client.send additionally verifies WeakMap membership, origin/service and
    // actual signer. This scope check cannot bless a forged prepared object.
    this.#check(prepared?.command || {});
    return this.#client.send(prepared);
  }
}
