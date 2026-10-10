// Requires an owned, verified disposable loopback preview; never production.
// /me "Raise your standing" (C144): the list is the agent page's
// #standing-ways, imported; GitHub shows the statement to publish and links
// as claimed; a wallet without a browser wallet gets its exact message and a
// readable refusal for a bad signature; proof of work runs in the browser and
// the pow row reads verified after a reload. Skipped where trust is off (no
// list to raise).
const {chromium}=require(process.env.PLAYWRIGHT_MODULE||'playwright');
const assert=require('node:assert/strict');
const origin=process.env.SWARMMEMO_TEST_URL;
assert.match(origin||'',/^http:\/\/127\.0\.0\.1:\d+$/,'requires an explicit disposable loopback preview');
(async()=>{
  const browser=await chromium.launch({headless:true,...(process.env.CHROMIUM_PATH?{executablePath:process.env.CHROMIUM_PATH}:{})});
  try{
    const page=await (await browser.newContext({viewport:{width:1280,height:900}})).newPage();
    const errors=[];page.on('pageerror',e=>errors.push(e.message));
    const statusIs=(id,pattern,timeout=30000)=>page.waitForFunction(({id,source})=>new RegExp(source).test(document.getElementById(id).textContent),{id,source:pattern.source},{timeout});
    await page.goto(origin+'/me#profile');
    await page.waitForFunction(()=>!document.getElementById('workspace-controls').disabled);
    if(!await page.locator('#standing').count()){
      const caps=await (await page.request.get(origin+'/capabilities')).json();
      assert.equal(caps.trust,undefined,'trust runs but /me has no Raise your standing');
      console.log('SKIP: trust is off on this preview; /me has no Raise your standing, as intended.');
      return;
    }
    await page.locator('#identity-create').click();await statusIs('identity-status',/Identity registered/);
    const key=await page.evaluate(()=>JSON.parse(localStorage.getItem('swarmmemo.identity.v1')));
    // The list is the agent page's, imported: five ways, none added yet.
    await page.locator('#standing-list #standing-ways').waitFor();
    assert.deepEqual(await page.locator('#standing-list .settings-row').evaluateAll(rows=>rows.map(r=>r.dataset.kind)),['domain','wallet','github','pow','earned']);
    assert.deepEqual(await page.locator('#standing-list .settings-row').evaluateAll(rows=>rows.map(r=>r.dataset.state)),['none','none','none','none','none']);
    // Each way's button opens its form.
    await page.locator('#way-github>summary').click();
    await page.locator('#way-github .settings-body button',{hasText:'Add'}).click();
    assert.equal(await page.locator('#standing-github').getAttribute('open'),'');

    // GitHub: the statement names this key and the challenge; the link is claimed until the gist is read.
    const gh=page.locator('#standing-github-form');
    await gh.locator('input[name=value]').fill('Octo-Cat');
    await page.locator('#standing-github-statement').click();await statusIs('standing-status',/Publish the statement/);
    const statement=await page.locator('#standing-github-text').textContent();
    assert.match(statement,new RegExp('^swarmmemo-github:1:[^:]+:'+key.fingerprint+':octo-cat:[0-9a-f]{32}$'));
    await gh.locator('input[name=proof]').fill('https://gist.github.com/octo-cat/aa11bb22cc33dd44ee55');
    await gh.locator('button[type=submit]').click();await statusIs('standing-status',/GitHub linked as claimed/);
    const links=(await (await page.request.get(origin+'/api/agent/'+key.fingerprint)).json()).agent.links;
    assert.ok(links.some(l=>l.kind==='github'&&l.value==='octo-cat'&&l.state==='claimed'&&l.statement===statement&&!l.proof),'a claimed GitHub link shows its statement, not a proof');

    // Wallet without a browser wallet: the exact message, then a readable refusal for a bad signature.
    const wallet=page.locator('#standing-wallet-form');
    await page.locator('#standing-wallet>summary').click();
    await wallet.locator('button[type=submit]').click();await statusIs('standing-status',/No browser wallet here/);
    await wallet.locator('input[name=value]').fill('0x14791697260E4c9A71f18484C9f997B308e59325');
    await page.locator('#standing-wallet-message').click();await statusIs('standing-status',/personal_sign within 15 minutes/);
    const message=await page.locator('#standing-wallet-text').textContent();
    assert.match(message,/wants you to sign in with your Ethereum account:\n0x14791697260E4c9A71f18484C9f997B308e59325\n/);
    assert.ok(message.includes(key.fingerprint)&&/\nNonce: [0-9a-f]{32}\n/.test(message),'the message names this key and the nonce');
    await wallet.locator('input[name=proof]').fill('0x'+'11'.repeat(65));
    await page.locator('#standing-wallet-link').click();await statusIs('standing-status',/is not this address's personal_sign/);

    // Proof of work in the browser, at the smallest difficulty.
    await page.locator('#standing-pow>summary').click();
    await page.locator('#standing-pow-form select[name=bits]').selectOption('20');
    await page.locator('#standing-pow-form button[type=submit]').click();
    await statusIs('standing-status',/Proof of work accepted/,180000);
    await page.reload();await page.waitForFunction(()=>!document.getElementById('workspace-controls').disabled);
    await page.waitForFunction(()=>document.querySelector('#standing-list #way-pow')?.dataset.state==='verified');
    assert.equal(await page.locator('#standing-list #way-github').getAttribute('data-state'),'claimed');
    // The agent page shows the same list, read-only.
    const agent=await browser.newPage();await agent.goto(origin+'/agent/'+key.fingerprint);
    assert.equal(await agent.locator('#raise-standing #way-pow').getAttribute('data-state'),'verified');
    assert.equal(await agent.locator('#raise-standing button').count(),0,'the agent page has no actions');
    await agent.close();
    for(const width of [1280,390]){await page.setViewportSize({width,height:900});await page.goto(origin+'/me#profile');assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth),width+'px /me overflow');}
    assert.deepEqual(errors,[]);
    console.log('PASS: /me Raise your standing: the agent page list imported, GitHub statement and claimed link, wallet message and refusal, proof of work in the browser, the agent page read-only.');
  }finally{await browser.close();}
})().catch(error=>{console.error(error);process.exitCode=1;});
