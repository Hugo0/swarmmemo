import test from 'node:test';
import assert from 'node:assert/strict';
import {createServer} from 'node:http';
import {once} from 'node:events';
import {mkdtemp, readFile, stat, chmod, symlink, rm, open, appendFile} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import {join} from 'node:path';
import {spawn, spawnSync} from 'node:child_process';
import {gzipSync} from 'node:zlib';
import {fileURLToPath} from 'node:url';
import {createECDH, createPrivateKey, createPublicKey, randomBytes, sign, verify} from 'node:crypto';
import {Client, DelegatedClient, ClientError, CONTRACT, FIELDS, canonical, delegationContext, generateKey, importKey, writeKey, loadKey, conversationRoom, loadSeal, agentSubjectID, workCreateData, workAnswerCommit, standingTier, WORK_STANDING_CREATE, markdownAuto, MARKDOWN_POST_DATA, keccak256, walletAddress, personalSign, loadWalletKey, walletChallengeOK, WALLET_KEY_ENV, dataFor, workRatingPost, USER_AGENT, isLoopbackHost, unb64} from './swarmmemo.mjs';
// sha256 of "ok\n", the output a C591 verifyPublish case publishes.
const OK_SHA256 = 'dc51b8c96c2d745df3bd5590d990230a482fd247123599548e0632fdbf97fc22';

// C546: a keyless write makes a device key; no test ever writes the real ~/.swarmmemo.
const {mkdtempSync, rmSync} = await import('node:fs');
process.env.SWARMMEMO_HOME = mkdtempSync(join(tmpdir(), 'swarmmemo-home-'));
process.on('exit', () => rmSync(process.env.SWARMMEMO_HOME, {recursive: true, force: true}));
delete process.env.SWARMMEMO_KEY;

const vector = JSON.parse(await readFile(new URL('../python/signing-vector.json', import.meta.url), 'utf8'));
const fixtureKey = importKey({private_key: Buffer.from(vector.seed_hex, 'hex').toString('base64url'), public_key: vector.command.public_key});

test('the generated CONTRACT block is exactly clients/contract.json', async () => {
  // Two-way equality: written by go generate ./internal/clientcontract from the Go code that enforces each fact.
  const contract = JSON.parse(await readFile(new URL('../contract.json', import.meta.url), 'utf8'));
  assert.deepEqual(JSON.parse(JSON.stringify(CONTRACT)), contract);
  assert.deepEqual([...FIELDS], contract.command.fields);
});

test('base64url decodes only its one canonical spelling (testdata/base64_vectors.json, C446)', async () => {
  const vectors = JSON.parse(await readFile(new URL('../../testdata/base64_vectors.json', import.meta.url), 'utf8')).base64url;
  for (const v of vectors.accept) assert.equal(unb64(v.value).toString('hex'), v.hex, v.value);
  for (const v of vectors.reject) assert.throws(() => unb64(v.value), error => error.code === 'invalid_key' && /canonical unpadded base64url/.test(error.message), `${JSON.stringify(v.value)}: ${v.why}`);
  assert.throws(() => unb64('aGVsbG8', 32), error => error.code === 'invalid_key' && /Expected 32 bytes/.test(error.message));
  // The split C446 closes: a signature with its unused trailing bits set names
  // the same bytes, and the client's verifier refuses it before checking.
  const alphabet = 'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_';
  const sig = vector.command.signature, twin = sig.slice(0, -1) + alphabet[alphabet.indexOf(sig.at(-1)) | 1];
  assert.notEqual(twin, sig);
  assert.throws(() => unb64(twin, 64), error => error.code === 'invalid_key');
});

test('find types and review options are checked against the contract before any network', async () => {
  const client = new Client('http://127.0.0.1:9', fixtureKey);
  await assert.rejects(client.find('x', {types: ['nonsense']}), error => error.code === 'invalid_option');
  await assert.rejects(client.find('x', {types: 'nonsense'}), error => error.code === 'invalid_option');
});

test('six shared ordinary, public and private canonical/proof vectors agree offline', async () => {
  const shared = JSON.parse(await readFile(new URL('../../testdata/private_read_vectors.json', import.meta.url), 'utf8'));
  assert.equal(shared.vectors.length, 6);
  for (const v of shared.vectors) {
    const bytes = canonical(v.command, v.service);
    assert.equal(bytes.toString(), v.canonical, v.name);
    const secret = createPrivateKey({key: Buffer.concat([Buffer.from('302e020100300506032b657004220420', 'hex'), Buffer.from(v.seed_hex, 'hex')]), format: 'der', type: 'pkcs8'});
    assert.equal(sign(null, bytes, secret).toString('base64url'), v.signature, v.name);
    assert.equal(verify(null, bytes, createPublicKey(secret), Buffer.from(v.signature, 'base64url')), true);
    if (v.proof) {
      const target = createPrivateKey({key: Buffer.concat([Buffer.from('302e020100300506032b657004220420', 'hex'), Buffer.from(v.target_seed_hex, 'hex')]), format: 'der', type: 'pkcs8'});
      assert.equal(sign(null, bytes, target).toString('base64url'), v.proof);
    }
  }
});

test('private canonical context cannot downgrade or activate Node transport', () => {
  const context = {schema: 1, grant_id: 'a'.repeat(64), generation: 'b'.repeat(32)};
  const command = {operation: 'room.get', room: 'private-lab', private_read: context};
  const parsed = JSON.parse(canonical(command));
  assert.equal(parsed.version, 3);
  assert.equal(Object.keys(parsed.command).at(-1), 'private_read');
  for (const value of [null, {}, [], {...context, schema: true}, {...context, extra: 0}]) assert.throws(() => canonical({...command, private_read: value}), errorCode('invalid_private_read_context'));
  assert.throws(() => canonical({...command, delegation: context}), errorCode('invalid_private_read_context'));
  const client = new Client();
  for (const transport of ['command', 'c64', 'get', 'base64']) assert.throws(() => client.prepare(command, {transport}), errorCode('unsupported_private_read'));
  for (const operation of ['private_read.create', 'private_read.revoke', 'private_read.get', 'private_read.list']) assert.throws(() => client.prepare({operation}), errorCode('unsupported_private_read'));
});
async function server(t, handler) {
  const instance = createServer(handler); instance.listen(0, '127.0.0.1'); await once(instance, 'listening');
  t.after(async () => {instance.closeAllConnections(); await new Promise(resolve => instance.close(resolve));});
  return `http://127.0.0.1:${instance.address().port}`;
}
async function temp(t) {const dir = await mkdtemp(join(tmpdir(), 'swarmmemo-node-')); t.after(() => rm(dir, {recursive: true, force: true})); return dir;}
function errorCode(code) {return error => error instanceof ClientError && error.code === code;}
function withoutRequestID(prepared, key) {
  const command = {...prepared.command}; delete command.request_id;
  const privateKey = createPrivateKey({key: Buffer.concat([Buffer.from('302e020100300506032b657004220420', 'hex'), Buffer.from(key.private_key, 'base64url')]), format: 'der', type: 'pkcs8'});
  command.signature = sign(null, canonical(command), privateKey).toString('base64url');
  return command;
}

test('canonical/signature match shared Python and Go vector exactly', () => {
  assert.equal(canonical(vector.command).toString(), vector.canonical);
  const unsigned = {...vector.command}; delete unsigned.signature;
  const prepared = new Client({key: fixtureKey}).prepare(unsigned);
  assert.equal(prepared.command.signature, vector.command.signature);
  assert.equal(canonical({operation: 'post', text: '<>&🌍\u2028\u2029', members: [], ttl: 0}).toString(), '{"version":1,"service":"swarmmemo.com","command":{"operation":"post","text":"<>&🌍\\u2028\\u2029"}}');
  assert.throws(() => canonical({...unsigned, invented: true}), errorCode('unknown_field'));
  for (const timestamp of [true, 1.5, NaN, Infinity, Number.MAX_SAFE_INTEGER + 1, null]) assert.throws(() => canonical({operation: 'post', timestamp}), errorCode('invalid_number'));
  assert.throws(() => canonical({operation: 'post', text: '\ud800'}), errorCode('invalid_string'));
  assert.throws(() => canonical({operation: 'post', members: [undefined]}), errorCode('invalid_string'));
});

test('delegated canonical v2 is explicit, ordered, immutable and never a v1 fallback', () => {
  const context = {generation: 'a'.repeat(32), grant_id: fixtureKey.fingerprint, schema: 1};
  const cmd = {operation: 'post', room: 'lab', text: 'hello 🌍', visibility: 'public', delegation: context};
  const bytes = canonical(cmd), envelope = JSON.parse(bytes);
  assert.equal(envelope.version, 2);
  assert.deepEqual(Object.keys(envelope.command).at(-1), 'delegation');
  assert.deepEqual(Object.keys(envelope.command.delegation), ['schema', 'grant_id', 'generation']);
  assert.equal(canonical({...cmd, delegation: {...context, schema: 1}}).toString(), bytes.toString());
  for (const bad of [null, undefined, {}, [], {...context, schema: true}, {...context, schema: '1'}, {...context, schema: 2}, {...context, generation: 'A'.repeat(32)}, {...context, grant_id: 'a'}, {...context, extra: 1}, {Schema: 1, grant_id: context.grant_id, generation: context.generation}]) {
    assert.throws(() => canonical({...cmd, delegation: bad}), errorCode('invalid_delegation_context'));
  }
  assert.throws(() => new Client().prepare(cmd), errorCode('missing_key'));
  const prepared = new Client({key: fixtureKey}).prepare(cmd);
  context.generation = 'b'.repeat(32);
  assert.equal(prepared.command.delegation.generation, 'a'.repeat(32));
  assert.throws(() => {prepared.command.delegation.generation = 'b'.repeat(32);}, TypeError);
  assert.equal(new Client().prepare(prepared.command).command.signature, prepared.command.signature);
  const stripped = {...prepared.command}; delete stripped.delegation;
  assert.throws(() => new Client().prepare(stripped), errorCode('invalid_signature'));
  assert.equal(canonical(vector.command).toString(), vector.canonical);
  assert.ok(Object.isFrozen(delegationContext(prepared.command.delegation)));
  const shared = {...vector.command, delegation: {schema: 1, grant_id: fixtureKey.fingerprint, generation: 'a'.repeat(32)}};
  delete shared.signature;
  assert.equal(new Client({key: fixtureKey}).prepare(shared).command.signature, 'HAO4YsZe-GXmGYSemmy-vuauCkEpcptTUF-E9fl_MMqZljGU6PEORKhk1Anv5fpDe_fMsqVUdw5njcv9N_D3Ag');
});

test('enrollment proves child possession without changing existing rotation semantics', () => {
  const root = new Client({key: fixtureKey}), child = generateKey();
  const request = root.prepare({operation: 'delegation.create', room: 'lab', ttl: 600, amount: 10000,
    data: JSON.stringify({schema: 1, generation: 'a'.repeat(32), operations: ['post'], disclosure: 'public'})}, {targetKey: child});
  assert.equal(JSON.parse(canonical(request.command)).version, 1);
  assert.equal(request.command.target, child.public_key);
  assert.equal(new Client().prepare(request.command).command.proof, request.command.proof);
  assert.throws(() => new Client().prepare({...request.command, proof: request.command.signature}), errorCode('invalid_proof'));
  assert.throws(() => root.prepare({operation: 'delegation.create'}), errorCode('missing_proof'));
  assert.throws(() => root.prepare({operation: 'post', text: 'x'}, {targetKey: child}), errorCode('invalid_option'));
  assert.throws(() => root.prepare({operation: 'delegation.create'}, {successorKey: child}), errorCode('invalid_option'));
  assert.throws(() => root.prepare({operation: 'agent.rotate'}, {successorKey: child, targetKey: child}), errorCode('invalid_option'));
});

test('every write result may carry next.allowance, not only a post', async t => {
  const note = {line: 'Free today: 4 MiB of posting (band signed), 3.9 MiB left, resets 00:00 UTC.', resource: 'post_bytes', band: 'signed', standing_cents: 0, entitlement: 4194304, remaining: 4089446, resets_at: 1759276800};
  const origin = await server(t, (req, res) => {req.resume(); req.on('end', () => {res.setHeader('Content-Type', 'application/json'); res.end(JSON.stringify({ok: true, data: {room: 'lab', visibility: 'public'}, next: {allowance: note}}));});});
  const client = new Client({origin, key: fixtureKey, allowInsecureLoopback: true});
  const result = await client.send(client.prepare({operation: 'room.create', room: 'lab', visibility: 'public'}));
  assert.deepEqual(result.next.allowance, note);
});

test('scoped client pins signer, room, generation and operations even for prepared relays', async t => {
  let received;
  const origin = await server(t, (req, res) => {let raw = ''; req.on('data', c => {raw += c;}); req.on('end', () => {received = JSON.parse(raw); res.end('{"ok":true}');});});
  const options = {origin, key: fixtureKey, allowInsecureLoopback: true, grantId: fixtureKey.fingerprint, generation: 'a'.repeat(32), room: 'lab', operations: ['post', 'message.get']};
  const client = new DelegatedClient(options);
  options.operations.push('agent.rotate'); options.generation = 'b'.repeat(32);
  const prepared = client.prepare({operation: 'post', room: 'lab', visibility: 'public', text: 'hello'});
  await client.send(prepared); assert.deepEqual(received, prepared.command);
  assert.equal(client.delegation.generation, 'a'.repeat(32));
  assert.throws(() => client.prepare({operation: 'agent.rotate'}), errorCode('delegation_scope'));
  for (const command of [{operation: 'post', text: 'x'}, {operation: 'post', room: 'lab', text: 'x'}, {operation: 'post', room: 'other', text: 'x', visibility: 'public'}, {operation: 'post', room: 'lab', text: 'x', visibility: 'public', handle: 'parent'}]) assert.throws(() => client.prepare(command), errorCode('delegation_scope'));
  assert.throws(() => client.prepare({operation: 'message.get', message_id: 'a'.repeat(32), delegation: {...client.delegation, generation: 'b'.repeat(32)}}), errorCode('delegation_binding_mismatch'));
  const ordinary = new Client({origin, key: fixtureKey, allowInsecureLoopback: true}).prepare({operation: 'message.get', message_id: 'a'.repeat(32)});
  assert.throws(() => client.prepare(ordinary.command), errorCode('delegation_binding_mismatch'));
  await assert.rejects(client.send(ordinary), errorCode('invalid_delegation_context'));
  assert.doesNotThrow(() => client.prepare({operation: 'delegation.get', target: fixtureKey.fingerprint}));
  assert.throws(() => client.prepare({operation: 'delegation.get', target: 'b'.repeat(64)}), errorCode('delegation_scope'));
  assert.throws(() => new DelegatedClient({...options, key: generateKey()}), errorCode('key_mismatch'));
  assert.throws(() => new DelegatedClient({...options, operations: ['post', 'post']}), errorCode('invalid_delegation_scope'));
  assert.throws(() => new DelegatedClient({...options, operations: ['blob.get']}), errorCode('invalid_delegation_scope'));
  assert.throws(() => client.prepare({operation: 'post', room: 'lab', visibility: 'public', text: 'x', attachments: ['a'.repeat(32)]}), errorCode('delegation_scope'));
  const worker = new DelegatedClient({...options, generation: 'a'.repeat(32), operations: ['work.claim']});
  for (const data of ['', 'null', '{', JSON.stringify({schema: 1, generation: 'b'.repeat(32)}), JSON.stringify({schema: 1, generation: 'a'.repeat(32), other: 1})]) assert.throws(() => worker.prepare({operation: 'work.claim', message_id: 'a'.repeat(32), ttl: 60, data}), errorCode('delegation_scope'));
  assert.doesNotThrow(() => worker.prepare({operation: 'work.claim', message_id: 'a'.repeat(32), ttl: 60, data: JSON.stringify({schema: 1, generation: 'a'.repeat(32)})}));
});

