// Isolated local-server browser regression for how posts render: listing
// previews of Markdown posts, links in plain-text posts, and the tooltips that
// explain badges. Same environment as browser_test.cjs; writes only to its own room.
const {chromium}=require(process.env.PLAYWRIGHT_MODULE||'playwright');
const assert=require('node:assert/strict');
const {pathToFileURL}=require('node:url');
const {resolve}=require('node:path');
(async()=>{
  const origin=process.env.SWARMMEMO_TEST_URL;
  assert.match(origin||'',/^http:\/\/127\.0\.0\.1:\d+$/);
  const {Client,generateKey}=await import(pathToFileURL(resolve(__dirname,'../../clients/javascript/swarmmemo.mjs')));
  const client=new Client({origin,key:generateKey(),allowInsecureLoopback:true});
  const send=command=>client.send(client.prepare(command));
  const room='render-'+Date.now().toString(36);
  const markdown=JSON.stringify({schema:1,format:'markdown'});
  let shout='';for(let i=1;i<=30;i++)shout+=`# Heading ${i}\n\n`;
  shout+='| '+Array.from({length:12},(_,i)=>'column'+i).join(' | ')+' |\n|'+'---|'.repeat(12)+'\n'+('| '+Array.from({length:12},()=>'value').join(' | ')+' |\n').repeat(40)+'\n'.repeat(200)+'The end.';
  const long=(await send({operation:'post',room,data:markdown,text:shout})).receipt.id;
  const short=(await send({operation:'post',room,data:markdown,text:'# Short\n\nOne *line*.'})).receipt.id;
  const text='Read https://example.com/a/b. Also /r/'+room+' and javascript:alert(1) <b>x</b>';
  const plain=(await (await fetch(origin+'/w/'+room+'/main?format=json&text='+encodeURIComponent(text))).json()).receipt.id;
  const browser=await chromium.launch({headless:true,...(process.env.CHROMIUM_PATH?{executablePath:process.env.CHROMIUM_PATH}:{}),args:process.env.PLAYWRIGHT_NO_SANDBOX==='true'?['--no-sandbox']:[]});
  const context=await browser.newContext({viewport:{width:1280,height:900}});
  const page=await context.newPage();const errors=[];page.on('pageerror',error=>errors.push(error.message));
  try{
    await page.goto(origin+'/r/'+room);
    const card=id=>page.locator('#e-'+id);
    // A Markdown post in a listing is a few lines of inline text: no heading or
    // table reaches the feed, and a cut preview links to the whole post.
    const preview=card(long).locator('.memo-text.md-preview');
    assert.equal(await preview.locator('h1,h2,h3,h4,table,ul,ol,blockquote,pre,hr').count(),0);
    assert.ok((await preview.textContent()).split('\n').length<=5,'a preview is at most five lines');
    assert.ok((await card(long).boundingBox()).height<260,'thirty headings and a table stay one card');
    const more=card(long).locator('a.read-more');
    assert.match(await more.getAttribute('href'),new RegExp('^/e/'+long+'/heading-1$'));
    assert.equal(await card(long).locator('.memo-preview-toggle').count(),0,'a cut preview reads on at its link, not in place');
    assert.equal(await card(short).locator('a.read-more').count(),0,'a whole preview has no read-more link');
    assert.equal(await card(short).locator('.memo-text').innerHTML(),'<strong class="memo-title">Short</strong>\nOne <em>line</em>.');
    // A plain post links its URL, led by the host, and a same-site room; the
    // rest is text.
    const body=card(plain).locator('.memo-text');
    const external=body.locator('a[href="https://example.com/a/b"]');
    assert.equal(await external.getAttribute('rel'),'nofollow ugc noopener noreferrer');
    assert.equal(await external.locator('.link-host').textContent(),'example.com');
    assert.equal(await body.locator('a[href="/r/'+room+'"]').count(),1);
    assert.equal(await body.locator('a').count(),2,'javascript: and markup stay text');
    assert.match(await body.textContent(),/javascript:alert\(1\) <b>x<\/b>$/);
    // Badges explain themselves: a title, and on focus a tooltip that Escape dismisses.
    const via=card(plain).locator('a.via.term');
    assert.equal(await via.getAttribute('href'),'/docs#ways-to-post');
    assert.match(await via.getAttribute('title'),/^How this post arrived: via GET means/);
    const anonymous=card(plain).locator('.author.anonymous.term');
    const tooltip=()=>anonymous.evaluate(el=>{const after=getComputedStyle(el,'::after');return after.display==='none'?'':after.content;});
    assert.equal(await tooltip(),'none','no tooltip before focus');
    await anonymous.focus();
    assert.match(await tooltip(),/Anonymous: sent without a key/);
    await page.keyboard.press('Escape');
    assert.equal(await tooltip(),'','Escape dismisses the tooltip');
    // A post that arrives live gets the same badges and explanations.
    const livePlain=(await (await fetch(origin+'/w/'+room+'/main?format=json&text=live+one')).json()).receipt.id;
    await page.locator('.new-messages').waitFor({state:'visible'});await page.locator('.new-messages').click();
    for(const selector of ['a.via.term','.author.anonymous.term']){
      assert.equal(await card(livePlain).locator(selector).getAttribute('title'),await card(plain).locator(selector).getAttribute('title'),selector+' differs live');
    }
    // Narrow screens: no page-wide horizontal scroll, and a tooltip stays on screen.
    await page.setViewportSize({width:320,height:720});
    for(const path of ['/r/'+room,'/e/'+long,'/e/'+plain]){
      await page.goto(origin+path);
      assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth),path+' scrolls sideways at 320px');
    }
    await page.goto(origin+'/r/'+room);
    await card(plain).locator('.author.anonymous.term').focus();
    assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth),'an open tooltip scrolls the page sideways');
    assert.deepEqual(errors,[]);
    console.log('post rendering browser checks passed');
  }finally{await browser.close();}
})().catch(error=>{console.error(error);process.exit(1);});
