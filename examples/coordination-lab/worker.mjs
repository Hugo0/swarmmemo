// Explicit LOCAL fixture, not a daemon or general task executor. Persisted
// acknowledgements are historical acceptance, never a fresh lease guarantee.
import {createHash, randomUUID} from 'node:crypto';
import {constants} from 'node:fs';
import {open, mkdir, lstat, link, unlink} from 'node:fs/promises';
import {dirname, join} from 'node:path';
import {Client, loadKey, ClientError} from '../../clients/javascript/swarmmemo.mjs';

const FIXTURE = 'Hello, swarms — café 🌍\n';
const TITLE = 'Simulation: cross-runtime UTF-8 accounting';
const STEPS = new Set(['claim', 'blob', 'post', 'submit']);
const hash = bytes => createHash('sha256').update(bytes).digest('hex');
const json = value => JSON.stringify(value);
const hex = value => typeof value === 'string' && /^[a-f0-9]{32}$/.test(value);
const integer = value => Number.isSafeInteger(value) && value >= 0;
const requireThat = value => {if (!value) throw Error('fixture_state_invalid');};
async function directory(path) {
  const info = await lstat(path);
  requireThat(info.isDirectory() && !info.isSymbolicLink() && !(info.mode & 0o077) && info.uid === process.getuid());
}
async function syncDirectory(path) {
  const file = await open(path, constants.O_RDONLY | constants.O_DIRECTORY | constants.O_NOFOLLOW);
  try {await file.sync();} finally {await file.close();}
}
async function readJSON(path) {
  let file;
  try {
    file = await open(path, constants.O_RDONLY | constants.O_NOFOLLOW | constants.O_NONBLOCK);
    const info = await file.stat();
    requireThat(info.isFile() && info.uid === process.getuid() && !(info.mode & 0o077) && info.size <= 262144);
    const bytes = Buffer.alloc(262145); let size = 0;
    while (size < bytes.length) {const read = await file.read(bytes, size, bytes.length - size, size); if (!read.bytesRead) break; size += read.bytesRead;}
    requireThat(size <= 262144);
    const raw = new TextDecoder('utf-8', {fatal: true}).decode(bytes.subarray(0, size));
    const result = JSON.parse(raw); requireThat(json(result) === raw);
    return result;
  } catch (error) {if (error.code === 'ENOENT') return null; throw error;}
  finally {await file?.close();}
}
async function persist(path, value) {
  // Atomic no-overwrite installation: concurrent callers reuse the winning
  // durable envelope, never their independently prepared losing command.
  const temporary = path + '.pending-' + randomUUID();
  const file = await open(temporary, constants.O_WRONLY | constants.O_CREAT | constants.O_EXCL | constants.O_NOFOLLOW, 0o600);
  try {
    const bytes = json(value); requireThat(Buffer.byteLength(bytes) <= 262144);
    await file.writeFile(bytes); await file.sync();
  } finally {await file.close();}
  try {await link(temporary, path);}
  catch (error) {if (error.code !== 'EEXIST') throw error;}
  finally {await unlink(temporary); await syncDirectory(dirname(path));}
  return readJSON(path);
}
function failpoint(position, step) {
  // Fixed local-only test exit, never an arbitrary command or hook.
  if (STEPS.has(step) && process.env.SWARMMEMO_LAB_FAILPOINT === position + '_' + step) process.exit(79);
}
function acknowledge(step, command, result, binding) {
  requireThat(result && result.ok === true);
  if (step === 'claim' || step === 'submit') {
    const ack = result.data?.ack;
    requireThat(ack && Object.keys(ack).sort().join(',') === 'accepted_at,claim_expires_at,deadline,fence,generation,service_id,state,work_id');
    requireThat(ack.work_id === binding.work_id && ack.service_id === binding.service && ack.generation === binding.generation
      && ['fence', 'accepted_at', 'deadline', 'claim_expires_at'].every(field => integer(ack[field]))
      && ack.fence > 0 && ack.accepted_at > 0 && ack.accepted_at < ack.claim_expires_at && ack.claim_expires_at <= ack.deadline
      && ack.state === (step === 'claim' ? 'claimed' : 'submitted'));
    if (step === 'claim') requireThat(ack.claim_expires_at - ack.accepted_at === command.ttl);
    else requireThat(ack.fence === command.amount);
  } else if (step === 'blob') {
    const blob = result.data?.blob, bytes = Buffer.from(command.data, 'base64url');
    requireThat(blob && hex(blob.id) && blob.room === command.room && blob.sha256 === hash(bytes)
      && blob.size === bytes.length && blob.filename === command.filename && blob.media_type === command.media_type);
  } else {
    const receipt = result.receipt;
    requireThat(receipt && hex(receipt.id) && receipt.sha256 === hash(command.text) && integer(receipt.accepted_at));
  }
}

