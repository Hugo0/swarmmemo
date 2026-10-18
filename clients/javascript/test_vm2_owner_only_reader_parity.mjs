// Offline only. Uses disposable random synthetic keys, never wallet/user credentials.
import test from 'node:test';
import assert from 'node:assert/strict';
import {mkdtemp, rm, open, appendFile, writeFile} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import {join} from 'node:path';
import {writeKey, loadKey, loadWalletKey} from './swarmmemo.mjs';

async function work(t) {
  const dir=await mkdtemp(join(tmpdir(),'vm2-swarmmemo-original-key-parity-'));
  t.after(async()=>rm(dir,{recursive:true,force:true}));
  return dir;
}
async function growAfterStat(t, path, extra) {
  const fd=await open(path,'r');
  const proto=Object.getPrototypeOf(fd);
  await fd.close();
  const before=proto.stat;
  let grew=false;
  proto.stat=async function(...args) {
    const original=await before.apply(this,args);
    if (!grew) {
      grew=true;
      await appendFile(path, Buffer.alloc(extra, 32));
    }
    return original;
  };
  t.after(()=>{proto.stat=before;});
  return ()=>grew;
}
test('one shared reader refuses a growing original identity key file',async t=>{
  const dir=await work(t);
  const path=join(dir,'synthetic-identity.json');
  await writeKey(path);
  const grew=await growAfterStat(t,path,16500);
  await assert.rejects(loadKey(path),error=>error?.code==='unsafe_key_file');
  assert.equal(grew(),true);
});
test('the same bounded helper also rejects growing EVM wallet file',async t=>{
  const dir=await work(t);
  const path=join(dir,'synthetic-wallet.txt');
  await writeFile(path,'42'.repeat(32),{mode:0o600});
  const grew=await growAfterStat(t,path,320);
  await assert.rejects(loadWalletKey({file:path}),error=>error?.code==='unsafe_key_file');
  assert.equal(grew(),true);
});
test('both original identity and synthetic wallet still load when unchanged',async t=>{
  const dir=await work(t);
  const identity=join(dir,'agent.json');
  const wallet=join(dir,'wallet.txt');
  await writeKey(identity);
  await writeFile(wallet,'42'.repeat(32),{mode:0o600});
  const key=await loadKey(identity);
  assert.ok(key.public_key);
  const evm=await loadWalletKey({file:wallet});
  assert.equal(evm.toString('hex'),'42'.repeat(32));
});
