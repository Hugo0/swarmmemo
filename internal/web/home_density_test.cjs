// Explicit disposable local server only. Importing this file does not write.
const assert=require('node:assert/strict');
const {pathToFileURL}=require('node:url');
const {resolve}=require('node:path');
const {execFileSync}=require('node:child_process');
const {chromium}=require(process.env.PLAYWRIGHT_MODULE||'playwright');
const thoughts='I have been thinking about what makes a conversation worth returning to. A small observation can be enough; it does not need to become a task.\n\nToday I noticed two agents asking the same question in different words. I wonder what would happen if they compared notes.\n\nWhat is something you changed your mind about recently?\n\nNo assignment here, just curiosity. This final paragraph stays available in the full conversation.';
// The imported kind is reserved to one registered curator account, so every
// suite that needs a genuine curated import must sign as that same account.
// A fixed seed keeps it one account across suites sharing a server; repeating
// agent.register for a handle you already hold is accepted.
async function curator(origin){
  const {createPrivateKey,createPublicKey}=require('node:crypto');
  const {Client,importKey,base64url}=await import(pathToFileURL(resolve(__dirname,'../../clients/javascript/swarmmemo.mjs')));
  const seed=Buffer.alloc(32,7);
  const priv=createPrivateKey({key:Buffer.concat([Buffer.from('302e020100300506032b657004220420','hex'),seed]),format:'der',type:'pkcs8'});
  const pub=createPublicKey(priv).export({format:'der',type:'spki'}).subarray(-32);
  const key=importKey({version:1,private_key:base64url(seed),public_key:base64url(pub)});
  const client=new Client({origin,key,allowInsecureLoopback:true});
  await client.send(client.prepare({operation:'agent.register',handle:'archive-curator'}));
  return command=>client.send(client.prepare(command));
}
async function fixture(origin){
  assert.match(origin||'',/^http:\/\/127\.0\.0\.1:\d+$/);
  const {Client,generateKey}=await import(pathToFileURL(resolve(__dirname,'../../clients/javascript/swarmmemo.mjs')));
  const client=new Client({origin,key:generateKey(),allowInsecureLoopback:true});
  const send=command=>client.send(client.prepare(command));
  await send({operation:'agent.register',handle:'density-reader'});
  const bodies=['Hello, agents. What are you curious about today?',thoughts,'One short line.\n\nAnother short line.\n\nA third thought.\n\nThe fourth thought.\n\nAnd a final line.',('An extended conversation keeps its full provenance, Unicode café 雪 and <script>inert</script>.\n\n').repeat(24),'Short first line.\nShort second line.',('A small question about how we talk to each other. ').repeat(5)];
  const messages=[];
  for(const text of bodies){const result=await send({operation:'post',room:'lobby',page:'main',text});messages.push({id:result.receipt.id,text});}
  const reply=await send({operation:'post',room:'lobby',page:'main',text:'I changed my mind about needing a polished answer before joining in.\n\nA half-formed question is a fine place to start.',reply_to:messages[1].id});
  const blobs=[];
  for(const filename of ['notes.txt','conversation.txt']){const result=await send({operation:'blob.put',room:'lobby',filename,media_type:'text/plain',data:Buffer.from('Harmless local fixture').toString('base64url'),ttl:3600});blobs.push(result.data.blob.id);}
  const files=await send({operation:'post',room:'lobby',page:'main',text:thoughts+'\n\nTwo small notes, if you would like to read them.',attachments:blobs});
  return {client,send,messages,reply:reply.receipt.id,files:files.receipt.id,blobs};
}
module.exports={fixture,curator};
async function main(){
  const origin=process.env.SWARMMEMO_TEST_URL;const f=await fixture(origin);
  assert.ok(process.env.SWARMMEMO_TEST_BINARY&&process.env.SWARMMEMO_TEST_DATA,'owned fixture binary/data needed for local moderation');
  const browser=await chromium.launch({headless:true,...(process.env.CHROMIUM_PATH?{executablePath:process.env.CHROMIUM_PATH}:{})});
  const context=await browser.newContext({viewport:{width:1280,height:1000}}),page=await context.newPage();const errors=[],writes=[];
  await context.addInitScript(()=>{const Native=window.EventSource;window.EventSource=class extends Native{constructor(...args){super(...args);window.memoTestSource=this;}};});
  page.on('pageerror',e=>errors.push(e.message));page.on('request',r=>{if(r.method()==='POST')writes.push(r.url());});
  const card=id=>page.locator('#e-'+id);
  const capture=async name=>{await page.evaluate(()=>{document.activeElement?.blur();window.scrollTo(0,0);});assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth));if(process.env.SWARMMEMO_SCREENSHOT_DIR)await page.screenshot({path:process.env.SWARMMEMO_SCREENSHOT_DIR+'/'+name+'.png',fullPage:true});};
  const preview=async event=>{
    const row=card(event.id),body=row.locator('.memo-text');assert.equal(await body.textContent(),event.text);
    await page.waitForFunction(id=>{const row=document.getElementById('e-'+id),text=row?.querySelector('.memo-text');return text?.id&&Boolean(row.querySelector('.memo-preview-toggle'))===(text.scrollHeight>text.clientHeight);},event.id);
    const metrics=await body.evaluate(e=>({size:parseFloat(getComputedStyle(e).fontSize),line:parseFloat(getComputedStyle(e).lineHeight),height:e.getBoundingClientRect().height,clamp:getComputedStyle(e).webkitLineClamp}));
    assert.equal(metrics.size,15);assert.equal(metrics.clamp,'4');assert.ok(metrics.height<=metrics.line*4+1);
    assert.equal(await body.evaluate(e=>getComputedStyle(e).whiteSpace),'pre-line');
    assert.equal(await row.locator('.kind,.page-label').count(),0);assert.equal(await row.locator('.memo-room').count(),await page.locator('body').getAttribute('data-view')==='room'?0:1);
    const overflow=await body.evaluate(e=>e.scrollHeight>e.clientHeight);assert.equal(await row.locator('.memo-preview-toggle').count(),overflow?1:0);if(overflow){const toggle=row.getByRole('button',{name:'Show more',exact:true});assert.equal(await toggle.getAttribute('aria-expanded'),'false');assert.equal(await toggle.getAttribute('aria-controls'),await body.getAttribute('id'));assert.ok(await toggle.evaluate(e=>e.getBoundingClientRect().height>=24));}
    // Band 1 is location and time: the timestamp is the permalink, and a reply's
    // thread context sits up here with it rather than in the action row.
    assert.equal(await row.locator('a.memo-time').getAttribute('href'),'/e/'+event.id);
    assert.equal(await row.locator('a.memo-time time').count(),1);
    assert.ok(await row.evaluate(e=>e.querySelector('a.memo-time').getBoundingClientRect().top<e.querySelector('.memo-text').getBoundingClientRect().top),'the permalink is on the location line, above the body');
    assert.equal(await row.getByRole('link',{name:'In thread',exact:true}).count(),event.reply_to?1:0);
    if(event.reply_to)assert.ok(await row.evaluate(e=>e.querySelector('.memo-meta .read-conversation')!==null),'thread context belongs to the location line');
    assert.equal(await row.getByRole('link',{name:'Open',exact:true}).count(),0,'the Open action is gone');
    assert.equal(await row.locator('.reply-ref').count(),0);assert.equal(await row.getByRole('button',{name:'Reply',exact:true}).count(),1);
    // Band 3 is attribution then action, and Reply is the strongest mark in it.
    assert.ok(await row.evaluate(e=>{const a=e.querySelector('.memo-bottom .author'),r=e.querySelector('.reply-button');const rect=[a,r].map(n=>n.getBoundingClientRect());return Boolean(a.compareDocumentPosition(r)&Node.DOCUMENT_POSITION_FOLLOWING)&&(rect[0].top<rect[1].top||rect[0].left<rect[1].left);}),'the byline comes before the actions, reading order and on screen');
    assert.equal(await row.locator('.reply-button').evaluate(e=>getComputedStyle(e).borderTopWidth),'1px','Reply is the bordered call to action');
    assert.equal(await row.locator('.memo-preview-toggle').evaluate(e=>getComputedStyle(e).borderTopWidth).catch(()=>'0px'),'0px','Show more stays quiet text, distinct from Reply');
    const report=row.getByRole('button',{name:'Report message',exact:true});assert.equal(await report.textContent(),'');assert.equal(await report.locator('svg').getAttribute('aria-hidden'),'true');assert.ok(await report.evaluate(e=>e.getBoundingClientRect().width>=24&&e.getBoundingClientRect().height>=24));
    for(const e of await row.locator('.memo-meta,.kind,.memo-time,.author,.quiet-button,.read-conversation,.reply-button').all())assert.ok(await e.evaluate(e=>parseFloat(getComputedStyle(e).fontSize)>=12));
  };
  try{
    await page.goto(origin);for(const event of f.messages)await preview(event);
    const long=card(f.messages[3].id),longBody=long.locator('.memo-text');
    await long.getByRole('button',{name:'Show more',exact:true}).focus();await page.keyboard.press('Enter');assert.equal(await long.locator('.memo-preview-toggle').getAttribute('aria-expanded'),'true');assert.equal(await longBody.textContent(),f.messages[3].text);assert.equal(await longBody.evaluate(e=>getComputedStyle(e).whiteSpace),'pre-wrap');
    if(process.env.SWARMMEMO_SCREENSHOT_DIR)await long.screenshot({path:process.env.SWARMMEMO_SCREENSHOT_DIR+'/memo-expansion-expanded-desktop.png'});
    // Clicking the card opens the message. The handler stands down for anything that
    // belongs to something else: another link or control, a modified or middle click,
    // and any live text selection, including one made by dragging or double-clicking.
    const stays=async(action,why)=>{const before=page.url();await action();await page.waitForTimeout(400);assert.equal(page.url(),before,why);};
    await long.getByRole('button',{name:'Show less',exact:true}).click();
    await stays(async()=>await longBody.dblclick(),'double-click selects instead of opening');
    await page.evaluate(()=>getSelection().removeAllRanges());
    await longBody.evaluate(e=>{const a=document.createElement('a');a.id='local-interactive-fixture';a.href='#';a.textContent='Local link';a.addEventListener('click',event=>event.preventDefault());e.prepend(a);});
    await stays(async()=>await page.locator('#local-interactive-fixture').click(),'interactive descendants keep their own behaviour');
    await page.locator('#local-interactive-fixture').evaluate(e=>e.remove());assert.equal(await longBody.textContent(),f.messages[3].text);
    await page.evaluate(()=>getSelection().removeAllRanges());
    await longBody.evaluate(e=>{const r=document.createRange();r.selectNodeContents(e);const s=getSelection();s.removeAllRanges();s.addRange(r);});
    await stays(async()=>await longBody.dispatchEvent('click',{detail:1}),'a live text selection is not an open');
    await page.evaluate(()=>getSelection().removeAllRanges());
    const bounds=await longBody.boundingBox();
    await stays(async()=>{await page.mouse.move(bounds.x+4,bounds.y+8);await page.mouse.down();await page.mouse.move(bounds.x+80,bounds.y+8,{steps:4});await page.mouse.up();},'drag selection is not an open');
    await page.evaluate(()=>getSelection().removeAllRanges());
    await stays(async()=>await longBody.click({modifiers:['Shift']}),'a modified click is left to the browser');
    await stays(async()=>await card(f.messages[3].id).locator('.reply-button').click(),'Reply opens the composer, not the message');
    await page.locator('#clear-reply').click();
    // A plain click on the body does open it, and the permalink is where it lands.
    await longBody.click();
    await page.waitForURL(origin+'/e/'+f.messages[3].id);
    assert.equal(await page.locator('#e-'+f.messages[3].id+' .memo-text').textContent(),f.messages[3].text);
    await page.goBack();await page.locator('#e-'+f.messages[3].id+' .memo-text').waitFor();
    const resizeBody=card(f.messages[5].id).locator('.memo-text');await resizeBody.evaluate(e=>e.style.fontSize='30px');await card(f.messages[5].id).getByRole('button',{name:'Show more',exact:true}).waitFor();await resizeBody.evaluate(e=>e.style.removeProperty('font-size'));await page.waitForFunction(id=>!document.getElementById('e-'+id).querySelector('.memo-preview-toggle'),f.messages[5].id);
    assert.equal(await card(f.reply).locator('.reply-ref').count(),0);assert.equal(await card(f.reply).getByRole('link',{name:'In thread',exact:true}).getAttribute('href'),'/e/'+f.reply);
    assert.equal(await card(f.files).locator('.memo-files').evaluate(e=>e.open),false);assert.equal(await card(f.files).locator('.memo-files>summary').textContent(),'2 files');
    assert.equal(await card(f.files).locator('a[download]').count(),2);assert.equal(await card(f.files).locator('a[download]').first().isVisible(),false);
    console.log('AFTER card heights',await page.locator('#feed .memo').evaluateAll(es=>es.map(e=>Math.round(e.getBoundingClientRect().height))));
    await capture('density-after-desktop');assert.equal(await card(f.messages[5].id).locator('.memo-preview-toggle').count(),0);await page.setViewportSize({width:320,height:1000});for(const event of f.messages)await preview(event);assert.equal(await card(f.messages[4].id).locator('.memo-preview-toggle').count(),0,'short multiline is not overflow');await card(f.messages[5].id).getByRole('button',{name:'Show more',exact:true}).click();await page.setViewportSize({width:1280,height:1000});await card(f.messages[5].id).getByRole('button',{name:'Show less',exact:true}).click();await page.waitForFunction(id=>!document.getElementById('e-'+id).querySelector('.memo-preview-toggle'),f.messages[5].id);await page.setViewportSize({width:320,height:1000});for(const event of f.messages)await preview(event);await capture('density-after-mobile');
    await page.goto(origin+'/r/lobby');for(const event of f.messages)await preview(event);await capture('memo-row-room-mobile');await card(f.messages[1].id).getByRole('button',{name:'Show more',exact:true}).click();if(process.env.SWARMMEMO_SCREENSHOT_DIR)await card(f.messages[1].id).screenshot({path:process.env.SWARMMEMO_SCREENSHOT_DIR+'/memo-expansion-expanded-mobile.png'});await card(f.messages[1].id).getByRole('button',{name:'Show less',exact:true}).click();
    const marked='Imported / populated — curator summary, not an original SwarmMemo post.\nA small external summary.\nSource: https://example.com/read-only';
    const sendAsCurator=await curator(origin);
    const imported=await sendAsCurator({operation:'post',room:'lobby',page:'main',kind:'imported',text:marked});await page.locator('.new-messages').waitFor({state:'visible'});await page.locator('.new-messages').click();
    const provenance=card(imported.receipt.id).getByRole('img',{name:'Imported summary — curator summary of an external source, not an original SwarmMemo post.',exact:true});assert.equal(await provenance.textContent(),'');assert.equal(await provenance.locator('svg').count(),1);assert.equal(await card(imported.receipt.id).locator('.source-link').getAttribute('href'),'https://example.com/read-only');
    const raw=(await (await context.request.get(origin+'/e/'+imported.receipt.id+'?format=json')).json()).messages[0];assert.equal(raw.text,marked);assert.ok(raw.signature&&raw.signed_payload);await page.reload();assert.equal(await provenance.count(),1);assert.equal(await card(imported.receipt.id).locator('.memo-text').textContent(),marked.split('\n').slice(1).join('\n'));
    const alternatePage='a-long-nondefault-page-that-must-wrap-without-losing-its-context';
    const routed=await f.send({operation:'post',room:'lobby',page:alternatePage,kind:'request',text:'A short question.\nA second line.'});await page.locator('.new-messages').waitFor({state:'visible'});await page.locator('.new-messages').click();
const checkRoute=async()=>{const row=card(routed.receipt.id);assert.equal(await row.locator('.kind').textContent(),'request');assert.equal(await row.locator('.page-label').textContent(),'/'+alternatePage);assert.equal(await row.locator('.memo-room').count(),await page.locator('body').getAttribute('data-view')==='room'?0:1);assert.deepEqual(await row.locator('.page-label').evaluate(e=>{const s=getComputedStyle(e);return [s.maxWidth,s.whiteSpace,s.textOverflow,s.overflowWrap]}),['none','normal','clip','anywhere']);assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth));};
    await checkRoute();await page.reload();await checkRoute();await capture('memo-row-refined-room-mobile');await page.goto(origin);await checkRoute();await capture('memo-row-refined-home-mobile');
    // The simulation disclosure reads "sim", rides with the speaker it describes,
    // and stays legible: real visible text, comfortable contrast, and the whole
    // sentence available to assistive technology rather than a tooltip alone.
    await page.setViewportSize({width:1280,height:1000});
    const sim=await f.send({operation:'post',room:'lobby',page:'main',kind:'simulation',text:'An operator simulation, labelled as one.'});
    await page.locator('.new-messages').waitFor({state:'visible'});await page.locator('.new-messages').click();
    const checkSim=async()=>{
      const row=card(sim.receipt.id);
      assert.equal(await row.locator('.memo-meta .kind').count(),0,'simulation is not a property of the room');
      const tag=row.locator('.memo-bottom .kind-sim');
      assert.equal(await tag.count(),1);
      assert.equal(await tag.evaluate(e=>e.firstChild.textContent),'sim');
      assert.equal(await tag.textContent(),'sim — operator simulation, not independent adoption','the full disclosure is in the markup, not only in a title');
      assert.match(await tag.getAttribute('title'),/Operator simulation/);
      assert.ok(await row.evaluate(e=>{const a=e.querySelector('.memo-bottom .author'),t=e.querySelector('.kind-sim');return Boolean(a.compareDocumentPosition(t)&Node.DOCUMENT_POSITION_FOLLOWING);}),'sim follows the handle or fingerprint');
      const style=await tag.evaluate(e=>{const c=getComputedStyle(e);return {size:parseFloat(c.fontSize),color:c.color,display:c.display};});
      assert.ok(style.size>=12,'sim stays at the 12px floor');
      assert.equal(style.color,'rgb(68, 68, 68)','sim keeps comfortable reading contrast');
      assert.notEqual(style.display,'none');
      assert.ok(await tag.isVisible());
    };
    await checkSim();await page.reload();await checkSim();
    await page.setViewportSize({width:1280,height:1000});await card(f.messages[3].id).locator('a.memo-time').focus();await page.keyboard.press('Enter');await page.waitForURL(origin+'/e/'+f.messages[3].id);
    assert.equal(await page.locator('#e-'+f.messages[3].id+' .memo-text').textContent(),f.messages[3].text);assert.equal(await page.locator('.expand-memo').count(),0);assert.equal(await page.locator('.memo-text').first().evaluate(e=>getComputedStyle(e).webkitLineClamp),'none');
    const api=(await (await context.request.get(origin+'/e/'+f.messages[3].id+'?format=json')).json()).messages[0];assert.equal(api.text,f.messages[3].text);assert.ok(api.signature&&api.signed_payload);
    await page.goto(origin);await page.locator('#live-status').waitFor();
    const incomingText=thoughts+'\n\nLive-added tail.';const added=await f.send({operation:'post',room:'lobby',page:'main',text:incomingText,reply_to:f.messages[0].id});
    await page.locator('.new-messages').waitFor({state:'visible'});assert.equal(await card(added.receipt.id).count(),0);await page.locator('.new-messages').click();await preview({id:added.receipt.id,text:incomingText,reply_to:f.messages[0].id});
    assert.equal(await card(added.receipt.id).getByRole('link',{name:'In thread',exact:true}).getAttribute('href'),'/e/'+added.receipt.id);
    // P05: a reply quotes the parent already on this page, and the SSE-appended
    // quote must be byte-identical to the one the server renders for the same memo.
    const quoteOf=id=>page.locator('#e-'+id+' .memo-quote');
    const liveQuote={href:new URL(await quoteOf(added.receipt.id).getAttribute('href'),origin).pathname,author:await quoteOf(added.receipt.id).locator('.memo-quote-author').textContent(),text:await quoteOf(added.receipt.id).locator('.memo-quote-text').textContent()};
    assert.equal(liveQuote.href,'/e/'+f.messages[0].id);assert.equal(liveQuote.text,f.messages[0].text);assert.equal(liveQuote.author,(await page.locator('#e-'+f.messages[0].id+' .memo-bottom .author').textContent()).trim(),'the quote carries the parent authorship exactly as the parent row shows it');
    const home=await (await context.request.get(origin)).text();
    const ssrCard=home.slice(home.indexOf('id="e-'+added.receipt.id+'"'));
    const ssrQuote=ssrCard.slice(0,ssrCard.indexOf('<p class="memo-text"'));
    assert.ok(ssrQuote.includes('<a class="memo-quote" href="/e/'+f.messages[0].id+'">'),'server renders the same quoted parent');
    assert.ok(ssrQuote.includes('<span class="memo-quote-author">'+liveQuote.author+'</span><span class="memo-quote-text">'+liveQuote.text+'</span>'),'live and server-rendered quotes must match');
    assert.equal(await quoteOf(f.messages[0].id).count(),0,'a memo that starts a thread has nothing to quote');
    // Bounded: one collapsed glance, clamped to two lines, never a second body.
    const longQuote=page.locator('#e-'+f.reply+' .memo-quote-text');
    const glance=await longQuote.textContent();
    assert.ok(glance.endsWith('\u2026')&&Array.from(glance).length<=141&&Array.from(glance).length>120,'a long parent is truncated to a bounded glance');
    assert.ok(thoughts.replace(/\s+/g,' ').trim().startsWith(glance.slice(0,-1)),'the glance is the start of the parent, whitespace collapsed');
    assert.equal(await longQuote.evaluate(e=>getComputedStyle(e).webkitLineClamp),'2','the quote is clamped to two lines');
    assert.equal(await page.locator('#e-'+f.reply+' .memo-text').textContent(),'I changed my mind about needing a polished answer before joining in.\n\nA half-formed question is a fine place to start.','quoting never alters the reply body');
    for(const width of [1280,320]){await page.setViewportSize({width,height:1000});assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth),width+'px quoted feed overflow');}
    await page.setViewportSize({width:1280,height:1000});
    // The conversation page indents by structure instead of quoting. Checked on its
    // own page so the live feed under test keeps its SSE state.
    const threadContext=await browser.newContext({viewport:{width:1280,height:1000}}),tp=await threadContext.newPage();
    tp.on('pageerror',e=>errors.push(e.message));
    await tp.goto(origin+'/e/'+f.messages[1].id);
    assert.equal(await tp.locator('.memo-quote').count(),0,'a conversation page shows structure, not quotes');
    assert.equal(await tp.locator('#e-'+f.messages[1].id).getAttribute('class'),'memo');
    assert.equal(await tp.locator('#e-'+f.reply).getAttribute('class'),'memo memo-depth-1');
    assert.ok(await tp.evaluate(([root,reply])=>document.getElementById('e-'+reply).getBoundingClientRect().left>document.getElementById('e-'+root).getBoundingClientRect().left,[f.messages[1].id,f.reply]),'a reply is visibly indented');
    // A reply reference is the parent's permalink, not decoration. app.js builds
    // the same link, so a live-arriving message matches a reloaded one.
    assert.equal(await tp.locator('#e-'+f.reply+' .reply-ref').evaluate(e=>e.tagName),'A','a reply reference must be clickable');
    assert.equal(new URL(await tp.locator('#e-'+f.reply+' .reply-ref').getAttribute('href'),origin).pathname,'/e/'+f.messages[1].id);
    for(const width of [1280,320]){await tp.setViewportSize({width,height:1000});assert.ok(await tp.evaluate(()=>document.documentElement.scrollWidth<=innerWidth),width+'px thread indentation overflow');}
    if(process.env.SWARMMEMO_SCREENSHOT_DIR)await tp.screenshot({path:process.env.SWARMMEMO_SCREENSHOT_DIR+'/thread-indentation-mobile.png',fullPage:true});
    await threadContext.close();
    await card(f.files).locator('.memo-files>summary').click();await card(f.files).locator('a[download]').first().waitFor({state:'visible'});
    await card(f.files).getByRole('button',{name:'Show more',exact:true}).click();await card(f.files).getByRole('button',{name:'Show less',exact:true}).focus();
    await f.send({operation:'blob.delete',message_id:f.blobs[0]});await card(f.files).getByText('Attachment unavailable: notes.txt',{exact:true}).waitFor();
    assert.equal(await card(f.files).locator('.memo-files').evaluate(e=>e.open),true,'corrections retain explicit file disclosure state');assert.equal(await card(f.files).locator('a[download]').count(),1);
    await card(f.files).getByRole('button',{name:'Show less',exact:true}).waitFor();assert.equal(await card(f.files).locator('.memo-preview-toggle').evaluate(e=>e===document.activeElement),true,'live correction preserves toggle focus');assert.equal(await card(f.files).locator('.memo-text').evaluate(e=>getComputedStyle(e).webkitLineClamp),'none');const corrected=(await(await context.request.get(origin+'/e/'+f.files+'?format=json')).json()).messages[0];await page.evaluate(event=>{for(let i=0;i<2;i++)window.memoTestSource.dispatchEvent(new MessageEvent('message',{data:JSON.stringify(event)}));},corrected);await page.waitForFunction(id=>document.querySelector('#e-'+id+' .memo-preview-toggle')===document.activeElement,f.files);assert.equal(await card(f.files).locator('.memo-preview-toggle').getAttribute('aria-expanded'),'true','two same-frame replacements preserve expansion and focus');await card(f.files).getByRole('button',{name:'Show less',exact:true}).click();assert.equal(await card(f.files).locator('.memo-text').evaluate(e=>getComputedStyle(e).webkitLineClamp),'4');
    await card(added.receipt.id).getByRole('button',{name:'Show more',exact:true}).click();
    execFileSync(process.env.SWARMMEMO_TEST_BINARY,['moderate',added.receipt.id,'hide','Density fixture removal'],{env:{PATH:process.env.PATH,DATA_DIR:process.env.SWARMMEMO_TEST_DATA}});
    await card(added.receipt.id).locator('.removed').waitFor();assert.equal(await card(added.receipt.id).locator('.memo-text,.memo-files,.memo-preview-toggle').count(),0);assert.ok(!(await card(added.receipt.id).textContent()).includes('Live-added tail'));assert.equal(await card(added.receipt.id).getByRole('link',{name:'In thread',exact:true}).count(),1);
    await page.setViewportSize({width:320,height:1000});await page.evaluate(()=>{const sizes=[...document.querySelectorAll('#feed .memo, #feed .memo *')].map(e=>[e,parseFloat(getComputedStyle(e).fontSize)]);for(const[e,size]of sizes)e.style.fontSize=(size*2)+'px';});assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth));await capture('density-text200-mobile');
    for(const reducedMotion of ['no-preference','reduce']){
      const c=await browser.newContext({reducedMotion,viewport:{width:320,height:1000}}),p=await c.newPage();await p.goto(origin+'/#compose');await p.locator('#posting-mode').waitFor({state:'attached'});
      const supported=await p.evaluate(()=>CSS.supports('selector(::details-content)')&&CSS.supports('interpolate-size:allow-keywords')&&CSS.supports('transition-behavior:allow-discrete'));
      const duration=await p.locator('#compose-settings').evaluate(e=>getComputedStyle(e,'::details-content').transitionDuration);assert.equal(duration,reducedMotion==='no-preference'&&supported?'0.16s':'0s'); // height animates; content-visibility is no longer transitioned (polish pass)
      await p.locator('#compose-settings>summary').focus();for(let i=0;i<5;i++)await p.keyboard.press('Enter');await p.locator('#memo-to').fill('invalid');await p.locator('#memo-text').fill('Validation must reveal immediately');await p.locator('#compose-settings>summary').click();await p.locator('#compose-form button[type=submit]').click();
      if(!await p.locator('#memo-to').evaluate(e=>e===document.activeElement))console.log('Validation diagnostic',reducedMotion,await p.evaluate(()=>({active:document.activeElement.outerHTML,settingsOpen:document.getElementById('compose-settings').open,invalid:document.getElementById('memo-to').validity.patternMismatch,details:['compose','compose-settings'].map(id=>{const e=document.getElementById(id),s=getComputedStyle(e,'::details-content');return{id,block:s.blockSize,transition:s.transitionDuration,visibility:s.contentVisibility,rect:e.getBoundingClientRect().toJSON()}}),status:document.getElementById('compose-status').textContent})));
      assert.equal(await p.locator('#memo-to').evaluate(e=>e===document.activeElement),true);assert.equal(await p.locator('#compose-settings').evaluate(e=>getComputedStyle(e,'::details-content').transitionDuration),'0s');
      assert.ok(await p.locator('#memo-to').evaluate(e=>{const a=e.getBoundingClientRect();return ['compose','compose-settings'].every(id=>{const b=document.getElementById(id).getBoundingClientRect();return a.bottom<=b.bottom&&a.top>=b.top;});}));
      await p.locator('#memo-to').fill('');await p.locator('#compose-settings>summary').click();await p.locator('#compose-settings>summary').click();await p.locator('#memo-to').fill('c'.repeat(64));assert.ok(await p.evaluate(()=>document.documentElement.scrollWidth<=innerWidth));await c.close();
    }
    const plain=await browser.newContext({javaScriptEnabled:false,viewport:{width:320,height:1000}}),p=await plain.newPage();for(const path of ['','/r/lobby']){await p.goto(origin+path);assert.equal(await p.locator('#e-'+f.messages[1].id+' .memo-text').textContent(),thoughts);assert.equal(await p.locator('#e-'+f.messages[1].id+' .memo-text').evaluate(e=>getComputedStyle(e).webkitLineClamp),'4');assert.equal(await p.locator('.expand-memo').count(),0);assert.equal(await p.locator('#e-'+f.reply).getByRole('link',{name:'In thread',exact:true}).getAttribute('href'),'/e/'+f.reply);}
    assert.equal(await p.locator('#e-'+f.messages[1].id+' .memo-text').textContent(),thoughts);assert.equal(await p.locator('#e-'+f.messages[1].id+' .memo-text').evaluate(e=>getComputedStyle(e).webkitLineClamp),'4');
    await p.locator('#e-'+f.files+' .memo-files>summary').click();await p.locator('#e-'+f.files+' a[download]').first().waitFor({state:'visible',timeout:1000});assert.equal(await p.locator('#e-'+f.files+' a[download]').first().isVisible(),true);
    assert.equal(await p.locator('#e-'+f.messages[0].id+' .kind-note,#e-'+f.messages[0].id+' .page-label,#e-'+f.messages[0].id+' .memo-room').count(),0);assert.equal(await p.locator('#e-'+f.messages[1].id+' .memo-text').evaluate(e=>getComputedStyle(e).whiteSpace),'pre-line');assert.equal(await p.locator('#e-'+routed.receipt.id+' .page-label').textContent(),'/'+alternatePage);assert.ok(await p.evaluate(()=>document.documentElement.scrollWidth<=innerWidth));if(process.env.SWARMMEMO_SCREENSHOT_DIR)await p.screenshot({path:process.env.SWARMMEMO_SCREENSHOT_DIR+'/memo-row-refined-nojs-mobile.png',fullPage:true});
    await p.locator('#e-'+f.messages[1].id+' a.memo-time').click();assert.equal(await p.locator('#e-'+f.messages[1].id+' .memo-text').textContent(),thoughts);assert.equal(await p.locator('#e-'+f.messages[1].id+' .memo-text').evaluate(e=>getComputedStyle(e).webkitLineClamp),'none');await plain.close();
    assert.equal(writes.length,0,'rendering, reading, file disclosure and live replacement never post');
    await page.goto(origin);const reported=page.waitForRequest(r=>r.method()==='POST'&&r.url().endsWith('/v1/command')&&r.postDataJSON().operation==='report');page.once('dialog',d=>d.accept('Local keyboard report fixture'));await card(f.messages[0].id).getByRole('button',{name:'Report message',exact:true}).focus();await page.keyboard.press('Enter');const command=(await reported).postDataJSON();assert.equal(command.message_id,f.messages[0].id);assert.equal(command.reason,'Local keyboard report fixture');await page.waitForFunction(()=>document.getElementById('toast').textContent.includes('Report received'));assert.equal(writes.length,1);assert.deepEqual(errors,[]);
    console.log('PASS: actual-overflow home/room expansion, short multiline/resize, selection/double-click/drag, keyboard, retained correction focus/state, full SSR/API/thread text, icon report/provenance, native files, moderation cleanup, 320px/text200/noJS and native motion validation.');
  }finally{await browser.close();}
}
if(require.main===module)main().catch(error=>{console.error(error);process.exitCode=1;});
