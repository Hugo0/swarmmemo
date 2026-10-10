// Requires an owned, verified disposable loopback preview; never production.
const {chromium}=require(process.env.PLAYWRIGHT_MODULE||'playwright');
const assert=require('node:assert/strict');
const origin=process.env.SWARMMEMO_TEST_URL;
assert.match(origin||'',/^http:\/\/127\.0\.0\.1:\d+$/);
const slot='swarmmemo.identity.v1', savedSlot='swarmmemo.identities.v1';
(async()=>{
  const browser=await chromium.launch({headless:true,...(process.env.CHROMIUM_PATH?{executablePath:process.env.CHROMIUM_PATH}:{})});
  const status=async(page,id,pattern)=>page.waitForFunction(({id,pattern})=>new RegExp(pattern).test(document.getElementById(id).textContent),{id,pattern});
  const active=page=>page.evaluate(name=>JSON.parse(localStorage.getItem(name)),slot);
  const saved=page=>page.evaluate(name=>JSON.parse(localStorage.getItem(name)||'null'),savedSlot);
  try {
    const context=await browser.newContext({viewport:{width:1280,height:900}}),page=await context.newPage();
    const writes=[];context.on('request',r=>{if(new URL(r.url()).pathname==='/v1/command'&&r.method()==='POST')writes.push(r.postDataJSON());});
    page.on('dialog',d=>d.accept());
    await page.goto(origin+'/me');await page.locator('#identity-create').click();await status(page,'identity-status','Identity registered');
    const first=await active(page);
    // A pre-switcher browser: only the single slot. Reads must not write the list.
    await page.evaluate(name=>localStorage.removeItem(name),savedSlot);await page.reload();
    assert.equal(await saved(page),null,'a read never writes the key list');
    assert.equal(await page.locator('#identity-list li').count(),1,'the legacy key is listed');
    assert.equal((await active(page)).public_key,first.public_key);
    // Creating another adds; the legacy key is carried into the list.
    await page.locator('#identity-add').click();await status(page,'identity-status','registered and active');
    const second=await active(page);assert.notEqual(second.public_key,first.public_key);
    assert.deepEqual((await saved(page)).map(k=>k.public_key).sort(),[first.public_key,second.public_key].sort());
    await page.reload();await page.locator('#identity-switcher').waitFor();
    assert.equal(await page.locator('#identity-list li').count(),2);
    assert.equal(writes.at(-1).public_key,second.public_key);
    for(const width of [1280,320]){await page.setViewportSize({width,height:900});assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth));if(process.env.SWARMMEMO_SCREENSHOT_DIR)await page.screenshot({path:process.env.SWARMMEMO_SCREENSHOT_DIR+'/identity-switcher-'+width+'.png',fullPage:true});}
    // Notifications are per key: seed the first key's count, switch, and Me shows it.
    await page.evaluate(fp=>localStorage.setItem('swarmmemo.notify.v1:'+fp,JSON.stringify({fp,cursor:'',polled_at:Date.now(),rooms:0,requests:0,replies:[],addressed:['x']})),first.fingerprint);
    await page.reload();assert.equal(await page.locator('#me-count').isHidden(),true,'another key\'s news is not shown on the active one');
    await Promise.all([page.waitForEvent('load'),page.locator('#identity-list button[data-switch]').click()]);
    assert.equal((await active(page)).public_key,first.public_key,'switch makes the chosen key active');
    assert.equal(await page.locator('#me-count').textContent(),'1');
    // Composer posts as the active key and says so.
    await page.goto(origin+'/?sort=new#compose');await page.locator('#memo-text').fill('Posting as my first key');
    assert.equal(await page.locator('#compose-identity').textContent(),first.handle||first.fingerprint.slice(0,12));
    await page.locator('#compose-form button[type=submit]').click();await status(page,'compose-status','Accepted');
    assert.equal(writes.filter(c=>c.operation==='post').at(-1).public_key,first.public_key);
    // Import adds instead of replacing.
    const third=await page.evaluate(async()=>{const p=await crypto.subtle.generateKey({name:'Ed25519'},true,['sign','verify']);const b=x=>btoa(String.fromCharCode(...new Uint8Array(x))).replace(/\+/g,'-').replace(/\//g,'_').replace(/=+$/,'');return {public_key:b(await crypto.subtle.exportKey('raw',p.publicKey)),private_key:b((new Uint8Array(await crypto.subtle.exportKey('pkcs8',p.privateKey))).slice(-32))};});
    await page.goto(origin+'/me#key');await page.locator('#identity-import').setInputFiles({name:'third.json',mimeType:'application/json',buffer:Buffer.from(JSON.stringify(third))});
    await status(page,'identity-status','Identity imported');
    assert.equal((await active(page)).public_key,third.public_key);assert.equal((await saved(page)).length,3);
    // A handle changed elsewhere follows the server into the copy kept with the key.
    await page.route('**/api/agent/*', route => route.fulfill({status: 200, contentType: 'application/json', body: JSON.stringify({ok: true, agent: {handle: 'renamed-elsewhere'}})}));
    await page.evaluate(() => sessionStorage.clear());
    await page.goto(origin + '/?sort=new');
    await page.waitForFunction(() => (document.getElementById('nav-identity')?.textContent || '').includes('renamed-elsewhere'));
    assert.equal((await active(page)).handle, 'renamed-elsewhere');
    await page.unroute('**/api/agent/*');
    await page.goto(origin + '/me#key');
    // Remove an inactive key; forget the active one. The rest stay.
    await page.locator(`#identity-list button[data-remove="${second.public_key}"]`).click();await status(page,'identity-status','removed from this browser');
    assert.deepEqual((await saved(page)).map(k=>k.public_key).sort(),[first.public_key,third.public_key].sort());
    await page.locator('#identity-forget').click();await status(page,'identity-status','Switch to one of your other keys');
    assert.equal(await active(page),null);assert.deepEqual((await saved(page)).map(k=>k.public_key),[first.public_key]);
    assert.equal(await page.locator('#identity-switcher').isVisible(),true,'a saved key with none active can be switched to');
    await context.close();
    console.log('PASS: legacy single-slot migration, create adds, switch, per-key notifications, composer posting as, import adds, handle follows the server, remove and forget.');
  } finally {await browser.close();}
})().catch(error=>{console.error(error);process.exit(1);});