test('keys are portable, exclusive, owner-only, and never confused with a public key', async t => {
  const dir = await temp(t), path = join(dir, 'key.json'), summary = await writeKey(path, fixtureKey);
  assert.equal(summary.private_key, undefined); assert.equal((await stat(path)).mode & 0o777, 0o600);
  assert.equal((await loadKey(path)).public_key, fixtureKey.public_key);
  await assert.rejects(writeKey(path), errorCode('key_write_failed'));
  const link = join(dir, 'symlink.json'); await symlink(path, link);
  await assert.rejects(loadKey(link), errorCode('key_read_failed'));
  await assert.rejects(writeKey(link), errorCode('key_write_failed'));
  await chmod(path, 0o644); await assert.rejects(loadKey(path), errorCode('unsafe_key_file'));
  await assert.rejects(loadKey(dir), errorCode('unsafe_key_file'));
  const fifo = join(dir, 'fifo');
  if (spawnSync('mkfifo', [fifo]).status === 0) await assert.rejects(loadKey(fifo), errorCode('unsafe_key_file'));
  const legacy = {...fixtureKey, private_key: Buffer.concat([Buffer.from('302e020100300506032b657004220420', 'hex'), Buffer.from(fixtureKey.private_key, 'base64url')]).toString('base64url')};
  assert.equal(importKey(legacy).private_key, fixtureKey.private_key);
  assert.throws(() => importKey({...fixtureKey, public_key: generateKey().public_key}), errorCode('key_mismatch'));
  assert.throws(() => importKey({...fixtureKey, private_key: fixtureKey.private_key + '='}), errorCode('invalid_key'));
  assert.throws(() => new Client({key: {...fixtureKey, service: 'another.example'}}), errorCode('service_mismatch'));
});

test('prepare is deeply immutable, read IDs remain absent, rotation proves both keys', () => {
  const client = new Client({key: fixtureKey}), attachments = ['blob-one'];
  const prepared = client.prepare({operation: 'post', text: 'hello', attachments}); attachments[0] = 'changed';
  assert.equal(prepared.command.attachments[0], 'blob-one'); assert.ok(prepared.command.request_id);
  assert.throws(() => prepared.command.attachments.push('other'), TypeError);
  assert.throws(() => {prepared.path = '/evil';}, TypeError);
  const read = client.prepare({operation: 'messages.list'}); assert.equal(read.command.request_id, undefined); assert.ok(read.command.nonce);
  const next = generateKey(), rotation = client.prepare({operation: 'agent.rotate'}, {successorKey: next});
  const pub = createPublicKey({key: Buffer.concat([Buffer.from('302a300506032b6570032100', 'hex'), Buffer.from(next.public_key, 'base64url')]), format: 'der', type: 'spki'});
  assert.ok(verify(null, canonical(rotation.command), pub, Buffer.from(rotation.command.proof, 'base64url')));
  assert.equal(new Client().prepare(rotation.command).command.signature, rotation.command.signature);
  assert.throws(() => client.prepare({operation: 'agent.rotate'}), errorCode('missing_proof'));
  assert.throws(() => new Client({service: 'other.example'}).prepare(rotation.command), errorCode('invalid_signature'));
});

test('key creation syncs its parent and preserves a created key after directory sync failure', async t => {
  const dir = await temp(t), probe = await open(dir), prototype = Object.getPrototypeOf(probe);
  await probe.close(); const original = prototype.sync, steps = []; let rejectDirectory = false;
  t.mock.method(prototype, 'sync', async function () {
    const directory = (await this.stat()).isDirectory(); steps.push(directory ? 'directory' : 'file');
    if (directory && rejectDirectory) throw Error('private_sentinel_sync_error');
    return original.call(this);
  });
  await writeKey(join(dir, 'good.json'), fixtureKey); assert.deepEqual(steps, ['file', 'directory']);
  rejectDirectory = true; const failed = join(dir, 'preserved.json');
  await assert.rejects(writeKey(failed, fixtureKey), error => error.code === 'key_write_failed' && !error.message.includes('private_sentinel'));
  assert.equal(JSON.parse(await readFile(failed, 'utf8')).private_key, fixtureKey.private_key);
  assert.equal((await stat(failed)).mode & 0o777, 0o600);
});

test('keyed dispatch cannot switch signer; unkeyed relay preserves nonce-only signed mutations', async t => {
  let received;
  const origin = await server(t, (req, res) => {let body = ''; req.on('data', chunk => {body += chunk;}); req.on('end', () => {received = JSON.parse(body); res.end('{"ok":true}');});});
  const a = new Client({origin, key: fixtureKey, allowInsecureLoopback: true});
  const b = new Client({origin, key: generateKey(), allowInsecureLoopback: true});
  const envelope = withoutRequestID(a.prepare({operation: 'post', text: 'nonce-only'}), fixtureKey);
  assert.throws(() => b.prepare(envelope), errorCode('key_mismatch'));
  const prepared = a.prepare(envelope); assert.equal(prepared.command.request_id, undefined);
  await assert.rejects(b.send(prepared), errorCode('key_mismatch'));
  const relay = new Client({origin, allowInsecureLoopback: true});
  assert.equal(relay.prepare(envelope).command.request_id, undefined);
  await relay.send(prepared); assert.deepEqual(received, envelope);
  assert.throws(() => new Client({origin: 'http://remote.example'}), errorCode('https_required'));
});

test('conversation and sealed commands are signed exactly, with the shared seal module', async t => {
  const bodies = [];
  const origin = await server(t, (req, res) => {let body = ''; req.on('data', chunk => {body += chunk;}); req.on('end', () => {bodies.push(JSON.parse(body)); res.setHeader('Content-Type', 'application/json'); res.end('{"ok":true,"receipt":{"id":"x"}}');});});
  const client = new Client({origin, key: fixtureKey, allowInsecureLoopback: true});
  const rooms = new Set(Array.from({length: 64}, conversationRoom));
  assert.equal(rooms.size, 64);
  for (const room of rooms) assert.match(room, /^~[a-z2-7]{26}$/);
  await client.openConversation({members: ['a'.repeat(64)], sealed: true});
  await client.conversation([...rooms][0], {markRead: true, reveal: ['b'.repeat(32)]});
  await client.respond([...rooms][0], 'accept');
  await assert.rejects(client.respond([...rooms][0], 'maybe'), errorCode('invalid_option'));
  const seal = await loadSeal();
  const pair = await seal.generateKeyPair();
  await client.publishSealKey(seal.b64(pair.publicKey));
  const data = await seal.rotationData(seal.newEpochKey(), [...rooms][0], 1, 1, [{agent: 'a'.repeat(64), x25519: seal.b64(pair.publicKey)}]);
  await client.sealRotate([...rooms][0], data);
  await client.postSealed([...rooms][0], 'sealed1.1.AAAAAAAAAAAAAAAA.AAAAAAAAAAAAAAAAAAAAAA');
  assert.deepEqual(bodies.map(b => b.operation), ['conversation.open', 'conversation.get', 'conversation.respond', 'identity.link', 'conversation.seal', 'post']);
  assert.match(bodies[0].room, /^~[a-z2-7]{26}$/);
  assert.deepEqual(JSON.parse(bodies[0].data), {schema: 1, kind: 'dm', sealed: true});
  assert.deepEqual(JSON.parse(bodies[1].data), {schema: 1, mark_read: true, reveal: ['b'.repeat(32)]});
  assert.deepEqual(JSON.parse(bodies[3].data), {schema: 1, kind: 'x25519', value: seal.b64(pair.publicKey)});
  assert.equal(JSON.parse(bodies[4].data).wraps[0].kid, await seal.kid(pair.publicKey));
  assert.deepEqual(JSON.parse(bodies[5].data), {schema: 1, format: 'sealed'});
  for (const body of bodies) assert.equal(verify(null, canonical(body), createPublicKey({key: Buffer.concat([Buffer.from('302a300506032b6570032100', 'hex'), Buffer.from(body.public_key, 'base64url')]), format: 'der', type: 'spki'}), Buffer.from(body.signature, 'base64url')), true);
});

test('origins are plain, signed HTTP is loopback only, dispatch is bound', async () => {
  for (const origin of ['https://user:pass@swarmmemo.com', 'https://swarmmemo.com/path', 'https://swarmmemo.com/?', 'https://swarmmemo.com/#', 'https://swarmmemo.com?x=1', 'https://swarmmemo.com/../', ' https://swarmmemo.com', 'https://@swarmmemo.com', 'file:///tmp/test']) assert.throws(() => new Client({origin}), errorCode('invalid_origin'));
  assert.throws(() => new Client({origin: 'http://example.com', key: fixtureKey, allowInsecureLoopback: true}), errorCode('https_required'));
  const client = new Client(), prepared = client.prepare({operation: 'messages.list'});
  await assert.rejects(client.send({...prepared}), errorCode('binding_mismatch'));
  await assert.rejects(new Client({origin: 'https://publicbbs.com'}).send(prepared), errorCode('binding_mismatch'));
});

test('one transport rule (C454): plain HTTP only to loopback, signed or not, no flag', () => {
  // Same cases as the Python client and the MCP bridge (test_helpers.py, test_policy.py).
  for (const origin of ['http://localhost', 'http://localhost:8080', 'http://127.0.0.1:8080', 'http://127.0.0.2', 'http://127.255.255.254:9', 'http://[::1]:8080', 'http://[::ffff:7f00:1]']) {
    for (const key of [null, fixtureKey]) assert.equal(new Client({origin, key}).origin, origin);
    assert.equal(new Client({origin, key: fixtureKey, allowInsecureLoopback: true}).origin, origin, 'the deprecated flag stays accepted');
    assert.equal(new Client({origin, key: fixtureKey, allowInsecureLoopback: false}).origin, origin, 'the deprecated flag is a no-op');
  }
  for (const origin of ['http://localhost.evil.com', 'http://127.0.0.1.nip.io', 'http://evil.com', 'http://128.0.0.1', 'http://10.0.0.1', 'http://0.0.0.0', 'http://[::2]', 'http://[::ffff:808:808]', 'http://[::]', 'http://localhost.:8080']) {
    for (const key of [null, fixtureKey]) assert.throws(() => new Client({origin, key}), error => error.code === 'https_required' && /needs HTTPS/.test(error.message), origin);
  }
  // Userinfo and normalized forms never reach the host test.
  for (const origin of ['http://127.0.0.1@evil.com', 'http://evil.com@127.0.0.1', 'http://127.1', 'http://2130706433', 'http://0x7f.1', 'http://LOCALHOST', 'http://[::ffff:127.0.0.1]']) assert.throws(() => new Client({origin, key: fixtureKey}), errorCode('invalid_origin'), origin);
  assert.throws(() => new Client({origin: 'http://localhost', allowInsecureLoopback: 'yes'}), errorCode('invalid_option'));
  for (const host of ['localhost', '127.0.0.1', '127.9.9.9', '[::1]', '::1', '[::ffff:7f00:1]', '::ffff:127.0.0.1']) assert.equal(isLoopbackHost(host), true, host);
  for (const host of ['', 'localhost.evil.com', 'evil.localhost.com', '127.0.0.1.nip.io', 'nip.io', '128.0.0.1', '[::2]', '::ffff:8.8.8.8', '::1%eth0', undefined, null, 127]) assert.equal(isLoopbackHost(host), false, String(host));
});

test('unsigned clients need HTTPS for remote origins; loopback HTTP stays local development', async () => {
  // Anonymous reads over plaintext HTTP can be observed or rewritten in transit (Python parity, C413).
  for (const origin of ['http://example.org', 'http://example.org:8080']) {
    assert.throws(() => new Client({origin}), error => error.code === 'https_required' && /needs HTTPS/.test(error.message));
    assert.throws(() => new Client({origin, allowInsecureLoopback: true}), errorCode('https_required'));
  }
  for (const origin of ['http://localhost:8080', 'http://127.0.0.1:8080', 'http://[::1]:8080']) assert.equal(new Client({origin}).origin, origin);
});

test('lost response retries the exact signed envelope and stable request ID', async t => {
  const bodies = [];
  const origin = await server(t, (req, res) => {
    let body = ''; req.on('data', chunk => {body += chunk;}); req.on('end', () => {bodies.push(body); if (bodies.length === 1) req.socket.destroy(); else {res.setHeader('Content-Type', 'application/json'); res.end('{"ok":true,"receipt":{"duplicate":true}}');}});
  });
  const client = new Client({origin, key: fixtureKey, allowInsecureLoopback: true});
  const prepared = client.prepare({operation: 'post', text: 'private-sentinel', to: 'a'.repeat(64)});
  await assert.rejects(client.send(prepared), errorCode('network_error'));
  assert.equal((await client.send(prepared)).receipt.duplicate, true);
  assert.equal(bodies[0], bodies[1]); assert.equal(JSON.parse(bodies[1]).request_id, prepared.command.request_id);
});

test('explicit GET/base64/c64 transports preserve bytes and reject oversized URLs', async t => {
  const paths = [];
  const origin = await server(t, (req, res) => {paths.push(req.url); res.end('{"ok":true}');});
  const client = new Client({origin, anonymous: true});
  const command = {operation: 'post', room: 'lobby', page: 'main', text: 'café <>& 🌍', request_id: 'transport-1'};
  for (const transport of ['get', 'base64', 'c64']) await client.send(client.prepare(command, {transport}));
  assert.equal(new URL(paths[0], origin).searchParams.get('text'), command.text);
  assert.equal(Buffer.from(new URL(paths[1], origin).pathname.split('/').at(-1), 'base64url').toString(), command.text);
  assert.deepEqual(JSON.parse(Buffer.from(paths[2].split('/').at(-1), 'base64url').toString()), command);
  assert.throws(() => client.prepare({...command, text: 'a'.repeat(9000)}, {transport: 'c64'}), errorCode('request_too_large'));
  assert.throws(() => client.prepare({...command, to: 'a'.repeat(64)}, {transport: 'get'}), errorCode('invalid_transport'));
  assert.throws(() => client.prepare({...command, room: '..'}, {transport: 'base64'}), errorCode('invalid_post'));
});

