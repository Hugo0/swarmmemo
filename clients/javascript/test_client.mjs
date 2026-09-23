import test from 'node:test';
import assert from 'node:assert/strict';
import {createServer} from 'node:http';
import {once} from 'node:events';
import {mkdtemp, readFile, stat, chmod, symlink, rm, open} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import {join} from 'node:path';
import {spawn, spawnSync} from 'node:child_process';
import {createPrivateKey, createPublicKey, sign, verify} from 'node:crypto';
import {Client, DelegatedClient, ClientError, canonical, delegationContext, generateKey, importKey, writeKey, loadKey} from './swarmmemo.mjs';

const vector = JSON.parse(await readFile(new URL('../python/signing-vector.json', import.meta.url), 'utf8'));
const fixtureKey = importKey({private_key: Buffer.from(vector.seed_hex, 'hex').toString('base64url'), public_key: vector.command.public_key});

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
  assert.doesNotThrow(() => new Client({origin: 'http://remote.example'}));
});

test('origins are plain, signed HTTP is explicit loopback only, dispatch is bound', async () => {
  for (const origin of ['https://user:pass@swarmmemo.com', 'https://swarmmemo.com/path', 'https://swarmmemo.com/?', 'https://swarmmemo.com/#', 'https://swarmmemo.com?x=1', 'https://swarmmemo.com/../', ' https://swarmmemo.com', 'https://@swarmmemo.com', 'file:///tmp/test']) assert.throws(() => new Client({origin}), errorCode('invalid_origin'));
  assert.throws(() => new Client({origin: 'http://localhost', key: fixtureKey}), errorCode('https_required'));
  assert.throws(() => new Client({origin: 'http://example.com', key: fixtureKey, allowInsecureLoopback: true}), errorCode('https_required'));
  const client = new Client(), prepared = client.prepare({operation: 'messages.list'});
  await assert.rejects(client.send({...prepared}), errorCode('binding_mismatch'));
  await assert.rejects(new Client({origin: 'https://publicbbs.com'}).send(prepared), errorCode('binding_mismatch'));
  const local = new Client({origin: 'http://127.0.0.1', key: fixtureKey, allowInsecureLoopback: true});
  await assert.rejects(new Client({origin: local.origin}).send(local.prepare({operation: 'messages.list'})), errorCode('https_required'));
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
  const client = new Client({origin});
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
  await assert.rejects(limited.send(limited.prepare({operation: 'messages.list'})), error => error.code === 'request_rate' && error.status === 429 && error.retryAfter === 4 && !error.message.includes('sentinel'));
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

test('malformed responses and declared oversize bodies fail without echoing contents', async t => {
  const malformed = await server(t, (_req, res) => res.end(Buffer.from([0xc0, 0xaf])));
  const client = new Client({origin: malformed});
  await assert.rejects(client.send(client.prepare({operation: 'messages.list'})), errorCode('invalid_response'));
  const oversized = await server(t, (_req, res) => {res.writeHead(200, {'Content-Length': String(9 * 1024 * 1024)}); res.flushHeaders();});
  const bounded = new Client({origin: oversized});
  await assert.rejects(bounded.send(bounded.prepare({operation: 'messages.list'})), errorCode('response_too_large'));
});

test('direct module execution has no implicit CLI side effects', () => {
  const result = spawnSync(process.execPath, [new URL('./swarmmemo.mjs', import.meta.url).pathname], {encoding: 'utf8'});
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
