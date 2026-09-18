// Attached images render inline, safely: bytes decide the served type, SVG and
// mislabelled files stay downloads, and the gallery works without JavaScript.
const {chromium} = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const assert = require('node:assert/strict');
const {curator} = require('./home_density_test.cjs');
const zlib = require('node:zlib');

// A minimal 2x2 PNG, built here so the test owns its fixture.
function png() {
  const chunk = (type, body) => {
    const head = Buffer.alloc(8);
    head.writeUInt32BE(body.length, 0);
    head.write(type, 4, 'ascii');
    const crc = Buffer.alloc(4);
    crc.writeUInt32BE(zlib.crc32(Buffer.concat([Buffer.from(type, 'ascii'), body])) >>> 0, 0);
    return Buffer.concat([head, body, crc]);
  };
  const ihdr = Buffer.alloc(13);
  ihdr.writeUInt32BE(2, 0); ihdr.writeUInt32BE(2, 4);
  ihdr[8] = 8; ihdr[9] = 2; // 8-bit truecolour
  const raw = Buffer.concat([Buffer.from([0, 255, 0, 0, 0, 0, 255]), Buffer.from([0, 0, 0, 255, 255, 255, 0])]);
  return Buffer.concat([
    Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]),
    chunk('IHDR', ihdr), chunk('IDAT', zlib.deflateSync(raw)), chunk('IEND', Buffer.alloc(0)),
  ]);
}

(async () => {
  const browser = await chromium.launch({headless: true, ...(process.env.CHROMIUM_PATH ? {executablePath: process.env.CHROMIUM_PATH} : {})});
  const origin = process.env.SWARMMEMO_TEST_URL || 'http://127.0.0.1:8089';
  const context = await browser.newContext({viewport: {width: 1280, height: 1000}});
  const page = await context.newPage();
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  try {
    const send = await curator(origin);
    const b64 = data => data.toString('base64url').replace(/=+$/, '');
    const blob = async (filename, mediaType, data) =>
      (await send({operation: 'blob.put', room: 'images-test', filename, media_type: mediaType, data: b64(data), ttl: 3600})).data.blob.id;

    // blob.put needs the room to exist, so open it with a message first.
    await send({operation: 'post', room: 'images-test', page: 'main', text: 'Opening the room for attachment tests.'});

    const one = await blob('meme.png', 'image/png', png());
    const two = await blob('second.png', 'image/png', png());
    const notes = await blob('notes.txt', 'text/plain', Buffer.from('plain text, not an image'));

    // An image type must be borne out by its bytes, and SVG is never accepted.
    for (const [name, type, data] of [
      ['fake.png', 'image/png', Buffer.from('<!doctype html><script>alert(1)</script>')],
      ['drawing.svg', 'image/svg+xml', Buffer.from('<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>')],
    ]) {
      await assert.rejects(() => blob(name, type, data), error => error.status === 400 && error.code === 'invalid_image', `${type} was accepted`);
    }

    const single = (await send({operation: 'post', room: 'images-test', page: 'main', text: 'One image.', attachments: [one]})).receipt.id;
    const many = (await send({operation: 'post', room: 'images-test', page: 'main', text: 'Two images and a file.', attachments: [one, two, notes]})).receipt.id;

    await page.goto(origin + '/r/images-test');
    const gallery = page.locator(`#e-${many} .memo-images`);
    assert.equal(await gallery.locator('.memo-image').count(), 2, 'only the images are in the gallery');
    assert.ok(await gallery.evaluate(e => e.classList.contains('multi')), 'multiple images use the grid');
    assert.equal(await page.locator(`#e-${single} .memo-images .memo-image`).count(), 1);
    assert.match(await gallery.locator('img').first().getAttribute('alt'), /meme\.png/, 'alt names the file');
    assert.equal(await gallery.locator('img').first().getAttribute('loading'), 'lazy');
    assert.ok(await gallery.locator('img').first().evaluate(i => i.complete && i.naturalWidth > 0), 'the image actually decoded');

    // Served inline as its real type; the text file keeps the download path.
    const image = await context.request.get(origin + '/a/' + one);
    assert.equal(image.headers()['content-type'], 'image/png');
    assert.match(image.headers()['content-disposition'], /^inline/);
    assert.equal(image.headers()['x-content-type-options'], 'nosniff');
    assert.match(image.headers()['content-security-policy'], /sandbox/);
    const text = await context.request.get(origin + '/a/' + notes);
    assert.equal(text.headers()['content-type'], 'application/octet-stream');
    assert.match(text.headers()['content-disposition'], /^attachment/);

    // The lightbox opens, steps between images, and closes on Escape.
    await gallery.locator('.memo-image').first().click();
    const dialog = page.locator('dialog.lightbox');
    await dialog.waitFor({state: 'visible'});
    assert.equal(await dialog.locator('.lightbox-count').textContent(), '1 / 2');
    await page.keyboard.press('ArrowRight');
    assert.equal(await dialog.locator('.lightbox-count').textContent(), '2 / 2');
    assert.match(await dialog.locator('img').getAttribute('src'), new RegExp(two));
    await page.keyboard.press('Escape');
    await dialog.waitFor({state: 'hidden'});

    // Without JavaScript the thumbnail is still a link to the file.
    const plain = await browser.newContext({javaScriptEnabled: false});
    const plainPage = await plain.newPage();
    await plainPage.goto(origin + '/r/images-test');
    assert.equal(await plainPage.locator(`#e-${single} .memo-image`).getAttribute('href'), '/a/' + one);
    assert.equal(await plainPage.locator('dialog.lightbox').count(), 0);
    await plain.close();

    assert.deepEqual(errors, []);
    console.log('PASS: inline gallery, multi-image grid, real decode, inline vs download headers, lightbox keys, no-JS links, SVG and mislabelled bytes refused.');
  } finally {
    await browser.close();
  }
})().catch(error => {console.error(error); process.exitCode = 1;});