test('redirects never reach a second origin and error details remain sanitized', async t => {
  let hits = 0;
  const sink = await server(t, (_req, res) => {hits++; res.end('{"ok":true}');});
  const origin = await server(t, (_req, res) => {res.writeHead(307, {Location: sink + '/leak'}); res.end('private-sentinel');});
  const client = new Client({origin});
  await assert.rejects(client.send(client.prepare({operation: 'messages.list'})), errorCode('redirect_refused')); assert.equal(hits, 0);
  const errors = await server(t, (_req, res) => {res.writeHead(429, {'Retry-After': '4'}); res.end(JSON.stringify({ok: false, error: {code: 'request_rate', message: 'private-sentinel\u001b[2J'}}));});
  const limited = new Client({origin: errors});
  await assert.rejects(limited.send(limited.prepare({operation: 'messages.list'})), error => error.code === 'request_rate' && error.status === 429 && error.retryAfter === 4 && error.nextOpenAt === undefined && !error.message.includes('sentinel'));
  // C596: a timed refusal says when it lifts (next_open_at), and its body's retry_after stands in for a missing header.
  const timed = await server(t, (_req, res) => {res.writeHead(409); res.end(JSON.stringify({ok: false, error: {code: 'duplicate_reply', message: 'x', retry_after: 600, next_open_at: 1792972800}}));});
  const early = new Client({origin: timed});
  await assert.rejects(early.send(early.prepare({operation: 'messages.list'})), error => error.code === 'duplicate_reply' && error.status === 409 && error.retryAfter === 600 && error.nextOpenAt === 1792972800);
  const echo = await server(t, (_req, res) => {res.writeHead(400); res.end('{"ok":false,"error":{"code":"private_sentinel_account_token"}}');});
  const reflected = new Client({origin: echo});
  await assert.rejects(reflected.send(reflected.prepare({operation: 'post', text: 'private_sentinel_account_token'})), error => error.code === 'http_error' && !error.message.includes('private_sentinel'));
});

test('responses are bounded while streaming and deadline includes a slow body', async t => {
  const large = await server(t, (_req, res) => {res.write('{"text":"'); for (let i = 0; i < 8; i++) res.write('x'.repeat(32)); res.end('"}');});
  const bounded = new Client({origin: large, maxResponseBytes: 64});
  await assert.rejects(bounded.send(bounded.prepare({operation: 'messages.list'})), errorCode('response_too_large'));
  assert.throws(() => new Client({maxResponseBytes: 8 * 1024 * 1024 + 1}), errorCode('invalid_limit'));
  const slow = await server(t, (req, res) => {res.write('{'); const timer = setInterval(() => res.write(' '), 10); req.on('close', () => clearInterval(timer));});
  const deadline = new Client({origin: slow, timeoutMs: 60}), start = Date.now();
  await assert.rejects(deadline.send(deadline.prepare({operation: 'messages.list'})), errorCode('timeout'));
  assert.ok(Date.now() - start < 1000, 'overall deadline must not reset on received bytes');
});

test('the response limit is on decoded bytes, not an encoded Content-Length', async t => {
  // gzip level 0 stores the bytes, so the encoded body is larger than the JSON.
  const json = Buffer.from(JSON.stringify({ok: true, data: {text: 'y'.repeat(200)}}));
  const encoded = gzipSync(json, {level: 0});
  assert.ok(encoded.length > json.length);
  const origin = await server(t, (_req, res) => {res.writeHead(200, {'Content-Type': 'application/json', 'Content-Encoding': 'gzip', 'Content-Length': String(encoded.length)}); res.end(encoded);});
  const exact = new Client({origin, maxResponseBytes: json.length});
  assert.equal((await exact.send(exact.prepare({operation: 'messages.list'}))).data.text.length, 200);
  const under = new Client({origin, maxResponseBytes: json.length - 1});
  await assert.rejects(under.send(under.prepare({operation: 'messages.list'})), errorCode('response_too_large'));
});

test('malformed responses and declared oversize bodies fail without echoing contents', async t => {
  const malformed = await server(t, (_req, res) => res.end(Buffer.from([0xc0, 0xaf])));
  const client = new Client({origin: malformed});
  await assert.rejects(client.send(client.prepare({operation: 'messages.list'})), errorCode('invalid_response'));
  const oversized = await server(t, (_req, res) => {res.writeHead(200, {'Content-Length': String(9 * 1024 * 1024)}); res.flushHeaders();});
  const bounded = new Client({origin: oversized});
  await assert.rejects(bounded.send(bounded.prepare({operation: 'messages.list'})), errorCode('response_too_large'));
});

test('allowance, memory, trust and vouch helpers send the documented command shapes', async t => {
  const bodies = [];
  const origin = await server(t, (req, res) => {
    let body = ''; req.on('data', chunk => {body += chunk;});
    req.on('end', () => {bodies.push(JSON.parse(body)); res.writeHead(503, {'Content-Type': 'application/json'}); res.end('{"ok":false,"error":{"code":"service_unavailable","message":"not enabled"}}');});
  });
  const agent = 'a'.repeat(64), signed = new Client({origin, key: fixtureKey, allowInsecureLoopback: true}), anonymous = new Client({origin, anonymous: true});
  const cases = [
    [() => signed.allowance(), {operation: 'allowance.get'}],
    [() => anonymous.allowance(agent), {operation: 'allowance.get', target: agent}],
    [() => signed.transfer(agent, 500, {resource: 'credit', requestId: 'gift-1'}), {operation: 'allowance.transfer', target: agent, amount: 500, request_id: 'gift-1', data: '{"schema":1,"resource":"credit"}'}],
    [() => signed.transfer(agent, 4096, {resource: 'post_bytes', requestId: 'gift-2'}), {operation: 'allowance.transfer', target: agent, amount: 4096, request_id: 'gift-2', data: '{"schema":1,"resource":"post_bytes"}'}],
    [() => signed.cancelTransfer('t'.repeat(32), {requestId: 'cancel-1'}), {operation: 'allowance.transfer.cancel', target: 't'.repeat(32), request_id: 'cancel-1'}],
    [() => anonymous.ledger(), {operation: 'ledger.list'}],
    [() => anonymous.ledger({agent, cursor: 'c1', limit: 5}), {operation: 'ledger.list', target: agent, cursor: 'c1', limit: 5}],
    [() => anonymous.services(), {operation: 'services.list'}],
    [() => signed.memoryPut('notes/a', 'café', {visibility: 'public', requestId: 'put-1'}), {operation: 'service.call', target: 'memory', request_id: 'put-1', data: '{"schema":1,"method":"put","args":{"key":"notes/a","value":"café","visibility":"public"},"max_cost":268}'}],
    [() => signed.memoryPut('k', 'v', {maxCost: 999, requestId: 'put-2'}), {operation: 'service.call', target: 'memory', request_id: 'put-2', data: '{"schema":1,"method":"put","args":{"key":"k","value":"v","visibility":"private"},"max_cost":999}'}],
    [() => signed.memoryDelete('k', {requestId: 'del-1'}), {operation: 'service.call', target: 'memory', request_id: 'del-1', data: '{"schema":1,"method":"delete","args":{"key":"k"},"max_cost":64}'}],
    [() => signed.memoryGet('k'), {operation: 'service.read', target: 'memory', data: '{"schema":1,"method":"get","args":{"key":"k"}}'}],
    [() => anonymous.memoryGet('k', {agent}), {operation: 'service.read', target: 'memory', data: `{"schema":1,"method":"get","args":{"key":"k","agent":"${agent}"}}`}],
    [() => anonymous.memoryList(), {operation: 'service.read', target: 'memory', data: '{"schema":1,"method":"list","args":{}}'}],
    [() => anonymous.memoryList({prefix: 'notes/', cursor: 'c1', agent}), {operation: 'service.read', target: 'memory', data: `{"schema":1,"method":"list","args":{"prefix":"notes/","after":"c1","agent":"${agent}"}}`}],
    [() => anonymous.trust(agent), {operation: 'trust.get', target: agent}],
    [() => signed.trust(), {operation: 'trust.get'}], // your own record: no target
    [() => signed.vouch(agent, {requestId: 'vouch-1'}), {operation: 'vouch', target: agent, request_id: 'vouch-1', data: '{"schema":1,"value":1,"sponsor":false}'}],
    [() => signed.vouch(agent, {value: 0, sponsor: true, requestId: 'vouch-2'}), {operation: 'vouch', target: agent, request_id: 'vouch-2', data: '{"schema":1,"value":0,"sponsor":true}'}],
    [() => signed.webhookCreate('https://example.org/hook', {requestId: 'hook-1'}), {operation: 'webhook.create', request_id: 'hook-1', data: '{"schema":1,"url":"https://example.org/hook"}'}],
    [() => signed.webhookCreate('https://example.org/hook', {kinds: ['room_activity', 'reply'], requestId: 'hook-2'}), {operation: 'webhook.create', request_id: 'hook-2', data: '{"schema":1,"url":"https://example.org/hook","kinds":["room_activity","reply"]}'}],
    [() => signed.webhooks({limit: 5}), {operation: 'webhook.list', limit: 5}],
    [() => signed.webhookDelete('s'.repeat(32), {requestId: 'hook-3'}), {operation: 'webhook.delete', target: 's'.repeat(32), request_id: 'hook-3'}],
    [() => signed.withdraw('e'.repeat(32), {requestId: 'withdraw-1'}), {operation: 'message.withdraw', message_id: 'e'.repeat(32), request_id: 'withdraw-1'}],
    [() => signed.updates({records: 'include'}), {operation: 'updates.get', target: fixtureKey.fingerprint, data: '{"schema":1,"records":"include"}'}],
  ];
  for (const [call, expected] of cases) {
    await assert.rejects(call(), error => errorCode('service_unavailable')(error) && error.status === 503);
    const body = bodies.shift(), shown = {...body};
    for (const field of ['public_key', 'timestamp', 'nonce', 'signature']) delete shown[field];
    assert.deepEqual(shown, expected);
    assert.equal(!!body.signature, call.toString().startsWith('() => signed.'));
    if (!body.signature) {assert.equal(body.public_key, undefined); continue;}
    const unsigned = {...body}; delete unsigned.signature;
    assert.equal(body.public_key, fixtureKey.public_key);
    assert.equal(verify(null, canonical(unsigned), createPublicKey({key: Buffer.concat([Buffer.from('302a300506032b6570032100', 'hex'), Buffer.from(fixtureKey.public_key, 'base64url')]), format: 'der', type: 'spki'}), Buffer.from(body.signature, 'base64url')), true);
  }
  // transfer names what it moves (C365): no resource, or an unknown one, is refused before anything is sent.
  for (const options of [{}, {resource: 'bytes'}]) {
    await assert.rejects(signed.transfer(agent, 500, options), error => errorCode('invalid_option')(error) && /credit, |post_bytes/.test(error.message));
  }
  assert.equal(bodies.length, 0);
  for (const call of [() => signed.memoryDelete('k'), () => signed.vouch(agent), () => signed.cancelTransfer('t'.repeat(32))]) {
    await assert.rejects(call(), errorCode('service_unavailable'));
    assert.match(bodies.shift().request_id, /^[0-9a-f-]{36}$/);
  }
  await assert.rejects(signed.memoryPut('k', 'v', {visibility: 'team'}), errorCode('invalid_option'));
  await assert.rejects(signed.vouch(agent, {value: 2}), errorCode('invalid_option'));
  await assert.rejects(signed.webhookCreate('https://example.org/hook', {kinds: 'reply'}), errorCode('invalid_option'));
  await assert.rejects(signed.memoryPut('k', 'v', {maxCost: 1.5}), errorCode('invalid_option'));
  assert.equal(bodies.length, 0);
});

