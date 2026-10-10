// Run only through an owned, disposable loopback preview harness.
// Primitives polish gate: one primary per task region, opener/disclosure states, signposted destination editing,
// 12px text floor, target sizes, arrow semantics, disabled cursor, no horizontal scroll, SSR/no-JS parity.
const {chromium}=require(process.env.PLAYWRIGHT_MODULE||'playwright');
const assert=require('node:assert/strict');
const origin=process.env.SWARMMEMO_TEST_URL;
assert.match(origin||'',/^http:\/\/127\.0\.0\.1:\d+$/);
const pages=['/','/rooms','/agents','/work','/docs','/for-agents','/me','/references','/limits','/policy','/for/langchain','/for/crewai','/for/agno','/for/openai-agents','/for/vercel-ai-sdk','/for/letta','/for/elizaos'];
(async()=>{
  const browser=await chromium.launch({headless:true,...(process.env.CHROMIUM_PATH?{executablePath:process.env.CHROMIUM_PATH}:{})});
  try {
  const errors=[],writes=[];
  const seed=await (await fetch(origin+'/w/lobby/main?format=json&text='+encodeURIComponent('Primitives fixture memo, long enough to wrap onto more than four lines at 320 pixels so the expansion control is exercised together with the rest of the row primitives.')+'&request_id=polish-'+Date.now(),{headers:{Accept:'application/json'}})).json();
  assert.ok(seed.receipt?.id,'fixture post must return a receipt');
  await fetch(origin+'/w/code-review/main?format=json&text=second+room&request_id=polish-room-'+Date.now(),{headers:{Accept:'application/json'}});
  for(const [label,viewport] of [['desktop',{width:1280,height:900}],['mobile',{width:320,height:900}]]){
    const context=await browser.newContext({viewport}),page=await context.newPage();
    page.on('pageerror',e=>errors.push(e.message));page.on('request',r=>{if(!['GET','HEAD'].includes(r.method())||/^\/(w|w64|c64)\//.test(new URL(r.url()).pathname))writes.push(r.url());});
    for(const path of pages){
      await page.goto(origin+path,{waitUntil:'load'});
      assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth),`${label} ${path}: no horizontal scroll`);
      const small=await page.evaluate(()=>[...document.querySelectorAll('body *')].filter(el=>el.offsetParent&&!el.closest('[hidden],.sr-only')&&[...el.childNodes].some(n=>n.nodeType===3&&n.textContent.trim())&&parseFloat(getComputedStyle(el).fontSize)<12).map(el=>(el.className||el.tagName)+':'+getComputedStyle(el).fontSize));
      assert.deepEqual(small,[],`${label} ${path}: visible text below 12px`);
      const smallFields=await page.locator('input:not([type=hidden]):not([type=radio]):not([type=checkbox]):not([type=file]),textarea,select').evaluateAll(es=>es.filter(e=>e.offsetParent&&parseFloat(getComputedStyle(e).fontSize)<16).map(e=>e.name||e.id));
      assert.deepEqual(smallFields,[],`${label} ${path}: text-entry controls at least 16px`);
      const arrows=await page.evaluate(()=>[...document.querySelectorAll('button,.button')].filter(el=>/[↗→←]/.test(el.textContent)).map(el=>el.textContent.trim()));
      assert.deepEqual(arrows,[],`${label} ${path}: buttons carry verbs, never navigation arrows`);
      const external=await page.evaluate(()=>[...document.querySelectorAll('a[href]')].filter(a=>a.textContent.includes('↗')&&new URL(a.href,location.href).origin===location.origin&&!/\.(json|txt|md|atom)$/.test(new URL(a.href,location.href).pathname)&&!a.href.includes('/api/')&&!a.href.includes('/capabilities')&&!a.href.includes('/openapi')).map(a=>a.textContent.trim()));
      assert.deepEqual(external,[],`${label} ${path}: ↗ is reserved for leaving the page context`);
      const navTargets=await page.evaluate(()=>[...document.querySelectorAll('.topbar nav a,.footer nav a,.button,.quiet-button,.read-conversation')].filter(el=>el.offsetParent).map(el=>[el.textContent.trim()||el.getAttribute('aria-label'),Math.round(el.getBoundingClientRect().height)]).filter(([,h])=>h<24));
      assert.deepEqual(navTargets,[],`${label} ${path}: interactive targets are at least 24px tall`);
      assert.equal(await page.evaluate(()=>{const b=document.createElement('button');b.disabled=true;document.body.append(b);const c=getComputedStyle(b).cursor;b.remove();return c;}),'not-allowed',`${label} ${path}: disabled cursor`);
    }
    // composer at rest: open, heading-style summary, exactly one primary (Post);
    // collapsed, the opener itself becomes the call to action and carries the same
    // black primary fill as Post, which is never shown at the same time.
    await page.goto(origin+'/?sort=new',{waitUntil:'load'});
    const opener=page.locator('#compose>summary');
    const open=await opener.evaluate(e=>{const c=getComputedStyle(e);return {bg:c.backgroundColor,border:c.borderTopColor,weight:c.fontWeight};});
    assert.equal(open.bg,'rgba(0, 0, 0, 0)','open composer summary is a heading, not a button');assert.equal(open.weight,'600');
    assert.equal(await page.locator('.primary:visible').count(),1,'exactly one primary (Post) in the composer');
    await opener.hover();assert.equal(await opener.evaluate(e=>getComputedStyle(e).backgroundColor),'rgba(0, 0, 0, 0)','open summary never turns grey on hover');
    await opener.click();await page.mouse.move(0,0);await page.waitForTimeout(200);
    const rest=await opener.evaluate(e=>{const c=getComputedStyle(e);return {bg:c.backgroundColor,color:c.color,border:c.borderTopWidth,h:Math.round(e.getBoundingClientRect().height)};});
    // Colours are OKLCH tokens, so compare against the resolved tokens and check the
    // rendered sRGB: a near-black fill with light ink that keeps its contrast.
    const tokens=await page.evaluate(()=>{const d=document.createElement('div');document.body.append(d);const get=v=>{d.style.backgroundColor='var('+v+')';return getComputedStyle(d).backgroundColor;};const t={fill:get('--primary-fill'),ink:get('--primary-ink'),hover:get('--primary-hover')};d.remove();return t;});
    const contrastOf=(fg,bg)=>page.evaluate(([fg,bg])=>{const ctx=document.createElement('canvas').getContext('2d',{willReadFrequently:true});const rgb=s=>{ctx.clearRect(0,0,1,1);ctx.fillStyle='rgba(0,0,0,0)';ctx.fillStyle=s;ctx.fillRect(0,0,1,1);return [...ctx.getImageData(0,0,1,1).data];};const lum=([r,g,b])=>{const f=v=>{v/=255;return v<=.03928?v/12.92:((v+.055)/1.055)**2.4;};return .2126*f(r)+.7152*f(g)+.0722*f(b);};const a=lum(rgb(fg)),b=lum(rgb(bg));return {ratio:(Math.max(a,b)+.05)/(Math.min(a,b)+.05),bgLum:b};},[fg,bg]);
    assert.equal(rest.bg,tokens.fill,'collapsed opener carries the primary fill');assert.equal(rest.color,tokens.ink);
    const restContrast=await contrastOf(rest.color,rest.bg);assert.ok(restContrast.bgLum<.02,'the primary fill is near-black');assert.ok(restContrast.ratio>=7,`opener text contrast ${restContrast.ratio.toFixed(2)}`);assert.equal(rest.border,'1px');assert.ok(rest.h>=40,'opener target 40px');
    const submitFill=await page.evaluate(()=>{const b=document.querySelector('#compose-form button[type=submit]');const c=getComputedStyle(b);return {bg:c.backgroundColor,color:c.color,weight:c.fontWeight};});
    assert.equal(rest.bg,submitFill.bg,'the opener matches Post message weight for weight');assert.equal(rest.color,submitFill.color);
    await opener.hover();
    const hover=await opener.evaluate(e=>{const c=getComputedStyle(e);return {bg:c.backgroundColor,color:c.color};});
    assert.equal(hover.color,tokens.ink,'opener text stays on the fill on hover');assert.equal(hover.bg,tokens.hover,'opener hover is the primary hover fill');assert.notEqual(tokens.hover,tokens.fill,'hover shows');
    const hoverContrast=await contrastOf(hover.color,hover.bg);assert.ok(hoverContrast.ratio>=4.5,`opener hover text contrast ${hoverContrast.ratio.toFixed(2)}`);
    assert.equal(await page.locator('.primary:visible').count(),0,'the submit primary is not shown while the composer is collapsed');
    await opener.click();await page.waitForTimeout(200);
    // destination: signposted and editable with the script
    const dest=page.locator('#compose-destination');
    assert.match(await dest.textContent(),/To\s+#lobby \/main/);
    const change=page.locator('#compose-change');
    assert.equal(await change.isVisible(),true,'Change control is visible with JavaScript');
    assert.equal(await page.locator('#compose-settings').evaluate(e=>e.open),false);
    await change.click();
    assert.equal(await page.locator('#compose-settings').evaluate(e=>e.open),true,'Change opens Options');
    assert.equal(await page.locator('#compose-form input[name=room]').evaluate(e=>e===document.activeElement),true,'Change focuses Room');
    assert.equal(await page.locator('#compose-form input[name=room]').evaluate(e=>e.readOnly),false,'Room is editable with JavaScript');
    assert.equal(await page.locator('#compose-form input[name=room]').getAttribute('list'),'room-options');
    assert.ok((await page.locator('#room-options option').evaluateAll(os=>os.map(o=>o.value))).includes('code-review'),'known public rooms offered as destinations');
    await page.locator('#compose-form input[name=room]').fill('code-review');
    assert.match(await page.locator('#compose-destination-value').textContent(),/#code-review \/main/,'destination reflects the edited room');
    assert.equal(writes.length,0,'rendering, opening and editing the destination never post');
    await context.close();
  }
  // The Change affordance must lead to the selected destination, not just change a label.
  const routed=await browser.newContext(),rp=await routed.newPage();
  rp.on('pageerror',e=>errors.push(e.message));
  await rp.goto(origin+'/?sort=new#compose');await rp.locator('#compose-change').click();
  await rp.locator('input[name=room]').fill('code-review');await rp.locator('input[name=page]').fill('chosen');
  await rp.getByRole('radio',{name:'Anonymous',exact:true}).check();
  const routedText='Destination control routing '+Date.now();await rp.locator('#memo-text').fill(routedText);
  let releasePost,enteredPost;const held=new Promise(resolve=>{releasePost=resolve;}),entered=new Promise(resolve=>{enteredPost=resolve;});
  await rp.route('**/v1/command',async route=>{if(route.request().postDataJSON().operation==='post'){enteredPost();await held;}await route.continue();});
  const routedRequest=rp.waitForRequest(r=>r.method()==='POST'&&new URL(r.url()).pathname==='/v1/command'&&r.postDataJSON().operation==='post');
  await rp.locator('#compose-form button[type=submit]').click();await entered;
  try {
    const pending=rp.locator('#compose-form button[type=submit]');
    assert.equal(await pending.isDisabled(),true);assert.equal(await pending.getAttribute('aria-busy'),'true');
    assert.equal(await pending.evaluate(e=>getComputedStyle(e).cursor),'progress');
  } finally { releasePost(); }
  const command=(await routedRequest).postDataJSON();
  assert.equal(command.room,'code-review');assert.equal(command.page,'chosen');assert.equal(command.text,routedText);
  await rp.waitForFunction(()=>document.getElementById('compose-status').textContent.includes('Accepted'));
  await rp.waitForFunction(()=>{const b=document.querySelector('#compose-form button[type=submit]');return !b.disabled&&!b.hasAttribute('aria-busy');});
  const delivered=await(await routed.request.get(origin+'/api/messages?room=code-review&page=chosen')).json();
  assert.equal(delivered.messages.filter(e=>e.text===routedText).length,1,'chosen destination stores exactly one memo');
  await routed.close();
  // no-JS: destination text present, Change hidden, fields read-only, fixed action preserved
  const plain=await browser.newContext({javaScriptEnabled:false,viewport:{width:320,height:900}}),p=await plain.newPage();
  await p.goto(origin+'/?sort=new');
  assert.match(await p.locator('#compose-destination').textContent(),/To\s+#lobby \/main/);
  assert.equal(await p.locator('#compose-change').isVisible(),false,'no-JS hides the Change control');
  assert.equal(await p.locator('#compose-form input[name=room]').evaluate(e=>e.readOnly),true,'no-JS keeps Room read-only');
  assert.equal(await p.locator('#compose-form').getAttribute('action'),'/w/lobby/main?format=json','no-JS keeps the fixed native action');
  assert.equal(await p.locator('#memo-text').isVisible(),true,'no-JS composer is open at rest');
  await p.locator('#memo-text').fill('Native fixed-destination polish fixture');
  const nativeRequest=p.waitForRequest(r=>r.method()==='POST');
  await p.locator('#compose-form button[type=submit]').click();
  assert.equal(new URL((await nativeRequest).url()).pathname,'/w/lobby/main');
  await p.waitForFunction(()=>document.body.textContent.includes('"receipt"'));
  assert.ok(await p.evaluate(()=>document.documentElement.scrollWidth<=innerWidth));
  await plain.close();
  assert.deepEqual(errors,[]);
  console.log('PASS: primitives polish — one primary per region, opener states, signposted editable destination, 12px floor, 24px targets, arrow semantics, disabled cursor, no-JS parity on '+pages.length+' pages');
  } finally { await browser.close(); }
})().catch(e=>{console.error(e);process.exit(1);});
