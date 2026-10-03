// Explicit disposable local server only (SWARMMEMO_TEST_URL); reads, never writes.
// The launch film on the /messages guide (it is not a home-page highlight): the frame holds its aspect ratio before the video loads (no
// layout shift), the browser picks the 1:1 cut on a phone and the 16:9 cut otherwise, it plays muted
// and inline by itself, the sound button unmutes it, reduced motion leaves the poster under a play
// button, no page scrolls sideways at 390 px, and the page logs no errors (CSP included) in light or
// dark. H.264 and AAC need a browser with those codecs: set CHROMIUM_PATH to Chrome (not Chromium).
// SWARMMEMO_TEST_SHOTS=<dir> also saves a screenshot of each page and size.
const assert=require('node:assert/strict');
const {chromium}=require(process.env.PLAYWRIGHT_MODULE||'playwright');
const origin=process.env.SWARMMEMO_TEST_URL;
assert.match(origin||'',/^http:\/\/127\.0\.0\.1:\d+$/);
const shots=process.env.SWARMMEMO_TEST_SHOTS;

(async()=>{
  const browser=await chromium.launch({executablePath:process.env.CHROMIUM_PATH||undefined,args:['--autoplay-policy=no-user-gesture-required']});
  try{
    for(const scheme of ['light','dark'])for(const [w,h] of [[1280,900],[390,844]])for(const path of ['/messages']){
      const ctx=await browser.newContext({viewport:{width:w,height:h},colorScheme:scheme,deviceScaleFactor:1});
      const page=await ctx.newPage(),errors=[];
      page.on('pageerror',e=>errors.push(String(e)));
      page.on('console',m=>{if(m.type()==='error')errors.push(m.text());});
      await page.goto(origin+path,{waitUntil:'domcontentloaded'});
      const at=`${path} ${w}px ${scheme}`;
      const box0=await page.locator('.film-frame').boundingBox();
      assert.ok(box0&&box0.width>0,`${at}: no film frame`);
      const ratio=w<=600?1:16/9;
      assert.ok(Math.abs(box0.width/box0.height-ratio)<0.02,`${at}: frame ratio ${(box0.width/box0.height).toFixed(3)}`);
      const canPlay=await page.evaluate(()=>document.createElement('video').canPlayType('video/mp4; codecs="avc1.640028, mp4a.40.2"'));
      if(canPlay){
        await page.waitForFunction(()=>{const v=document.querySelector('.film-video');return v&&!v.paused&&v.currentTime>0.2;},null,{timeout:15000});
        const st=await page.evaluate(()=>{const v=document.querySelector('.film-video');return {muted:v.muted,src:v.currentSrc,controls:v.controls,inline:v.playsInline,loop:v.loop,poster:v.poster};});
        assert.ok(st.muted&&!st.controls&&st.inline&&st.loop,`${at}: ${JSON.stringify(st)}`);
        assert.ok(st.src.endsWith(w<=600?'/assets/film-1x1.mp4':'/assets/film-16x9.mp4'),`${at}: picked ${st.src}`);
        assert.ok(st.poster.endsWith(w<=600?'film-poster-1x1.jpg':'film-poster-16x9.jpg'),`${at}: poster ${st.poster}`);
        const box1=await page.locator('.film-frame').boundingBox();
        assert.deepEqual([box1.x,box1.y,box1.width,box1.height].map(Math.round),[box0.x,box0.y,box0.width,box0.height].map(Math.round),`${at}: the frame moved once the video loaded`);
        assert.ok(await page.locator('.film-sound').isVisible()&&await page.locator('.film-play').isHidden(),`${at}: controls`);
        if(shots)await page.screenshot({path:`${shots}/film${path==='/'?'-home':path.replace('/','-')}-${w}-${scheme}.png`,fullPage:false});
        await page.locator('.film-sound').click();
        assert.equal(await page.evaluate(()=>document.querySelector('.film-video').muted),false,`${at}: sound button`);
        assert.equal(await page.locator('.film-sound').getAttribute('aria-pressed'),'true');
        assert.equal(await page.locator('.film-sound').textContent(),'Sound off');
      } else console.log(`${at}: this browser has no H.264/AAC; playback not checked (use Chrome)`);
      assert.ok(await page.locator('.film a[href="/assets/film-transcript.txt"]').isVisible(),`${at}: transcript link`);
      assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=document.documentElement.clientWidth),`${at}: scrolls sideways`);
      assert.deepEqual(errors,[],`${at}: errors`);
      await ctx.close();
    }
    // Reduced motion: no autoplay; the poster waits under a play button, which starts it with sound.
    const ctx=await browser.newContext({viewport:{width:1280,height:900},reducedMotion:'reduce'});
    const page=await ctx.newPage();
    await page.goto(origin+'/messages',{waitUntil:'load'});
    await page.waitForTimeout(800);
    assert.equal(await page.evaluate(()=>document.querySelector('.film-video').paused),true,'reduced motion: autoplayed');
    assert.ok(await page.locator('.film-play').isVisible()&&await page.locator('.film-sound').isHidden(),'reduced motion: play button');
    if(shots)await page.screenshot({path:`${shots}/film-messages-1280-reduced-motion.png`});
    await ctx.close();
    console.log('film: ok');
  }finally{await browser.close();}
})().catch(e=>{console.error(e);process.exit(1);});
