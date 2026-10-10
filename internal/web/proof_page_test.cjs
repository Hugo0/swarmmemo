// Explicit disposable local server only. Importing this file does not write.
// The log, told for people: /e/ID/proof renders its three steps without
// scripts and fits a phone; a memo's ⓘ details open from the keyboard, sit
// over the page without moving it or widening it, read the log status once,
// and close with Escape back to their button.
const assert=require('node:assert/strict');
const {chromium}=require(process.env.PLAYWRIGHT_MODULE||'playwright');
const origin=process.env.SWARMMEMO_TEST_URL;
assert.match(origin||'',/^http:\/\/127\.0\.0\.1:\d+$/);
const room='proof-fixture';
const stamp=Date.now();

async function seed(){
  const url=origin+'/w/'+room+'/main?format=json&text='+encodeURIComponent('Proof page fixture '+stamp+'\n\nA second line.')+'&request_id=proof-'+stamp;
  const result=await (await fetch(url,{headers:{Accept:'application/json'}})).json();
  assert.ok(result.receipt?.id,'fixture post must return a receipt');
  return result.receipt.id;
}

const overflow=page=>page.evaluate(()=>document.scrollingElement.scrollWidth-document.scrollingElement.clientWidth);

async function main(){
  const id=await seed();
  const browser=await chromium.launch({headless:true,...(process.env.CHROMIUM_PATH?{executablePath:process.env.CHROMIUM_PATH}:{})});
  const errors=[];
  try{
    // 1. The proof page, without JavaScript: three steps and the ways to check.
    const plain=await browser.newContext({viewport:{width:375,height:740},javaScriptEnabled:false});
    let page=await plain.newPage();
    const res=await page.goto(origin+'/e/'+id+'/proof',{waitUntil:'load'});
    assert.equal(res.status(),200,'the proof page answers 200 before the post is in a checkpoint');
    assert.match(await page.locator('h1').textContent(),/^On the record: Proof page fixture/);
    assert.equal(await page.locator('.proof-step').count(),3,'three steps');
    assert.equal(await page.locator('#step-posted.is-done').count(),1,'posting is done');
    assert.equal(await page.locator('meta[name=robots][content=noindex]').count(),1,'the page is noindex');
    assert.equal(await page.locator(`a[href="/api/log/proof?message=${id}"]`).count(),1,'the JSON proof is linked');
    assert.ok((await page.locator('.proof-verify').textContent()).includes('python3 verify_log.py message '+id),'the verifier command is shown');
    assert.ok(await overflow(page)<=0,'the proof page fits a 375px phone');
    assert.equal((await page.goto(origin+'/e/'+'0'.repeat(32)+'/proof')).status(),404,'an unknown message is a 404');
    await plain.close();

    // 2. The memo details link the page (once; the byline does not repeat it), and open from the keyboard.
    const context=await browser.newContext({viewport:{width:375,height:740}});
    page=await context.newPage();
    page.on('pageerror',e=>errors.push(e.message));
    let proofRequests=0;
    page.on('request',r=>{if(r.url().includes('/api/log/proof'))proofRequests++;});
    await page.goto(origin+'/e/'+id,{waitUntil:'load'});
    assert.equal(await page.locator('#e-'+id+' .memo-info-log a').getAttribute('href'),'/e/'+id+'/proof','the details link the human proof page');
    assert.equal(await page.locator('a[href="/e/'+id+'/proof"]').count(),1,'the post page links its proof once');
    await page.goto(origin+'/r/'+room+'/main',{waitUntil:'load'});
    const memo=page.locator('#e-'+id);
    const info=memo.locator('details.memo-info');
    await info.waitFor({state:'attached',timeout:5000});
    assert.equal(await info.evaluate(d=>d.open),false,'details start collapsed');
    assert.ok(await info.locator('summary').evaluate(s=>{const a=s.getBoundingClientRect(),b=s.querySelector('svg').getBoundingClientRect();return Math.abs(a.left+a.right-b.left-b.right)<1.2&&Math.abs(a.top+a.bottom-b.top-b.bottom)<1.2;}),'the details glyph is centred in its button');
    assert.equal(proofRequests,0,'nothing is fetched per memo on page load');
    await info.locator('summary').focus();
    const before=await memo.evaluate(el=>{const b=el.getBoundingClientRect();return [b.top,b.height];});
    await page.keyboard.press('Enter');
    assert.equal(await info.evaluate(d=>d.open),true,'Enter opens the details');
    const body=info.locator('.tip-body');
    assert.ok(await body.isVisible(),'the details are visible');
    const after=await memo.evaluate(el=>{const b=el.getBoundingClientRect();return [b.top,b.height];});
    assert.ok(Math.abs(after[0]-before[0])<1&&Math.abs(after[1]-before[1])<1,`opening the details moved the memo (${before} -> ${after})`);
    const box=await body.boundingBox();
    assert.ok(box.x>=0&&box.x+box.width<=375,`the details stay inside the window (${box.x}..${box.x+box.width})`);
    assert.ok(await overflow(page)<=0,'opening the details does not widen the page');
    const text=await body.textContent();
    for(const want of [id,'#'+room+'/main','Signed','no','Text SHA-256','Public log']) assert.ok(text.includes(want),'the details show '+want);
    await page.waitForFunction(i=>/entry \d+|pending/.test(document.querySelector('#e-'+i+' .memo-info-log').textContent),id,{timeout:5000});
    assert.equal(await memo.locator('.memo-info-log a').getAttribute('href'),'/e/'+id+'/proof','the log status links the proof page');
    await page.keyboard.press('Escape');
    assert.equal(await info.evaluate(d=>d.open),false,'Escape closes the details');
    assert.equal(await page.evaluate(()=>document.activeElement?.closest('details')?.className),'tip memo-info','focus returns to the details button');
    await page.keyboard.press('Enter');
    await page.waitForTimeout(150);
    assert.equal(proofRequests,1,'the log status is read once per memo');
    assert.deepEqual(errors,[],'no script errors');
    await context.close();
    console.log('PASS: /e/ID/proof renders its three steps without scripts and fits 375px; the memo details open by keyboard without moving or widening the page, read the log once, and close with Escape.');
  } finally { await browser.close(); }
}
main().catch(error=>{console.error(error);process.exit(1);});
