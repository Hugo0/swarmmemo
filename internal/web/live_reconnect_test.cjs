// Explicit disposable local server only. Importing this file does not write.
// Live-stream recovery gate. One dropped stream used to leave the tab polling for good,
// because onerror closed the EventSource and nothing reopened it. The stream must come
// back, and a poll that lands late must not relabel a live stream as polling.
const assert=require('node:assert/strict');
const {chromium}=require(process.env.PLAYWRIGHT_MODULE||'playwright');
const origin=process.env.SWARMMEMO_TEST_URL;
assert.match(origin||'',/^http:\/\/127\.0\.0\.1:\d+$/);
const room='reconnect-fixture',stamp=Date.now();

(async()=>{
  const seeded=await (await fetch(origin+'/w/'+room+'/main?format=json&text='+encodeURIComponent('Reconnect fixture message.')+'&request_id=reconnect-'+stamp,{headers:{Accept:'application/json'}})).json();
  assert.ok(seeded.receipt?.id,'fixture post must return a receipt');
  const browser=await chromium.launch({headless:true,...(process.env.CHROMIUM_PATH?{executablePath:process.env.CHROMIUM_PATH}:{})});
  const errors=[];
  try{
    const page=await (await browser.newContext({viewport:{width:1280,height:800}})).newPage();
    page.on('pageerror',e=>errors.push(e.message));
    let streams=0;
    await page.route('**/api/stream**',async route=>{streams++;if(streams===1)await route.abort('failed');else await route.continue();});
    await page.goto(origin+'/r/'+room+'/main',{waitUntil:'load'});
    const live=()=>page.evaluate(()=>document.getElementById('live-status')?.dataset.live);

    await page.waitForFunction(()=>document.getElementById('live-status')?.dataset.live==='live',null,{timeout:20000});
    assert.ok(streams>=2,'the stream must be reopened after the first failure, not merely opened once');
    assert.equal((await page.locator('#live-status').textContent()).trim(),'Live updates');

    // Longer than one 15s poll interval: a poll already in flight when the stream
    // reopened must not overwrite the live label.
    await page.waitForTimeout(16500);
    assert.equal(await live(),'live','a late poll must not relabel a live stream as polling');

    assert.deepEqual(errors,[],'no page errors');
    console.log('PASS: stream reopens after a failure, the label returns to live, and stays live past a poll interval.');
  }finally{await browser.close();}
})().catch(e=>{console.error(e);process.exitCode=1;});
