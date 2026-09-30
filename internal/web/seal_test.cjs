// assets/seal.js against clients/python/seal-vector.json (shared with the Python
// and Go tests): RFC 9180 A.1, the wrap, envelope, file and safety vectors,
// AAD binding, a removed member and pinning. It runs in Node 22 always and, on
// an owned loopback preview, in Chromium too: one module, both runtimes.
const assert=require('node:assert/strict');
const path=require('node:path');
const {pathToFileURL}=require('node:url');
const crypto=require('node:crypto');
const vector=require('../../clients/python/seal-vector.json');
const origin=process.env.SWARMMEMO_TEST_URL;

// The checks, as one function source so the browser runs exactly these.
async function checks(seal,vector,signer){
  const h=s=>Uint8Array.from(s.match(/../g)||[],b=>parseInt(b,16));
  const eq=(a,b,what)=>{if(a!==b)throw Error(what+': '+a+' != '+b);};
  const rejects=async(work,what)=>{try{await work();}catch(error){if(error.name==='SealError')return;throw error;}throw Error(what+' did not fail');};
  const a1=vector.hpke_a1;
  eq(seal.hex(await seal.publicKeyOf(h(a1.skEm))),a1.pkEm,'pkEm');
  eq(seal.hex(await seal.publicKeyOf(h(a1.skRm))),a1.pkRm,'pkRm');
  const sealed=await seal.hpkeSeal(h(a1.pkRm),h(a1.info),h(a1.aad),h(a1.pt),h(a1.skEm));
  eq(seal.hex(sealed.enc),a1.enc,'A.1 enc');eq(seal.hex(sealed.ct),a1.ct,'A.1 ct');
  eq(seal.hex(await seal.hpkeOpen(h(a1.skRm),sealed.enc,h(a1.info),h(a1.aad),sealed.ct)),a1.pt,'A.1 open');
  await rejects(()=>seal.hpkeOpen(h(a1.skRm),sealed.enc,h(a1.info),h('436f756e742d31'),sealed.ct),'A.1 wrong aad');
  const w=vector.wrap;
  eq(seal.hex(seal.wrapInfo(w.room,w.epoch)),w.info_hex,'wrap info');
  eq(await seal.kid(seal.unb64(w.recipient_public)),w.kid,'kid');
  const wrapped=await seal.wrap(seal.unb64(w.recipient_public),seal.unb64(w.epoch_key),w.room,w.epoch,{ephemeral:seal.unb64(w.ephemeral_private)});
  eq(JSON.stringify(wrapped),JSON.stringify({kid:w.kid,enc:w.enc,ct:w.ct}),'wrap');
  eq(seal.b64(await seal.unwrap(seal.unb64(w.recipient_private),wrapped,w.room,w.epoch)),w.epoch_key,'unwrap');
  await rejects(()=>seal.unwrap(seal.unb64(w.recipient_private),wrapped,w.room,w.epoch+1),'wrap moved to another epoch');
  const e=vector.envelope,key=seal.unb64(e.epoch_key);
  eq(seal.hex(seal.envelopeAAD(e.service,e.room,e.epoch,e.author)),e.aad_hex,'aad');
  eq(await seal.seal(key,e.service,e.room,e.epoch,e.author,new TextEncoder().encode(e.plaintext),{nonce:seal.unb64(e.nonce)}),e.envelope,'envelope');
  eq(seal.envelopeEpoch(e.envelope),e.epoch,'envelope epoch');
  eq(JSON.stringify(await seal.openEnvelope({[e.epoch]:key},e.service,e.room,e.author,e.envelope)),JSON.stringify(JSON.parse(e.plaintext)),'open envelope');
  // AAD binding: a member reposting another's ciphertext as its own, or the
  // server moving it to another room, epoch or service, fails for every reader.
  await rejects(()=>seal.openEnvelope({[e.epoch]:key},e.service,e.room,e.other_author,e.envelope),'repost as another author');
  await rejects(()=>seal.openEnvelope({[e.epoch]:key},e.service,'~'+'b'.repeat(26),e.author,e.envelope),'moved room');
  await rejects(()=>seal.openEnvelope({[e.epoch]:key},'publicbbs.com',e.room,e.author,e.envelope),'other service');
  await rejects(()=>seal.openEnvelope(new Map([[4,key]]),e.service,e.room,e.author,e.envelope.replace('sealed1.3.','sealed1.4.')),'relabelled epoch');
  const f=vector.file;
  const file=await seal.encryptFile(h(f.plaintext_hex),{key:seal.unb64(f.key),nonce:seal.unb64(f.nonce)});
  eq(seal.b64(file.blob),f.blob,'file blob');eq(file.entry.sha256,f.sha256,'file sha256');
  eq(seal.hex(await seal.decryptFile(file.blob,file.entry)),f.plaintext_hex,'file open');
  const tampered=file.blob.slice();tampered[tampered.length-1]^=1;
  await rejects(()=>seal.decryptFile(tampered,file.entry),'tampered file');
  eq(await seal.safetyNumber(vector.safety.public_key,vector.safety.x25519),vector.safety.number,'safety number');
  // Limits: the largest plaintext still fits the 16 KiB post text.
  const big=seal.plaintext('x'.repeat(seal.SEALED_PLAINTEXT_BYTES-22));
  eq(big.length,seal.SEALED_PLAINTEXT_BYTES,'limit');
  if((await seal.seal(seal.newEpochKey(),'swarmmemo.com',w.room,1,e.author,big)).length>16384)throw Error('envelope over 16 KiB');
  await rejects(async()=>seal.plaintext('x'.repeat(seal.SEALED_PLAINTEXT_BYTES)),'over the limit');
  // A removed member cannot read the next epoch.
  const members={};for(const name of ['ada','bo','cy'])members[name]=await seal.generateKeyPair();
  const next=seal.newEpochKey();
  const data=JSON.parse(await seal.rotationData(next,w.room,2,2,['ada','bo'].map(agent=>({agent,x25519:seal.b64(members[agent].publicKey)}))));
  eq(data.wraps.map(x=>x.agent).join(),'ada,bo','wraps follow the members');
  for(const x of data.wraps)await rejects(()=>seal.unwrap(members.cy.privateKey,x,w.room,2),'removed member unwrap');
  eq(seal.hex(await seal.unwrap(members.bo.privateKey,data.wraps[1],w.room,2)),seal.hex(next),'member unwrap');
  // Pinning: a signed creating command pins sealed; a server that later says
  // unsealed, or swaps the creating command, is refused.
  const room=w.room,pins={};
  const created=await signer({operation:'conversation.open',room,data:JSON.stringify({schema:1,kind:'group',sealed:true})});
  const conversation={room,sealed:true,created};
  eq(await seal.checkPin(pins,conversation,'swarmmemo.com'),true,'pin');
  await rejects(async()=>seal.refuseCleartext(pins,room),'cleartext to a pinned room');
  await rejects(()=>seal.checkPin(pins,{...conversation,sealed:false},'swarmmemo.com'),'downgrade by flag');
  const swapped=await signer({operation:'conversation.open',room,data:JSON.stringify({schema:1,kind:'group',sealed:false})},true);
  await rejects(()=>seal.checkPin(pins,{room,sealed:false,created:swapped},'swarmmemo.com'),'downgrade by a new creator');
  await rejects(()=>seal.verifyCreated({...conversation,created:{...created,signed_payload:created.signed_payload.replace('true','false')}},'swarmmemo.com'),'forged creating command');
  return 'ok';
}

