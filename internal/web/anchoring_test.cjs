// Explicit disposable local server only. Importing this file does not write.
// Scroll anchoring gate: no mutation above the reader's anchor may move what they
// are reading. Every assertion measures a real getBoundingClientRect().top across
// the mutation, in pixels, rather than trusting the browser to do the right thing.
const assert=require('node:assert/strict');
const {chromium}=require(process.env.PLAYWRIGHT_MODULE||'playwright');
const origin=process.env.SWARMMEMO_TEST_URL;
assert.match(origin||'',/^http:\/\/127\.0\.0\.1:\d+$/);
const room='anchor-fixture';
const body=n=>('Anchoring fixture message '+n+'. ')+('This paragraph is deliberately long so that the listing clamps it to four lines and offers an expansion control, which is the exact mutation under test. café 雪 ').repeat(6);
const stamp=Date.now();

async function seed(){
  const ids=[];
  for(let i=1;i<=12;i++){
    const url=origin+'/w/'+room+'/main?format=json&text='+encodeURIComponent(body(i))+'&request_id=anchor-'+stamp+'-'+i;
    const result=await (await fetch(url,{headers:{Accept:'application/json'}})).json();
    assert.ok(result.receipt?.id,'fixture post must return a receipt');
    ids.push(result.receipt.id);
  }
  return ids;
}

// Put the target message's top edge above the viewport top, which is the case the
// browser's own anchoring gets wrong, then report the top edge before and after.
const topOf=(page,id)=>page.evaluate(i=>document.getElementById('e-'+i).getBoundingClientRect().top,id);

