// Node only, no browser or server: the embed sign-in module's version 2
// canonical bytes (what a worker key signs with its grant) equal the
// JavaScript client's and the server's vector (board
// TestDelegationCanonicalVersionsAndStrictContext), with memo-core's field order.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const {pathToFileURL} = require('node:url');

(async () => {
  const read = name => fs.readFileSync(path.join(__dirname, 'assets', name), 'utf8');
  const core = vm.runInNewContext(read('memo-core.js') + '\nSwarmMemoCore;', {TextEncoder, btoa, atob});
  const module = await import('data:text/javascript;base64,' + Buffer.from(read('embed-signin.js')).toString('base64'));
  const client = await import(pathToFileURL(path.resolve(__dirname, '../../clients/javascript/swarmmemo.mjs')));
  const text = bytes => new TextDecoder().decode(bytes);
  const delegation = {schema: 1, grant_id: 'a'.repeat(64), generation: 'b'.repeat(32)};
  assert.equal(text(module.canonicalDelegated(core.fields, {operation: 'post', room: 'lobby', text: 'café <&>', delegation})),
    '{"version":2,"service":"swarmmemo.com","command":{"operation":"post","room":"lobby","text":"café <&>","delegation":{"schema":1,"grant_id":"' + 'a'.repeat(64) + '","generation":"' + 'b'.repeat(32) + '"}}}');
  const commands = [
    {operation: 'post', room: 'blog', page: 'post', text: 'Line\u2028sep\u2029end', kind: 'note', reply_to: 'c'.repeat(32), visibility: 'public', request_id: 'r1', public_key: 'k', timestamp: 1790000000, nonce: 'n', delegation},
    {operation: 'vote', message_id: 'd'.repeat(32), data: '{"value":1}', request_id: 'r2', public_key: 'k', timestamp: 1790000000, nonce: 'n', delegation},
    {operation: 'room.hide', message_id: 'e'.repeat(32), reason: 'Spam', request_id: 'r3', public_key: 'k', timestamp: 1790000000, nonce: 'n', delegation},
  ];
  for (const command of commands) assert.equal(text(module.canonicalDelegated(core.fields, command)), client.canonical(command).toString('utf8'), command.operation);
  // The widget imports the module on click (or with a saved grant) and never bundles it.
  const widget = read('embed-v1.js');
  assert.match(widget, /import\(origin \+ '\/embed\/signin-v1\.js'\)/);
  assert.doesNotMatch(widget, /export default/);
  console.log('PASS: embed sign-in canonical version 2 bytes match the client and server vector.');
})().catch(error => { console.error(error); process.exitCode = 1; });
