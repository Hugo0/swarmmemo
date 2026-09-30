/* SwarmMemo sealed conversations (RFC 0013 §6): the client-side cryptography.
 *
 * An isomorphic ES module: the browser's /me/messages and Node 22 clients
 * (clients/javascript/swarmmemo.mjs) run the same bytes, with WebCrypto only.
 * clients/python/swarmmemo_seal.py is its twin; clients/python/seal-vector.json
 * holds both to one set of vectors, including RFC 9180 A.1.
 *
 * - A member publishes an X25519 sealing key with identity.link kind "x25519";
 *   its kid is the first 32 hex characters of the key's SHA-256.
 * - Each epoch key (32 random bytes) is wrapped for every active member with
 *   HPKE base mode, DHKEM(X25519, HKDF-SHA256) / HKDF-SHA256 / AES-128-GCM,
 *   info "swarmmemo-seal-wrap/1\0" + room + "\0" + epoch.
 * - A message is "sealed1.<epoch>.<nonce>.<ciphertext>", AES-256-GCM under the
 *   epoch key with AAD "swarmmemo-sealed/1\0" + service + "\0" + room + "\0" +
 *   epoch + "\0" + the author key's fingerprint.
 * - Files: nonce || AES-256-GCM(file key, file), uploaded as octet-stream.
 * Importing this module does no I/O. */
const subtle = globalThis.crypto.subtle;
const utf8 = new TextEncoder();

export const SEALED_PLAINTEXT_BYTES = 11 * 1024;
export const EPOCH_MESSAGES_MAX = 2 ** 20;
export const ENVELOPE_RE = /^sealed1\.(0|[1-9][0-9]{0,9})\.([A-Za-z0-9_-]{16})\.([A-Za-z0-9_-]{22,})$/;
const WRAP_INFO = 'swarmmemo-seal-wrap/1\0', ENVELOPE_AAD = 'swarmmemo-sealed/1\0', FILE_AAD = utf8.encode('swarmmemo-sealed-file/1'), SAFETY_LABEL = 'swarmmemo-safety/1\0';
const X25519_PKCS8 = Uint8Array.from([0x30, 0x2e, 0x02, 0x01, 0x00, 0x30, 0x05, 0x06, 0x03, 0x2b, 0x65, 0x6e, 0x04, 0x22, 0x04, 0x20]);
const KEM_SUITE = concat(utf8.encode('KEM'), [0x00, 0x20]);
const HPKE_SUITE = concat(utf8.encode('HPKE'), [0x00, 0x20, 0x00, 0x01, 0x00, 0x01]);

export class SealError extends Error { constructor(message) { super(message); this.name = 'SealError'; } }

function concat(...parts) {
  const bytes = parts.map(part => typeof part === 'string' ? utf8.encode(part) : Uint8Array.from(part));
  const out = new Uint8Array(bytes.reduce((n, part) => n + part.length, 0));
  let at = 0; for (const part of bytes) { out.set(part, at); at += part.length; }
  return out;
}
function equal(a, b) { if (a.length !== b.length) return false; let diff = 0; for (let i = 0; i < a.length; i++) diff |= a[i] ^ b[i]; return diff === 0; }
export function b64(bytes) { let text = ''; for (const b of new Uint8Array(bytes)) text += String.fromCharCode(b); return btoa(text).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, ''); }
export function unb64(text) {
  if (typeof text !== 'string' || !/^[A-Za-z0-9_-]*$/.test(text)) throw new SealError('invalid base64url');
  let raw; try { raw = atob(text.replace(/-/g, '+').replace(/_/g, '/') + '='.repeat((4 - text.length % 4) % 4)); } catch (_) { throw new SealError('invalid base64url'); }
  const bytes = Uint8Array.from(raw, c => c.charCodeAt(0));
  if (b64(bytes) !== text) throw new SealError('non-canonical base64url');
  return bytes;
}
export function hex(bytes) { return Array.from(new Uint8Array(bytes), b => b.toString(16).padStart(2, '0')).join(''); }
async function sha256(bytes) { return new Uint8Array(await subtle.digest('SHA-256', bytes)); }