async function main() {
  const [action, origin, keyPath, stateDir, workID] = process.argv.slice(2);
  requireThat(['claim', 'retry-claim', 'result', 'retry-result'].includes(action)
    && /^http:\/\/127\.0\.0\.1:[0-9]+$/.test(origin || '') && hex(workID));
  await directory(stateDir);
  const local = join(stateDir, workID);
  try {await mkdir(local, {mode: 0o700}); await syncDirectory(stateDir);}
  catch (error) {if (error.code !== 'EEXIST') throw error;}
  await directory(local);
  const key = await loadKey(keyPath);
  const client = new Client({origin, key, allowInsecureLoopback: true, timeoutMs: 10000, maxResponseBytes: 262144});
  const base = {version: 1, origin, service: client.service, public_key: key.public_key, work_id: workID};
  let binding = await readJSON(join(local, 'binding.json'));
  const getWork = async () => {
    const work = (await client.send(client.prepare({operation: 'work.get', message_id: workID}))).data?.work;
    requireThat(work && work.id === workID && work.simulated === true && work.room === 'coordination-lab'
      && work.title === TITLE && work.service_id === client.service && hex(work.service_generation)
      && work.generation === work.service_generation);
    if (binding) requireThat(work.service_generation === binding.generation);
    return work;
  };
  if (!binding) {
    requireThat(action === 'claim');
    const work = await getWork();
    binding = await persist(join(local, 'binding.json'), {...base, generation: work.service_generation});
  }
  requireThat(Object.keys(binding).length === 6 && hex(binding.generation)
    && Object.entries(base).every(([field, value]) => binding[field] === value));
  const bindingHash = hash(json(binding));
  const data = json({schema: 1, generation: binding.generation});
  async function step(name, intent, allowNew, beforeNew = async () => {}, loadOnly = false) {
    const envelopePath = join(local, name + '.json');
    let envelope = await readJSON(envelopePath);
    if (!envelope) {
      requireThat(allowNew); await beforeNew();
      const prepared = client.prepare(intent);
      envelope = await persist(envelopePath, {version: 1, binding_sha256: bindingHash, intent_sha256: hash(json(intent)), body: prepared.body});
    }
    requireThat(envelope.version === 1 && Object.keys(envelope).length === 4 && envelope.binding_sha256 === bindingHash
      && envelope.intent_sha256 === hash(json(intent)) && typeof envelope.body === 'string');
    const command = JSON.parse(envelope.body);
    const business = Object.fromEntries(Object.entries(command).filter(([field]) => !['request_id', 'public_key', 'timestamp', 'nonce', 'signature'].includes(field)));
    requireThat(json(business) === json(intent) && command.public_key === key.public_key);
    const prepared = client.prepare(command); requireThat(prepared.body === envelope.body);
    failpoint('after_prepare', name);
    const responsePath = join(local, name + '.response.json'), envelopeHash = hash(envelope.body);
    let saved = await readJSON(responsePath);
    if (!saved) {
      requireThat(!loadOnly);
      // Blob/post have no server-enforced work epoch/fence. Recheck the claim
      // before unresolved sends too; never replay their public side effects into
      // a KNOWN stale epoch merely to discover whether they were accepted.
      // This read is not atomic downstream fencing. work.submit is fenced by
      // the server and can replay historical acceptance without a live claim.
      if (name === 'blob' || name === 'post') await beforeNew();
      const result = await client.send(prepared);
      acknowledge(name, command, result, binding);
      failpoint('after_accept', name); // Server accepted; process loses acknowledgement.
      saved = await persist(responsePath, {version: 1, envelope_sha256: envelopeHash, response: result});
    }
    requireThat(saved.version === 1 && Object.keys(saved).length === 3 && saved.envelope_sha256 === envelopeHash);
    acknowledge(name, command, saved.response, binding);
    failpoint('after_record', name);
    return saved.response;
  }
  const claimIntent = {operation: 'work.claim', message_id: workID, data, ttl: 300};
  if (action === 'claim' || action === 'retry-claim') {
    const result = await step('claim', claimIntent, action === 'claim', async () => {
      const work = await getWork(); requireThat(work.state === 'open');
    });
    process.stdout.write(json(result)); return;
  }
  const claim = await step('claim', claimIntent, false, async () => {}, true);
  const fence = claim.data.ack.fence;
  const currentClaim = async () => {
    const work = await getWork();
    requireThat(work.state === 'claimed' && work.fence === fence && work.worker?.id === key.fingerprint);
    return work;
  };
  const evidence = {schema: 1, simulated: true, task: 'utf8-accounting-v1', utf8_bytes: Buffer.byteLength(FIXTURE),
    sha256: hash(FIXTURE), base64url: Buffer.from(FIXTURE).toString('base64url')};
  const intentPath = join(local, 'result-intent.json');
  let intent = await readJSON(intentPath);
  if (action === 'result') {
    requireThat(!intent);
    const work = await currentClaim();
    intent = await persist(intentPath, {version: 1, binding_sha256: bindingHash, fence, to: work.requester.id, evidence});
  }
  requireThat(intent && Object.keys(intent).length === 5 && intent.version === 1 && intent.binding_sha256 === bindingHash
    && intent.fence === fence && typeof intent.to === 'string' && /^[a-f0-9]{64}$/.test(intent.to) && json(intent.evidence) === json(evidence));
  // Replay of an existing envelope needs no fresh claim. Preparing a NEW step
  // does require the original attempt still being current in the same epoch.
  const uploaded = await step('blob', {operation: 'blob.put', room: 'coordination-lab', filename: 'utf8-evidence.json',
    media_type: 'application/json', data: Buffer.from(json(intent.evidence)).toString('base64url'), ttl: 3600}, true, currentClaim);
  const posted = await step('post', {operation: 'post', room: 'coordination-lab', kind: 'simulation', reply_to: workID, to: intent.to,
    text: 'Operator-run Node simulation result; no payment or independent adoption claim.\n' + json(intent.evidence),
    attachments: [uploaded.data.blob.id]}, true, currentClaim);
  const submitted = await step('submit', {operation: 'work.submit', message_id: workID, data, amount: fence, target: posted.receipt.id}, true, currentClaim);
  process.stdout.write(json({result_id: posted.receipt.id, attachment_id: uploaded.data.blob.id, evidence: intent.evidence,
    acknowledgement: submitted.data.ack, acknowledgement_scope: 'historical_acceptance_not_current_claim'}));
}
main().catch(error => {
  process.stderr.write(json({error: error instanceof ClientError ? error.code : 'simulation_worker_failed'}) + '\n');
  process.exitCode = 1;
});
