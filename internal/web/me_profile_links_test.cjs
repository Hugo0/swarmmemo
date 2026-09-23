// Requires an owned, verified disposable loopback preview; never production.
// The /me Profile and Links panels: publish, show and remove a bio; link a
// domain (the TXT record is shown), an Ed25519 key with its proof and a URL;
// states render the one way the agent page renders them; server errors and the
// link cap surface as readable statuses; /agents shows the result.
const {chromium}=require(process.env.PLAYWRIGHT_MODULE||'playwright');
const assert=require('node:assert/strict');
const crypto=require('node:crypto');
const origin=process.env.SWARMMEMO_TEST_URL;
assert.match(origin||'',/^http:\/\/127\.0\.0\.1:\d+$/,'requires an explicit disposable loopback preview');
const shots=process.env.SWARMMEMO_SCREENSHOT_DIR;
const fields=['operation','room','page','text','kind','reply_to','to','request_id','public_key','timestamp','nonce','handle','visibility','members','target','amount','ttl','message_id','cursor','limit','query','before','reason','data','filename','media_type','attachments'];
const canonical=c=>{const o={};for(const f of fields)if(c[f]!==undefined&&c[f]!==''&&c[f]!==0&&(!Array.isArray(c[f])||c[f].length))o[f]=c[f];return Buffer.from(JSON.stringify({version:1,service:'swarmmemo.com',command:o}));};
const privateFrom=raw=>crypto.createPrivateKey({key:Buffer.concat([Buffer.from('302e020100300506032b657004220420','hex'),Buffer.from(raw,'base64url')]),format:'der',type:'pkcs8'});
(async()=>{
  const browser=await chromium.launch({headless:true,...(process.env.CHROMIUM_PATH?{executablePath:process.env.CHROMIUM_PATH}:{})});
  try{
    const context=await browser.newContext({viewport:{width:1280,height:900}}),page=await context.newPage();
    const errors=[];page.on('pageerror',e=>errors.push(e.message));
    const statusIs=(id,pattern)=>page.waitForFunction(({id,source})=>new RegExp(source).test(document.getElementById(id).textContent),{id,source:pattern.source});
    await page.goto(origin+'/me');
    await page.waitForFunction(()=>!document.getElementById('workspace-controls').disabled);
    assert.equal(await page.locator('#profile-current').isVisible(),false,'no key, no profile line');
    await page.locator('#identity-create').click();await statusIs('identity-status',/Identity registered/);
    const key=await page.evaluate(()=>JSON.parse(localStorage.getItem('swarmmemo.identity.v1')));
    await statusIs('profile-current',/No bio yet/);
    assert.equal(await page.locator('#profile-remove').isVisible(),false,'nothing to remove yet');

    // Client-side validation names the problem before signing anything.
    const form=page.locator('#profile-form');
    await form.locator('textarea[name=description]').fill('I review small Go changes and answer questions about the board.');
    await form.locator('input[name=capabilities]').fill('code-review, Bad Cap!');
    await form.locator('button[type=submit]').click();await statusIs('profile-status',/not a capability/);
    // Publish: capabilities are split, lowercased and deduplicated; lifetime is in days.
    await form.locator('input[name=capabilities]').fill('code-review, Go, go');
    await form.locator('select[name=availability]').selectOption('busy');
    await form.locator('input[name=days]').fill('3');
    await form.locator('button[type=submit]').click();await statusIs('profile-status',/Profile published/);
    await statusIs('profile-current',/Published · availability confirmed until/);
    assert.equal(await page.locator('#profile-remove').isVisible(),true);
    const published=(await (await page.request.get(origin+'/api/agent/'+key.fingerprint)).json()).agent.profile;
    assert.deepEqual(published.capabilities,['code-review','go']);assert.equal(published.availability,'busy');
    assert.ok(Math.abs(published.expires_at-published.published_at-3*86400)<5,'lifetime follows the days field');
    await page.reload();await page.waitForFunction(()=>!document.getElementById('workspace-controls').disabled);
    await statusIs('profile-current',/Published · availability confirmed until/);
    assert.equal(await form.locator('textarea[name=description]').inputValue(),published.description,'the published bio prefills the form');

    // Domain: the TXT record to create is shown live, with copy buttons.
    const links=page.locator('#link-form');
    await links.locator('select[name=kind]').selectOption('domain');
    await links.locator('input[name=value]').fill('Atlas.Example.org');
    assert.equal(await page.locator('#link-txt-name').textContent(),'_swarmmemo.atlas.example.org');
    assert.equal(await page.locator('#link-txt-value').textContent(),'swarmmemo-fingerprint='+key.fingerprint);
    assert.equal(await page.locator('#link-domain-help button.copy-button').count(),2,'name and value can be copied');
    assert.equal(await page.locator('#link-ed25519-help').isVisible(),false);
    if(shots){await page.locator('#links').screenshot({path:shots+'/ui-me-links-form-1280.png'});}
    await links.locator('button[type=submit]').click();await statusIs('link-status',/Linked as claimed.*_swarmmemo\.atlas\.example\.org/);
    await page.locator('#links-list li[data-kind="domain"]').waitFor();
    assert.match(await page.locator('#links-list li[data-kind="domain"]').textContent(),/atlas\.example\.org\s*claimed/);
    assert.equal(await page.locator('#links-list li[data-kind="domain"] .badge').count(),0,'a claim is never a badge');

    // Ed25519 with a proof over the exact statement shown.
    const other=crypto.generateKeyPairSync('ed25519'),otherKey=other.publicKey.export({format:'der',type:'spki'}).subarray(-32).toString('base64url');
    await links.locator('select[name=kind]').selectOption('ed25519');
    await links.locator('input[name=value]').fill(otherKey);
    const statement=await page.locator('#link-statement').textContent();
    assert.equal(statement,'swarmmemo-identity-link:1:swarmmemo.com:'+key.fingerprint+':'+otherKey);
    await links.locator('input[name=proof]').fill(crypto.sign(null,Buffer.from(statement),other.privateKey).toString('base64url'));
    await links.locator('button[type=submit]').click();await statusIs('link-status',/signed proof/);
    await page.locator('#links-list li[data-kind="ed25519"]').waitFor();
    assert.match(await page.locator('#links-list li[data-kind="ed25519"]').textContent(),/signed proof attached/);

    // A URL claim, then a server-side rejection rendered from its error message.
    await links.locator('select[name=kind]').selectOption('url');
    await links.locator('input[name=value]').fill('https://atlas.example.org/about');
    await links.locator('button[type=submit]').click();await statusIs('link-status',/claimed: your key's word alone/);
    await page.locator('#links-list li[data-kind="url"] a[rel="nofollow noopener ugc"]').waitFor();
    await links.locator('select[name=kind]').selectOption('domain');
    await links.locator('input[name=value]').fill('not a domain');
    await links.locator('button[type=submit]').click();await statusIs('link-status',/not a valid canonical value/);
    assert.equal(await page.locator('#link-status.error').count(),1);
    if(shots){for(const width of [1280,390]){await page.setViewportSize({width,height:900});await page.locator('#profile').scrollIntoViewIfNeeded();await page.screenshot({path:shots+'/ui-me-profile-links-'+width+'.png',fullPage:true});}await page.setViewportSize({width:1280,height:900});}

    // /me shows exactly the list the agent page renders.
    const mine=await page.locator('#links-list .identity-links').evaluate(ul=>{const c=ul.cloneNode(true);c.querySelectorAll('.link-actions').forEach(a=>a.remove());return c.outerHTML;});
    const agentPage=await browser.newPage();await agentPage.goto(origin+'/agent/'+key.fingerprint);
    assert.equal(await agentPage.locator('#elsewhere .identity-links').evaluate(ul=>ul.outerHTML),mine,'/me and the agent page render links one way');
    assert.equal(await agentPage.locator('#profile').count(),1);
    if(shots){for(const width of [1280,390]){await agentPage.setViewportSize({width,height:900});await agentPage.screenshot({path:shots+'/ui-agent-page-'+width+'.png',fullPage:true});}}
    await agentPage.close();

    // The cap: fill to 8 with signed calls, then the browser shows the server's reason.
    const signer=privateFrom(key.private_key);
    for(let i=0;i<5;i++){
      const c={operation:'identity.link',data:JSON.stringify({schema:1,kind:'url',value:'https://atlas.example.org/'+i}),request_id:crypto.randomUUID(),public_key:key.public_key,timestamp:Math.floor(Date.now()/1000),nonce:crypto.randomUUID()};
      c.signature=crypto.sign(null,canonical(c),signer).toString('base64url');
      assert.equal((await page.request.post(origin+'/v1/command',{data:c})).status(),200);
    }
    await links.locator('select[name=kind]').selectOption('url');
    await links.locator('input[name=value]').fill('https://atlas.example.org/ninth');
    await links.locator('button[type=submit]').click();await statusIs('link-status',/maximum of 8/);

    // /agents: the bio and link chips on the viewer's own row.
    await page.goto(origin+'/agents');
    const row=page.locator('#agent-'+key.fingerprint);
    assert.match(await row.locator('.peer-description').textContent(),/review small Go changes/);
    assert.equal(await row.locator('.identity-links li').count(),8);
    assert.equal(await row.locator('.domain-handle').count(),0,'an unverified domain is not a handle');
    assert.match(await row.locator('.peer-meta').textContent(),/Renewed today/);
    assert.equal(await page.locator('.sort-tabs a[aria-current]').textContent(),'Newest');
    await page.locator('.sort-tabs a',{hasText:'Recently active'}).click();await page.waitForURL(/sort=active/);
    assert.equal(await page.locator('.sort-tabs a[aria-current]').textContent(),'Recently active');
    assert.equal(await row.count(),1,'the recently active order still lists the agent');
    assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth));
    if(shots){for(const width of [1280,390]){await page.setViewportSize({width,height:900});await row.scrollIntoViewIfNeeded();await page.screenshot({path:shots+'/ui-agents-'+width+'.png'});}await page.setViewportSize({width:1280,height:900});}

    // Remove a link and the profile; the directory then says so quietly and
    // points the viewer's own row at /me.
    await page.goto(origin+'/me');await page.waitForFunction(()=>!document.getElementById('workspace-controls').disabled);
    await page.locator('#links-list li[data-kind="ed25519"]').waitFor();
    await page.locator('#links-list li[data-kind="ed25519"] button.danger').click();await statusIs('link-status',/Link removed/);
    await page.waitForFunction(()=>!document.querySelector('#links-list li[data-kind="ed25519"]'));
    await page.locator('#links-list li[data-kind="domain"] button',{hasText:'Check now'}).click();await statusIs('link-status',/Linked as claimed/);
    await page.locator('#profile-remove').click();await statusIs('profile-status',/Profile removed/);
    await statusIs('profile-current',/No bio yet/);
    assert.equal((await (await page.request.get(origin+'/api/agent/'+key.fingerprint)).json()).agent.profile,undefined);
    await page.goto(origin+'/agents');
    assert.equal(await row.locator('.no-bio').textContent(),'No bio yet.');
    assert.equal(await row.locator('a[href="/me#profile"]').count(),1,'own row links to /me#profile');
    const stranger=await browser.newPage();await stranger.goto(origin+'/agents');
    assert.equal(await stranger.locator('#agent-'+key.fingerprint+' a[href="/me#profile"]').count(),0,'other viewers get no prompt');
    await stranger.close();
    for(const width of [1280,390]){await page.setViewportSize({width,height:900});await page.goto(origin+'/me');assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth),width+'px /me overflow');}
    assert.deepEqual(errors,[]);
    console.log('PASS: /me profile publish/show/remove, domain TXT record, Ed25519 proof, URL claim, server errors and the link cap, one link rendering shared with the agent page, /agents bio, chips and own-row prompt.');
  }finally{await browser.close();}
})().catch(error=>{console.error(error);process.exitCode=1;});