test('work, updates, journal, docs, tools and /call helpers send the documented shapes, signed (C56)', async t => {
  const requests = [], agents = new Set(), generation = 'g'.repeat(32), agent = 'a'.repeat(64), id = 'm'.repeat(26);
  const origin = await server(t, (req, res) => {
    let body = ''; req.on('data', chunk => {body += chunk;});
    req.on('end', () => {
      const parsed = req.method === 'GET' ? {} : JSON.parse(body); requests.push({path: req.url, body: parsed});
      agents.add(req.headers['user-agent']);
      const reply = req.url === '/api/changes?after=-1' ? {ok: true, generation, changes: []}
        : parsed.operation === 'work.get' ? {ok: true, data: {work: {id, service_generation: generation, fence: 3, worker_subject: 'agent:' + agent}}}
        : parsed.operation === 'agent.get' ? {ok: true, agent: {id: agent}}
        : parsed.operation === 'updates.get' ? {ok: true, messages: [], next_cursor: parsed.cursor ? parsed.cursor + '+' : 'c1', data: {}}
        : parsed.operation === 'subject.resolve' ? {ok: true, data: {subject: {id: 's'.repeat(26), uri: 'site:https://example.org/', exists: false}}}
        : {ok: true};
      res.writeHead(200, {'Content-Type': 'application/json'}); res.end(JSON.stringify(reply));
    });
  });
  const signed = new Client({origin, key: fixtureKey, allowInsecureLoopback: true}), anonymous = new Client({origin, anonymous: true});
  const verifySigned = body => {
    const unsigned = {...body}; delete unsigned.signature;
    assert.equal(body.public_key, fixtureKey.public_key);
    assert.equal(verify(null, canonical(unsigned), createPublicKey({key: Buffer.concat([Buffer.from('302a300506032b6570032100', 'hex'), Buffer.from(fixtureKey.public_key, 'base64url')]), format: 'der', type: 'spki'}), Buffer.from(body.signature, 'base64url')), true);
  };
  const shown = body => {const copy = {...body}; for (const field of ['public_key', 'timestamp', 'nonce', 'signature']) delete copy[field]; return copy;};
  const work = (fields, extra = '') => ({...fields, data: `{"schema":1,"generation":"${generation}"${extra}}`});
  const cases = [
    [() => anonymous.works({kind: 'rewarded', limit: 5}), {operation: 'works.list', kind: 'rewarded', limit: 5}],
    [() => signed.works({kind: 'earn', eligibleFor: agent}), {operation: 'works.list', kind: 'earn', data: `{"schema":1,"eligible_for":"${agent}"}`}],
    [() => anonymous.works({standing: id}), {operation: 'works.list', data: `{"schema":1,"standing":"${id}"}`}],
    [() => anonymous.work(id, {agent}), {operation: 'work.get', message_id: id, target: agent}],
    [() => signed.claimWork(id, {generation, requestId: 'c-1'}), work({operation: 'work.claim', message_id: id, ttl: 3600, request_id: 'c-1'})],
    [() => signed.claimWork(id, {result: 'r1', generation, resultSha256: 'f'.repeat(64), requestId: 'c-2'}), work({operation: 'work.claim', message_id: id, target: 'r1', request_id: 'c-2'}, `,"result_sha256":"${'f'.repeat(64)}"`)],
    [() => signed.submitWork(id, 3, 'r1', {generation, requestId: 's-1'}), work({operation: 'work.submit', message_id: id, amount: 3, target: 'r1', request_id: 's-1'})],
    [() => signed.submitWork(id, 3, 'r1', {generation, requestId: 's-2', payoutAddress: '0xabc'}), work({operation: 'work.submit', message_id: id, amount: 3, target: 'r1', request_id: 's-2'}, ',"payout_address":"0xabc"')],
    [() => signed.claimWork(id, {result: 'r1', generation, requestId: 'c-3', payoutAddress: '0xabc'}), work({operation: 'work.claim', message_id: id, target: 'r1', request_id: 'c-3'}, ',"payout_address":"0xabc"')],
    [() => signed.acceptWork(id, 3, {generation, requestId: 'a-1'}), work({operation: 'work.accept', message_id: id, amount: 3, request_id: 'a-1'})],
    [() => signed.acceptWork(id, 3, {generation, tier: 'bug+fix', requestId: 'a-5'}), work({operation: 'work.accept', message_id: id, amount: 3, request_id: 'a-5'}, ',"tier":"bug+fix"')],
    [() => signed.rejectWork(id, 3, 'Breaks the build.', {generation, requestId: 'j-1'}), work({operation: 'work.reject', message_id: id, amount: 3, reason: 'Breaks the build.', request_id: 'j-1'})],
    [() => signed.acceptWork(id, 3, {generation, resultSha256: 'f'.repeat(64), checks: [{property: 'integrity', state: 'pass', subject_sha256: 'a'.repeat(64)}, {property: 'conformance', state: 'not_checked'}], requestId: 'a-3'}), work({operation: 'work.accept', message_id: id, amount: 3, request_id: 'a-3'}, `,"result_sha256":"${'f'.repeat(64)}","checks":[{"property":"integrity","state":"pass","subject_sha256":"${'a'.repeat(64)}"},{"property":"conformance","state":"not_checked"}]`)],
    [() => signed.rejectWork(id, 3, 'Fails conformance.', {generation, checks: [{property: 'conformance', state: 'fail', evidence: 'https://example.org/run/1'}], requestId: 'j-2'}), work({operation: 'work.reject', message_id: id, amount: 3, reason: 'Fails conformance.', request_id: 'j-2'}, ',"checks":[{"property":"conformance","state":"fail","evidence":"https://example.org/run/1"}]')],
    [() => signed.updates(), {operation: 'updates.get', target: fixtureKey.fingerprint}],
    [() => signed.updates({cursor: 'c9', wait: 25, limit: 10}), {operation: 'updates.get', target: fixtureKey.fingerprint, cursor: 'c9', limit: 10, data: '{"schema":1,"wait":25}'}],
    [() => anonymous.updates({agent, wait: 5}), {operation: 'updates.get', target: agent}],
    [() => anonymous.updates({agent, cursor: 'c9', counts: true}), {operation: 'updates.get', target: agent, cursor: 'c9', data: '{"schema":1,"counts":true}'}],
    [() => signed.journal({cursor: 'c9', limit: 20}), {operation: 'journal.get', cursor: 'c9', limit: 20}],
    [() => signed.dispose(['e1', id], 'answered_elsewhere', {requestId: 'x-1'}), {operation: 'updates.dispose', request_id: 'x-1', data: `{"schema":1,"ids":["e1","${id}"],"state":"answered_elsewhere"}`}],
    [() => signed.markRead([id], {requestId: 'x-2'}), {operation: 'updates.mark_read', request_id: 'x-2', data: `{"schema":1,"ids":["${id}"]}`}],
    [() => signed.markRead(null, {all: true, requestId: 'x-3'}), {operation: 'updates.mark_read', request_id: 'x-3', data: '{"schema":1,"all":true}'}],
    [() => signed.docsCreate('Build log', 'All green.', {visibility: 'unlisted', expiresIn: 86400, maxCost: 4, requestId: 'd-1'}), {operation: 'service.call', target: 'docs', request_id: 'd-1', data: '{"schema":1,"method":"create","args":{"title":"Build log","text":"All green.","visibility":"unlisted","expires_in":86400},"max_cost":4}'}],
    [() => signed.docsWrite('DOC', 1, 'Deployed.', {requestId: 'd-2'}), {operation: 'service.call', target: 'docs', request_id: 'd-2', data: '{"schema":1,"method":"write","args":{"id":"DOC","base_version":1,"text":"Deployed."},"max_cost":1099511627776}'}],
    [() => signed.docsRead('DOC', {version: 2, requestId: 'd-3'}), {operation: 'service.call', target: 'docs', request_id: 'd-3', data: '{"schema":1,"method":"read","args":{"id":"DOC","version":2},"max_cost":1099511627776}'}],
    [() => signed.docsRead(null, {hash: 'a'.repeat(64), requestId: 'd-6'}), {operation: 'service.call', target: 'docs', request_id: 'd-6', data: `{"schema":1,"method":"read","args":{"hash":"${'a'.repeat(64)}"},"max_cost":1099511627776}`}],
    [() => anonymous.docsOpen('DOC', {screen: false, requestId: 'd-4'}), {operation: 'service.call', target: 'docs', request_id: 'd-4', data: '{"schema":1,"method":"open","args":{"id":"DOC","screen":false},"max_cost":1099511627776}'}],
    [() => signed.docsDelete('DOC', {requestId: 'd-5'}), {operation: 'service.call', target: 'docs', request_id: 'd-5', data: '{"schema":1,"method":"delete","args":{"id":"DOC"},"max_cost":1099511627776}'}],
    [() => signed.docsHistory('DOC', {limit: 5}), {operation: 'service.read', target: 'docs', data: '{"schema":1,"method":"history","args":{"id":"DOC","limit":5}}'}],
    [() => signed.docsList({kind: 'paste', limit: 20}), {operation: 'service.read', target: 'docs', data: '{"schema":1,"method":"list","args":{"kind":"paste","limit":20}}'}],
    [() => anonymous.serviceRead('public_data', 'datasets'), {operation: 'service.read', target: 'public_data', data: '{"schema":1,"method":"datasets","args":{}}'}],
    [() => anonymous.toolsSearch('weather forecast', {kind: 'catalogue'}), {operation: 'service.read', target: 'tools', data: '{"schema":1,"method":"search","args":{"query":"weather forecast","kind":"catalogue"}}'}],
    [() => signed.toolsCall('tool:wx', {city: 'Lisbon'}, {maxCost: 30, requestId: 't-1'}), {operation: 'service.call', target: 'tools', request_id: 't-1', data: '{"schema":1,"method":"call","args":{"id":"tool:wx","args":{"city":"Lisbon"}},"max_cost":30}'}],
    [() => signed.serviceCall('wakeup', 'schedule', {key: 'replies', on: 'reply'}, {maxCost: 1, requestId: 'w-1'}), {operation: 'service.call', target: 'wakeup', request_id: 'w-1', data: '{"schema":1,"method":"schedule","args":{"key":"replies","on":"reply"},"max_cost":1}'}],
    [() => anonymous.subjectResolve('example.org'), {operation: 'subject.resolve', target: 'example.org'}],
    [() => anonymous.subject('site:https://example.org/', {limit: 5}), {operation: 'subject.get', target: 'site:https://example.org/', limit: 5}],
    [() => anonymous.find('weather', {types: ['agent', 'mcp'], maxPrice: 0.01, limit: 5}), {operation: 'find', query: 'weather', limit: 5, data: '{"schema":1,"max_price":"0.01","type":["agent","mcp"]}'}],
    [() => anonymous.find(), {operation: 'find'}],
    [() => signed.verifyPublish({repo: 'https://github.com/o/r', commit: 'c'.repeat(40), command: 'pytest -q', environment: 'linux', outputSha256: 'a'.repeat(64), checks: [{property: 'tests', state: 'pass', note: '41 passed'}], limits: 'Windows', requestId: 'vp-1'}),
      {operation: 'verify.publish', request_id: 'vp-1', data: `{"schema":1,"repo":"https://github.com/o/r","commit":"${'c'.repeat(40)}","command":"pytest -q","environment":"linux","output_sha256":"${'a'.repeat(64)}","checks":[{"property":"tests","state":"pass","note":"41 passed"}],"limits":"Windows"}`}],
    [() => signed.verifyPublish({repo: 'https://github.com/o/r', commit: 'c'.repeat(40), command: 'pytest -q', environment: 'linux', outputSha256: 'a'.repeat(64), checks: [{property: 'tests', state: 'pass', note: 'ok'}], limits: 'Windows', supersedes: '0123456789abcdef', requestId: 'vp-sup'}),
      {operation: 'verify.publish', request_id: 'vp-sup', data: `{"schema":1,"repo":"https://github.com/o/r","commit":"${'c'.repeat(40)}","command":"pytest -q","environment":"linux","output_sha256":"${'a'.repeat(64)}","checks":[{"property":"tests","state":"pass","note":"ok"}],"limits":"Windows","supersedes":"0123456789abcdef"}`}],
    [() => signed.verifyPublish({fileSha256: 'd'.repeat(64), command: 'pytest -q', environment: 'windows', outputSha256: OK_SHA256, checks: [{property: 'tests', state: 'pass'}], limits: 'Linux', outputBytes: 'ok\n', stdioEncoding: 'utf-8', requestId: 'vp-out'}),
      {operation: 'verify.publish', request_id: 'vp-out', data: `{"schema":1,"file_sha256":"${'d'.repeat(64)}","command":"pytest -q","environment":"windows","output_sha256":"${OK_SHA256}","checks":[{"property":"tests","state":"pass"}],"limits":"Linux","output":"b2sK","stdio_encoding":"utf-8"}`}],
    [() => anonymous.verification('0123456789abcdef'), {operation: 'verify.get', target: '0123456789abcdef'}],
    [() => anonymous.verifications({repo: 'https://github.com/o/r', commit: 'c'.repeat(40), limit: 5}), {operation: 'verify.list', target: 'https://github.com/o/r', query: 'c'.repeat(40), limit: 5}],
    [() => anonymous.find('weather', {types: 'agent'}), {operation: 'find', query: 'weather', data: '{"schema":1,"type":["agent"]}'}],
    [() => anonymous.find('sign up', {protocol: 'mpp', door: 'agent', parse: 'off'}), {operation: 'find', query: 'sign up', data: '{"schema":1,"protocol":"mpp","door":"agent","parse":"off"}'}],
  ];
  for (const [call, expected] of cases) {
    await call();
    const {path, body} = requests.shift();
    assert.equal(path, '/v1/command');
    assert.deepEqual(shown(body), expected, call.toString());
    assert.equal(!!body.signature, call.toString().startsWith('() => signed.'));
    if (body.signature) verifySigned(body); else assert.equal(body.public_key, undefined);
  }
  // A review resolves its subject, then signs the post the board expects; unsigned, it is refused.
  await signed.review('https://Example.org/page', 'good', 'Answered fast.', {dimensions: {works: 90}, evidence: ['call:' + 'b'.repeat(32)], relationship: 'customer', requestId: 'r-1'});
  assert.deepEqual(requests.map(r => r.body.operation), ['subject.resolve', 'post']);
  assert.deepEqual(shown(requests[1].body), {operation: 'post', room: 'reviews', page: 's'.repeat(26), kind: 'review', text: 'Answered fast.', request_id: 'r-1',
    data: `{"schema":1,"review":{"subject":"site:https://example.org/","verdict":"good","dimensions":{"works":90},"relationship":"customer","evidence":["call:${'b'.repeat(32)}"]}}`});
  verifySigned(requests[1].body); requests.length = 0;
  // A lone evidence id is one item, not its characters (db91c24a).
  await signed.review('https://Example.org/page', 'good', 'One call.', {evidence: 'call:' + 'c'.repeat(32), requestId: 'r-3'});
  assert.deepEqual(JSON.parse(requests[1].body.data).review.evidence, ['call:' + 'c'.repeat(32)]); requests.length = 0;
  await signed.review('site:https://example.org/', 'bad', 'Worse now.', {supersedes: 'e'.repeat(32), requestId: 'r-2'});
  assert.equal(JSON.parse(requests[1].body.data).supersedes, 'e'.repeat(32)); requests.length = 0;
  await assert.rejects(anonymous.review('example.org', 'good', 'Unsigned.'), /signed/);
  assert.equal(requests.length, 0);
  // work.reviewer.set signs the reviewer's fingerprint; a handle is read once with agent.get.
  await signed.setWorkReviewer(id, agent, {generation, requestId: 'v-1'});
  assert.deepEqual(requests.map(r => shown(r.body)), [{operation: 'work.reviewer.set', message_id: id, request_id: 'v-1', data: `{"schema":1,"generation":"${generation}","reviewer":"${agent}"}`}]);
  verifySigned(requests[0].body); requests.length = 0;
  await signed.setWorkReviewer(id, 'judge', {generation, requestId: 'v-2'});
  assert.deepEqual(requests.map(r => r.body.operation), ['agent.get', 'work.reviewer.set']);
  assert.equal(requests[0].body.target, 'judge');
  assert.equal(requests[1].body.data, `{"schema":1,"generation":"${generation}","reviewer":"${agent}"}`); verifySigned(requests[1].body); requests.length = 0;
  // work.create of a standing task (C311) signs exactly the data testdata/work_standing_create.json
  // names, as the Python client and CLI build it; options are the wire names in camelCase.
  const standing = JSON.parse(await readFile(new URL('../../testdata/work_standing_create.json', import.meta.url), 'utf8'));
  const options = {};
  for (let i = 0; i < standing.argv.length; i += 2) {
    const name = standing.argv[i].slice(2).replace(/-([a-z])/g, (_, c) => c.toUpperCase()), value = standing.argv[i + 1];
    if (name === 'tier') (options.tiers ??= []).push(value);
    else options[name] = name.startsWith('usdc') ? value : Number(value);
  }
  assert.deepEqual([...WORK_STANDING_CREATE], ['day_max', 'per_agent_day', 'results_max', 'seats', 'tiers', 'usdc_day_max', 'usdc_per_agent_day']);
  assert.equal(JSON.stringify(workCreateData({title: standing.title, capabilities: standing.capabilities, generation: standing.generation, ...options})), standing.data);
  for (const tier of standing.tiers_refused) assert.throws(() => standingTier(tier), errorCode('invalid_option'), tier); // C331, as Python refuses them
  await signed.createWork(id, {title: standing.title, capabilities: standing.capabilities, generation: standing.generation, ttl: 864000, requestId: 'w-1', ...options});
  assert.deepEqual(requests.map(r => shown(r.body)), [{operation: 'work.create', message_id: id, ttl: 864000, request_id: 'w-1', data: standing.data}]);
  verifySigned(requests[0].body); requests.length = 0;
  // Left out, the generation is the service's; a handle reviewer is read once with agent.get.
  await signed.createWork(id, {title: 'Review', reward: 300, reviewer: 'judge', reviewerFee: 20});
  assert.deepEqual(requests.map(r => r.path === '/v1/command' ? r.body.operation : r.path), ['/api/changes?after=-1', 'agent.get', 'work.create']);
  assert.equal(requests[2].body.data, `{"schema":1,"generation":"${generation}","title":"Review","capabilities":[],"reward":300,"reviewer":"${agent}","reviewer_fee":20}`); requests.length = 0;
  for (const bad of [{seats: 0, reward: 5}, {dayMax: 3}, {seats: 2, perAgentDay: 3}, {seats: 2, tiers: ['a:1', 'b:1', 'c:1', 'd:1']}, {seats: 2, tiers: ['Bad:1']}, {seats: 2, tiers: ['bug:0']}, {seats: 2, tiers: ['bug']}, {seats: 2, nonsense: 1},
    {seats: 2, resultsMax: 1}, {seats: 2, resultsMax: CONTRACT.limits.work_standing_results_maximum + 1}, {seats: 2, resultsMax: '3'}, {resultsMax: 3}])
    assert.throws(() => workCreateData({title: 't', generation: 'g', ...bad}), errorCode('invalid_option'), JSON.stringify(bad));
  assert.equal(requests.length, 0);
  // work.cancel closes a standing task early (C346), signing its reason and the generation.
  await signed.cancelWork(id, 'closed: enough results', {generation, requestId: 'c-1'});
  assert.deepEqual(requests.map(r => shown(r.body)), [{operation: 'work.cancel', message_id: id, reason: 'closed: enough results', request_id: 'c-1', data: `{"schema":1,"generation":"${generation}"}`}]);
  verifySigned(requests[0].body); requests.length = 0;
  // work.settle signs the transaction that paid a USDC reward.
  const paidTx = '0x' + 'b'.repeat(64);
  await signed.settleWork(id, paidTx, {generation, requestId: 't-1'});
  assert.deepEqual(requests.map(r => shown(r.body)), [{operation: 'work.settle', message_id: id, request_id: 't-1', data: `{"schema":1,"generation":"${generation}","tx_hash":"${paidTx}"}`}]);
  verifySigned(requests[0].body); requests.length = 0;
  await assert.rejects(signed.settleWork(id, '0xABC', {generation}), /txHash/);
  // Left out, the generation is read once with work.get, then signed into the transition.
  await signed.acceptWork(id, 3, {requestId: 'a-2'});
  assert.deepEqual(requests.map(r => r.body.operation), ['work.get', 'work.accept']);
  assert.equal(requests[1].body.data, `{"schema":1,"generation":"${generation}"}`); verifySigned(requests[1].body); requests.length = 0;
  // A rated accept (C300): one work.get (worker_subject), then the accept carrying the signed review post.
  assert.equal(agentSubjectID('a'.repeat(64)), 'gv3vfww72ravwux2dfdyxmfhbm'); // the Go and Python vector
  await signed.acceptWork(id, 3, {rating: 40, note: 'Late, but right.', requestId: 'a-4'});
  assert.deepEqual(requests.map(r => r.body.operation), ['work.get', 'work.accept']);
  const rated = JSON.parse(requests[1].body.data);
  assert.equal(rated.generation, generation); verifySigned(requests[1].body); verifySigned(rated.review);
  assert.deepEqual(shown(rated.review), {operation: 'post', room: 'reviews', page: agentSubjectID(agent), kind: 'review', text: 'Late, but right.', request_id: rated.review.request_id,
    data: `{"schema":1,"review":{"subject":"agent:${agent}","verdict":"mixed","dimensions":{"good":40},"relationship":"customer","evidence":["work:${id}"]}}`});
  requests.length = 0;
  await assert.rejects(signed.acceptWork(id, 3, {rating: 101}), errorCode('invalid_option')); requests.length = 0;
  await assert.rejects(anonymous.acceptWork(id, 3, {rating: 50}), errorCode('missing_key'));
  await assert.rejects(signed.acceptWork(id, 3, {generation, tier: 'Bug Fix'}), errorCode('invalid_option')); requests.length = 0;
  // /call: unsigned POST, args as the body's fields, with max_cost and request_id.
  await signed.callUrl('fetch', 'page', {url: 'https://example.com/'}, {maxCost: 8, requestId: 'r'.repeat(16)});
  assert.deepEqual(requests.shift(), {path: '/call/fetch/page', body: {url: 'https://example.com/', max_cost: 8, request_id: 'r'.repeat(16)}});
  await anonymous.callUrl('public_data', 'fetch', {dataset: 'sea_ice_extent'});
  assert.deepEqual(requests.shift(), {path: '/call/public_data/fetch', body: {dataset: 'sea_ice_extent'}});
  // followUpdates resumes from and saves the cursor file, owner-only, after the page is handled.
  const cursorFile = join(await temp(t), 'cursor.json');
  const pages = signed.followUpdates({cursorFile, wait: 20});
  assert.equal((await pages.next()).value.next_cursor, 'c1');
  assert.equal(requests.shift().body.cursor, undefined);
  await assert.rejects(stat(cursorFile));
  await pages.next();
  assert.deepEqual(JSON.parse(await readFile(cursorFile, 'utf8')), {cursor: 'c1'});
  assert.equal((await stat(cursorFile)).mode & 0o777, 0o600);
  assert.deepEqual(shown(requests.shift().body), {operation: 'updates.get', target: fixtureKey.fingerprint, cursor: 'c1', data: '{"schema":1,"wait":20}'});
  await pages.return();
  const resumed = signed.followUpdates({cursorFile});
  await resumed.next(); await resumed.return();
  assert.equal(requests.shift().body.cursor, 'c1');
  // Refused locally, before anything is sent.
  await assert.rejects(signed.docsRead('DOC', {hash: 'a'.repeat(64)}), errorCode('invalid_option'));
  await assert.rejects(signed.docsRead(null), errorCode('invalid_option'));
  await assert.rejects(signed.toolsCall('tool:wx', {}), errorCode('invalid_option'));
  await assert.rejects(signed.updates({cursor: 'c', wait: 26}), errorCode('invalid_option'));
  await assert.rejects(anonymous.callUrl('https://x', 'page'), errorCode('invalid_service'));
  await assert.rejects(anonymous.callUrl('fetch', 'page', {}, {maxCost: -1}), errorCode('invalid_option'));
  assert.equal(requests.length, 0);
  assert.deepEqual([...agents], [USER_AGENT]); // C443: under Node every request names the client
});

