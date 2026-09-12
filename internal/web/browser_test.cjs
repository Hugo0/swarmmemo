// Optional full browser integration check. Run against an isolated local server:
// PLAYWRIGHT_MODULE=/path/to/playwright SWARMMEMO_TEST_URL=http://127.0.0.1:8089 node internal/web/browser_test.cjs
const {chromium} = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const assert = require('node:assert/strict');
const {execFileSync}=require('node:child_process');

(async () => {
  const browser = await chromium.launch({headless:true, ...(process.env.CHROMIUM_PATH?{executablePath:process.env.CHROMIUM_PATH}:{}), args:process.env.PLAYWRIGHT_NO_SANDBOX==='true'?['--no-sandbox']:[]});
  const context = await browser.newContext({viewport:{width:1280,height:1000},acceptDownloads:true});
  const page = await context.newPage(); const errors=[];
  page.on('pageerror',e=>errors.push(e.message));
  page.on('console',m=>{if(m.type()==='error'&&/Content Security Policy|Refused to/.test(m.text()))errors.push(m.text());});
  const url=process.env.SWARMMEMO_TEST_URL||'http://127.0.0.1:8089';
  const suffix=Date.now().toString(36);
  const waitStatus=async(id,pattern)=>{await page.waitForFunction(({id,source})=>new RegExp(source).test(document.getElementById(id).textContent),{id,source:pattern.source},{timeout:15000}).catch(async error=>{throw Error(error.message+' Current status: '+await page.locator('#'+id).textContent());});};
  try{
    await page.goto(url);
    // P06: the composer is the resting state, so there is nothing to open first.
    assert.equal(await page.locator('#compose').evaluate(e=>e.open),true,'composer open at rest');
    assert.equal(await page.locator('#memo-text').isVisible(),true);
    // Remember is now the browser default; preserve this explicit anonymous case.
    await page.locator('#compose-settings>summary').click();
    await page.getByRole('radio',{name:'Anonymous',exact:true}).check();
    await page.locator('#memo-text').fill('Browser integration check '+suffix+' <script>not executable</script>');
    await page.locator('#compose-form button[type=submit]').click(); await waitStatus('compose-status',/Accepted/);
    await page.goto(url+'/me'); await page.locator('#identity-create').click(); await waitStatus('identity-status',/registered/);
    await page.locator('#handle-form input').fill('browser-'+suffix);await page.locator('#handle-form button').click();await waitStatus('identity-status',/Alias registered/);
    const firstIdentity=await page.evaluate(()=>JSON.parse(localStorage.getItem('swarmmemo.identity.v1')));
    assert.equal(firstIdentity.fingerprint.length,64);
    assert.equal(Buffer.from(firstIdentity.private_key,'base64url').length,32,'portable raw Ed25519 seed');
    await page.locator('#quota-refresh').click();await waitStatus('quota-status',/loaded/);assert.match(await page.locator('#quota-values').textContent(),/Remaining/);
    await page.goto(url+'/#compose');
    await page.locator('#memo-text').fill('Signed attachment check '+suffix+' — café < > & \u2028');
    await page.locator('#compose-form input[type=file]').setInputFiles({name:'notes.txt',mimeType:'text/plain',buffer:Buffer.from('Public attachment '+suffix)});
    await page.locator('#compose-form button[type=submit]').click();await waitStatus('compose-status',/Accepted/);
    await page.reload();const signed=page.locator('.memo').filter({hasText:'Signed attachment check '+suffix});await signed.waitFor();assert.match(await signed.textContent(),new RegExp(firstIdentity.fingerprint.slice(0,12)));
    const publicFile=await signed.locator('a[download]').getAttribute('href');const fileResponse=await context.request.get(url+publicFile);assert.equal(fileResponse.status(),200);assert.match(await fileResponse.text(),/Public attachment/);
    // Accept a signed upload and post at the origin, then drop each first response.
    // The browser must retry the identical signature/nonce, not merely the request ID.
    await page.goto(url+'/#compose');await page.locator('#memo-text').fill('Lost response check '+suffix);
    await page.locator('#compose-form input[type=file]').setInputFiles({name:'retry.txt',mimeType:'text/plain',buffer:Buffer.from('Retry attachment '+suffix)});
    const captured={};const retried=new Set();
    await page.route('**/v1/command',async route=>{const command=route.request().postDataJSON();if(!['blob.put','post'].includes(command.operation)){await route.continue();return;}const body=route.request().postData();if(!captured[command.operation]){captured[command.operation]=body;const accepted=await route.fetch();assert.equal(accepted.status(),200);await route.abort('failed');}else{assert.equal(body,captured[command.operation],'retry exact '+command.operation+' envelope');retried.add(command.operation);await route.continue();}});
    await page.locator('#compose-form button[type=submit]').click();await waitStatus('compose-status',/Could not reach/);
    await page.locator('#compose-form button[type=submit]').click();await waitStatus('compose-status',/Could not reach/);
    await page.locator('#compose-form button[type=submit]').click();await waitStatus('compose-status',/Accepted/);
    await page.unroute('**/v1/command');assert.equal(retried.size,2);
    await page.goto(url+'/me');await page.locator('#private-create-form').locator('..').locator('summary').click();
    await page.locator('#private-create-form input[name=room]').fill('private-'+suffix);await page.locator('#private-create-form button').click();await waitStatus('private-status',/Private room created/);
    await page.locator('#private-compose-form textarea').fill('Private attachment check '+suffix);
    await page.locator('#private-compose-form input[type=file]').setInputFiles({name:'private.txt',mimeType:'text/plain',buffer:Buffer.from('Private bytes '+suffix)});
    await page.locator('#private-compose-form button').click();await waitStatus('private-status',/Accepted/);assert.match(await page.locator('#private-feed').textContent(),/Private attachment check/);
    const downloadPromise=page.waitForEvent('download');await page.locator('#private-feed .attachment button').click();const download=await downloadPromise;assert.equal(download.suggestedFilename(),'private.txt');
    const publicRoom=await context.request.get(url+'/r/private-'+suffix);assert.equal(publicRoom.status(),404);assert.ok(!(await publicRoom.text()).includes('Private attachment check'));
    await page.goto(url);await page.waitForTimeout(800);
    const beforeIncoming=await page.locator('.memo').first().boundingBox();
    const incoming=await (await context.request.get(url+'/w/lobby/main?format=json&text='+encodeURIComponent('External incoming '+suffix))).json();
    await page.locator('.new-messages').waitFor({state:'visible'});assert.equal(await page.locator('.memo').filter({hasText:'External incoming '+suffix}).count(),0);const afterIncoming=await page.locator('.memo').first().boundingBox();assert.equal(beforeIncoming.y,afterIncoming.y,'incoming notice must not move existing messages');await page.locator('.new-messages').click();assert.equal(await page.locator('.memo').filter({hasText:'External incoming '+suffix}).count(),1);
    if(process.env.SWARMMEMO_TEST_DATA){execFileSync(process.env.SWARMMEMO_TEST_BINARY||'/tmp/swarmmemo-ui',['moderate',incoming.receipt.id,'hide','Browser live moderation check'],{env:{...process.env,DATA_DIR:process.env.SWARMMEMO_TEST_DATA}});await page.locator('#e-'+incoming.receipt.id+' .removed').waitFor();assert.equal(await page.locator('.memo').filter({hasText:'External incoming '+suffix}).count(),0);console.log('PASS: open SSE page replaces moderated content without reload.');}
    await page.screenshot({path:'/tmp/swarmmemo-ui-desktop.png',fullPage:true});
    await page.setViewportSize({width:390,height:844});await page.screenshot({path:'/tmp/swarmmemo-ui-mobile.png',fullPage:true});assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth),'mobile horizontal overflow');
    await page.goto(url+'/me');await page.locator('#identity-forget').click({trial:true});
    page.once('dialog',d=>d.accept());await page.locator('#identity-forget').click();await waitStatus('identity-status',/removed/);
    const portableKey={public_key:firstIdentity.public_key,private_key:firstIdentity.private_key};
    await page.locator('#identity-import').setInputFiles({name:'key.json',mimeType:'application/json',buffer:Buffer.from(JSON.stringify(portableKey))});await waitStatus('identity-status',/imported/);
    page.once('dialog',d=>d.accept());const rotationDownload=page.waitForEvent('download');await page.locator('#identity-rotate').click();await rotationDownload;await waitStatus('rotation-status',/Key rotated/);
    const rotated=await page.evaluate(()=>JSON.parse(localStorage.getItem('swarmmemo.identity.v1')));assert.notEqual(rotated.fingerprint,firstIdentity.fingerprint);
    await page.locator('#quota-refresh').click();await waitStatus('quota-status',/loaded/);
    assert.deepEqual(errors,[]);console.log('PASS: anonymous/signed Unicode posts; attachment upload/download; private room isolation; key create/import/rotation; quotas; queued live feed; mobile width; no script/CSP errors.');
    const plain=await browser.newContext({javaScriptEnabled:false});const plainPage=await plain.newPage();await plainPage.goto(url);assert.equal(await plainPage.locator('#memo-text').isVisible(),true,'no-JS composer is open at rest');await plainPage.locator('#memo-text').fill('No JavaScript check '+suffix);await plainPage.locator('#compose-form button[type=submit]').click();assert.match(await plainPage.textContent('body'),/"receipt"/);await plain.close();console.log('PASS: no-JavaScript HTML form returns an accepted receipt.');
  } finally {await browser.close();}
})().catch(error=>{console.error(error);process.exitCode=1;});
