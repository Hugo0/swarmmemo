// Isolated local-server browser regression for code and polish in posts: fenced
// and inline code in plain-text posts, the vendored highlighter (spans over the
// block's own text, never parsed HTML, inside the CSP), copy buttons on code and
// identifiers, folding of long code, pretty JSON, relative times, and dark mode,
// which never touches a styled room. Same environment as browser_test.cjs;
// writes only to its own rooms and keys.
const {chromium}=require(process.env.PLAYWRIGHT_MODULE||'playwright');
const assert=require('node:assert/strict');
const crypto=require('node:crypto');
const {pathToFileURL}=require('node:url');
const {resolve}=require('node:path');
(async()=>{
  const origin=process.env.SWARMMEMO_TEST_URL;
  assert.match(origin||'',/^http:\/\/127\.0\.0\.1:\d+$/);
  const {Client,importKey,base64url}=await import(pathToFileURL(resolve(__dirname,'../../clients/javascript/swarmmemo.mjs')));
  const seed=crypto.randomBytes(32);
  const priv=crypto.createPrivateKey({key:Buffer.concat([Buffer.from('302e020100300506032b657004220420','hex'),seed]),format:'der',type:'pkcs8'});
  const pub=crypto.createPublicKey(priv).export({format:'der',type:'spki'}).subarray(-32);
  const client=new Client({origin,key:importKey({version:1,private_key:base64url(seed),public_key:base64url(pub)}),allowInsecureLoopback:true});
  const send=command=>client.send(client.prepare(command));
  const fingerprint=crypto.createHash('sha256').update(pub).digest('hex');
  const tag=crypto.randomBytes(3).toString('hex');
  const room='code-'+tag, styled='codestyled-'+tag;
  const anon=async text=>(await (await fetch(origin+'/w/'+room+'/main?format=json',{method:'POST',headers:{'Content-Type':'text/plain'},body:text})).json()).receipt.id;
  // A plain post with a fence, inline code, and a hostile block.
  const goBody='package main\n\nfunc main() {\n\tprintln("<b>hi</b>") // https://a.example/\n}';
  const hostile='<img src=x onerror="window.__pwned=1"><script>window.__pwned=2</script>';
  const codeText='Run `go vet ./...` first:\n```go\n'+goBody+'\n```\nThen:\n```html\n'+hostile+'\n```\n# not a heading';
  const codeID=await anon(codeText);
  const longBody=Array.from({length:40},(_,i)=>'line_'+i+' = '+i+'  # '+'x'.repeat(i===7?300:10)).join('\n');
  const longID=await anon('```python\n'+longBody+'\n```');
  const jsonText='{"task":"summarise","limits":{"words":120,"lang":"en"},"sources":[1,2,3]}';
  const jsonID=await anon(jsonText);
  const signedID=(await send({operation:'post',room,page:'main',text:'Signed, with a link https://example.com/docs and `code`.'})).receipt.id;
  // A styled room: its page must look the same in light and dark mode.
  await send({operation:'room.create',room:styled});
  await send({operation:'room.style.set',room:styled,data:JSON.stringify({css:':scope{--surface:#fdf6e3;--ink:#073642} .post-text{font-family:Georgia,serif}'})});
  const styledID=(await send({operation:'post',room:styled,page:'main',text:'Styled room post https://example.com/x with ```\ncode\n```'})).receipt.id;
  const browser=await chromium.launch({headless:true,...(process.env.CHROMIUM_PATH?{executablePath:process.env.CHROMIUM_PATH}:{}),args:process.env.PLAYWRIGHT_NO_SANDBOX==='true'?['--no-sandbox']:[]});
  const errors=[],csp=[];
  const open=async(options={})=>{
    const context=await browser.newContext({viewport:{width:1280,height:900},...options});
    if(options.javaScriptEnabled!==false)await context.grantPermissions(['clipboard-read','clipboard-write'],{origin});
    const page=await context.newPage();
    page.on('pageerror',e=>errors.push(e.message));
    page.on('console',m=>{if(/Content Security Policy/i.test(m.text()))csp.push(m.text());});
    return {context,page};
  };
  try{
    // ---- no scripts: a clean code block, nothing added ----
    {
      const {context,page}=await open({javaScriptEnabled:false});
      await page.goto(origin+'/e/'+codeID);
      const body=page.locator('#e-'+codeID+' .memo-text');
      assert.equal(await body.evaluate(e=>e.tagName),'DIV','a body holding blocks is a div');
      assert.equal(await body.locator('pre > code[data-lang="go"]').textContent(),goBody);
      assert.equal(await body.locator('pre > code[data-lang="html"]').textContent(),hostile);
      assert.deepEqual(await body.locator(':scope > code').allTextContents(),['go vet ./...']);
      assert.equal(await body.locator('h1,h2,h3,h4,img,script').count(),0,'no heading or markup from the post');
      assert.equal(await body.locator('pre a').count(),0,'a URL in code stays text');
      assert.equal(await page.locator('.copy-button,.code-unfold,.hljs-keyword').count(),0,'copy, fold and colour need scripts');
      assert.match(await body.textContent(),/# not a heading$/);
      await context.close();
    }
    // ---- highlighting, copy, fold, JSON, identifiers, times ----
    const {context,page}=await open();
    await page.goto(origin+'/e/'+codeID);
    const body=page.locator('#e-'+codeID+' .memo-text');
    const go=body.locator('code[data-lang="go"]'),html=body.locator('code[data-lang="html"]');
    await page.waitForFunction(()=>document.querySelectorAll('code[data-highlighted]').length>=2);
    assert.equal(await go.getAttribute('data-highlighted'),'go');
    assert.ok(await go.locator('.hljs-keyword').count()>0,'Go keywords are coloured');
    assert.equal(await go.textContent(),goBody,'highlighting never changes the text');
    assert.equal(await html.textContent(),hostile);
    for(const code of [go,html])assert.ok(await code.evaluate(c=>[...c.querySelectorAll('*')].every(e=>e.tagName==='SPAN'&&/^(hljs-|language-|[a-z]+_)/.test(e.className)&&e.attributes.length===1)),'only class-bearing spans');
    assert.equal(await page.evaluate(()=>window.__pwned),undefined,'author markup never runs');
    assert.equal(await page.locator('.memo-text img,.memo-text script').count(),0);
    assert.equal(await page.locator('script[src="/assets/highlight.js"]').count(),1,'the highlighter loads once, from our own assets');
    // Copy: a labelled button that copies the block's exact text, by mouse or keyboard.
    const copy=body.locator('.copy-example').first().getByRole('button',{name:'Copy'});
    await copy.click();
    assert.equal(await page.evaluate(()=>navigator.clipboard.readText()),goBody);
    assert.match(await page.locator('#toast[role="status"]').textContent(),/Copied/);
    await page.evaluate(()=>navigator.clipboard.writeText(''));
    await body.locator('.copy-example').nth(1).getByRole('button',{name:'Copy'}).focus();await page.keyboard.press('Enter');
    assert.equal(await page.evaluate(()=>navigator.clipboard.readText()),hostile);
    // Times: the server's words, the exact UTC on hover, and on focus of the permalink.
    const time=page.locator('#e-'+codeID+' a.memo-time time[data-rel]');
    const raw=await (await fetch(origin+'/e/'+codeID,{headers:{Accept:'text/html'}})).text();
    assert.match(raw,/<time datetime="[^"]+" title="\d{4}-\d\d-\d\d \d\d:\d\d UTC" data-rel>(just now|\d+ min ago)<\/time>/);
    assert.match(await time.textContent(),/^(just now|\d+ min ago)$/);
    assert.match(await time.getAttribute('title'),/ UTC$/);
    await page.locator('#e-'+codeID+' a.memo-time').focus();
    assert.match(await time.evaluate(e=>getComputedStyle(e,'::after').content),/UTC/);
    // A permalink on a conversation page copies as a full URL.
    await page.locator('.permalink .copy-id').click();
    assert.equal(await page.evaluate(()=>navigator.clipboard.readText()),origin+'/e/'+codeID);
    // Long code folds; its lines scroll inside the block, never the page, at 320 px.
    await page.goto(origin+'/e/'+longID);
    const fold=page.locator('#e-'+longID+' .copy-example');
    assert.ok(await fold.evaluate(e=>e.classList.contains('code-folded')));
    const pre=fold.locator('pre');
    assert.ok(await pre.evaluate(p=>p.scrollHeight>p.clientHeight+50),'folded');
    const more=fold.getByRole('button',{name:'Show all 40 lines'});
    assert.equal(await more.getAttribute('aria-expanded'),'false');
    await more.click();
    assert.equal(await fold.getByRole('button',{name:'Show fewer lines'}).getAttribute('aria-expanded'),'true');
    assert.ok(await pre.evaluate(p=>p.scrollHeight<=p.clientHeight+1),'unfolded');
    await page.setViewportSize({width:320,height:700});
    assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=320),'no page overflow at 320 px');
    assert.ok(await pre.evaluate(p=>p.scrollWidth>p.clientWidth),'the long line scrolls inside its block');
    await page.setViewportSize({width:1280,height:900});
    // Pretty JSON: shown indented and coloured, copied as sent.
    await page.goto(origin+'/e/'+jsonID);
    const json=page.locator('#e-'+jsonID+' code[data-lang="json"]');
    assert.equal(await json.textContent(),JSON.stringify(JSON.parse(jsonText),null,2));
    await page.waitForFunction(id=>document.querySelector('#e-'+id+' code[data-lang="json"]')?.dataset.highlighted==='json',jsonID);
    await page.locator('#e-'+jsonID+' .copy-example button').click();
    assert.equal(await page.evaluate(()=>navigator.clipboard.readText()),jsonText);
    const api=await (await fetch(origin+'/e/'+jsonID+'?format=json')).json();
    assert.ok(JSON.stringify(api).includes(JSON.stringify(jsonText)),'the API keeps the text as sent');
    // An agent's fingerprint is one click to copy, whole.
    await page.goto(origin+'/agent/'+fingerprint);
    await page.locator('p.fingerprint + .copy-id').click();
    assert.equal(await page.evaluate(()=>navigator.clipboard.readText()),fingerprint);
    await context.close();
    // ---- dark mode ----
    const look=async(scheme,path,selectors)=>{
      const {context,page}=await open({colorScheme:scheme});
      await page.goto(origin+path);await page.waitForFunction(()=>document.readyState==='complete');
      const styles=await page.evaluate(selectors=>Object.fromEntries(selectors.map(s=>{const e=document.querySelector(s);if(!e)return [s,null];const c=getComputedStyle(e);return [s,[c.color,c.backgroundColor,c.borderTopColor,c.fontFamily].join(' | ')];})),selectors);
      const shot=await page.locator('#e-'+(path.includes(styled)?styledID:signedID)).screenshot();
      await context.close();return {styles,shot};
    };
    // An unstyled room follows the reader's scheme.
    const chrome=['body','.site-nav a','#e-'+signedID,'#e-'+signedID+' .memo-text','#e-'+signedID+' .author','#e-'+signedID+' .link-host','#e-'+signedID+' .memo-text code'];
    const light=await look('light','/r/'+room,chrome),dark=await look('dark','/r/'+room,chrome);
    assert.notEqual(light.styles.body,dark.styles.body,'the site has a dark mode');
    // Tokens are OKLCH, so computed colours serialize as oklch(); the dark
    // surface is the near-black rgb(19, 19, 18) once rendered to sRGB.
    const surface=await (async()=>{const {context,page}=await open({colorScheme:'dark'});await page.goto(origin+'/r/'+room);
      const c=await page.evaluate(()=>{const x=document.createElement('canvas').getContext('2d');x.fillStyle=getComputedStyle(document.body).backgroundColor;x.fillRect(0,0,1,1);return [...x.getImageData(0,0,1,1).data];});
      await context.close();return c;})();
    assert.ok(surface[3]===255&&[19,19,18].every((v,i)=>Math.abs(surface[i]-v)<=3),'dark surface '+surface.join(','));
    // A styled room renders identically in both.
    const styledSelectors=['html','body','.site-nav a','#e-'+styledID,'#e-'+styledID+' .memo-text','#e-'+styledID+' .author','#e-'+styledID+' .link-host','#e-'+styledID+' pre','#room-style-strip'];
    const styledLight=await look('light','/r/'+styled,styledSelectors),styledDark=await look('dark','/r/'+styled,styledSelectors);
    assert.ok(styledLight.styles['#e-'+styledID+' .link-host'],'the styled post has its trust mark');
    assert.deepEqual(styledDark.styles,styledLight.styles,'a styled room ignores dark mode');
    assert.ok(styledDark.shot.equals(styledLight.shot),'a styled room post is pixel-identical in dark and light mode');
    // Trust marks keep their contrast in both modes and in a styled room.
    const contrast=async(scheme,path,selectors)=>{
      const {context,page}=await open({colorScheme:scheme});
      await page.goto(origin+path);await page.waitForFunction(()=>document.readyState==='complete');
      const ratios=await page.evaluate(selectors=>{
        // Canvas renders any CSS colour (oklch tokens included) to sRGB bytes.
        const ctx=document.createElement('canvas').getContext('2d',{willReadFrequently:true});
        const rgb=s=>{ctx.clearRect(0,0,1,1);ctx.fillStyle='rgba(0,0,0,0)';ctx.fillStyle=s;ctx.fillRect(0,0,1,1);const d=[...ctx.getImageData(0,0,1,1).data];return [d[0],d[1],d[2],d[3]/255];};
        const lum=([r,g,b])=>{const f=v=>{v/=255;return v<=.03928?v/12.92:((v+.055)/1.055)**2.4;};return .2126*f(r)+.7152*f(g)+.0722*f(b);};
        const bg=e=>{for(;e;e=e.parentElement){const c=rgb(getComputedStyle(e).backgroundColor);if(c.length<4||c[3]>0)return c;}return [255,255,255];};
        return Object.fromEntries(selectors.map(s=>{const e=document.querySelector(s);if(!e)return [s,0];const a=lum(rgb(getComputedStyle(e).color)),b=lum(bg(e));return [s,(Math.max(a,b)+.05)/(Math.min(a,b)+.05)];}));
      },selectors);
      await context.close();return ratios;
    };
    const marks=['#e-'+signedID+' .author','#e-'+signedID+' .link-host','#e-'+signedID+' .via','#e-'+signedID+' .memo-text'];
    for(const scheme of ['light','dark'])for(const [mark,ratio] of Object.entries(await contrast(scheme,'/r/'+room,marks)))assert.ok(ratio>=4.5,`${mark} contrast ${ratio.toFixed(2)} in ${scheme} mode`);
    const styledMarks=['#e-'+styledID+' .author','#e-'+styledID+' .link-host'];
    for(const scheme of ['light','dark'])for(const [mark,ratio] of Object.entries(await contrast(scheme,'/r/'+styled,styledMarks)))assert.ok(ratio>=4.5,`${mark} contrast ${ratio.toFixed(2)} in a styled room, ${scheme} mode`);
    assert.deepEqual(csp,[],'nothing breaks the CSP');
    assert.deepEqual(errors,[]);
    console.log('code blocks and polish: ok');
  }finally{await browser.close();}
})().catch(error=>{console.error(error);process.exit(1);});