test('direct module execution has no implicit CLI side effects', () => {
  const result = spawnSync(process.execPath, [fileURLToPath(new URL('./swarmmemo.mjs', import.meta.url))], {encoding: 'utf8'});
  assert.equal(result.status, 0); assert.equal(result.stdout, ''); assert.equal(result.stderr, '');
});

test('optional real Go key/canonical/signing interoperability', {skip: !process.env.SWARMMEMO_TEST_BINARY}, async t => {
  const binary = process.env.SWARMMEMO_TEST_BINARY, dir = await temp(t);
  assert.equal(spawnSync(binary, ['keygen', join(dir, 'go-key.json')]).status, 0);
  const goKey = await loadKey(join(dir, 'go-key.json')); assert.equal(Buffer.from(goKey.private_key, 'base64url').length, 32);
  const go = spawnSync(binary, ['canonical'], {input: JSON.stringify(vector.command), encoding: 'utf8', env: {...process.env, SERVICE_ID: 'swarmmemo.com'}}); assert.equal(go.status, 0); assert.equal(go.stdout, vector.canonical);
  const probe = createServer(); probe.listen(0, '127.0.0.1'); await once(probe, 'listening'); const port = probe.address().port; await new Promise(resolve => probe.close(resolve));
  const child = spawn(binary, ['serve'], {env: {...process.env, DATA_DIR: join(dir, 'data'), LISTEN_ADDR: `127.0.0.1:${port}`, ALLOW_INSECURE_LOCAL: 'true', ADMIN_TOKEN_FILE: '', SERVICE_ID: 'swarmmemo.com', PUBLIC_URL: `http://127.0.0.1:${port}`, TRUST_LOOPBACK_PROXY: 'false'}, stdio: 'ignore'});
  t.after(async () => {if (child.exitCode === null) {child.kill('SIGTERM'); await once(child, 'exit');}});
  const origin = `http://127.0.0.1:${port}`;
  let ready = false;
  for (let i = 0; i < 100; i++) {try {if ((await fetch(origin + '/health', {signal: AbortSignal.timeout(100)})).ok) {ready = true; break;}} catch (_) {} await new Promise(resolve => setTimeout(resolve, 25));}
  assert.ok(ready, 'isolated Go server did not become healthy');
  const client = new Client({origin, key: goKey, allowInsecureLoopback: true});
  await client.send(client.prepare({operation: 'agent.register'}));
  const anonymous = new Client({origin});
  for (const transport of ['get', 'base64']) assert.ok((await anonymous.send(anonymous.prepare({operation: 'post', text: 'Go transport check café ' + transport}, {transport}))).receipt.id);
  const posted = client.prepare({operation: 'post', text: 'Node to Go café <🌍>\u2028\u2029', to: goKey.fingerprint});
  const receipt = await client.send(posted); assert.ok(receipt.receipt.id); assert.equal((await client.send(posted)).receipt.duplicate, true);
  const nonceOnly = client.prepare(withoutRequestID(client.prepare({operation: 'post', text: 'Original nonce-only envelope'}), goKey));
  assert.equal(nonceOnly.command.request_id, undefined); assert.ok((await client.send(nonceOnly)).receipt.id); assert.equal((await client.send(nonceOnly)).receipt.duplicate, true);
  const next = generateKey(); await client.send(client.prepare({operation: 'agent.rotate'}, {successorKey: next}));
  const successor = new Client({origin, key: next, allowInsecureLoopback: true});
  const inbox = await successor.send(successor.prepare({operation: 'messages.list', to: next.fingerprint}, {transport: 'c64'})); assert.ok(inbox.messages.some(event => event.id === receipt.receipt.id));
});

test('optional real Go delegated work lifecycle, revocation and attribution', {skip: !process.env.SWARMMEMO_DELEGATION_TEST_BINARY}, async t => {
  const binary = process.env.SWARMMEMO_DELEGATION_TEST_BINARY, dir = await temp(t);
  const probe = createServer(); probe.listen(0, '127.0.0.1'); await once(probe, 'listening');
  const port = probe.address().port; await new Promise(resolve => probe.close(resolve));
  const origin = `http://127.0.0.1:${port}`;
  const serverProcess = spawn(binary, ['serve'], {env: {...process.env, DATA_DIR: join(dir, 'data'), LISTEN_ADDR: `127.0.0.1:${port}`, ALLOW_INSECURE_LOCAL: 'true', ADMIN_TOKEN_FILE: '', SERVICE_ID: 'swarmmemo.com', PUBLIC_URL: origin, TRUST_LOOPBACK_PROXY: 'false'}, stdio: 'ignore'});
  t.after(async () => {if (serverProcess.exitCode === null) {serverProcess.kill('SIGTERM'); await once(serverProcess, 'exit');}});
  let ready = false;
  for (let i = 0; i < 100; i++) {try {if ((await fetch(origin + '/health', {signal: AbortSignal.timeout(100)})).ok) {ready = true; break;}} catch (_) {} await new Promise(resolve => setTimeout(resolve, 25));}
  assert.ok(ready, 'isolated delegated Go server did not become healthy');
  const root = new Client({origin, key: generateKey(), allowInsecureLoopback: true});
  const requester = new Client({origin, key: generateKey(), allowInsecureLoopback: true});
  const send = (client, command, options) => client.send(client.prepare(command, options));
  await send(root, {operation: 'room.create', room: 'worker-lab', visibility: 'public'});
  const {generation} = await (await fetch(origin + '/api/changes?after=-1')).json();
  const data = JSON.stringify({schema: 1, generation});
  const key = generateKey(), operations = ['post', 'work.claim', 'work.renew', 'work.submit', 'work.get', 'work.history'];
  const enrollment = root.prepare({operation: 'delegation.create', room: 'worker-lab', ttl: 600, amount: 65536,
    data: JSON.stringify({schema: 1, generation, operations, disclosure: 'public'})}, {targetKey: key});
  const grant = await root.send(enrollment);
  assert.equal(grant.data.ack.grant_id, key.fingerprint);
  assert.deepEqual(await root.send(enrollment), grant);
  const worker = new DelegatedClient({origin, key, allowInsecureLoopback: true, grantId: key.fingerprint, generation, room: 'worker-lab', operations});
  const request = await send(requester, {operation: 'post', room: 'worker-lab', visibility: 'public', kind: 'simulation', text: 'Local-only worker delegation fixture; no reward or external execution.'});
  const id = request.receipt.id;
  await send(requester, {operation: 'work.create', message_id: id, ttl: 600, data: JSON.stringify({schema: 1, generation, title: 'Local delegation lifecycle', capabilities: []})});
  const claim = worker.prepare({operation: 'work.claim', message_id: id, ttl: 120, data}, {transport: 'c64'});
  const claimed = await worker.send(claim); assert.equal(claimed.data.ack.fence, 1);
  assert.deepEqual(await worker.send(claim), claimed);
  await send(worker, {operation: 'work.renew', message_id: id, ttl: 180, amount: 1, data});
  const result = worker.prepare({operation: 'post', room: 'worker-lab', visibility: 'public', kind: 'simulation', reply_to: id, text: 'Local result café 🌍\u2028\u2029'});
  const canonicalGo = spawnSync(binary, ['canonical'], {input: JSON.stringify(result.command), encoding: 'utf8', env: {...globalThis.process.env, SERVICE_ID: 'swarmmemo.com'}});
  assert.equal(canonicalGo.status, 0); assert.equal(canonicalGo.stdout, canonical(result.command).toString());
  const posted = await worker.send(result);
  await send(worker, {operation: 'work.submit', message_id: id, target: posted.receipt.id, amount: 1, data});
  await send(requester, {operation: 'work.accept', message_id: id, amount: 1, data});
  const work = (await send(worker, {operation: 'work.get', message_id: id})).data.work;
  assert.equal(work.state, 'accepted'); assert.equal(work.attempt_grant_id, key.fingerprint);
  const history = (await send(worker, {operation: 'work.history', message_id: id})).data.transitions;
  assert.equal(history.filter(entry => entry.delegation_id === key.fingerprint).length, 3);
  const event = (await send(new Client({origin}), {operation: 'message.get', message_id: posted.receipt.id})).messages[0];
  assert.equal(event.author, key.fingerprint); assert.equal(event.delegation_id, key.fingerprint);
  assert.equal(event.signed_payload, canonical(result.command).toString());
  const revocation = root.prepare({operation: 'delegation.revoke', target: key.fingerprint, data});
  await root.send(revocation);
  assert.equal((await worker.send(result)).receipt.duplicate, true, 'accepted exact retry survives revocation');
  assert.equal((await send(worker, {operation: 'delegation.get', target: key.fingerprint})).data.delegation.state, 'revoked');
  await assert.rejects(send(worker, {operation: 'work.get', message_id: id}), errorCode('delegation_inactive'));
  const proof = await (await fetch(origin + '/api/delegation/' + key.fingerprint)).json();
  assert.equal(proof.data.delegation.state, 'revoked');
  assert.equal(proof.data.delegation.signed_payload, canonical(enrollment.command).toString());
  assert.equal(proof.data.delegation.proof, enrollment.command.proof);
});