// Signs like any client, with a fresh Ed25519 key (second: another key).
const fields=['operation','room','page','text','kind','reply_to','to','request_id','public_key','timestamp','nonce','handle','visibility','members','target','amount','ttl','message_id','cursor','limit','query','before','reason','data','filename','media_type','attachments'];
const keys=[crypto.generateKeyPairSync('ed25519'),crypto.generateKeyPairSync('ed25519')];
function nodeSigner(command,second=false){
  const pair=keys[second?1:0],public_key=pair.publicKey.export({format:'der',type:'spki'}).subarray(-32).toString('base64url');
  const full={...command,public_key,timestamp:1700000000,nonce:'n'};const o={};
  for(const f of fields)if(full[f]!==undefined&&full[f]!=='')o[f]=full[f];
  const signed_payload=JSON.stringify({version:1,service:'swarmmemo.com',command:o});
  return {public_key,signature:crypto.sign(null,Buffer.from(signed_payload),pair.privateKey).toString('base64url'),signed_payload};
}

(async()=>{
  const seal=await import(pathToFileURL(path.join(__dirname,'assets','seal.js')).href);
  assert.equal(await checks(seal,vector,async(c,s)=>nodeSigner(c,s)),'ok');
  console.log('seal.js vectors pass in Node');
  if(!origin){console.log('SWARMMEMO_TEST_URL unset: browser half skipped');return;}
  assert.match(origin,/^http:\/\/127\.0\.0\.1:\d+$/,'requires an explicit disposable loopback preview');
  const {chromium}=require(process.env.PLAYWRIGHT_MODULE||'playwright');
  const browser=await chromium.launch({headless:true,...(process.env.CHROMIUM_PATH?{executablePath:process.env.CHROMIUM_PATH}:{})});
  try{
    // bypassCSP only so the page can eval the shared check source; seal.js
    // itself loads under the site's own script-src 'self'.
    const page=await (await browser.newContext({bypassCSP:true})).newPage();const errors=[];page.on('pageerror',e=>errors.push(e.message));
    await page.goto(origin+'/me');
    // Signatures are made in Node; the page receives them precomputed by command.
    const signatures={};
    for(const [name,command,second] of [['sealed',{operation:'conversation.open',room:vector.wrap.room,data:JSON.stringify({schema:1,kind:'group',sealed:true})},false],['swapped',{operation:'conversation.open',room:vector.wrap.room,data:JSON.stringify({schema:1,kind:'group',sealed:false})},true]])signatures[name]=nodeSigner(command,second);
    const result=await page.evaluate(async({source,vector,signatures})=>{
      const seal=await import('/assets/seal.js');
      const checks=(0,eval)('('+source+')');
      return checks(seal,vector,async(command,second)=>signatures[second?'swapped':'sealed']);
    },{source:checks.toString(),vector,signatures});
    assert.equal(result,'ok');assert.deepEqual(errors,[]);
    console.log('seal.js vectors pass in Chromium');
  }finally{await browser.close();}
})().catch(error=>{console.error(error);process.exit(1);});