async function main(){
  const ids=await seed();
  const browser=await chromium.launch({headless:true,...(process.env.CHROMIUM_PATH?{executablePath:process.env.CHROMIUM_PATH}:{})});
  const errors=[];
  try{
    const context=await browser.newContext({viewport:{width:1280,height:800}});
    const page=await context.newPage();
    page.on('pageerror',e=>errors.push(e.message));
    await page.goto(origin+'/r/'+room+'/main',{waitUntil:'load'});
    await page.locator('#e-'+ids[6]+' .memo-preview-toggle').waitFor({state:'attached',timeout:5000});

    // Native scroll anchoring must be off over the board; the explicit correction owns it.
    assert.equal(await page.evaluate(()=>getComputedStyle(document.getElementById('feed')).overflowAnchor),'none','#feed opts out of native scroll anchoring');

    const target=ids[6];
    const place=async off=>{await page.evaluate(([i,o])=>{const el=document.getElementById('e-'+i);const s=document.scrollingElement;s.scrollTop=s.scrollTop+el.getBoundingClientRect().top-o;},[target,off]);await page.waitForTimeout(50);};

    // 1. Show more, with the message's top edge 60px above the viewport top.
    await place(-60);
    const before=await topOf(page,target);
    await page.locator('#e-'+target+' .memo-preview-toggle').click();
    await page.waitForTimeout(120);
    const after=await topOf(page,target);
    assert.ok(Math.abs(after-before)<2,`expanding moved the message ${after-before}px (top ${before} -> ${after})`);
    assert.equal(await page.locator('#e-'+target+' .memo-preview-toggle').textContent(),'Show less');
    // Content below it really did move down: the next message is further away.
    const gap=await page.evaluate(i=>{const el=document.getElementById('e-'+i);return el.nextElementSibling.getBoundingClientRect().top-el.getBoundingClientRect().top;},target);
    assert.ok(gap>200,'the expanded body pushes the following message down');

    // 2. Collapse holds the same edge.
    const beforeCollapse=await topOf(page,target);
    await page.locator('#e-'+target+' .memo-preview-toggle').click();
    await page.waitForTimeout(120);
    const afterCollapse=await topOf(page,target);
    assert.ok(Math.abs(afterCollapse-beforeCollapse)<2,`collapsing moved the message ${afterCollapse-beforeCollapse}px`);

    // 3. The inline reply composer opening under a message, and cancelling it.
    await place(-40);
    const beforeReply=await topOf(page,target);
    await page.locator('#e-'+target+' .reply-button').click();
    await page.waitForTimeout(400);
    const afterReply=await topOf(page,target);
    assert.ok(Math.abs(afterReply-beforeReply)<2,`opening the inline composer moved the message ${afterReply-beforeReply}px`);
    assert.equal(await page.evaluate(i=>document.getElementById('compose').closest('.memo').id==='e-'+i,target),true,'the composer relocated under the message');
    // Relocated inline the summary is the reply's label. Closed, it must not become
    // the board-wide black call to action sitting inside a message.
    assert.match(await page.locator('#compose > summary').textContent(),/^Reply to /);
    await page.evaluate(()=>{document.getElementById('compose').open=false;});
    const inlineLabel=await page.evaluate(()=>{const c=getComputedStyle(document.querySelector('#compose>summary'));return {bg:c.backgroundColor,color:c.color};});
    assert.equal(inlineLabel.bg,'rgb(255, 255, 255)','a closed inline reply label is not a filled primary');
    assert.equal(inlineLabel.color,'rgb(23, 23, 23)');
    await page.evaluate(()=>{document.getElementById('compose').open=true;});
    await page.waitForTimeout(250);
    // A typed draft survives cancelling; only the composer moves home.
    await page.locator('#memo-text').fill('Anchored draft that must survive');
    await place(-40);
    const beforeCancel=await topOf(page,target);
    await page.locator('#clear-reply').click();
    await page.waitForTimeout(400);
    const afterCancel=await topOf(page,target);
    assert.ok(Math.abs(afterCancel-beforeCancel)<2,`cancelling the reply moved the message ${afterCancel-beforeCancel}px`);
    assert.equal(await page.locator('#memo-text').inputValue(),'Anchored draft that must survive');
    await page.locator('#memo-text').fill('');

    // 4. A live message prepended to the top of the feed while the reader is below it.
    await place(-40);
    await fetch(origin+'/w/'+room+'/main?format=json&text='+encodeURIComponent('A live arrival while the reader is further down the feed.')+'&request_id=anchor-live-'+stamp,{headers:{Accept:'application/json'}});
    await page.locator('.new-messages').waitFor({state:'attached',timeout:20000});
    await page.waitForFunction(()=>!document.querySelector('.new-messages').hidden,null,{timeout:20000});
    await place(-40);
    const beforeArrival=await topOf(page,target);
    const countBefore=await page.locator('#feed .memo').count();
    await page.evaluate(()=>document.querySelector('.new-messages').click());
    await page.waitForTimeout(150);
    const afterArrival=await topOf(page,target);
    assert.equal(await page.locator('#feed .memo').count(),countBefore+1,'the arrival really was inserted');
    assert.ok(Math.abs(afterArrival-beforeArrival)<2,`a message arriving above the reader moved them ${afterArrival-beforeArrival}px`);

    // 5. A correction replacing a message above the reader, delivered through the
    // same refresh path the board uses, with a much taller body.
    const above=ids[2];
    await page.route('**/e/'+above+'?format=json',async route=>{
      const response=await route.fetch();
      const payload=await response.json();
      for(const message of payload.messages||[]) if(message.id===above) message.text=('A correction with a far longer body than the message it replaces. ').repeat(40);
      await route.fulfill({status:200,contentType:'application/json',body:JSON.stringify(payload)});
    });
    await place(-40);
    const beforeCorrection=await topOf(page,target);
    await page.evaluate(()=>document.dispatchEvent(new Event('visibilitychange')));
    await page.waitForFunction(i=>document.getElementById('e-'+i)?.textContent.includes('A correction with a far longer body'),above,{timeout:15000});
    await page.waitForTimeout(150);
    const afterCorrection=await topOf(page,target);
    assert.ok(Math.abs(afterCorrection-beforeCorrection)<3,`a correction above the reader moved them ${afterCorrection-beforeCorrection}px`);
    await page.unroute('**/e/'+above+'?format=json');

    assert.deepEqual(errors,[],'no script errors');
    await context.close();
    console.log('PASS: expand/collapse, inline composer open/cancel, live arrival and correction all hold the reader\'s anchor to within 2px; native overflow-anchor disabled over the board.');
  } finally { await browser.close(); }
}
main().catch(error=>{console.error(error);process.exit(1);});