test('cursor files reject JSON arrays before requesting an updates page', async t => {
  const dir = await temp(t), cursorFile = join(dir, 'cursor.json');
  let calls = 0;
  class OfflineClient extends Client {async updates() {calls++; return {ok: true};}}
  for (const raw of ['[]', '["saved"]']) {
    const file = await open(cursorFile, 'w');
    await file.writeFile(raw); await file.close();
    const pages = new OfflineClient().followUpdates({cursorFile});
    try {await assert.rejects(pages.next(), errorCode('cursor_read_failed'));}
    finally {await pages.return();}
  }
  assert.equal(calls, 0);
});

test('C324: posts sign format markdown by default, by the detector Go wrote testdata/markdown_auto.json from', async () => {
  const {vectors} = JSON.parse(await readFile(new URL('../../testdata/markdown_auto.json', import.meta.url), 'utf8'));
  assert.ok(vectors.length > 20);
  for (const v of vectors) assert.equal(markdownAuto(v.text), v.render, v.name);
  const table = '| a | b |\n|---|---|\n| **x** | `y` |';
  const signed = new Client({key: fixtureKey}), anonymous = new Client({anonymous: true});
  const data = (client, text, options) => client.prepare({operation: 'post', room: 'lobby', text, ...(options?.data ? {data: options.data} : {})}, options).command.data;
  assert.equal(data(signed, table), MARKDOWN_POST_DATA);
  assert.equal(data(signed, 'Plain words, 2*3*4.'), undefined);
  assert.equal(data(signed, table, {format: 'plain'}), undefined);
  assert.equal(data(signed, 'plain', {format: 'markdown'}), MARKDOWN_POST_DATA);
  const edit = `{"schema":1,"supersedes":"${'a'.repeat(32)}"}`;
  assert.equal(data(signed, table, {data: edit}), edit); // the caller's data wins
  assert.equal(data(anonymous, table), undefined); // an unsigned post cannot sign a format
  assert.throws(() => signed.prepare({operation: 'post', text: 'x'}, {format: 'html'}), error => error.code === 'invalid_option');
});

test('C376: markdownAuto agrees with every text of the seeded parity set Go wrote to testdata/markdown_auto_fuzz.json', async () => {
  const {vectors} = JSON.parse(await readFile(new URL('../../testdata/markdown_auto_fuzz.json', import.meta.url), 'utf8'));
  assert.ok(vectors.length >= 5000);
  assert.deepEqual(vectors.filter(([text, render]) => markdownAuto(text) !== render), []);
});

const walletKeyHex ='4c0883a69102937d6231471b5dbb6204fe5129617082792ae468d01a3f362318', walletAddressHex = '0x2c7536E3605D9C16a7a3D7b1898e529396a65c23';
test('C343: personalSign reproduces the personal_sign vectors Go wrote to testdata/wallet_sign.json', async () => {
  assert.equal(keccak256(Buffer.alloc(0)).toString('hex'), 'c5d2460186f7233c927e7db2dcc703c0e500b653ca82273b7bfad8045d85a470');
  assert.equal(keccak256(Buffer.from('abc')).toString('hex'), '4e03657aea45a94fc7d47ba826c8d667c0d1e6e33a64a036ec44f58fa12d6c45');
  const {vectors} = JSON.parse(await readFile(new URL('../../testdata/wallet_sign.json', import.meta.url), 'utf8'));
  assert.ok(vectors.length >= 4);
  for (const v of vectors) {
    const key = Buffer.from(v.key, 'hex');
    assert.equal(walletAddress(key), v.address, v.name); assert.equal(personalSign(key, v.message), v.signature, v.name);
  }
  for (const key of [Buffer.alloc(32), Buffer.from('fffffffffffffffffffffffffffffffebaaedce6af48a03bbfd25e8cd0364141', 'hex'), Buffer.alloc(31), '00'.repeat(32)]) assert.throws(() => personalSign(key, 'x'), errorCode('invalid_wallet_key'));
});

test('C345: the OpenSSL (createECDH) and plain BigInt paths agree on the vectors and random keys', async () => {
  const {vectors} = JSON.parse(await readFile(new URL('../../testdata/wallet_sign.json', import.meta.url), 'utf8'));
  for (const v of vectors) {
    const key = Buffer.from(v.key, 'hex');
    assert.equal(walletAddress(key, {plain: true}), v.address, v.name); assert.equal(personalSign(key, v.message, {plain: true}), v.signature, v.name);
  }
  const ecdh = createECDH('secp256k1'); ecdh.setPrivateKey(Buffer.from(walletKeyHex, 'hex'));
  assert.equal('0x' + keccak256(ecdh.getPublicKey().subarray(1)).subarray(12).toString('hex'), walletAddressHex.toLowerCase());
  for (let i = 0; i < 10; i++) {
    const key = randomBytes(32), message = 'm'.repeat(i * 17) + i;
    assert.equal(walletAddress(key), walletAddress(key, {plain: true}));
    const signature = personalSign(key, message);
    assert.equal(signature, personalSign(key, message, {plain: true}));
    assert.ok(['1b', '1c'].includes(signature.slice(-2)));
    assert.ok(BigInt('0x' + signature.slice(66, 130)) <= 0xFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFEBAAEDCE6AF48A03BBFD25E8CD0364141n / 2n);
  }
});

test('C343: loadWalletKey reads an owner-only file or the environment, never quoting the key', async t => {
  const dir = await temp(t), file = join(dir, 'wallet.key');
  const handle = await open(file, 'wx', 0o644); await handle.writeFile('0x' + walletKeyHex + '\n'); await handle.close();
  await chmod(file, 0o644);
  await assert.rejects(loadWalletKey({file}), error => errorCode('unsafe_key_file')(error) && !error.message.includes(walletKeyHex));
  await chmod(file, 0o600);
  assert.equal((await loadWalletKey({file})).toString('hex'), walletKeyHex);
  const saved = {default: process.env[WALLET_KEY_ENV], other: process.env.SWARMMEMO_TEST_OTHER};
  t.after(() => { for (const [name, value] of [[WALLET_KEY_ENV, saved.default], ['SWARMMEMO_TEST_OTHER', saved.other]]) if (value === undefined) delete process.env[name]; else process.env[name] = value; });
  process.env[WALLET_KEY_ENV] = walletKeyHex; process.env.SWARMMEMO_TEST_OTHER = 'not a key ' + walletKeyHex.slice(0, 20);
  assert.equal((await loadWalletKey()).toString('hex'), walletKeyHex);
  await assert.rejects(loadWalletKey({env: 'SWARMMEMO_TEST_OTHER'}), error => errorCode('invalid_wallet_key')(error) && !error.message.includes(walletKeyHex.slice(0, 20)));
  delete process.env[WALLET_KEY_ENV];
  await assert.rejects(loadWalletKey(), errorCode('invalid_wallet_key'));
});

test('C343: linkWallet signs only this service’s sign-in for its own address', async t => {
  for (const message of [`evil.example wants you to sign in with your Ethereum account:\n${walletAddressHex}\n\nNonce: ${'n'.repeat(32)}\n`,
    `swarmmemo.com wants you to sign in with your Ethereum account:\n0x0000000000000000000000000000000000000001\n\nNonce: ${'n'.repeat(32)}\n`]) {
    const operations = [];
    const origin = await server(t, (req, res) => {let raw = ''; req.on('data', c => {raw += c;}); req.on('end', () => {operations.push(JSON.parse(raw).operation); res.setHeader('Content-Type', 'application/json'); res.end(JSON.stringify({ok: true, data: {value: walletAddressHex, nonce: 'n'.repeat(32), message}}));});});
    const client = new Client({origin, key: fixtureKey, allowInsecureLoopback: true});
    await assert.rejects(client.linkWallet(Buffer.from(walletKeyHex, 'hex')), errorCode('invalid_wallet_challenge'));
    assert.deepEqual(operations, ['standing.challenge']);
  }
});

const siweFingerprint = '4f5d0c1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6', siweNonce = '0123456789abcdef0123456789abcdef';
const siwe = ({service = 'swarmmemo.com', address = walletAddressHex, fingerprint = siweFingerprint, nonce = siweNonce, uri, chain = '1', tail = ''} = {}) =>
  `${service} wants you to sign in with your Ethereum account:\n${address}\n\nLink this address to SwarmMemo agent ${fingerprint} to raise its standing.\n\n`
  + `URI: ${uri ?? 'https://' + service}\nVersion: 1\nChain ID: ${chain}\nNonce: ${nonce}\nIssued At: 2026-10-15T00:00:00Z\nExpiration Time: 2026-10-15T00:15:00Z${tail}`;
test('C343-sec: the wallet challenge must be the exact sign-in, line for line', () => {
  const ok = (message, nonce = siweNonce) => walletChallengeOK(message, 'swarmmemo.com', walletAddressHex, siweFingerprint, nonce);
  assert.equal(ok(siwe()), true);
  for (const [name, message] of Object.entries({
    domain: siwe({service: 'evil.example'}), uri: siwe({uri: 'https://app.example.org'}), chain: siwe({chain: '137'}),
    agent: siwe({fingerprint: '0'.repeat(64)}), address: siwe({address: '0x0000000000000000000000000000000000000001'}),
    resources: siwe({tail: '\nResources:\n- https://evil.example/permit'}), trailing: siwe({tail: '\n'}),
    statement: siwe().replace('to raise its standing.', 'and approve transfers.'), stamp: siwe().replace('00:15:00Z', '00:15:00+01:00'),
    regexDomain: siwe({service: 'swarmmemoxcom'}),
  })) assert.equal(ok(message), false, name);
  for (const nonce of ['short', 'n'.repeat(65), 'abc def12', 'abcdefgh\nURI: x', 'abcdefgh١']) assert.equal(ok(siwe({nonce}), nonce), false, nonce);
  assert.equal(walletChallengeOK(siwe(), 'swarmmemo.com', walletAddressHex, undefined, siweNonce), false);
});

test('C343-sec: web3.js personal_sign vector and linkWallet signs the exact challenge', async t => {
  assert.equal(personalSign(Buffer.from(walletKeyHex, 'hex'), 'Some data'), '0xb91467e570a6466aa9e9876cbcd013baba02900b8979d43fe208a4a4f339f5fd6007e74cd82e037b800186422fc2da167c747ef045e5d18a5f5d4300f8e1a0291c');
  for (const tail of ['', '\nResources:\n- https://evil.example']) {
    const sent = [], message = siwe({fingerprint: fixtureKey.fingerprint, tail});
    const origin = await server(t, (req, res) => {let raw = ''; req.on('data', c => {raw += c;}); req.on('end', () => {sent.push(JSON.parse(raw)); res.setHeader('Content-Type', 'application/json'); res.end(JSON.stringify({ok: true, data: {value: walletAddressHex, nonce: siweNonce, message}}));});});
    const client = new Client({origin, key: fixtureKey, allowInsecureLoopback: true});
    if (tail) {
      await assert.rejects(client.linkWallet(Buffer.from(walletKeyHex, 'hex')), errorCode('invalid_wallet_challenge'));
      assert.deepEqual(sent.map(c => c.operation), ['standing.challenge']);
    } else {
      await client.linkWallet(Buffer.from(walletKeyHex, 'hex'));
      assert.deepEqual(sent.map(c => c.operation), ['standing.challenge', 'identity.link']);
      assert.equal(JSON.parse(sent[1].data).proof, personalSign(Buffer.from(walletKeyHex, 'hex'), message));
    }
  }
});

test('C343-sec: loadWalletKey refuses a symlink, FIFO, directory or missing file without hanging', async t => {
  const dir = await temp(t), file = join(dir, 'wallet.key');
  const handle = await open(file, 'wx', 0o600); await handle.writeFile(walletKeyHex); await handle.close();
  await symlink(file, join(dir, 'link.key'));
  const made = spawnSync('mkfifo', ['-m', '600', join(dir, 'fifo.key')]);
  for (const name of ['link.key', ...(made.status === 0 ? ['fifo.key'] : []), 'missing.key', '.']) {
    await assert.rejects(loadWalletKey({file: join(dir, name)}), error => error instanceof ClientError && !error.message.includes(walletKeyHex), name);
  }
});

test('C343: optional real Go board verifies linkWallet in one step', {skip: !process.env.SWARMMEMO_TEST_BINARY}, async t => {
  const binary = process.env.SWARMMEMO_TEST_BINARY, dir = await temp(t);
  const probe = createServer(); probe.listen(0, '127.0.0.1'); await once(probe, 'listening'); const port = probe.address().port; await new Promise(resolve => probe.close(resolve));
  const origin = `http://127.0.0.1:${port}`;
  const child = spawn(binary, ['serve'], {env: {...process.env, DATA_DIR: join(dir, 'data'), LISTEN_ADDR: `127.0.0.1:${port}`, ALLOW_INSECURE_LOCAL: 'true', ADMIN_TOKEN_FILE: '', SERVICE_ID: 'swarmmemo.com', PUBLIC_URL: origin, TRUST_LOOPBACK_PROXY: 'false'}, stdio: 'ignore'});
  t.after(async () => {if (child.exitCode === null) {child.kill('SIGTERM'); await once(child, 'exit');}});
  let ready = false;
  for (let i = 0; i < 100; i++) {try {if ((await fetch(origin + '/health', {signal: AbortSignal.timeout(100)})).ok) {ready = true; break;}} catch (_) {} await new Promise(resolve => setTimeout(resolve, 25));}
  assert.ok(ready, 'isolated Go server did not become healthy');
  const key = generateKey(), client = new Client({origin, key, allowInsecureLoopback: true});
  const file = join(dir, 'wallet.key'), handle = await open(file, 'wx', 0o600); await handle.writeFile(walletKeyHex + '\n'); await handle.close();
  const walletKey = await loadWalletKey({file});
  const linked = await client.linkWallet(walletKey);
  assert.equal(linked.data.state, 'verified'); assert.equal(linked.data.value, walletAddressHex);
  const link = (await client.send(client.prepare({operation: 'agent.get', target: key.fingerprint}))).agent.links.find(l => l.kind === 'wallet');
  assert.equal(link.state, 'verified'); assert.equal(personalSign(walletKey, link.statement), link.proof);
});

// C377: every board data field is sendable from each JS wrapper of its operation, derived from
// the contract (CONTRACT.operations[op].data, generated from the board's DataSpec). Each field is
// sent through the wrapper to a recording fetch and checked in the signed data. A field the board
// adds fails here until the wrapper sends it or JS_EXCLUDED says why; no wrapper sends a field the
// contract does not know or marks removed.
const P = {generation: 'a'.repeat(32), message: 'b'.repeat(32), agent: 'c'.repeat(64), tx: '0x' + 'd'.repeat(64), room: '~' + 'a'.repeat(26)};
const PATTERN_SAMPLES = {work_id: 'e'.repeat(32), message_id: 'e'.repeat(32), fingerprint: 'f'.repeat(64), tx_hash: P.tx, standing_tier: 'bug'};
const FIELD_SAMPLES = {checks: [{property: 'tests', state: 'pass'}], tiers: [{name: 'bug', credits: 5}], capabilities: ['review'], ids: [P.message],
  review: {subject: 'site:https://example.org/', verdict: 'good'}};
