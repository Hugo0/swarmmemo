// Run against an isolated local candidate started with INBOX_ENTRIES=read, never production.
// PLAYWRIGHT_MODULE=/path/to/playwright SWARMMEMO_TEST_URL=http://127.0.0.1:8094 node internal/web/inbox_waiting_test.cjs
// C71: Me lists what waits for this key's answer, marks one done (updates.dispose, private),
// and the Me count follows data.waiting.
const {chromium}=require(process.env.PLAYWRIGHT_MODULE||'playwright');
const assert=require('node:assert/strict');
const crypto=require('node:crypto');
const fields=['operation','room','page','text','kind','reply_to','to','request_id','public_key','timestamp','nonce','handle','visibility','members','target','amount','ttl','message_id','cursor','limit','query','before','reason','data','filename','media_type','attachments'];
const canonical=c=>{const ordered={};for(const f of fields)if(c[f]!==undefined&&c[f]!==''&&c[f]!==0&&(!Array.isArray(c[f])||c[f].length))ordered[f]=c[f];return Buffer.from(JSON.stringify({version:1,service:'swarmmemo.com',command:ordered}).replace(/\u2028/g,'\\u2028').replace(/\u2029/g,'\\u2029'));};
function key(){const pair=crypto.generateKeyPairSync('ed25519');const raw=pair.publicKey.export({format:'der',type:'spki'}).subarray(-32);return {...pair,public_key:raw.toString('base64url'),private_key:pair.privateKey.export({format:'der',type:'pkcs8'}).subarray(-32).toString('base64url'),fingerprint:crypto.createHash('sha256').update(raw).digest('hex'),version:1,service:'swarmmemo.com'};}
function sign(key,c){const command={...c,public_key:key.public_key,timestamp:Math.floor(Date.now()/1000),nonce:crypto.randomUUID()};command.signature=crypto.sign(null,canonical(command),key.privateKey).toString('base64url');return command;}
(async()=>{
  const browser=await chromium.launch({headless:true,...(process.env.CHROMIUM_PATH?{executablePath:process.env.CHROMIUM_PATH}:{}),args:process.env.PLAYWRIGHT_NO_SANDBOX==='true'?['--no-sandbox']:[]});
  const url=process.env.SWARMMEMO_TEST_URL||'http://127.0.0.1:8094';
  const context=await browser.newContext({viewport:{width:1280,height:900}}),page=await context.newPage();
  const errors=[];page.on('pageerror',e=>errors.push(e.message));
  const call=async c=>{const response=await context.request.post(url+'/v1/command',{data:c});const result=await response.json();assert.equal(response.status(),200,JSON.stringify(result));return result;};
  const signed=async(k,c)=>call(sign(k,{...c,request_id:crypto.randomUUID()}));
  try{
    const caps=await (await context.request.get(url+'/capabilities')).json();
    if(caps.inbox?.enabled!==true||caps.agent_return?.entries?.enabled!==true)
      throw new Error('MISCONFIGURED: '+url+' does not read the inbox entry log (/capabilities inbox.enabled='+caps.inbox?.enabled+', agent_return.entries.enabled='+caps.agent_return?.entries?.enabled+'). Start this candidate with INBOX_ENTRIES=read.');
    const me=key(),sender=key();
    for(const k of [me,sender])await signed(k,{operation:'agent.register',handle:'wait-'+k.fingerprint.slice(0,10)});
    await signed(sender,{operation:'post',text:'A question for you <img src=x onerror=alert(1)>',to:me.fingerprint});
    await signed(sender,{operation:'post',text:'Another question',to:me.fingerprint});
    await context.addInitScript(value=>localStorage.setItem('swarmmemo.identity.v1',JSON.stringify(value)),{version:1,service:'swarmmemo.com',public_key:me.public_key,private_key:me.private_key,fingerprint:me.fingerprint,handle:''});
    await page.goto(url+'/me#messages');
    await page.locator('#me-waiting .me-waiting-item').first().waitFor();
    assert.equal(await page.locator('#me-waiting .me-waiting-item').count(),2);
    assert.equal(await page.locator('#me-waiting img,#me-waiting script').count(),0,'previews stay text');
    await page.locator('#me-waiting-count:not([hidden])').waitFor();
    assert.equal((await page.locator('#me-waiting-count').textContent()).trim(),'2');
    await page.locator('#me-waiting .me-waiting-item').first().getByRole('button',{name:'Answered elsewhere'}).click();
    await page.waitForFunction(()=>/Marked done/.test(document.getElementById('me-waiting-status').textContent));
    assert.equal(await page.locator('#me-waiting .me-waiting-item').count(),1);
    await page.waitForFunction(()=>document.getElementById('me-waiting-count').textContent.trim()==='1');
    await page.setViewportSize({width:320,height:780});assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth),'320px waiting list overflow');
    await page.screenshot({path:'/tmp/swarmmemo-me-waiting-mobile.png',fullPage:true});
    assert.deepEqual(errors,[]);
    console.log('inbox waiting browser test passed');
  }finally{await browser.close();}
})().catch(error=>{console.error(error);process.exit(1);});
