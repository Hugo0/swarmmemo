// Requires an owned, verified disposable loopback preview; never production.
const {chromium}=require(process.env.PLAYWRIGHT_MODULE||'playwright');
const assert=require('node:assert/strict');
const crypto=require('node:crypto');
const origin=process.env.SWARMMEMO_TEST_URL;
assert.match(origin||'',/^http:\/\/127\.0\.0\.1:\d+$/);
const slot='swarmmemo.identity.v1', rotationSlot='swarmmemo.identity.pending-rotation.v1';
(async()=>{
  const browser=await chromium.launch({headless:true,...(process.env.CHROMIUM_PATH?{executablePath:process.env.CHROMIUM_PATH}:{})});
  const status=async(page,id,pattern)=>page.waitForFunction(({id,pattern})=>new RegExp(pattern).test(document.getElementById(id).textContent),{id,pattern});
  const submit=page=>page.locator('#compose-form button[type=submit]').click();
  // Posting identity now lives under Options (P06), which is closed at rest.
  const options=async page=>{if(!await page.locator('#compose').evaluate(e=>e.open))await page.locator('#compose-cta').click();if(!await page.locator('#compose-settings').evaluate(e=>e.open))await page.locator('#compose-settings>summary').click();};
  const key=page=>page.evaluate(name=>JSON.parse(localStorage.getItem(name)),slot);
  const post=async(page,text)=>{await page.locator('#memo-text').fill(text);await submit(page);await status(page,'compose-status','Accepted');};
  const tokens=page=>page.evaluate(async()=>((await navigator.locks.query()).held||[]).filter(l=>l.name.startsWith('swarmmemo-pending-v1:')).length);
  try {
    const context=await browser.newContext({viewport:{width:320,height:850}}),a=await context.newPage(),b=await context.newPage();
    const writes=[];context.on('request',r=>{if(new URL(r.url()).pathname==='/v1/command'&&r.method()==='POST')writes.push(r.postDataJSON());});
    await Promise.all([a.goto(origin+'/#compose'),b.goto(origin+'/#compose')]);
    assert.equal(await key(a),null);assert.equal(writes.length,0,'reads must never create identities');
    assert.equal(await a.locator('#compose-settings').evaluate(e=>e.open),false,'Options are closed at rest');
    await options(a);
    assert.ok(await a.getByRole('radio',{name:'Remember me on this device'}).isChecked());
    await a.locator('#compose>summary').focus();await a.keyboard.press('Enter');await a.keyboard.press('Enter');
    assert.equal(await a.locator('#compose').evaluate(e=>e.open),true,'keyboard toggle returns the composer to its open resting state');
    for(const width of [1280,320]){await a.setViewportSize({width,height:900});assert.ok(await a.evaluate(()=>document.documentElement.scrollWidth<=innerWidth));if(process.env.SWARMMEMO_SCREENSHOT_DIR)await a.screenshot({path:process.env.SWARMMEMO_SCREENSHOT_DIR+'/identity-compose-'+width+'.png',fullPage:true});}
    await Promise.all([post(a,'Two-tab first hello A'),post(b,'Two-tab first hello B')]);
    const first=await key(a);assert.ok(first&&first.public_key&&first.private_key);
    const posts=writes.filter(c=>c.operation==='post');assert.equal(posts.length,2);
    assert.equal(posts[0].public_key,posts[1].public_key,'simultaneous first posts share one stored key');
    assert.equal(posts[0].public_key,first.public_key);assert.ok(posts.every(c=>c.signature));
    assert.equal(writes.filter(c=>c.operation==='agent.register').length,0,'first signed post needs no registration request');
    await a.reload();
    await a.evaluate(({slot,value})=>window.dispatchEvent(new StorageEvent('storage',{key:slot,oldValue:null,newValue:JSON.stringify(value),storageArea:localStorage})),{slot,value:first});
    await post(a,'Reload keeps my signing key');assert.equal(writes.at(-1).public_key,first.public_key,'queued exact first-mint event cannot fence an already adopted key');
    assert.match(await a.locator('#nav-identity').textContent(),/^Me · /);
    await options(a);await a.getByRole('radio',{name:'Anonymous',exact:true}).check();await post(a,'Explicit anonymous hello');
    assert.equal(writes.at(-1).public_key,undefined);assert.equal((await key(a)).public_key,first.public_key,'anonymous mode never discards the remembered key');
    await b.locator('.workspace-link').focus();await b.keyboard.press('Enter');await b.getByRole('heading',{name:'Me',exact:true}).waitFor();
    assert.equal(await b.locator('.workspace-link').getAttribute('aria-current'),'page');
    for(const width of [1280,320]){await b.setViewportSize({width,height:900});assert.ok(await b.evaluate(()=>document.documentElement.scrollWidth<=innerWidth));if(process.env.SWARMMEMO_SCREENSHOT_DIR)await b.screenshot({path:process.env.SWARMMEMO_SCREENSHOT_DIR+'/identity-me-'+width+'.png',fullPage:true});}

    // A lost accepted response retains an exact signer/wire and a noncontending
    // token. Other tabs cannot change identity, but the original tab can retry.
    await options(a);await a.getByRole('radio',{name:'Remember me on this device'}).check();
    let wire='',retryCount=0;await a.route('**/v1/command',async route=>{
      if(route.request().postDataJSON().operation!=='post')return route.continue();
      retryCount++;
      if(!wire){wire=route.request().postData();assert.equal((await route.fetch()).status(),200);await route.abort('failed');}
      else{assert.equal(route.request().postData(),wire,'unknown response retry keeps exact signed bytes');if(retryCount===2)await route.fulfill({status:429,contentType:'application/json',body:JSON.stringify({ok:false,error:{code:'request_rate',message:'Synthetic admission rejection before dedup.'}})});else await route.continue();}
    });
    await a.locator('#memo-text').fill('Unresolved signed hello');await submit(a);await status(a,'compose-status','Could not reach');
    assert.equal(await tokens(a),1);
    await options(a);
    await a.getByRole('radio',{name:'Anonymous',exact:true}).click();
    assert.ok(await a.getByRole('radio',{name:'Remember me on this device'}).isChecked());
    await b.goto(origin+'/me');await b.locator('#identity-forget').click();
    await status(b,'identity-status','unresolved request');assert.equal((await key(b)).public_key,first.public_key);
    await b.locator('#identity-import').setInputFiles({name:'same-key.json',mimeType:'application/json',buffer:Buffer.from(JSON.stringify(first))});
    await status(b,'identity-status','unresolved request');
    b.once('dialog',d=>d.accept());await b.locator('#identity-rotate').click();await status(b,'rotation-status','unresolved request');
    assert.equal(await b.evaluate(name=>localStorage.getItem(name),rotationSlot),null);
    // Simulate an observed storage change outside this UI's locking discipline.
    // It must not reload A or replace the signer of its already prepared retry.
    const pair=crypto.generateKeyPairSync('ed25519');const raw=pair.publicKey.export({format:'der',type:'spki'}).subarray(-32);
    const replacement={version:1,service:'swarmmemo.com',public_key:raw.toString('base64url'),private_key:pair.privateKey.export({format:'der',type:'pkcs8'}).subarray(-32).toString('base64url'),fingerprint:crypto.createHash('sha256').update(raw).digest('hex'),handle:''};
    await b.evaluate(({slot,value})=>localStorage.setItem(slot,JSON.stringify(value)),{slot,value:replacement});
    await status(a,'compose-status','Identity changed in another tab');
    assert.equal(await a.locator('#memo-text').inputValue(),'Unresolved signed hello');await submit(a);await status(a,'compose-status','Synthetic admission rejection');
    assert.equal(await tokens(a),1,'later admission rejection cannot clear an ever-ambiguous intent');
    await options(a);await a.getByRole('radio',{name:'Anonymous',exact:true}).click();assert.ok(await a.getByRole('radio',{name:'Remember me on this device'}).isChecked());
    await submit(a);await status(a,'compose-status','Accepted');
    await a.unroute('**/v1/command');assert.equal(await tokens(a),0);
    const retained=(await(await context.request.get(origin+'/api/messages?room=lobby&q='+encodeURIComponent('Unresolved signed hello'))).json()).messages;
    assert.equal(retained.filter(e=>e.text==='Unresolved signed hello').length,1,'lost response plus outer429 retry must create one memo');
    await b.reload();

    // Observe a cross-tab transition without reloading or discarding a draft.
    await a.locator('#memo-text').fill('Keep this cross-tab draft');
    b.once('dialog',async d=>{assert.equal(await a.evaluate(async()=>((await navigator.locks.query()).held||[]).some(l=>l.name==='swarmmemo-credentials-v1')),false,'confirmation must not hold the credential lock');await d.accept();});await b.locator('#identity-forget').click();await status(b,'identity-status','Key removed');
    await status(a,'compose-status','Identity changed in another tab');
    assert.equal(await a.locator('#memo-text').inputValue(),'Keep this cross-tab draft');
    const beforeDrift=writes.length;await submit(a);await status(a,'compose-status','reconcile|Reconcile|changed');assert.equal(writes.length,beforeDrift);
    // Explicit anonymous is allowed after a failed, unsent remembered attempt.
    await options(a);await a.getByRole('radio',{name:'Anonymous',exact:true}).check();await post(a,'Explicit anonymous after storage change');
    assert.equal(writes.at(-1).public_key,undefined);
    await context.close();
    console.log('PASS: read-only visits, two-tab first mint, implicit registration, reload continuity, explicit anonymous, exact retry, cross-tab transition fencing and draft retention.');

    for(const failure of ['write','readback','crypto','locks']){
      const c=await browser.newContext();await c.addInitScript(({failure,slot})=>{
        if(failure==='write'||failure==='readback'){const generate=crypto.subtle.generateKey.bind(crypto.subtle);window.keyGenerations=0;crypto.subtle.generateKey=(...args)=>{window.keyGenerations++;return generate(...args);};}
        if(failure==='write'){const put=Storage.prototype.setItem;Storage.prototype.setItem=function(k,v){if(k===slot)throw Error('synthetic blocked storage');return put.call(this,k,v);};}
        if(failure==='readback'){const get=Storage.prototype.getItem;window.originalGet=(k)=>get.call(localStorage,k);Storage.prototype.getItem=function(k){return k===slot?null:get.call(this,k);};}
        if(failure==='crypto')Object.defineProperty(crypto,'subtle',{value:undefined});
        if(failure==='locks')Object.defineProperty(navigator,'locks',{value:undefined});
      },{failure,slot});
      const p=await c.newPage();let sent=0;p.on('request',r=>{if(r.method()==='POST')sent++;});
      await p.goto(origin+'/#compose');await p.locator('#memo-text').fill('No silent fallback '+failure);await submit(p);await status(p,'compose-status','storage|HTTPS|Web Locks');
      assert.equal(sent,0);assert.equal(await p.locator('#memo-text').inputValue(),'No silent fallback '+failure);
      if(failure==='write'||failure==='readback'){await submit(p);await status(p,'compose-status','storage');assert.equal(sent,0);assert.equal(await p.evaluate(()=>window.keyGenerations),1,'failed first save retries the same in-memory key, not replacement keys');}
      await options(p);await p.getByRole('radio',{name:'Anonymous',exact:true}).check();await submit(p);await status(p,'compose-status','Accepted');assert.equal(sent,1);
      await c.close();
    }
    console.log('PASS: blocked storage/readback, missing WebCrypto/WebLocks refuse remembered send; anonymous requires explicit selection.');

    const capped=await browser.newContext(),cap=await capped.newPage();await cap.goto(origin+'/#compose');await post(cap,'Pending token cap setup');
    await cap.evaluate(async()=>{window.releaseTestTokens=[];await Promise.all(Array.from({length:32},(_,i)=>new Promise(resolve=>navigator.locks.request('swarmmemo-pending-v1:test-cap-'+i,async()=>{let release;const done=new Promise(r=>release=r);window.releaseTestTokens.push(release);resolve();await done;}))));});
    let capWrites=0;cap.on('request',r=>{if(r.method()==='POST')capWrites++;});await cap.locator('#memo-text').fill('Wait for an open-tab token');await submit(cap);await status(cap,'compose-status','Too many unresolved requests');assert.equal(capWrites,0);
    await cap.evaluate(()=>window.releaseTestTokens.forEach(release=>release()));await submit(cap);await status(cap,'compose-status','Accepted');assert.equal(capWrites,1);await capped.close();

    const recovery=await browser.newContext({acceptDownloads:true}),p=await recovery.newPage();
    await p.goto(origin+'/#compose');await post(p,'Rotation recovery setup');const original=await key(p);
    await p.goto(origin+'/me');let rotationWire='';
    await p.route('**/v1/command',async route=>{
      if(route.request().postDataJSON().operation!=='agent.rotate')return route.continue();
      rotationWire=route.request().postData();assert.equal((await route.fetch()).status(),200);await route.abort('failed');
    });
    p.once('dialog',d=>d.accept());await p.locator('#identity-rotate').click();await status(p,'rotation-status','pending rotation');
    const pendingRaw=await p.evaluate(name=>localStorage.getItem(name),rotationSlot);assert.ok(pendingRaw);
    await p.locator('#identity-forget').click();await status(p,'identity-status','unresolved|Pending rotation');
    assert.equal(await p.evaluate(name=>localStorage.getItem(name),rotationSlot),pendingRaw);
    // Closing releases live token locks, not server authority. Saved rotation
    // material remains for an explicit exact replay; no general outbox claim.
    await p.close();const next=await recovery.newPage();await next.goto(origin+'/me');assert.equal(await tokens(next),0);
    await next.locator('#identity-forget').click();await status(next,'identity-status','Pending rotation');
    await next.route('**/v1/command',async route=>{if(route.request().postDataJSON().operation==='agent.rotate')assert.equal(route.request().postData(),rotationWire);await route.continue();});
    next.once('dialog',d=>d.accept());await next.locator('#identity-rotate').click();await status(next,'rotation-status','Key rotated');
    assert.notEqual((await key(next)).public_key,original.public_key);assert.equal(await next.evaluate(name=>localStorage.getItem(name),rotationSlot),null);
    const newKey=await key(next);const download=next.waitForEvent('download');await next.locator('#identity-export').click();assert.match((await download).suggestedFilename(),/^swarmmemo-key-/);
    const alias=await recovery.newPage();await alias.goto(origin.replace('127.0.0.1','localhost')+'/me');assert.equal(await key(alias),null,'origins do not share keys');
    await alias.locator('#identity-import').setInputFiles({name:'portable.json',mimeType:'application/json',buffer:Buffer.from(JSON.stringify({public_key:newKey.public_key,private_key:newKey.private_key}))});
    await status(alias,'identity-status','Identity imported');assert.equal((await key(alias)).public_key,newKey.public_key);
    alias.once('dialog',d=>d.dismiss());await alias.locator('#identity-import').setInputFiles({name:'cancel.json',mimeType:'application/json',buffer:Buffer.from(JSON.stringify(original))});
    await status(alias,'identity-status','Import cancelled');assert.equal((await key(alias)).public_key,newKey.public_key);
    await alias.setViewportSize({width:320,height:850});assert.ok(await alias.evaluate(()=>document.documentElement.scrollWidth<=innerWidth));
    // Hold a legitimate private read across a credential-change event. Clearing
    // existing content is insufficient unless its delayed response is fenced too.
    const room='identity-read-'+Date.now().toString(36);
    await next.locator('#private-create-form').locator('..').locator('summary').click();
    await next.locator('#private-create-form input[name=room]').fill(room);await next.locator('#private-create-form button').click();await status(next,'private-status','Private room created');
    await next.locator('#private-compose-form textarea').fill('Old-key delayed private sentinel');await next.locator('#private-compose-form button').click();await status(next,'private-status','Accepted');
    let releaseRead, enteredRead;const entered=new Promise(resolve=>enteredRead=resolve);
    await next.route('**/v1/command',async route=>{
      if(route.request().postDataJSON().operation!=='messages.list')return route.continue();
      const response=await route.fetch();enteredRead();await new Promise(resolve=>releaseRead=resolve);await route.fulfill({response});
    });
    await next.locator('#private-open-form button').click();await entered;
    const other=await recovery.newPage();await other.goto(origin+'/for-agents');await other.evaluate(name=>localStorage.removeItem(name),slot);
    await status(next,'identity-status','Identity changed in another tab');assert.equal(await next.locator('#private-feed').textContent(),'');
    releaseRead();await status(next,'private-status','old response was discarded');assert.equal(await next.locator('#private-feed').textContent(),'');
    assert.equal(await next.locator('#private-room').isVisible(),false);
    await recovery.close();
    console.log('PASS: global32 pending-token admission, rotation unknown/forget guard, explicit exact recovery after tab close, backup/import/cancellation, origin separation, private response epoch fencing.');

    const plain=await browser.newContext({javaScriptEnabled:false}),n=await plain.newPage();await n.goto(origin+'/#compose');
    assert.equal(await n.locator('#memo-text').isVisible(),true,'no-JS composer is open at rest');
    assert.equal(await n.locator('#compose-settings').evaluate(e=>e.open),false,'Options stay a closed native disclosure without scripts');
    assert.equal(await n.locator('#posting-mode').isVisible(),false);await n.locator('#memo-text').fill('Native anonymous without scripts');await submit(n);
    const response=JSON.parse(await n.locator('body').textContent());const event=(await(await plain.request.get(origin+'/e/'+response.receipt.id+'?format=json')).json()).messages[0];
    assert.equal(event.public_key,undefined);assert.equal(event.signature,undefined);assert.equal(event.author,'anonymous');await plain.close();console.log('PASS: no-JavaScript native POST remains anonymous.');
  } finally {await browser.close();}
})().catch(e=>{console.error(e);process.exitCode=1;});
