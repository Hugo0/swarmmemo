// Run only through an owned, disposable loopback preview harness.
const {chromium}=require(process.env.PLAYWRIGHT_MODULE||'playwright');
const assert=require('node:assert/strict');
const origin=process.env.SWARMMEMO_TEST_URL;
assert.match(origin||'',/^http:\/\/127\.0\.0\.1:\d+$/);
(async()=>{
  const browser=await chromium.launch({headless:true,...(process.env.CHROMIUM_PATH?{executablePath:process.env.CHROMIUM_PATH}:{})});
  const context=await browser.newContext({viewport:{width:320,height:900}}),page=await context.newPage();
  const errors=[],writes=[];page.on('pageerror',e=>errors.push(e.message));
  page.on('request',r=>{if(r.method()==='POST')writes.push(r);});
  const settings=()=>page.locator('#compose-settings');
  const openOptions=async()=>{if(!await settings().evaluate(e=>e.open))await page.locator('#compose-settings>summary').click();};
  const submit=()=>page.locator('#compose-form button[type=submit]').click();
  const status=text=>page.waitForFunction(text=>document.getElementById('compose-status').textContent.includes(text),text);
  const screenshot=async name=>{
    await page.waitForFunction(()=>document.getElementById('toast').hidden);
    await page.evaluate(()=>{document.activeElement?.blur();window.scrollTo(0,0);});
    assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth));
    assert.equal(await page.locator('.skip').evaluate(e=>getComputedStyle(e).top),'-100px');
    if(process.env.SWARMMEMO_SCREENSHOT_DIR)await page.screenshot({path:process.env.SWARMMEMO_SCREENSHOT_DIR+'/'+name+'.png',fullPage:true});
  };
  try{
    await page.goto(origin+'/#compose');await page.locator('#posting-mode').waitFor({state:'attached'});
    assert.equal(await settings().evaluate(e=>e.open),false);
    // P06: the resting composer is a large field, one primary button and a caret cue.
    assert.equal(await page.locator('#compose').evaluate(e=>e.open),true);
    assert.equal(await page.locator('#memo-text').isVisible(),true);
    assert.ok(await page.locator('#memo-text').evaluate(e=>e.getBoundingClientRect().height>=120));
    assert.equal(await page.locator('.compose-caret').isVisible(),true);
    // Everything optional, the identity choice included, is one closed disclosure away.
    assert.equal(await page.locator('#compose-settings #posting-mode').count(),1);
    assert.equal(await page.locator('#posting-mode').isVisible(),false);
    assert.equal(await page.locator('#memo-to').isVisible(),false);
    await openOptions();assert.equal(await page.locator('#posting-mode').isVisible(),true);assert.equal(await page.locator('#memo-to').isVisible(),true);
    await page.locator('#compose-settings>summary').click();assert.equal(await settings().evaluate(e=>e.open),false);
    // Change is a two-way door. The summary's toggle event is async, so wait for
    // aria-expanded to follow it before asserting. Open, close, open again from Change, and the Options
    // summary still toggles on its own without the two fighting over the state.
    const change=page.locator('#compose-change');
    assert.equal(await change.getAttribute('aria-expanded'),'false');
    await change.click();assert.equal(await settings().evaluate(e=>e.open),true,'Change opens Options');assert.equal(await change.getAttribute('aria-expanded'),'true');
    await change.click();assert.equal(await settings().evaluate(e=>e.open),false,'Change closes Options again');assert.equal(await change.getAttribute('aria-expanded'),'false');
    await change.click();assert.equal(await settings().evaluate(e=>e.open),true,'and opens them a second time');
    await page.locator('#compose-settings>summary').click();assert.equal(await settings().evaluate(e=>e.open),false,'the summary closes what Change opened');await page.waitForFunction(()=>document.querySelector('#compose-change').getAttribute('aria-expanded')==='false',null,{timeout:2000}).catch(()=>{});assert.equal(await change.getAttribute('aria-expanded'),'false','Change follows the summary');
    await page.locator('#compose-settings>summary').click();await page.waitForFunction(()=>document.querySelector('#compose-change').getAttribute('aria-expanded')==='true',null,{timeout:2000}).catch(()=>{});assert.equal(await change.getAttribute('aria-expanded'),'true','Change follows the summary when it opens');
    await change.click();assert.equal(await settings().evaluate(e=>e.open),false,'Change closes what the summary opened');
    // Three labelled groups, a plain-language line under every field, and the
    // identity choice first. Icons are decorative and always sit beside words.
    await openOptions();
    assert.deepEqual(await page.locator('#compose-settings .option-group-title').allTextContents(),['Who is posting','Where it goes','More']);
    assert.deepEqual(await page.locator('#compose-settings [role=group]').evaluateAll(gs=>gs.map(g=>document.getElementById(g.getAttribute('aria-labelledby')).textContent)),['Who is posting','Where it goes','More']);
    for(const [field,help] of [['#memo-room',/Rooms are topics/],['#memo-page',/named stream inside the room/],['#memo-to',/It stays public: this is not a private message/],['#memo-kind',/Leave it as Note/],['#memo-files',/1 MiB each\. Files expire after 30 days/]]){
      const id=await page.locator(field).getAttribute('aria-describedby');assert.ok(id,field+' has help');
      assert.match(await page.locator('#'+id).textContent(),help);assert.equal(await page.locator('#'+id).isVisible(),true,field+' help is shown inline');
    }
    assert.equal(await page.getByLabel('Room',{exact:true}).count(),1);assert.equal(await page.getByLabel('Page',{exact:true}).count(),1);
    assert.deepEqual(await page.locator('#memo-kind option').evaluateAll(os=>os.map(o=>o.value)),['note','request','offer','result','checkpoint'],'kind never offers a value the server rejects');
    const icons=page.locator('#compose-settings svg');assert.ok(await icons.count()>=5);
    assert.deepEqual([...new Set(await icons.evaluateAll(es=>es.map(e=>e.getAttribute('aria-hidden')+'|'+e.getAttribute('focusable')+'|'+e.getAttribute('stroke'))))],['true|false|currentColor']);
    assert.ok(await icons.evaluateAll(es=>es.every(e=>(e.parentElement.textContent||'').trim().length>0)),'no icon without a text label');
    assert.equal(await page.locator('#posting-mode').evaluate(e=>e.closest('.option-group').querySelector('.option-group-title').textContent),'Who is posting');
    // 320px: Room and Page stack; desktop: they are a real pair on one row.
    const pair=()=>page.evaluate(()=>{const [a,b]=['memo-room','memo-page'].map(id=>document.getElementById(id).getBoundingClientRect());return {sameRow:Math.abs(a.top-b.top)<2,aligned:Math.abs(a.height-b.height)<1};});
    assert.equal((await pair()).sameRow,false,'Room and Page stack at 320px');
    await page.setViewportSize({width:1280,height:900});const wide=await pair();assert.equal(wide.sameRow,true,'Room and Page pair up on desktop');assert.equal(wide.aligned,true);await page.setViewportSize({width:320,height:900});
    await page.locator('#compose-settings>summary').click();
    assert.equal(await page.locator('#compose-form').count(),1);
    assert.match(await page.locator('#compose-destination').textContent(),/#lobby \/main/);
    assert.equal(await page.locator('nav[aria-label="Main navigation"] a[href="/work"]').count(),0);
    // Deliberate reversal of the earlier footer-discoverability decision: work is
    // a specialised layer, not something every page advertises. The routes stay.
    assert.equal(await page.getByRole('link',{name:'Advanced work',exact:true}).count(),0);
    assert.equal((await (await context.request.get(origin+'/work')).status()),200,'/work must stay live');
    assert.equal(writes.length,0);assert.equal(await page.evaluate(()=>localStorage.getItem('swarmmemo.identity.v1')),null);
    for(const selector of ['#memo-text','#compose-form input[name=room]','#memo-to','#memo-kind'])assert.ok(await page.locator(selector).evaluate(e=>parseFloat(getComputedStyle(e).fontSize)>=16));
    for(const selector of ['#compose-destination','#byte-counter','.compose-policy','#posting-mode legend'])assert.ok(await page.locator(selector).evaluate(e=>parseFloat(getComputedStyle(e).fontSize)>=12));
    await screenshot('options-mobile-default');await page.setViewportSize({width:1280,height:900});await screenshot('options-desktop-default');await page.setViewportSize({width:320,height:900});
    await page.locator('#compose-settings>summary').focus();await page.keyboard.press('Enter');assert.equal(await settings().evaluate(e=>e.open),true);
    await page.locator('#memo-to').fill('invalid');await page.locator('#memo-text').fill('Validation retains this draft');
    await page.locator('#compose-settings>summary').click();const beforeInvalid=writes.length;await submit();
    assert.equal(await settings().evaluate(e=>e.open),true);assert.equal(await page.locator('#memo-to').evaluate(e=>e===document.activeElement),true);assert.equal(writes.length,beforeInvalid);
    const recipient='c'.repeat(64),room='options-'+Date.now().toString(36);
    await page.locator('#memo-to').fill(recipient);await page.locator('#compose-form input[name=room]').fill(room);await page.locator('#compose-form input[name=page]').fill('chat');await page.locator('#memo-kind').selectOption('request');
    await page.locator('#compose-settings>summary').click();
    assert.match(await page.locator('#compose-destination').textContent(),new RegExp('#'+room+' /chat'));
    assert.ok((await page.locator('#compose-context').textContent()).includes(recipient));assert.match(await page.locator('#compose-context').textContent(),/Kind: request/);
    await openOptions();await page.getByRole('radio',{name:'Anonymous',exact:true}).check();await page.locator('#compose-settings>summary').click();await page.locator('#memo-text').fill('Options preserve selected signed-wire fields');
    let original='',attempts=0;
    await page.route('**/v1/command',async route=>{
      const c=route.request().postDataJSON();if(c.operation!=='post')return route.continue();attempts++;
      if(!original){original=route.request().postData();assert.equal(c.room,room);assert.equal(c.page,'chat');assert.equal(c.to,recipient);assert.equal(c.kind,'request');const accepted=await route.fetch();assert.equal(accepted.status(),200);return route.abort('failed');}
      assert.equal(route.request().postData(),original);return route.continue();
    });
    await submit();await status('Could not reach');await openOptions();await page.locator('#memo-kind').selectOption('note');await submit();await status('previous message is unresolved');assert.equal(attempts,1);
    await page.locator('#memo-kind').selectOption('request');await page.locator('#compose-settings>summary').click();await submit();await status('Accepted');assert.equal(attempts,2);await page.unroute('**/v1/command');
    const result=await (await context.request.get(origin+'/api/messages?room='+room+'&page=chat')).json();assert.equal(result.messages.length,1);const event=result.messages[0];assert.equal(event.to,recipient);assert.equal(event.kind,'request');
    await page.goto(origin+'/r/'+room+'/chat?reply='+event.id+'&to='+recipient+'#compose');
    assert.equal(await settings().evaluate(e=>e.open),true,'nondefault room/reply context starts expanded');
    await page.locator('#compose-settings>summary').click();assert.equal(await page.locator('#reply-preview').isVisible(),true);assert.ok((await page.locator('#compose-context').textContent()).includes(recipient));
    await page.locator('#e-'+event.id+' .reply-button').click();assert.match(await page.locator('#compose-destination').textContent(),new RegExp('#'+room+' /chat'));assert.equal(await page.locator('#reply-preview').isVisible(),true);
    await page.locator('#clear-reply').click();assert.equal(await page.locator('#reply-preview').isVisible(),false);
    await openOptions();await screenshot('options-mobile-expanded');
    await page.emulateMedia({forcedColors:'active'});
    assert.ok(await page.locator('#compose-settings svg').first().evaluate(e=>{const c=getComputedStyle(e);return c.display!=='none'&&c.visibility!=='hidden'&&e.getBoundingClientRect().width>0&&getComputedStyle(e).stroke!=='none';}),'icons survive forced colours');
    await page.emulateMedia({forcedColors:'none'});
    // Test this patch's controls at 200% text size, not CSS page zoom that
    // shrinks the entire site's 320px layout to 160px without media reflow.
    await page.evaluate(()=>{
      const sizes=[...document.querySelectorAll('#compose-form, #compose-form *')].map(e=>[e,parseFloat(getComputedStyle(e).fontSize)]);
      for(const [element,size] of sizes)element.style.fontSize=(size*2)+'px';
    });
    assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth),'200% composer text must wrap at320px');
    await screenshot('options-mobile-text200');
    await page.goto(origin+'/inbox/'+recipient+'#compose');assert.equal(await settings().evaluate(e=>e.open),true);assert.ok((await page.locator('#compose-context').textContent()).includes(recipient));
    await page.goto(origin+'/#compose');await page.locator('#memo-text').fill('Remembered identity with compact composer');await submit();await status('Accepted');
    await page.reload();await openOptions();await page.locator('#compose-form input[type=file]').setInputFiles({name:'visible-choice.txt',mimeType:'text/plain',buffer:Buffer.from('a')});await page.locator('#compose-settings>summary').click();assert.match(await page.locator('#compose-context').textContent(),/1 file selected/);
    const beforeFiles=writes.length;await page.locator('#compose-form input[type=file]').setInputFiles([]);assert.equal(await page.locator('#compose-context').isVisible(),false);assert.equal(writes.length,beforeFiles,'selecting files never uploads');
    // Retired addresses are gone, not redirected: the browser stays on the old
    // URL and is told once, in plain words, where the one remaining name is.
    const retired=await page.goto(origin+'/peers');assert.equal(retired.status(),410,'the retired peer directory must answer 410');assert.equal(new URL(page.url()).pathname,'/peers','a retired address must not redirect');assert.equal(await page.locator('a[href="/agents"]').count()>0,true,'the 410 page must name /agents');assert.equal(await page.locator('a[href="/migration"]').count()>0,true,'the 410 page must link the migration map');
    await page.goto(origin+'/agents');assert.equal(await page.locator('form.peer-search').getAttribute('action'),'/agents');assert.equal(await page.locator('nav[aria-label="Main navigation"] a[href="/peers"]').count(),0);assert.equal(await page.locator('nav[aria-label="Main navigation"] a[href="/identities"]').count(),0);
    for(const mode of ['disabled','blocked']){
      const plain=await browser.newContext({javaScriptEnabled:mode!=='disabled',viewport:{width:320,height:900}}),p=await plain.newPage(),requests=[];
      if(mode==='blocked')await p.route('**/assets/app.js',route=>route.abort());p.on('request',r=>requests.push(r));
      await p.goto(origin+'/?to='+recipient);
      // Without scripts the composer is still open at rest and Options is still a
      // native closed disclosure; the addressed context stays visible outside it.
      assert.equal(await p.locator('#memo-text').isVisible(),true);
      assert.equal(await p.locator('#compose-settings').evaluate(e=>e.open),false);
      assert.ok((await p.locator('#compose-context').textContent()).includes(recipient));
      await p.locator('#compose-settings>summary').click();assert.equal(await p.locator('#compose-settings').evaluate(e=>e.open),true);
      assert.equal(await p.locator('#posting-mode').isVisible(),false);assert.equal(await p.locator('input[name=room]').getAttribute('readonly'),'');
      assert.equal(await p.locator('#memo-to').inputValue(),recipient);await p.locator('#memo-kind').selectOption('request');await p.locator('#memo-text').fill('Native options '+mode+' body sentinel');
      assert.ok(await p.evaluate(()=>document.documentElement.scrollWidth<=innerWidth));
      if(process.env.SWARMMEMO_SCREENSHOT_DIR&&mode==='disabled'){await p.evaluate(()=>{document.activeElement?.blur();window.scrollTo(0,0);});await p.screenshot({path:process.env.SWARMMEMO_SCREENSHOT_DIR+'/options-mobile-nojs.png',fullPage:true});}
      await p.locator('#compose-form button[type=submit]').click();const receipt=JSON.parse(await p.locator('body').textContent());assert.equal(receipt.ok,true);
      const saved=(await (await plain.request.get(origin+'/e/'+receipt.receipt.id+'?format=json')).json()).messages[0];assert.equal(saved.room,'lobby');assert.equal(saved.page,'main');assert.equal(saved.to,recipient);assert.equal(saved.kind,'request');assert.equal(saved.public_key,undefined);
      assert.ok(requests.some(r=>r.method()==='POST'));assert.ok(requests.every(r=>!decodeURIComponent(r.url()).includes('body sentinel')));await plain.close();
    }
    assert.deepEqual(errors,[]);console.log('PASS: compact defaults, visible addressed/reply/routing context, native Options validation, exact pending retry, file selection without upload, advanced navigation, 320px/zoom/keyboard and no-JS/script-blocked posting.');
  }finally{await browser.close();}
})().catch(error=>{console.error(error);process.exitCode=1;});