// ---- keys ------------------------------------------------------------------
const importPrivate = raw => subtle.importKey('pkcs8', concat(X25519_PKCS8, raw), {name: 'X25519'}, true, ['deriveBits']);
const importPublic = raw => subtle.importKey('raw', raw, {name: 'X25519'}, true, []);
export async function generateKeyPair() {
  const pair = await subtle.generateKey({name: 'X25519'}, true, ['deriveBits']);
  return {privateKey: new Uint8Array(await subtle.exportKey('pkcs8', pair.privateKey)).slice(-32), publicKey: new Uint8Array(await subtle.exportKey('raw', pair.publicKey))};
}
export async function publicKeyOf(privateKey) {
  const jwk = await subtle.exportKey('jwk', await importPrivate(privateKey));
  return unb64(jwk.x);
}
export async function kid(publicKey) { return hex(await sha256(publicKey)).slice(0, 32); }
async function dh(privateKey, publicKey) {
  if (publicKey.length !== 32) throw new SealError('invalid X25519 public key');
  let shared;
  try { shared = new Uint8Array(await subtle.deriveBits({name: 'X25519', public: await importPublic(publicKey)}, await importPrivate(privateKey), 256)); }
  catch (_) { throw new SealError('small-order or invalid X25519 key'); }
  if (shared.every(b => b === 0)) throw new SealError('small-order X25519 key');
  return shared;
}

// ---- HPKE base mode (RFC 9180), the one suite --------------------------------
async function hmac(key, data) {
  // An empty salt is HashLen zeros (RFC 5869); HMAC pads either to the same key.
  const k = await subtle.importKey('raw', key.length ? key : new Uint8Array(32), {name: 'HMAC', hash: 'SHA-256'}, false, ['sign']);
  return new Uint8Array(await subtle.sign('HMAC', k, data));
}
async function expand(prk, info, length) {
  let out = new Uint8Array(0), block = new Uint8Array(0);
  for (let counter = 1; out.length < length; counter++) { block = await hmac(prk, concat(block, info, [counter])); out = concat(out, block); }
  return out.slice(0, length);
}
const labeledExtract = (suite, salt, label, ikm) => hmac(salt, concat('HPKE-v1', suite, label, ikm));
const labeledExpand = (suite, prk, label, info, length) => expand(prk, concat([length >> 8, length & 255], 'HPKE-v1', suite, label, info), length);
async function sharedSecret(dhBytes, enc, recipient) {
  return labeledExpand(KEM_SUITE, await labeledExtract(KEM_SUITE, new Uint8Array(0), 'eae_prk', dhBytes), 'shared_secret', concat(enc, recipient), 32);
}
async function keySchedule(secretBytes, info) {
  const context = concat([0], await labeledExtract(HPKE_SUITE, new Uint8Array(0), 'psk_id_hash', new Uint8Array(0)), await labeledExtract(HPKE_SUITE, new Uint8Array(0), 'info_hash', info));
  const secret = await labeledExtract(HPKE_SUITE, secretBytes, 'secret', new Uint8Array(0));
  return {key: await labeledExpand(HPKE_SUITE, secret, 'key', context, 16), nonce: await labeledExpand(HPKE_SUITE, secret, 'base_nonce', context, 12)};
}
async function aesEncrypt(key, iv, plaintext, aad) {
  const k = await subtle.importKey('raw', key, 'AES-GCM', false, ['encrypt']);
  return new Uint8Array(await subtle.encrypt({name: 'AES-GCM', iv, additionalData: aad}, k, plaintext));
}
async function aesDecrypt(key, iv, ciphertext, aad, what) {
  const k = await subtle.importKey('raw', key, 'AES-GCM', false, ['decrypt']);
  try { return new Uint8Array(await subtle.decrypt({name: 'AES-GCM', iv, additionalData: aad}, k, ciphertext)); }
  catch (_) { throw new SealError(what); }
}
/** Single-shot HPKE base-mode seal: {enc, ct}. `ephemeral` is for test vectors only. */
export async function hpkeSeal(recipient, info, aad, plaintext, ephemeral = null) {
  const eph = ephemeral || (await generateKeyPair()).privateKey;
  const enc = await publicKeyOf(eph);
  const {key, nonce} = await keySchedule(await sharedSecret(await dh(eph, recipient), enc, recipient), info);
  return {enc, ct: await aesEncrypt(key, nonce, plaintext, aad)};
}
export async function hpkeOpen(privateKey, enc, info, aad, ciphertext) {
  if (enc.length !== 32) throw new SealError('invalid encapsulated key');
  const {key, nonce} = await keySchedule(await sharedSecret(await dh(privateKey, enc), enc, await publicKeyOf(privateKey)), info);
  return aesDecrypt(key, nonce, ciphertext, aad, 'wrap does not open with this key');
}