const camelName = name => name.replace(/_([a-z])/g, (_, c) => c.toUpperCase());
function dataSample(operation, name) {
  const field = CONTRACT.operations[operation].data.fields[name], kind = field.type.split('|')[0];
  if (Object.hasOwn(FIELD_SAMPLES, name)) return FIELD_SAMPLES[name];
  if (field.enum) { const first = CONTRACT.enums[field.enum][0]; return kind === 'array' ? [first] : first; }
  if (kind === 'string') return PATTERN_SAMPLES[field.pattern] ?? 'x1';
  return {int: 2, number: 1.5, bool: true, object: {k: 'v'}, array: ['x1']}[kind];
}
// operation: [call(client, options), the fields the call sends from its own arguments].
const JS_WRAPPERS = {
  'allowance.transfer': [(c, o) => c.transfer(P.agent, 5, {resource: 'credit', ...o}), ['resource']],
  vouch: [(c, o) => c.vouch(P.agent, o), ['value', 'sponsor']],
  'webhook.create': [(c, o) => c.webhookCreate('https://example.org/hook', o), ['url']],
  'conversation.get': [(c, o) => c.conversation(P.room, o), []],
  'conversation.open': [(c, o) => c.openConversation({members: [P.agent], ...o}), ['kind', 'sealed']],
  'conversation.respond': [(c, o) => c.respond(P.room, 'accept', o), ['action']],
  'messaging.policy.set': [(c, o) => c.messagingPolicy(o), []],
  post: [(c, o) => c.review('https://example.org/', 'good', 'text', o), ['review']],
  'works.list': [(c, o) => c.works(o), []],
  'work.create': [(c, o) => c.createWork(P.message, {title: 't', generation: P.generation, ...o}), ['generation', 'title', 'capabilities']],
  'work.claim': [(c, o) => c.claimWork(P.message, {generation: P.generation, ...o}), ['generation']],
  'work.submit': [(c, o) => c.submitWork(P.message, 3, 'r'.repeat(32), {generation: P.generation, ...o}), ['generation']],
  'work.accept': [(c, o) => c.acceptWork(P.message, 3, {generation: P.generation, ...o}), ['generation']],
  'work.reject': [(c, o) => c.rejectWork(P.message, 3, 'no', {generation: P.generation, ...o}), ['generation']],
  'work.cancel': [(c, o) => c.cancelWork(P.message, 'done', {generation: P.generation, ...o}), ['generation']],
  'work.settle': [(c, o) => c.settleWork(P.message, P.tx, {generation: P.generation, ...o}), ['generation', 'tx_hash']],
  'work.reviewer.set': [(c, o) => c.setWorkReviewer(P.message, P.agent, {generation: P.generation, ...o}), ['generation', 'reviewer']],
  'work.successor.set': [(c, o) => c.setWorkSuccessor(P.message, P.message, {generation: P.generation, ...o}), ['generation', 'successor']],
  'work.reveal': [(c, o) => c.revealWork(P.message, '42', 's', {generation: P.generation, ...o}), ['generation', 'answer', 'salt']],
  'work.promise': [(c, o) => c.promiseWork(P.message, '0.10', {generation: P.generation, ...o}), ['generation', 'amount_usdc']],
  'updates.get': [(c, o) => c.updates({cursor: 'c1', ...o}), []],
  'updates.dispose': [(c, o) => c.dispose([P.message], 'replied', o), ['ids', 'state']],
  'updates.mark_read': [(c, o) => c.markRead([P.message], o), ['ids']],
  'verify.publish': [(c, o) => c.verifyPublish({command: 'c', environment: 'e', outputSha256: 'f'.repeat(64), checks: FIELD_SAMPLES.checks, limits: 'l', ...o}), ['command', 'environment', 'output_sha256', 'checks', 'limits']],
  find: [(c, o) => c.find('q', o), []],
};
// A field a wrapper takes under another option: the options that send it.
const JS_ARGUMENTS = {
  'work.accept review': () => ({rating: 80}), 'find type': v => ({types: v}),
  'work.create per_agent_day': v => ({seats: 2, perAgentDay: v}), 'work.create day_max': v => ({seats: 2, dayMax: v}),
  'work.create results_max': v => ({seats: 2, resultsMax: v}), 'work.create tiers': v => ({seats: 2, tiers: v}),
  'work.create usdc_per_agent_day': v => ({seats: 2, reward: 5, usdcPerAgentDay: v}), 'work.create usdc_day_max': v => ({seats: 2, reward: 5, usdcDayMax: v}),
  'updates.mark_read all': () => ({all: true}),
};
// A field a wrapper does not send ('operation field'), with the reason.
const JS_EXCLUDED = {};
// Operations no Client method wraps, with the reason.
const noWrapper = 'no wrapper: prepare({operation, data: JSON.stringify(dataFor(operation, fields))}) sends every field';
const JS_NO_WRAPPER = {
  ...Object.fromEntries(['agent.profile.publish', 'allowance.get', 'credits.topup', 'delegation.revoke', 'feed.get', 'feed.profile.fork', 'feed.profile.get', 'feed.profile.put',
    'gate.challenge', 'gate.verify', 'hosted.claim', 'hosted.recover', 'hosted.token', 'identity.unlink', 'identity.witness', 'key.backup.get', 'key.backup.put', 'messages.list',
    'offering.answer', 'offering.buy', 'offering.publish', 'offering.refund', 'room.policy.set', 'room.style.check', 'room.style.set', 'room.subscribe', 'spend_limit.set',
    'standing.work', 'subjects.list', 'vote', 'work.renew', 'x402.submit'].map(op => [op, noWrapper])),
  ...Object.fromEntries(['blob.put', 'room.invite.accept', 'service.call', 'service.read', 'trust.get'].map(op => [op, 'its data is a raw string, not fields'])),
  ...Object.fromEntries(['device.add', 'device.request', 'device.request.get'].map(op => [op, 'linked devices (C576) are web and Python only in v1; ' + noWrapper])),
  'identity.link': 'linkWallet and publishSealKey send fixed kinds; ' + noWrapper,
  'standing.challenge': 'linkWallet sends kind wallet only; ' + noWrapper,
  'conversation.seal': 'sealRotate sends the data string loadSeal().rotationData makes',
  'delegation.create': 'enrollment needs the child key as targetKey: prepare({operation, data}, {targetKey})',
  ...Object.fromEntries(['private_read.create', 'private_read.revoke'].map(op => [op, 'private grants use the Python private-inbox helper, not this client'])),
};

function recordingFetch(t) {
  const sent = [], original = globalThis.fetch;
  globalThis.fetch = async (url, {body} = {}) => {
    const parsed = body ? JSON.parse(body) : {};
    if (body) sent.push(parsed);
    const reply = String(url).endsWith('/api/changes?after=-1') ? {ok: true, generation: P.generation}
      : parsed.operation === 'work.get' ? {ok: true, data: {work: {id: P.message, service_generation: P.generation, fence: 3, worker_subject: 'agent:' + P.agent}}}
      : parsed.operation === 'agent.get' ? {ok: true, agent: {id: P.agent}}
      : parsed.operation === 'subject.resolve' ? {ok: true, data: {subject: {id: 's'.repeat(26), uri: 'site:https://example.org/'}}}
      : {ok: true, data: {}};
    return new Response(JSON.stringify(reply), {status: 200, headers: {'Content-Type': 'application/json'}});
  };
  t.after(() => {globalThis.fetch = original;});
  const data = operation => {
    const body = sent.findLast(b => b.operation === operation);
    assert.ok(body, `no ${operation} command was sent`);
    return JSON.parse(body.data);
  };
  return {sent, data};
}

test('every JS wrapper sends every data field of its operation (C377)', async t => {
  const {sent, data} = recordingFetch(t), client = new Client({origin: 'https://example.org', key: fixtureKey});
  const operations = Object.keys(CONTRACT.operations).filter(op => CONTRACT.operations[op].data).sort();
  assert.deepEqual([...new Set([...Object.keys(JS_WRAPPERS), ...Object.keys(JS_NO_WRAPPER)])].sort(), operations);
  for (const op of Object.keys(JS_WRAPPERS)) assert.ok(!Object.hasOwn(JS_NO_WRAPPER, op), op);
  for (const [operation, [call, base]] of Object.entries(JS_WRAPPERS)) {
    for (const name of Object.keys(CONTRACT.operations[operation].data.fields)) {
      if (name === 'schema' || Object.hasOwn(JS_EXCLUDED, `${operation} ${name}`)) continue;
      const value = dataSample(operation, name);
      const options = base.includes(name) ? {} : (JS_ARGUMENTS[`${operation} ${name}`] ?? (v => ({[camelName(name)]: v})))(value);
      sent.length = 0;
      await call(client, options);
      assert.ok(Object.hasOwn(data(operation), name), `${operation} data.${name} is not sent: give the wrapper the field or record why in JS_EXCLUDED`);
      for (const body of sent) {
        const spec = CONTRACT.operations[body.operation]?.data;
        if (!spec || spec.raw || body.data === undefined) continue;
        for (const key of Object.keys(JSON.parse(body.data))) {
          assert.ok(Object.hasOwn(spec.fields, key), `${body.operation} sent ${key}, which the contract does not take`);
          assert.ok(!Object.hasOwn(spec.removed, key), `${body.operation} sent removed ${key}`);
        }
      }
    }
  }
  for (const key of Object.keys(JS_EXCLUDED)) { const [op, name] = key.split(' '); assert.ok(Object.hasOwn(CONTRACT.operations[op].data.fields, name), `stale exclusion ${key}`); }
  for (const [op, reason] of Object.entries(JS_NO_WRAPPER)) {
    assert.ok(reason.trim(), op);
    const spec = CONTRACT.operations[op].data;
    if (spec.raw) continue;
    const fields = Object.fromEntries(Object.keys(spec.fields).filter(n => n !== 'schema').map(n => [camelName(n), dataSample(op, n)]));
    assert.deepEqual(Object.keys(dataFor(op, fields)).filter(n => n !== 'schema').sort(), Object.keys(spec.fields).filter(n => n !== 'schema').sort(), op);
  }
});

test('dataFor checks names, types and enums against the contract; the audit gaps send', async t => {
  assert.deepEqual(dataFor('works.list', {worker: 'w'}), {schema: 1, worker: 'w'});
  assert.deepEqual(dataFor('vote', {value: 1}), {value: 1});
  assert.deepEqual(dataFor('work.create', {generation: 'g', title: 't', capabilities: [], rejectFee: 5}), {schema: 1, generation: 'g', title: 't', capabilities: [], reject_fee: 5});
  for (const [op, fields] of [['works.list', {nonsense: 1}], ['work.create', {generation: 'g', title: 't', capabilities: [], rejectFee: '5'}], ['vote', {value: true}],
    ['find', {type: ['nonsense']}], ['work.renew', {}], ['blob.put', {}], ['agent.get', {}], ['works.list', {constructor: 'x'}]]) assert.throws(() => dataFor(op, fields), errorCode('invalid_option'), op);
  const spec = CONTRACT.operations['works.list'].data, saved = {...spec};
  try {
    spec.fields = Object.fromEntries(Object.entries(saved.fields).filter(([name]) => name !== 'worker'));
    spec.removed = {worker: '1.99.0'}; spec.aliases = {by: 'eligible_for'};
    assert.throws(() => dataFor('works.list', {worker: 'w'}), error => errorCode('invalid_option')(error) && /removed in 1.99.0/.test(error.message));
    assert.deepEqual(dataFor('works.list', {by: 'w'}), {schema: 1, eligible_for: 'w'});
  } finally { Object.assign(spec, saved); }
  // The audit's JS gaps: createWork refused rejectFee locally, works had no worker, vouch no weight.
  const {data} = recordingFetch(t), client = new Client({origin: 'https://example.org', key: fixtureKey});
  await client.createWork(P.message, {title: 't', generation: P.generation, reward: 10, rejectFee: 5});
  assert.equal(data('work.create').reject_fee, 5);
  await client.works({worker: 'alice'});
  assert.deepEqual(data('works.list'), {schema: 1, worker: 'alice'});
  await client.vouch(P.agent, {weight: 20});
  assert.deepEqual(data('vouch'), {schema: 1, value: 1, sponsor: false, weight: 20});
  await client.rejectWork(P.message, 3, 'no', {generation: P.generation, checks: FIELD_SAMPLES.checks});
  assert.deepEqual(data('work.reject').checks, FIELD_SAMPLES.checks);
});

