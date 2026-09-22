// Explicit disposable local server only. Importing this file does not write.
// Keyboard gate. The rules matter more than the bindings: no bare key fires while
// someone is typing, focus is real and visible, the help panel is a real modal
// dialog, and Escape never discards a draft.
const assert=require('node:assert/strict');
const {chromium}=require(process.env.PLAYWRIGHT_MODULE||'playwright');
const origin=process.env.SWARMMEMO_TEST_URL;
assert.match(origin||'',/^http:\/\/127\.0\.0\.1:\d+$/);
const room='keyboard-fixture',stamp=Date.now();

async function post(text,extra=''){
  const result=await (await fetch(origin+'/w/'+room+'/main?format=json&text='+encodeURIComponent(text)+'&request_id=kbd-'+stamp+'-'+Math.random().toString(36).slice(2)+extra,{headers:{Accept:'application/json'}})).json();
  assert.ok(result.receipt?.id,'fixture post must return a receipt');return result.receipt.id;
}

(async()=>{
  const ids=[];for(let i=1;i<=6;i++)ids.push(await post('Keyboard fixture message '+i+'. '+'A line long enough to be a real message. '.repeat(i===3?12:1)));
  const browser=await chromium.launch({headless:true,...(process.env.CHROMIUM_PATH?{executablePath:process.env.CHROMIUM_PATH}:{})});
  const errors=[];
  try{
    const context=await browser.newContext({viewport:{width:1280,height:800}});
    const page=await context.newPage();page.on('pageerror',e=>errors.push(e.message));
    const status=()=>page.locator('#shortcut-status').textContent();
    const active=()=>page.evaluate(()=>{const e=document.activeElement;return e?.id||e?.tagName;});
    await page.goto(origin+'/r/'+room+'/main',{waitUntil:'load'});
    await page.locator('#shortcuts-hint').waitFor({state:'visible'});
    assert.equal(await page.locator('#shortcut-status').getAttribute('aria-live'),'polite');

    // Rule 1: typing never triggers a shortcut. Every binding letter, typed into the
    // composer, lands as text; nothing navigates, moves, opens or submits.
    const url=page.url();
    await page.locator('#memo-text').click();
    const typed='reply jk n c o u e a y g f g r i . / ?';
    await page.keyboard.type(typed);
    await page.waitForTimeout(400);
    assert.equal(await page.locator('#memo-text').inputValue(),typed,'every key reached the field as text');
    assert.equal(page.url(),url,'typing did not navigate');
    assert.equal(await page.evaluate(()=>document.getElementById('reply-to').value),'','typing r did not open a reply');
    assert.equal(await page.locator('#shortcuts-dialog').count(),0,'typing ? did not open help');
    assert.equal(await active(),'memo-text','focus stayed in the field');
    // The same in the room/page option fields and with a modifier held at page level.
    // Options start open on a non-lobby room; open them only if closed, and restore.
    const optionsWereOpen=await page.locator('#compose-settings').evaluate(e=>e.open);
    if(!optionsWereOpen)await page.locator('#compose-change').click();
    await page.locator('#memo-page').fill('');await page.locator('#memo-page').type('jkr');
    assert.equal(await page.locator('#memo-page').inputValue(),'jkr');await page.locator('#memo-page').fill('main');
    if(!optionsWereOpen)await page.locator('#compose-change').click();
    await page.evaluate(()=>document.activeElement.blur());
    await page.keyboard.press('Control+j');await page.keyboard.press('Alt+k');
    assert.equal(await page.evaluate(()=>document.activeElement.closest?.('.memo')),null,'modified keys are left to the browser');

    // Esc in the composer: first press leaves the field, the draft stays, nothing closes.
    await page.locator('#memo-text').fill('A draft that Escape must never discard');
    await page.locator('#memo-text').focus();await page.keyboard.press('Escape');
    assert.notEqual(await active(),'memo-text','Escape left the field');
    assert.equal(await page.locator('#compose').evaluate(e=>e.open),true,'the composer stays open');
    assert.equal(await page.locator('#memo-text').inputValue(),'A draft that Escape must never discard');

    // j/k move real DOM focus to a card, visibly, and say where it landed.
    await page.keyboard.press('j');
    const first=await page.evaluate(()=>document.activeElement.dataset.messageId);
    assert.ok(first,'j focused a message card');
    const ring=await page.evaluate(()=>{const c=getComputedStyle(document.activeElement);return {style:c.outlineStyle,width:parseFloat(c.outlineWidth),shadow:c.boxShadow,visible:document.activeElement.matches(':focus-visible')};});
    assert.equal(ring.visible,true,'keyboard focus is focus-visible');assert.equal(ring.style,'solid');assert.ok(ring.width>=2);assert.notEqual(ring.shadow,'none','the ring is a shape, not a colour change alone');
    await page.waitForFunction(()=>/^Message 1 of \d+/.test(document.getElementById('shortcut-status').textContent));
    await page.keyboard.press('j');await page.keyboard.press('j');
    const third=await page.evaluate(()=>document.activeElement.dataset.messageId);
    assert.notEqual(third,first);await page.waitForFunction(()=>/^Message 3 of /.test(document.getElementById('shortcut-status').textContent));
    await page.keyboard.press('k');
    assert.equal(await page.evaluate(()=>[...document.querySelectorAll('#feed .memo')].indexOf(document.activeElement)),1,'k moved back one');
    // Forced colours keeps a visible ring.
    await page.emulateMedia({forcedColors:'active'});
    assert.equal(await page.evaluate(()=>getComputedStyle(document.activeElement).outlineStyle),'solid','the ring survives forced colours');
    await page.emulateMedia({forcedColors:'none'});
    // j/k scroll only as far as they must.
    const scrollBefore=await page.evaluate(()=>scrollY);await page.keyboard.press('j');await page.keyboard.press('k');
    assert.ok(Math.abs(await page.evaluate(()=>scrollY)-scrollBefore)<400,'j/k never yank the page');

    // . expands the focused message; r opens the inline reply under it; Esc closes it and keeps the draft.
    await page.locator('#e-'+ids[2]).focus();
    await page.keyboard.press('.');
    assert.equal(await page.locator('#e-'+ids[2]+' .memo-preview-toggle').getAttribute('aria-expanded'),'true','. expands');
    await page.keyboard.press('e');
    assert.equal(await page.locator('#e-'+ids[2]+' .memo-preview-toggle').getAttribute('aria-expanded'),'false','e collapses');
    await page.locator('#e-'+ids[2]).focus();await page.keyboard.press('r');
    assert.equal(await page.evaluate(id=>document.getElementById('reply-to').value==='' + id,ids[2]),true,'r addressed the composer to the focused message');
    assert.equal(await active(),'memo-text');
    await page.waitForFunction(()=>/Composer open/.test(document.getElementById('shortcut-status').textContent));
    await page.keyboard.press('Escape');assert.notEqual(await page.evaluate(()=>document.getElementById('reply-to').value),'','first Escape only leaves the field');
    await page.keyboard.press('Escape');assert.equal(await page.evaluate(()=>document.getElementById('reply-to').value),'','second Escape clears the reply');
    assert.equal(await page.locator('#memo-text').inputValue(),'A draft that Escape must never discard','Escape never discards typed text');
    assert.equal(await page.evaluate(id=>document.activeElement.id==='e-'+id,ids[2]),true,'focus returns to the message');

    // ? opens a real modal dialog; focus moves in, is trapped, Esc closes, focus returns.
    await page.keyboard.press('?');
    const dialog=page.locator('#shortcuts-dialog');
    assert.equal(await dialog.evaluate(d=>d.open&&d.matches(':modal')),true,'? opens a modal dialog');
    assert.equal(await dialog.getAttribute('role'),'dialog');assert.equal(await dialog.getAttribute('aria-modal'),'true');
    assert.equal(await page.locator('#'+await dialog.getAttribute('aria-labelledby')).textContent(),'Keyboard shortcuts');
    assert.equal(await page.evaluate(()=>document.getElementById('shortcuts-dialog').contains(document.activeElement)),true,'focus moved into the dialog');
    for(let i=0;i<4;i++){await page.keyboard.press('Tab');assert.equal(await page.evaluate(()=>document.getElementById('shortcuts-dialog').contains(document.activeElement)),true,'Tab stays inside');}
    await page.keyboard.press('Shift+Tab');assert.equal(await page.evaluate(()=>document.getElementById('shortcuts-dialog').contains(document.activeElement)),true);
    assert.match(await dialog.textContent(),/Shift\+Enter posts here/);
    await page.keyboard.press('j');assert.equal(await dialog.evaluate(d=>d.open),true,'bare keys are inert behind the dialog');
    await page.keyboard.press('Escape');
    assert.equal(await dialog.evaluate(d=>d.open),false,'Esc closes the dialog');
    assert.equal(await page.evaluate(id=>document.activeElement.id==='e-'+id,ids[2]),true,'focus returns to where it was');
    // The footer hint reaches the same dialog by keyboard alone.
    await page.locator('#shortcuts-hint').focus();await page.keyboard.press('Enter');
    assert.equal(await dialog.evaluate(d=>d.open),true,'the hint opens help from the keyboard');
    await page.keyboard.press('Escape');assert.equal(await active(),'shortcuts-hint');

    // y copies the focused permalink, with confirmation on screen and to assistive tech.
    await page.evaluate(()=>{window.copied=[];Object.defineProperty(navigator,'clipboard',{configurable:true,value:{writeText:async t=>{window.copied.push(t);}}});});
    await page.locator('#e-'+ids[1]).focus();await page.keyboard.press('y');
    await page.waitForFunction(()=>window.copied.length===1);
    assert.equal(await page.evaluate(()=>window.copied[0]),origin+'/e/'+ids[1]);
    await page.waitForFunction(()=>/Permalink copied/.test(document.getElementById('shortcut-status').textContent));

    // / focuses search where search exists (home).
    await page.goto(origin+'/',{waitUntil:'load'});await page.locator('#shortcuts-hint').waitFor({state:'visible'});
    await page.keyboard.press('/');assert.equal(await active(),'search');
    await page.keyboard.type('jk');assert.equal(await page.locator('#search').inputValue(),'jk','typing in search stays text');
    await page.keyboard.press('Escape');assert.notEqual(await active(),'search');

    // Shift+Enter posts through the same path as Post message; plain Enter is a newline.
    await page.locator('#memo-text').focus();
    await page.keyboard.type('Keyboard post line one');await page.keyboard.press('Enter');await page.keyboard.type('line two '+stamp);
    assert.equal(await page.locator('#memo-text').inputValue(),'Keyboard post line one\nline two '+stamp,'plain Enter adds a line and posts nothing');
    if(!await page.locator('#compose-settings').evaluate(e=>e.open))await page.locator('#compose-settings>summary').click();
    await page.getByRole('radio',{name:'Anonymous',exact:true}).check();
    await page.locator('#memo-text').focus();
    let release;const held=new Promise(r=>{release=r;});
    await page.route('**/v1/command',async route=>{if(route.request().postDataJSON().operation==='post')await held;await route.continue();});
    const request=page.waitForRequest(r=>r.method()==='POST'&&new URL(r.url()).pathname==='/v1/command');
    await page.keyboard.press('Shift+Enter');
    await request;
    assert.equal(await page.locator('#compose-form button[type=submit]').textContent(),'Posting…','the same busy state as clicking Post');
    await page.keyboard.press('Shift+Enter');
    release();
    await page.waitForFunction(()=>document.getElementById('compose-status').textContent.includes('Accepted'));
    await page.unroute('**/v1/command');
    const stored=await (await fetch(origin+'/api/messages?room=lobby&page=main&limit=50')).json();
    assert.equal(stored.messages.filter(m=>m.text==='Keyboard post line one\nline two '+stamp).length,1,'one post, with its newline, despite a second chord mid-flight');
    await page.locator('#memo-text').fill('Ctrl chord '+stamp);
    await page.keyboard.press('Control+Enter');
    await page.waitForFunction(s=>document.getElementById('memo-text').value===''&&document.getElementById('compose-status').textContent.includes('Accepted'),stamp);

    // g then a navigates; u goes up to a parent from a listed reply; nothing errors on a page without messages.
    // Focus is still in the composer after posting, where g and a are only text.
    await page.keyboard.press('g');assert.equal(new URL(page.url()).pathname,'/','g typed in the composer did not navigate');
    await page.locator('#memo-text').fill('');await page.evaluate(()=>document.activeElement.blur());
    await page.keyboard.press('g');await page.keyboard.press('a');await page.waitForURL(origin+'/agents');
    await page.keyboard.press('j');await page.keyboard.press('k');await page.keyboard.press('u');
    assert.equal(new URL(page.url()).pathname,'/agents','j/k/u do nothing without messages');
    const parent=ids[0];const child=await post('A reply for the u key.','&reply_to='+parent);
    await page.goto(origin+'/e/'+child,{waitUntil:'load'});
    await page.locator('#e-'+child).focus();await page.keyboard.press('u');
    await page.waitForURL(u=>new URL(u).pathname==='/e/'+parent);

    // 320px: the dialog fits without horizontal scroll.
    await page.setViewportSize({width:320,height:800});await page.keyboard.press('?');
    assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth&&document.getElementById('shortcuts-dialog').getBoundingClientRect().right<=innerWidth));
    await page.keyboard.press('Escape');

    // Without scripts none of this exists, and nothing pretends it does.
    const plain=await browser.newContext({javaScriptEnabled:false}),p=await plain.newPage();
    await p.goto(origin+'/');assert.equal(await p.locator('#shortcuts-hint').isVisible(),false,'no-JS hides the shortcuts hint');await plain.close();

    assert.deepEqual(errors,[]);
    console.log('PASS: typing never triggers shortcuts; j/k move real visible focus with announcements; . e r Esc y / g u work; ? opens a modal dialog with trapped focus and Esc returns focus; Shift/Ctrl+Enter post through the normal path; Escape keeps drafts; 320px and no-JS.');
  }finally{await browser.close();}
})().catch(error=>{console.error(error);process.exit(1);});
