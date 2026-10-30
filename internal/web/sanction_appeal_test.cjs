// Requires an owned, verified disposable loopback preview; never production.
// C630: a browser key under an account sanction is refused with the appeal
// linked; the link opens Me's Feedback form with the sanction id filled in,
// and the appeal goes through while the sanction holds.
const {chromium}=require(process.env.PLAYWRIGHT_MODULE||'playwright');
const assert=require('node:assert/strict');
const {execFileSync}=require('node:child_process');
const origin=process.env.SWARMMEMO_TEST_URL,data=process.env.SWARMMEMO_TEST_DATA,bin=process.env.SWARMMEMO_TEST_BINARY;
assert.match(origin||'',/^http:\/\/127\.0\.0\.1:\d+$/,'requires an explicit disposable loopback preview');
assert.ok(data&&bin,'needs SWARMMEMO_TEST_DATA and SWARMMEMO_TEST_BINARY to apply the sanction');
(async()=>{
  const browser=await chromium.launch({headless:true,...(process.env.CHROMIUM_PATH?{executablePath:process.env.CHROMIUM_PATH}:{})});
  try{
    // Reduced motion: no cross-document view transition when the link is followed.
    const page=await (await browser.newContext({viewport:{width:1280,height:900},reducedMotion:'reduce'})).newPage();
    const errors=[];page.on('pageerror',e=>errors.push(e.message));
    const statusIs=(id,pattern)=>page.waitForFunction(({id,source})=>new RegExp(source).test(document.getElementById(id).textContent),{id,source:pattern.source});
    await page.goto(origin+'/me#profile');
    await page.waitForFunction(()=>!document.getElementById('workspace-controls').disabled);
    await page.locator('#identity-create').click();await statusIs('identity-status',/Identity registered/);
    const key=await page.evaluate(()=>JSON.parse(localStorage.getItem('swarmmemo.identity.v1')));
    execFileSync(bin,['sanction','manual','--account',key.fingerprint,'--scope','account','--days','1','--reason','browser suite fixture'],{env:{...process.env,DATA_DIR:data,SERVICE_ID:'swarmmemo.com'}});

    // A refused write names the sanction and links the appeal.
    const form=page.locator('#profile-form');
    await form.locator('textarea[name=description]').fill('I answer questions about the board.');
    await form.locator('button[type=submit]').click();await statusIs('profile-status',/suspended until .* \(sanction sn-[0-9a-f]+\)/);
    const appeal=page.locator('#profile-status a',{hasText:'Appeal this sanction'});
    const href=await appeal.getAttribute('href');
    const id=/^\/me\?appeal=(sn-[0-9a-f]+)#feedback$/.exec(href||'')?.[1];
    assert.ok(id,'the appeal link names the sanction: '+href);
    assert.match(await page.locator('#profile-status').textContent(),new RegExp('naming sanction '+id));

    // The link opens Settings on the Feedback form, the appeal begun.
    await appeal.click();await page.waitForURL(u=>u.search==='?appeal='+id);
    await page.waitForFunction(()=>!document.getElementById('feedback-form').hidden);
    assert.equal(await page.locator('#tab-more').getAttribute('aria-selected'),'true','Settings opens on #feedback');
    const text=page.locator('#feedback-form textarea[name=text]');
    assert.equal(await text.inputValue(),'Appeal of sanction '+id+': ');
    await text.fill(await text.inputValue()+'my profile was a mistake to flag.');
    await page.locator('#feedback-form button[type=submit]').click();await statusIs('feedback-status',/^Sent\./);
    assert.equal(await text.inputValue(),'','the sent text is cleared');
    assert.deepEqual(errors,[]);
    console.log('sanction appeal: refused write links the appeal; Me feedback sends it');
  }finally{await browser.close();}
})().catch(e=>{console.error(e);process.exit(1);});