test('Markdown reviews and rated-accept notes sign format markdown (citepulse-ai 8b49d2b7)', async t => {
  const {data} = recordingFetch(t), client = new Client({origin: 'https://example.org', key: fixtureKey});
  await client.review('https://example.org/', 'good', '**Solid** work, see *notes*.');
  assert.equal(data('post').format, 'markdown');
  await client.review('https://example.org/', 'good', 'plain words');
  assert.equal(data('post').format, undefined);
  const work = {id: P.message, worker_subject: 'agent:' + P.agent};
  assert.equal(JSON.parse(workRatingPost(work, 80, '- fast\n- correct').data).format, 'markdown');
  assert.equal(JSON.parse(workRatingPost(work, 80, 'fast and correct').data).format, undefined);
  // C600: a rated accept by a standing task's own id names the results to judge.
  assert.throws(() => workRatingPost({id: P.message, standing: {seats: 2}, waiting_results: [{work_id: 'a'.repeat(32)}]}, 80), /judge a result by its own id \(a{32};/);
  await client.acceptWork(P.message, 3, {rating: 90, note: '**Great** result, see *notes*.'});
  assert.equal(JSON.parse(data('work.accept').review.data).format, 'markdown');
});

test('dataFor checks array entries as Python does (c80dfcaa456f report)', () => {
  const base = {command: 'go test ./...', environment: 'linux', limits: 'none', output_sha256: 'a'.repeat(64), repo: 'https://github.com/a/b', commit: 'b'.repeat(40)};
  for (const bad of [[{}], [{property: 'x', state: 'bogus'}], [{property: 123, state: 'pass'}], [{property: 'x', state: 'pass', passes: 'three'}], [{property: 'x', state: 'pass', zzz: 1}], ['x']]) {
    assert.throws(() => dataFor('verify.publish', {...base, checks: bad}), e => e.code === 'invalid_option', JSON.stringify(bad));
  }
  assert.deepEqual(dataFor('verify.publish', {...base, checks: [{property: 'x', state: 'pass'}]}).checks, [{property: 'x', state: 'pass'}]);
});


test('oversized but valid JSON cursor never reaches updates (bounded local-file read)', async t => {
  const dir = await temp(t), cursorFile = join(dir, 'oversized-cursor.json');
  const file = await open(cursorFile, 'w', 0o600);
  try {await file.writeFile(JSON.stringify({cursor: 'x'.repeat(131072)}));}
  finally {await file.close();}
  let calls = 0;
  class OfflineCursorClient extends Client {
    async updates() {calls++; return {ok: true};}
  }
  const pages = new OfflineCursorClient().followUpdates({cursorFile});
  try {
    await assert.rejects(pages.next(), errorCode('cursor_read_failed'));
  } finally {await pages.return();}
  assert.equal(calls, 0, 'large cursor must be rejected before any API call');
});

test('a normal saved cursor still resumes safely through the bounded reader', async t => {
  const dir = await temp(t), cursorFile = join(dir, 'normal-cursor.json');
  const file = await open(cursorFile, 'w', 0o600);
  try {await file.writeFile(JSON.stringify({cursor: 'test-cursor-123'}));}
  finally {await file.close();}
  let seen = '';
  class OfflineCursorClient extends Client {
    async updates({cursor} = {}) {seen = cursor; return {ok: true, next_cursor: cursor};}
  }
  const pages = new OfflineCursorClient().followUpdates({cursorFile});
  try {await pages.next();}
  finally {await pages.return();}
  assert.equal(seen, 'test-cursor-123');
});

test('a scoped worker may carry the work data contract fields beyond the generation (citepulse-ai 10-17)', () => {
  const g = 'a'.repeat(32);
  const worker = new DelegatedClient({origin: 'https://example.org', key: fixtureKey, grantId: fixtureKey.fingerprint, generation: g, room: 'lab', operations: ['work.claim', 'work.submit']});
  const claim = data => worker.prepare({operation: 'work.claim', message_id: 'b'.repeat(32), ttl: 60, data: JSON.stringify(data)});
  assert.doesNotThrow(() => claim({schema: 1, generation: g, result_sha256: 'd'.repeat(64)}));
  assert.throws(() => claim({schema: 1, generation: 'c'.repeat(32), payout_address: '0x' + '1'.repeat(40)}), errorCode('delegation_scope'));
  assert.throws(() => claim({schema: 1, generation: g, bogus: 1}));
  // C380: payout_address only when the grant pins it, and then exactly that address.
  assert.throws(() => claim({schema: 1, generation: g, payout_address: '0x' + '1'.repeat(40)}), errorCode('delegation_payout_unpinned'));
});

test('a scoped worker names only the payout address its grant pins (C380)', () => {
  const g = 'a'.repeat(32);
  const payee = '0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed'; // a valid EIP-55 address (C484)
  const options = {origin: 'https://example.org', key: fixtureKey, grantId: fixtureKey.fingerprint, generation: g, room: 'lab', operations: ['work.claim', 'work.submit']};
  const pinned = new DelegatedClient({...options, payoutAddress: payee});
  for (const operation of ['work.claim', 'work.submit']) {
    const send = data => pinned.prepare({operation, message_id: 'b'.repeat(32), ttl: 60, data: JSON.stringify(data)});
    assert.doesNotThrow(() => send({schema: 1, generation: g, payout_address: payee.toLowerCase()}));
    assert.doesNotThrow(() => send({schema: 1, generation: g}));
    assert.throws(() => send({schema: 1, generation: g, payout_address: '0x' + 'cd'.repeat(20)}), errorCode('delegation_payout_mismatch'));
  }
  for (const bad of ['0x' + '1'.repeat(39), '1'.repeat(42), 7]) assert.throws(() => new DelegatedClient({...options, payoutAddress: bad}), errorCode('invalid_payout_address'));
});


// Synthetic fixture-only regression; never opens a user wallet or credential.
const boundedKeyFixture = '0x' + '42'.repeat(32) + '\n';
async function walletFileFixture(t) {
  const dir = await temp(t);
  const file = join(dir, 'synthetic-wallet-key.txt');
  const fd = await open(file, 'wx', 0o600);
  await fd.writeFile(boundedKeyFixture);
  await fd.close();
  return file;
}
test('wallet-key bounded read does not use unbounded FileHandle.readFile', async t => {
  const file = await walletFileFixture(t);
  const fd = await open(file, 'r');
  const prototype = Object.getPrototypeOf(fd);
  await fd.close();
  const before = prototype.readFile;
  prototype.readFile = async () => { throw Error('unbounded read called after stat'); };
  t.after(() => { prototype.readFile = before; });
  assert.equal((await loadWalletKey({file})).toString('hex'), '42'.repeat(32));
});
test('wallet-key bounded read rejects a file grown after its original stat', async t => {
  const file = await walletFileFixture(t);
  const fd = await open(file, 'r');
  const prototype = Object.getPrototypeOf(fd);
  await fd.close();
  const before = prototype.stat;
  let grew = false;
  prototype.stat = async function(...args) {
    const status = await before.apply(this, args);
    if (!grew) { grew = true; await appendFile(file, 'A'.repeat(420), 'ascii'); }
    return status;
  };
  t.after(() => { prototype.stat = before; });
  await assert.rejects(loadWalletKey({file}), errorCode('unsafe_key_file'));
  assert.equal(grew, true);
});

test('followUpdates fits its default wait to the client deadline (round 6: moneymaker-ai, idris)', async () => {
  for (const [timeoutMs, want] of [[30000, 25], [120000, 25], [25000, 20], [20000, 15], [10000, 5], [5000, 1], [1500, 1], [1000, undefined]]) {
    const seen = [];
    class Offline extends Client {
      async updates({wait, cursor} = {}) {seen.push(cursor ? wait : 'first'); return {ok: true, next_cursor: `c${seen.length}`};}
    }
    const pages = new Offline({timeoutMs}).followUpdates();
    try {for (let i = 0; i < 3; i++) await pages.next();} finally {await pages.return();}
    assert.deepEqual(seen, ['first', want, want], `timeoutMs=${timeoutMs}`);
  }
  // An explicit wait is passed through and still checked by updates().
  const sent = [];
  class Explicit extends Client {
    async updates({wait, cursor} = {}) {if (cursor) sent.push(wait); return {ok: true, next_cursor: 'c'};}
  }
  const explicit = new Explicit({timeoutMs: 20000}).followUpdates({wait: 12});
  try {await explicit.next(); await explicit.next();} finally {await explicit.return();}
  assert.deepEqual(sent, [12]);
});

test('PR #16: the identity key and the wallet key share one bounded owner-only reader', async t => {
  const dir = await temp(t), path = join(dir, 'key.json'), wallet = join(dir, 'wallet.key');
  await writeKey(path);
  assert.ok((await loadKey(path)).public_key);
  await appendFile(path, ' '.repeat(16384));
  await assert.rejects(loadKey(path), errorCode('unsafe_key_file'));
  const file = await open(wallet, 'w', 0o600);
  try {await file.writeFile('0'.repeat(63) + '1' + ' '.repeat(193));} finally {await file.close();}
  await assert.rejects(loadWalletKey({file: wallet}), errorCode('unsafe_key_file'));
});

test('PR #17: saving the cursor never touches a fixed NAME.tmp, which may be the user\'s file', async t => {
  const dir = await temp(t), cursorFile = join(dir, 'cursor.json'), mine = cursorFile + '.tmp';
  await appendFile(mine, 'my notes');
  class Offline extends Client {async updates() {return {ok: true, next_cursor: 'c' + Math.random()};}}
  const pages = new Offline().followUpdates({cursorFile});
  try {for (let i = 0; i < 3; i++) await pages.next();} finally {await pages.return();}
  assert.equal(await readFile(mine, 'utf8'), 'my notes');
  assert.match(JSON.parse(await readFile(cursorFile, 'utf8')).cursor, /^c/);
  const {readdir} = await import('node:fs/promises');
  assert.deepEqual((await readdir(dir)).sort(), ['cursor.json', 'cursor.json.tmp']);
});

test('C484: evmAddressOK refuses the zero address and a bad EIP-55 checksum', async () => {
  const {evmAddressOK} = await import('./swarmmemo.mjs');
  const good = '0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed';
  for (const bad of ['0x' + '0'.repeat(40), '0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAeD']) assert.equal(evmAddressOK(bad), false, bad);
  for (const ok of [good, good.toLowerCase(), '0x' + good.slice(2).toUpperCase()]) assert.equal(evmAddressOK(ok), true, ok);
});

// C546 signed by default: a keyless write makes and keeps one device key per home.
async function recorder(t) {
  const seen = [];
  const origin = await server(t, (req, res) => {
    let body = ''; req.on('data', chunk => {body += chunk;});
    req.on('end', () => {seen.push({method: req.method, url: req.url, body: body ? JSON.parse(body) : null}); res.end('{"ok":true,"receipt":{"id":"r"}}');});
  });
  return {origin, seen};
}
test('C546: the first write makes an owner-only device key and signs; the next write reuses it', async t => {
  const {origin, seen} = await recorder(t), home = join(await temp(t), 'nested', 'home'), file = join(home, 'key.json');
  const client = new Client({origin, home});
  await client.send(client.prepare({operation: 'messages.list', room: 'lobby'}));
  await assert.rejects(stat(file), error => error.code === 'ENOENT'); // reads make nothing
  assert.equal(seen[0].body.signature, undefined);
  const prepared = client.prepare({operation: 'post', room: 'lobby', text: 'hello'});
  await assert.rejects(stat(file), error => error.code === 'ENOENT'); // prepare touches no file
  const first = await client.send(prepared);
  const key = await loadKey(file);
  assert.equal((await stat(file)).mode & 0o777, 0o600);
  assert.equal((await stat(home)).mode & 0o777, 0o700);
  assert.equal(seen[1].body.public_key, key.public_key);
  assert.ok(seen[1].body.signature);
  assert.equal(first.device_key.path, file);
  assert.equal(first.device_key.fingerprint, key.fingerprint);
  assert.match(first.device_key.note, /made your signing key at .*key\.json.*Keep the file: it is your identity.*rotate/);
  assert.doesNotMatch(JSON.stringify(first), new RegExp(key.private_key));
  const second = await client.send(client.prepare({operation: 'post', room: 'lobby', text: 'again'}));
  assert.equal(second.device_key, undefined); // the note is once
  assert.equal(seen[2].body.public_key, key.public_key);
  // Another client (and the Python CLI) on this home reuses the same file.
  const other = new Client({origin, home});
  await other.send(other.prepare({operation: 'post', room: 'lobby', text: 'third'}));
  assert.equal(seen[3].body.public_key, key.public_key);
  // A read that needs a signer uses an existing device key; it never makes one.
  await other.send(other.prepare({operation: 'conversations.list'}));
  assert.equal(seen[4].body.public_key, key.public_key);
  const fresh = new Client({origin, home: join(home, 'elsewhere')});
  await fresh.send(fresh.prepare({operation: 'conversations.list'}));
  assert.equal(seen[5].body.signature, undefined);
  await assert.rejects(stat(join(home, 'elsewhere')), error => error.code === 'ENOENT');
  // A device-key read that needs a signer (a private room's messages.list
  // answers not_found unsigned) signs with the existing key; it never makes one.
  await fresh.send(fresh.prepare({operation: 'messages.list', room: 'lab'}));
  assert.equal(seen[6].body.signature, undefined); // no key in that home yet
  const third = new Client({origin, home});
  await third.send(third.prepare({operation: 'messages.list', room: 'lab'}));
  assert.equal(seen[7].body.public_key, key.public_key);
  assert.ok(seen[7].body.signature);
});
test('C546: a retry of the prepared write is byte-identical and reuses the persisted key', async t => {
  const {origin, seen} = await recorder(t), home = await temp(t);
  const client = new Client({origin, home}), prepared = client.prepare({operation: 'post', room: 'lobby', text: 'retry me'});
  await client.send(prepared); await client.send(prepared);
  assert.deepEqual(seen[0].body, seen[1].body);
  assert.equal(seen[0].body.public_key, (await loadKey(join(home, 'key.json'))).public_key);
  // Another process made the file between prepare and send: nothing is sent.
  const raced = await temp(t), racer = new Client({origin, home: raced}), pending = racer.prepare({operation: 'post', room: 'lobby', text: 'race'});
  await writeKey(join(raced, 'key.json'));
  await assert.rejects(racer.send(pending), errorCode('key_changed'));
  assert.equal(seen.length, 2);
  await racer.send(racer.prepare({operation: 'post', room: 'lobby', text: 'race'}));
  assert.equal(seen[2].body.public_key, (await loadKey(join(raced, 'key.json'))).public_key);
});
test('C546: anonymous stays unsigned; get/base64 skip the device key; an explicit key there is still refused', async t => {
  const {origin, seen} = await recorder(t), home = await temp(t), file = join(home, 'key.json');
  const anonymous = new Client({origin, home, anonymous: true});
  await anonymous.send(anonymous.prepare({operation: 'post', room: 'lobby', text: 'no key'}));
  const client = new Client({origin, home});
  await client.send(client.prepare({operation: 'post', room: 'lobby', text: 'one off'}, {anonymous: true}));
  for (const transport of ['get', 'base64']) await client.send(client.prepare({operation: 'post', room: 'lobby', page: 'main', text: 'hi', request_id: 'x-' + transport}, {transport}));
  await assert.rejects(stat(file), error => error.code === 'ENOENT');
  assert.ok(seen.slice(0, 2).every(r => r.body.signature === undefined && r.body.public_key === undefined));
  assert.match(seen[2].url, /^\/w\/lobby\/main\?/); assert.match(seen[3].url, /^\/w64\/lobby\/main\//);
  // Even with a device key on disk, get/base64 stay anonymous.
  await client.send(client.prepare({operation: 'post', room: 'lobby', text: 'signed'}));
  await client.send(client.prepare({operation: 'post', room: 'lobby', page: 'main', text: 'hi', request_id: 'y'}, {transport: 'get'}));
  assert.ok(seen[4].body.signature); assert.match(seen[5].url, /^\/w\/lobby\/main\?/);
  assert.throws(() => new Client({origin, key: fixtureKey}).prepare({operation: 'post', room: 'lobby', text: 'hi'}, {transport: 'get'}), errorCode('invalid_transport'));
  assert.throws(() => new Client({key: fixtureKey, anonymous: true}), errorCode('invalid_option'));
});
test('C546: $SWARMMEMO_KEY comes before the device key; an unwritable home posts anonymously and says so', async t => {
  const {origin, seen} = await recorder(t), dir = await temp(t), keyFile = join(dir, 'agent.json');
  await writeKey(keyFile); const key = await loadKey(keyFile);
  process.env.SWARMMEMO_KEY = keyFile;
  try {
    const client = new Client({origin, home: join(dir, 'home')});
    const result = await client.send(client.prepare({operation: 'post', room: 'lobby', text: 'env key'}));
    assert.equal(seen[0].body.public_key, key.public_key); assert.equal(result.device_key, undefined);
    await assert.rejects(stat(join(dir, 'home')), error => error.code === 'ENOENT');
  } finally { delete process.env.SWARMMEMO_KEY; }
  // A home that cannot hold a key (here: under a plain file; a read-only one is the same).
  const client = new Client({origin, home: join(keyFile, 'home')});
  const result = await client.send(client.prepare({operation: 'post', room: 'lobby', text: 'read-only home'}));
  assert.equal(seen[1].body.signature, undefined);
  assert.equal(result.device_key.created, false); assert.match(result.device_key.note, /went anonymous/);
});

test('workAnswerCommit is sha256 of salt, a newline and the answer (C587)', () => {
  assert.equal(workAnswerCommit('3f9c2a7d1e6b4c58a0d2e4f6a8b0c1d2', '42'), 'c01544b3ecadb14ba1b4c8a99b3f94694509716d89e9b2c2914214e584b735b6');
  assert.throws(() => workAnswerCommit('a\nb', '42'), /newline/);
});
