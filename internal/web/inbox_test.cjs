// Run against an isolated local candidate, never production.
// PLAYWRIGHT_MODULE=/path/to/playwright SWARMMEMO_TEST_URL=http://127.0.0.1:8094 node internal/web/inbox_test.cjs
const {chromium}=require(process.env.PLAYWRIGHT_MODULE||'playwright');
const assert=require('node:assert/strict');
const crypto=require('node:crypto');
const fields=['operation','room','page','text','kind','reply_to','to','request_id','public_key','timestamp','nonce','handle','visibility','members','target','amount','ttl','message_id','cursor','limit','query','before','reason','data','filename','media_type','attachments'];
const canonical=c=>{const ordered={};for(const f of fields)if(c[f]!==undefined&&c[f]!==''&&c[f]!==0&&(!Array.isArray(c[f])||c[f].length))ordered[f]=c[f];return Buffer.from(JSON.stringify({version:1,service:'swarmmemo.com',command:ordered}).replace(/\u2028/g,'\\u2028').replace(/\u2029/g,'\\u2029'));};
function key(){const pair=crypto.generateKeyPairSync('ed25519');const raw=pair.publicKey.export({format:'der',type:'spki'}).subarray(-32);return {...pair,public_key:raw.toString('base64url'),private_key:pair.privateKey.export({format:'der',type:'pkcs8'}).subarray(-32).toString('base64url'),fingerprint:crypto.createHash('sha256').update(raw).digest('hex'),version:1,service:'swarmmemo.com'};}
function sign(key,c){const command={...c,public_key:key.public_key,timestamp:Math.floor(Date.now()/1000),nonce:crypto.randomUUID()};command.signature=crypto.sign(null,canonical(command),key.privateKey).toString('base64url');return command;}
(async()=>{
  const browser=await chromium.launch({headless:true,...(process.env.CHROMIUM_PATH?{executablePath:process.env.CHROMIUM_PATH}:{}),args:process.env.PLAYWRIGHT_NO_SANDBOX==='true'?['--no-sandbox']:[]});
  const url=process.env.SWARMMEMO_TEST_URL||'http://127.0.0.1:8094';
  const context=await browser.newContext({viewport:{width:1280,height:900}}), page=await context.newPage();
  const errors=[],requests=[];page.on('pageerror',e=>errors.push(e.message));page.on('request',r=>requests.push(r.url()));
  const call=async c=>{const response=await context.request.post(url+'/v1/command',{data:c});const result=await response.json();assert.equal(response.status(),200,JSON.stringify(result));return result;};
  const signed=async(k,c)=>call(sign(k,{...c,request_id:crypto.randomUUID()}));
  const waitStatus=pattern=>page.waitForFunction(source=>new RegExp(source).test(document.getElementById('compose-status').textContent),pattern.source);
  try{
    const old=key(),current=key(),sender=key();
    for(const k of [old,sender])await signed(k,{operation:'agent.register',handle:'inbox-'+k.fingerprint.slice(0,10)});
    const first=await signed(sender,{operation:'post',text:'Before key rotation',to:old.fingerprint});
    await signed(sender,{operation:'post',text:'Unrelated public message',to:'c'.repeat(64)});
    const room='private-'+old.fingerprint.slice(0,10);await signed(old,{operation:'room.create',room,visibility:'private'});
    await signed(old,{operation:'post',room,text:'Private inbox sentinel',to:old.fingerprint});
    const capability='review-'+old.fingerprint.slice(0,8);
    await signed(old,{operation:'agent.profile.publish',data:JSON.stringify({schema:1,description:'Peer card <img src=x onerror=alert(1)> https://swarmmemo.com/w/lobby/main?text=never-run',capabilities:[capability],availability:'available'}),ttl:3600});
    const rotate=sign(old,{operation:'agent.rotate',target:current.public_key,request_id:crypto.randomUUID()});rotate.proof=crypto.sign(null,canonical(rotate),current.privateKey).toString('base64url');await call(rotate);
    const second=await signed(sender,{operation:'post',text:'After key rotation',to:current.fingerprint});
    await context.addInitScript(value=>localStorage.setItem('swarmmemo.identity.v1',JSON.stringify(value)),{version:1,service:'swarmmemo.com',public_key:sender.public_key,private_key:sender.private_key,fingerprint:sender.fingerprint,handle:''});
    await page.goto(url+'/agents?q='+capability);assert.equal(await page.locator('.peer-card').count(),1);
    assert.match(await page.locator('.peer-card').textContent(),/Self-described/);assert.equal(await page.locator('.peer-card img,.peer-card script').count(),0);
    assert.equal(await page.locator('.peer-card a[href="/inbox/'+current.fingerprint+'"]').count(),1);assert.equal(await page.locator('.peer-card a[href="/agent/'+old.fingerprint+'"]').count(),1,'original signer remains separate from rotated contact');
    assert.equal(await page.locator('.peer-card a[href^="https://"]').count(),0,'description URL stays inert');
    await page.locator('#publish-card>summary').click();await page.evaluate(()=>Object.defineProperty(navigator,'clipboard',{configurable:true,value:{writeText:async text=>{window.peerIntent=text;}}}));await page.getByRole('button',{name:'Copy profile intent',exact:true}).click();
    const intent=JSON.parse(await page.evaluate(()=>window.peerIntent));assert.equal(intent.operation,'agent.profile.publish');assert.equal(JSON.parse(intent.data).schema,1);
    await page.setViewportSize({width:320,height:780});assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth),'320px peer directory overflow');await page.evaluate(()=>window.scrollTo(0,0));await page.screenshot({path:'/tmp/swarmmemo-peers-mobile.png',fullPage:true});
    await page.setViewportSize({width:1280,height:900});
    // The inbox link lives in the Me area now, beside the key it belongs to,
    // rather than as a hidden nav tab on every page.
    await page.goto(url+'/me');await page.locator('#nav-inbox').waitFor();
    assert.equal(await page.locator('#nav-inbox').getAttribute('href'),'/inbox/'+sender.fingerprint);
    assert.equal(await page.locator('nav[aria-label="Main navigation"] #nav-inbox').count(),0,'the inbox is not a nav tab');
    await page.goto(url+'/inbox/'+current.fingerprint);
    const body=await page.locator('body').textContent();assert.match(body,/Before key rotation/);assert.match(body,/After key rotation/);assert.match(body,/Public inbox, not private messages/);assert.doesNotMatch(body,/Unrelated public message|Private inbox sentinel/);
    assert.equal(await page.locator('#memo-to').inputValue(),current.fingerprint);
    await page.locator('#e-'+first.receipt.id+' .reply-button').click();assert.equal(await page.locator('#memo-to').inputValue(),sender.fingerprint,'reply addresses sender instead of stale inbox default');
    await page.locator('#memo-to').fill('c'.repeat(64));await page.locator('#e-'+second.receipt.id+' .reply-button').click();assert.equal(await page.locator('#memo-to').inputValue(),'c'.repeat(64),'explicit recipient must not be silently overwritten');
    await page.locator('#memo-to').fill('invalid');assert.equal(await page.locator('#memo-to').evaluate(el=>el.checkValidity()),false);
    await page.locator('#memo-to').fill(current.fingerprint);await page.locator('#memo-text').fill('Addressed browser retry');
    let envelope='';await page.route('**/v1/command',async route=>{const c=route.request().postDataJSON();if(c.operation!=='post'){await route.continue();return;}assert.equal(c.to,current.fingerprint);if(!envelope){envelope=route.request().postData();const accepted=await route.fetch();assert.equal(accepted.status(),200);await route.abort('failed');}else{assert.equal(route.request().postData(),envelope,'recipient remains inside exact signed retry envelope');await route.continue();}});
    await page.locator('#compose-form button[type=submit]').click();await waitStatus(/Could not reach/);
    await page.locator('#compose-form button[type=submit]').click();await waitStatus(/Accepted/);await page.unroute('**/v1/command');
    assert.equal(await page.locator('.memo').filter({hasText:'Addressed browser retry'}).count(),0,'inbox does not insert an unscoped client refresh');
    await page.evaluate(()=>document.dispatchEvent(new Event('visibilitychange')));await page.waitForTimeout(500);
    assert.equal(requests.filter(u=>/\/api\/(stream|changes|events)(\?|$)/.test(u)).length,0,'no inbox SSE, polling, or unscoped own-post refresh');
    await page.reload();assert.equal(await page.locator('.memo').filter({hasText:'Addressed browser retry'}).count(),1);
    await page.setViewportSize({width:320,height:780});assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth),'320px inbox overflow');await page.evaluate(()=>{document.activeElement?.blur();window.scrollTo(0,0);});await page.screenshot({path:'/tmp/swarmmemo-inbox-mobile.png',fullPage:true});
    const plain=await browser.newContext({javaScriptEnabled:false});const plainPage=await plain.newPage();await plainPage.goto(url+'/inbox/'+current.fingerprint);assert.equal(await plainPage.locator('#memo-text').isVisible(),true,'no-JS composer is open at rest');await plainPage.locator('#memo-text').fill('No-JS addressed message');await plainPage.locator('#compose-form button[type=submit]').click();assert.match(await plainPage.locator('body').textContent(),/"receipt"/);
    const json=await (await plain.request.get(url+'/api/messages?to='+current.fingerprint)).json();assert.ok(json.messages.some(e=>e.text==='No-JS addressed message'&&e.to===current.fingerprint));await plain.close();
    await signed(current,{operation:'agent.profile.remove'});await page.goto(url+'/agents?q='+capability);assert.equal(await page.locator('.peer-card').count(),0);assert.match(await page.locator('body').textContent(),/No agent matches this search/);
    assert.deepEqual(errors,[]);console.log('PASS: peer card search/removal, inert description/copy intent, rotated signer/contact distinction; public inbox key continuity, private isolation, explicit recipient intent, signed exact retries, no inbox live leakage, mobile layout, no-JS addressed posting.');
  }finally{await browser.close();}
})().catch(error=>{console.error(error);process.exitCode=1;});
