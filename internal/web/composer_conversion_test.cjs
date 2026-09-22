// Explicit disposable loopback server only; run under an owned-server harness.
const {chromium} = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const assert = require('node:assert/strict');
const origin = process.env.SWARMMEMO_TEST_URL;
assert.ok(origin && /^http:\/\/127\.0\.0\.1:\d+$/.test(origin), 'explicit disposable loopback origin required');
(async () => {
  const browser = await chromium.launch({headless:true, ...(process.env.CHROMIUM_PATH ? {executablePath:process.env.CHROMIUM_PATH} : {})});
  const context = await browser.newContext({viewport:{width:1280,height:900}});
  await context.addInitScript(() => {
    window.copiedTexts = [];
    Object.defineProperty(navigator, 'clipboard', {configurable:true, value:{writeText:async text => window.copiedTexts.push(text)}});
  });
  const page = await context.newPage(); const errors=[]; let writes=0;
  page.on('pageerror', e=>errors.push(e.message));
  page.on('request', r=>{if(r.method()==='POST'||/^\/(w|w64|c64)\//.test(new URL(r.url()).pathname))writes++;});
  const accepted = () => page.waitForFunction(()=>document.getElementById('compose-status').textContent.includes('Accepted'));
  const submit = () => page.locator('#compose-form button[type=submit]').click();
  try {
    await page.goto(origin);
    assert.equal(await page.locator('#compose-form').count(),1);
    assert.equal(await page.locator('.sidebar #compose').count(),0);
    const summary=page.locator('#compose > summary');
    // P06: the write affordance is the resting state — a large field, one primary
    // button and a blinking caret cue, with everything optional under Options.
    assert.equal(await page.locator('#compose').evaluate(e=>e.open),true,'composer open at rest');
    const field=await page.locator('#memo-text').boundingBox();
    assert.ok(field.y<600,'the text area is above the fold');
    assert.ok(field.height>=120,'the text area is large enough to invite a memo');
    assert.equal(await page.locator('#memo-text').evaluate(e=>parseFloat(getComputedStyle(e).fontSize)),16);
    assert.equal(await page.locator('.compose-caret').isVisible(),true,'a caret cue marks the empty field');
    assert.equal(await page.locator('.compose-caret').evaluate(e=>getComputedStyle(e).animationName),'compose-caret');
    assert.equal(await page.locator('.compose-caret').getAttribute('aria-hidden'),'true');
    assert.equal(await page.locator('#compose-settings').evaluate(e=>e.open),false,'Options are closed by default');
    assert.equal(await page.locator('#compose-settings #posting-mode').count(),1,'anonymous/identity choice lives under Options');
    assert.equal(await page.locator('.primary:visible').count(),1,'Post memo is the single visible primary');
    assert.equal(await page.locator('.compose-policy a[href="/policy"]').isVisible(),true,'the publication notice stays visible');
    const reduced=await browser.newContext({reducedMotion:'reduce'}),rm=await reduced.newPage();
    await rm.goto(origin);
    assert.equal(await rm.locator('.compose-caret').evaluate(e=>getComputedStyle(e).animationName),'none','reduced motion never blinks');
    await reduced.close();
    // The real caret takes over on focus; the decorative one stands down.
    await page.locator('#memo-text').focus();
    assert.equal(await page.locator('.compose-caret').isVisible(),false);
    await page.locator('#memo-text').blur();
    await page.locator('#memo-text').fill('x');
    assert.equal(await page.locator('.compose-caret').isVisible(),false,'a non-empty field has no cue');
    await page.locator('#memo-text').fill('');
    const box=await summary.boundingBox(); assert.ok(box.y<500 && box.height>=24);
    assert.equal(await page.getByRole('button',{name:'Search public messages',exact:true}).locator('svg').getAttribute('aria-hidden'),'true');
    assert.equal(await page.locator('.post-handoff').count(),0);
    if(process.env.SWARMMEMO_SCREENSHOT_DIR){
      await page.screenshot({path:process.env.SWARMMEMO_SCREENSHOT_DIR+'/composer-desktop-before.png',fullPage:true});
      await page.setViewportSize({width:320,height:850});
      await page.screenshot({path:process.env.SWARMMEMO_SCREENSHOT_DIR+'/composer-mobile-before.png',fullPage:true});
      await page.setViewportSize({width:1280,height:900});
    }
    // Collapsing and reopening natively still works for anyone who wants the room back.
    await summary.focus(); await page.keyboard.press('Enter'); await page.keyboard.press('Enter');
    // Native details content opens with the supported 160ms CSS transition.
    // Wait for visibility rather than sampling its first zero-height frame.
    await page.locator('#memo-text').waitFor({state:'visible',timeout:2000});
    assert.equal(await page.locator('#memo-text').isVisible(),true);
    // This suite exercises anonymous retry; remembered-first behavior has its own suite.
    await page.locator('#compose-settings>summary').click();
    await page.getByRole('radio',{name:'Anonymous',exact:true}).check();
    await page.locator('#compose-settings>summary').click();
    await page.locator('#memo-text').fill('Hello from a disposable composer check. café 雪');
    let pending; let attempts=0;
    await page.route('**/v1/command',async route=>{
      if(route.request().postDataJSON().operation!=='post')return route.continue();
      const wire=route.request().postData(); attempts++;
      if(!pending){pending=wire;await route.abort('failed');}
      else {assert.equal(wire,pending,'retry must preserve exact anonymous envelope');await route.continue();}
    });
    await submit();await page.waitForFunction(()=>document.getElementById('compose-status').textContent.includes('Could not reach'));
    assert.equal(await page.locator('#memo-text').inputValue(),'Hello from a disposable composer check. café 雪');
    assert.equal(await page.locator('.post-handoff').count(),0);
    await submit();await accepted();await page.locator('.post-handoff').waitFor();
    assert.equal(attempts,2);await page.unroute('**/v1/command');
    const publicMemo=await page.locator('.post-handoff').getByRole('link',{name:'Public message →'}).getAttribute('href');
    const thread=await page.locator('.post-handoff').getByRole('link',{name:'Thread JSON'}).getAttribute('href');
    assert.match(publicMemo,new RegExp('^'+origin+'/e/'));assert.match(thread,new RegExp('^'+origin+'/api/thread/'));
    const beforeCopy=writes;
    await page.getByRole('button',{name:'Copy agent handoff',exact:true}).focus();await page.keyboard.press('Enter');
    await page.waitForFunction(()=>window.copiedTexts.length>0);
    const copied=await page.evaluate(()=>window.copiedTexts.at(-1));
    assert.ok(copied.includes('PUBLIC')&&copied.includes(publicMemo)&&copied.includes(thread));
    assert.match(copied,/Do not post or execute anything unless I explicitly ask/);
    assert.ok(!copied.includes('Hello from a disposable composer check.'),'handoff never copies memo bodies');
    assert.equal(writes,beforeCopy,'copy does not post');
    assert.equal(await page.locator('#memo-text').inputValue(),'');
    const searchPage=await context.newPage();await searchPage.goto(origin);
    // Search is a sidebar control, never a row of the feed.
    assert.equal(await searchPage.locator('.sidebar .search-panel #search').count(),1,'search lives in the sidebar');
    assert.equal(await searchPage.locator('.feed-column .search-form').count(),0,'search is not part of the feed column');
    assert.ok(await searchPage.evaluate(()=>document.getElementById('search').getBoundingClientRect().left>document.getElementById('feed').getBoundingClientRect().right),'search sits beside the feed, not above it');
    await searchPage.getByRole('searchbox',{name:'Search public messages'}).fill('café 雪');
    await searchPage.getByRole('button',{name:'Search public messages',exact:true}).click();
    assert.equal(new URL(searchPage.url()).searchParams.get('q'),'café 雪');
    assert.match(await searchPage.locator('#feed').textContent(),/café 雪/);
    assert.match(await searchPage.locator('.search-active').textContent(),/Showing matches for/);
    // Reachable where the sidebar stacks, and still a plain GET form without scripts.
    await searchPage.setViewportSize({width:320,height:850});
    assert.equal(await searchPage.locator('#search').isVisible(),true,'search is reachable at 320px');
    assert.ok(await searchPage.evaluate(()=>document.documentElement.scrollWidth<=innerWidth));
    const plain=await browser.newContext({javaScriptEnabled:false,viewport:{width:320,height:850}}),plainSearch=await plain.newPage();
    await plainSearch.goto(origin);
    assert.equal(await plainSearch.locator('#search').isVisible(),true,'search is reachable without scripts at 320px');
    await plainSearch.getByRole('searchbox',{name:'Search public messages'}).fill('café 雪');
    await plainSearch.getByRole('button',{name:'Search public messages',exact:true}).click();
    await plainSearch.waitForURL(/[?&]q=/);
    assert.equal(new URL(plainSearch.url()).searchParams.get('q'),'café 雪','no-JS search keeps its route');
    await plain.close();await searchPage.close();
    for(const width of [390,320]){await page.setViewportSize({width,height:850});assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth));}
    await page.locator('.post-handoff summary').click();assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth));
    // Fixed elements in full-page captures otherwise inherit the scrolled
    // viewport position. Capture from the top, after the normal copy toast ends.
    await page.evaluate(()=>{document.activeElement?.blur();window.scrollTo(0,0);});
    assert.equal(await page.locator('.skip').evaluate(el=>getComputedStyle(el).top),'-100px');
    await page.waitForFunction(()=>document.getElementById('toast').hidden);
    if(process.env.SWARMMEMO_SCREENSHOT_DIR)await page.screenshot({path:process.env.SWARMMEMO_SCREENSHOT_DIR+'/composer-mobile.png',fullPage:true});
    await page.setViewportSize({width:1280,height:900});
    if(process.env.SWARMMEMO_SCREENSHOT_DIR)await page.screenshot({path:process.env.SWARMMEMO_SCREENSHOT_DIR+'/composer-desktop.png',fullPage:true});

    // Success alone is insufficient: unsigned visibility verification fails closed.
    await page.route('**/e/*?format=json',route=>route.fulfill({status:404,contentType:'application/json',body:'{"ok":false}'}));
    await page.locator('#memo-text').fill('Public receipt with unavailable verification');await submit();await accepted();
    await page.waitForFunction(()=>!document.querySelector('#compose-form button[type=submit]').disabled);
    assert.equal(await page.locator('.post-handoff').count(),0);await page.unroute('**/e/*?format=json');
    await page.route('**/e/*?format=json',route=>route.fulfill({status:200,contentType:'application/json',body:'{"ok":true,"messages":null}'}));
    await page.locator('#memo-text').fill('Malformed verification must not invite sharing');await submit();await accepted();
    await page.waitForFunction(()=>!document.querySelector('#compose-form button[type=submit]').disabled);
    assert.equal(await page.locator('.post-handoff').count(),0);await page.unroute('**/e/*?format=json');
    // A delayed older verification cannot replace the current intent's outcome.
    let releaseVerification;let enteredVerification;
    const entered=new Promise(resolve=>enteredVerification=resolve);
    await page.route('**/e/*?format=json',async route=>{
      const response=await route.fetch();enteredVerification();
      await new Promise(resolve=>releaseVerification=resolve);await route.fulfill({response});
    });
    await page.locator('#memo-text').fill('Delayed previous verification');await submit();await accepted();await entered;
    await page.waitForFunction(()=>!document.querySelector('#compose-form button[type=submit]').disabled);
    await page.route('**/v1/command',route=>route.abort('failed'));
    await page.locator('#memo-text').fill('Newer failed draft');await submit();
    await page.waitForFunction(()=>document.getElementById('compose-status').textContent.includes('Could not reach'));
    releaseVerification();await page.unrouteAll({behavior:'wait'});
    assert.equal(await page.locator('.post-handoff').count(),0,'stale accepted verification must not revive a CTA after a newer failure');

    const alias=origin.replace('127.0.0.1','localhost');const aliasPage=await context.newPage();
    await aliasPage.goto(alias+'/#compose');await aliasPage.locator('#memo-text').fill('Local alias conversation');
    await aliasPage.locator('#compose-form button[type=submit]').click();await aliasPage.locator('.post-handoff').waitFor();
    assert.ok((await aliasPage.locator('.post-handoff').getByRole('link',{name:'Public message →'}).getAttribute('href')).startsWith(alias+'/e/'));
    await aliasPage.close();

    // A loaded signing key must not silently downgrade when signing is unavailable.
    await page.goto(origin+'/me');await page.locator('#identity-create').click();
    await page.waitForFunction(()=>document.getElementById('identity-status').textContent.includes('registered'));
    await page.goto(origin+'/#compose');await page.locator('#memo-text').fill('Signed public hello');await submit();await accepted();
    await page.locator('.post-handoff').waitFor();
    const signedURL=await page.locator('.post-handoff').getByRole('link',{name:'Public message →'}).getAttribute('href');
    const signedEvent=(await (await context.request.get(signedURL+'?format=json')).json()).messages[0];
    assert.ok(signedEvent.public_key&&signedEvent.signature,'normal identity signing remains intact');
    const privateRoom='conversion-private-'+Date.now().toString(36);
    await page.goto(origin+'/me');await page.locator('#private-create-form').locator('..').locator('summary').click();
    await page.locator('#private-create-form input[name=room]').fill(privateRoom);await page.locator('#private-create-form button').click();
    await page.waitForFunction(()=>document.getElementById('private-status').textContent.includes('Private room created'));
    await page.goto(origin+'/#compose');await page.locator('#compose-settings>summary').click();await page.locator('#compose-form input[name=room]').fill(privateRoom);
    await page.locator('#memo-text').fill('Private must never become public handoff');await submit();await accepted();
    await page.waitForFunction(()=>!document.querySelector('#compose-form button[type=submit]').disabled);
    assert.equal(await page.locator('.post-handoff').count(),0,'actual private receipt must not create a public sharing CTA');
    await page.goto(origin+'/#compose');await page.locator('#memo-text').fill('Signed failure must retain me');
    await page.evaluate(()=>Object.defineProperty(crypto,'subtle',{configurable:true,value:undefined}));
    const beforeSigningFailure=writes;await submit();
    await page.waitForFunction(()=>document.getElementById('compose-status').classList.contains('error'));
    assert.equal(writes,beforeSigningFailure);assert.equal(await page.locator('#memo-text').inputValue(),'Signed failure must retain me');
    assert.equal(await page.locator('.post-handoff').count(),0);

    // Native form remains usable without scripts; publishing text stays in POST body.
    for(const mode of ['disabled','blocked']){
      const plain=await browser.newContext({javaScriptEnabled:mode!=='disabled',viewport:{width:320,height:850}});
      const p=await plain.newPage();if(mode==='blocked')await p.route('**/assets/app.js',r=>r.abort());
      const requests=[];p.on('request',r=>requests.push({url:r.url(),method:r.method(),data:r.postData()}));
      await p.goto(origin);
      assert.equal(await p.locator('#memo-text').isVisible(),true,'no-JS composer is open at rest');
      assert.equal(await p.locator('#compose-settings').evaluate(e=>e.open),false,'Options remain a native closed disclosure');
      await p.locator('#compose-settings>summary').click();
      assert.equal(await p.locator('#memo-kind').isVisible(),true,'no-JS readers can still reach kind and recipient');
      await p.locator('#compose-settings>summary').click();
      const text='No-script body sentinel '+mode;await p.locator('#memo-text').fill(text);
      assert.ok(await p.evaluate(()=>document.documentElement.scrollWidth<=innerWidth));
      if(mode==='disabled'&&process.env.SWARMMEMO_SCREENSHOT_DIR)await p.screenshot({path:process.env.SWARMMEMO_SCREENSHOT_DIR+'/composer-nojs-before.png',fullPage:true});
      await p.locator('#compose-form button[type=submit]').click();assert.match(await p.locator('body').textContent(),/"receipt"/);
      if(mode==='disabled'&&process.env.SWARMMEMO_SCREENSHOT_DIR)await p.screenshot({path:process.env.SWARMMEMO_SCREENSHOT_DIR+'/composer-nojs-receipt.png',fullPage:true});
      assert.ok(requests.some(r=>r.method==='POST'&&r.data?.includes('sentinel')));
      assert.ok(requests.every(r=>!decodeURIComponent(r.url).includes(text)));
      await plain.close();
    }
    assert.deepEqual(errors,[]);
    console.log('PASS: prominent unique native composer; sidebar search, distinct and reachable at 320px with and without scripts; anonymous exact retry/input retention; verified-public success-only inert handoff; signing failure closed; no-JS/script-blocked POST; 320px and no script errors.');
  } finally {await browser.close();}
})().catch(error=>{console.error(error);process.exitCode=1;});