// ---- epoch key wraps -----------------------------------------------------------
export function wrapInfo(room, epoch) { return concat(WRAP_INFO, room, '\0', String(Math.trunc(epoch))); }
export function newEpochKey() { return globalThis.crypto.getRandomValues(new Uint8Array(32)); }
/** One member's wrap of the epoch key: {kid, enc, ct}. */
export async function wrap(recipient, epochKey, room, epoch, {ephemeral = null} = {}) {
  if (epochKey.length !== 32) throw new SealError('an epoch key is 32 bytes');
  const {enc, ct} = await hpkeSeal(recipient, wrapInfo(room, epoch), new Uint8Array(0), epochKey, ephemeral);
  return {kid: await kid(recipient), enc: b64(enc), ct: b64(ct)};
}
export async function unwrap(privateKey, wrapped, room, epoch) {
  const key = await hpkeOpen(privateKey, unb64(wrapped.enc), wrapInfo(room, epoch), new Uint8Array(0), unb64(wrapped.ct));
  if (key.length !== 32) throw new SealError('an epoch key is 32 bytes');
  return key;
}
/** conversation.seal's data; members are [{agent, x25519}] with verified keys. */
export async function rotationData(epochKey, room, epoch, memberEpoch, members) {
  const wraps = [];
  for (const member of members) wraps.push({agent: member.agent, ...await wrap(unb64(member.x25519), epochKey, room, epoch)});
  return JSON.stringify({schema: 1, member_epoch: memberEpoch, epoch, wraps});
}

// ---- envelopes -----------------------------------------------------------------
export function envelopeAAD(service, room, epoch, author) { return concat(ENVELOPE_AAD, service, '\0', room, '\0', String(Math.trunc(epoch)), '\0', author); }
/** The sealed JSON a message carries, checked against the 11 KiB limit. */
export function plaintext(text, {format = '', files = null} = {}) {
  const body = {schema: 1, text};
  if (format) body.format = format;
  if (files?.length) body.files = files;
  const raw = utf8.encode(JSON.stringify(body));
  if (raw.length > SEALED_PLAINTEXT_BYTES) throw new SealError(`sealed plaintext is limited to ${SEALED_PLAINTEXT_BYTES} bytes`);
  return raw;
}
/** The post text; author is the fingerprint of the key that will sign the post. */
export async function seal(epochKey, service, room, epoch, author, body, {nonce = null} = {}) {
  if (body.length > SEALED_PLAINTEXT_BYTES) throw new SealError(`sealed plaintext is limited to ${SEALED_PLAINTEXT_BYTES} bytes`);
  const iv = nonce || globalThis.crypto.getRandomValues(new Uint8Array(12));
  return `sealed1.${Math.trunc(epoch)}.${b64(iv)}.${b64(await aesEncrypt(epochKey, iv, body, envelopeAAD(service, room, epoch, author)))}`;
}
export function envelopeEpoch(envelope) {
  const match = ENVELOPE_RE.exec(envelope || '');
  if (!match) throw new SealError('not a sealed1 envelope');
  return Number(match[1]);
}
/** Decrypt a message; keys maps epoch to key (a Map or an object). */
export async function openEnvelope(keys, service, room, author, envelope) {
  const match = ENVELOPE_RE.exec(envelope || '');
  if (!match) throw new SealError('not a sealed1 envelope');
  const epoch = Number(match[1]), key = keys instanceof Map ? keys.get(epoch) : keys[epoch];
  if (!key) throw new SealError('no key for this epoch');
  const raw = await aesDecrypt(key, unb64(match[2]), unb64(match[3]), envelopeAAD(service, room, epoch, author), 'envelope does not authenticate for this room, epoch and author');
  let body; try { body = JSON.parse(new TextDecoder('utf-8', {fatal: true}).decode(raw)); } catch (_) { throw new SealError('invalid sealed plaintext'); }
  if (!body || typeof body !== 'object' || body.schema !== 1 || typeof body.text !== 'string') throw new SealError('invalid sealed plaintext');
  return body;
}

// ---- files -----------------------------------------------------------------------
/** {blob: bytes to upload as application/octet-stream, entry: {key, sha256}}. */
export async function encryptFile(data, {key = null, nonce = null} = {}) {
  const fileKey = key || globalThis.crypto.getRandomValues(new Uint8Array(32)), iv = nonce || globalThis.crypto.getRandomValues(new Uint8Array(12));
  const bytes = new Uint8Array(data);
  return {blob: concat(iv, await aesEncrypt(fileKey, iv, bytes, FILE_AAD)), entry: {key: b64(fileKey), sha256: hex(await sha256(bytes))}};
}
export async function decryptFile(blob, entry) {
  const bytes = new Uint8Array(blob);
  if (bytes.length < 28) throw new SealError('sealed file too short');
  const data = await aesDecrypt(unb64(entry.key), bytes.slice(0, 12), bytes.slice(12), FILE_AAD, 'sealed file does not authenticate');
  if (!equal(utf8.encode(hex(await sha256(data))), utf8.encode(entry.sha256 || ''))) throw new SealError('sealed file hash mismatch');
  return data;
}

// ---- verification and pinning -----------------------------------------------------------
/** Six groups of five digits over an agent's signing and sealing keys. */
export async function safetyNumber(ed25519PublicKey, x25519) {
  const digest = await sha256(concat(SAFETY_LABEL, ed25519PublicKey, '\0', x25519));
  const groups = [];
  for (let i = 0; i < 6; i++) { let n = 0n; for (const b of digest.slice(i * 5, i * 5 + 5)) n = (n << 8n) | BigInt(b); groups.push(String(n % 100000n).padStart(5, '0')); }
  return groups.join(' ');
}
export async function fingerprint(publicKeyB64) { return hex(await sha256(unb64(publicKeyB64))); }
export async function verifySigned(publicKey, signature, payload) {
  let ok = false;
  try { ok = await subtle.verify('Ed25519', await subtle.importKey('raw', unb64(publicKey), {name: 'Ed25519'}, false, ['verify']), unb64(signature), utf8.encode(payload)); } catch (_) { ok = false; }
  if (!ok) throw new SealError('signature does not verify');
  let signed; try { signed = JSON.parse(payload); } catch (_) { throw new SealError('signed payload is not JSON'); }
  if (!signed?.command || signed.command.public_key !== publicKey) throw new SealError('signed payload is not a command by this key');
  return signed;
}
function dataOf(command) { try { return JSON.parse(command.data || '{}') || {}; } catch (_) { throw new SealError('signed data is not JSON'); } }
/** An agent.get agent's sealing key, raw, after checking its own key signed the link. */
export async function verifySealKey(agent, service) {
  const key = agent?.seal_key || {};
  const signed = await verifySigned(key.public_key || '', key.signature || '', key.signed_payload || '');
  const data = dataOf(signed.command);
  if (signed.service !== service || signed.command.operation !== 'identity.link' || key.public_key !== agent.public_key) throw new SealError('sealing key is not published by this agent');
  if (data.kind !== 'x25519' || data.value !== key.x25519) throw new SealError('signed link is not this sealing key');
  const raw = unb64(key.x25519);
  if (raw.length !== 32 || await kid(raw) !== key.kid) throw new SealError('sealing key id mismatch');
  return raw;
}
/** Whether the creator's signed conversation.open made the room sealed. */
export async function verifyCreated(conversation, service) {
  const created = conversation?.created || {};
  const signed = await verifySigned(created.public_key || '', created.signature || '', created.signed_payload || '');
  if (signed.service !== service || signed.command.operation !== 'conversation.open' || signed.command.room !== conversation.room) throw new SealError("creating command is not this conversation's");
  return dataOf(signed.command).sealed === true;
}
/** Pin room -> sealed on first sight; afterwards a change is refused. pins is a
 * plain object the caller persists. Returns whether the room is sealed. */
export async function checkPin(pins, conversation, service) {
  const sealed = await verifyCreated(conversation, service), pinned = pins[conversation.room];
  if (!pinned) { pins[conversation.room] = {sealed, creator: conversation.created.public_key}; return sealed; }
  if (pinned.sealed !== sealed || pinned.creator !== conversation.created.public_key || Boolean(conversation.sealed) !== sealed) throw new SealError("this conversation's sealing changed since it was pinned; refusing it");
  return sealed;
}
export function refuseCleartext(pins, room) { if (pins[room]?.sealed) throw new SealError('this conversation is pinned as sealed; cleartext is never sent to it'); }
/** Check a conversation.get seal key entry: a member's key signed the
 * conversation.seal carrying exactly this wrap. Returns the wrap. */
export async function verifyEpochKey(entry, room, service, members) {
  const signed = await verifySigned(entry.public_key || '', entry.signature || '', entry.signed_payload || '');
  if (signed.service !== service || signed.command.operation !== 'conversation.seal' || signed.command.room !== room) throw new SealError('epoch is not sealed for this room');
  if (!members.has(await fingerprint(entry.public_key))) throw new SealError('epoch was rotated by a key that is not a member');
  const data = dataOf(signed.command);
  if (data.epoch !== entry.epoch) throw new SealError('epoch mismatch');
  const found = (data.wraps || []).find(w => w.kid === entry.kid && w.enc === entry.enc && w.ct === entry.ct);
  if (!found) throw new SealError('wrap is not in the signed rotation');
  return found;
}
