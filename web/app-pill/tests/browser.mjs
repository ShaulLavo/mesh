import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { mkdir, readFile } from 'node:fs/promises';
import { resolve } from 'node:path';
import { chromium, webkit } from 'playwright';

const assets = new URL('../../../internal/apppill/assets/', import.meta.url);
const manager = 'https://apps.shaulavo.dev';
const artifacts = process.env.MESH_PILL_ARTIFACTS_DIR;
if (artifacts) await mkdir(resolve(artifacts), { recursive: true });

const script = await readFile(new URL('pill.js', assets));
const stylesheet = await readFile(new URL('pill.css', assets));
let origin;
const server = createServer((request, response) => {
  response.setHeader('Cache-Control', 'no-store');
  if (request.url === '/.mesh-app/pill.js') {
    response.setHeader('Content-Type', 'application/javascript; charset=utf-8');
    response.end(script);
    return;
  }
  if (request.url === '/.mesh-app/pill.css') {
    response.setHeader('Content-Type', 'text/css; charset=utf-8');
    response.end(stylesheet);
    return;
  }
  response.setHeader('Content-Type', 'text/html; charset=utf-8');
  response.setHeader('Content-Security-Policy', `default-src 'none'; script-src 'nonce-test'; style-src ${origin}/.mesh-app/pill.css; frame-src ${manager}`);
  response.end(`<!doctype html><html><head><meta name="viewport" content="width=device-width,initial-scale=1"><script defer nonce="test" data-mesh-app="7k3d" data-mesh-manager="${manager}" src="/.mesh-app/pill.js"></script></head><body><h1>Mesh pill interaction harness</h1><p>The owner frame is mocked. Authorization has separate Go integration tests.</p></body></html>`);
});

await new Promise((resolveListen, reject) => {
  server.once('error', reject);
  server.listen(0, '127.0.0.1', resolveListen);
});
const address = server.address();
assert(address && typeof address === 'object');
origin = `http://127.0.0.1:${address.port}`;

async function screenshot(page, name) {
  if (artifacts) await page.screenshot({ path: resolve(artifacts, `${name}.png`) });
}

async function checkInteractions(browser, name, reducedMotion) {
  const context = await browser.newContext({ viewport: { width: 390, height: 844 }, hasTouch: true, reducedMotion });
  try {
    const page = await context.newPage();
    page.setDefaultTimeout(5000);
    const errors = [];
    page.on('pageerror', error => errors.push(error.message));
    await page.route(`${manager}/**`, route => route.fulfill({
      status: 200,
      contentType: 'text/html; charset=utf-8',
      body: `<!doctype html><html><head><style>body{margin:0;padding:16px;color:#eef0f2;font:13px system-ui;background:#18181b}a{color:#a5b4fc}small{display:block;margin-bottom:12px}</style></head><body><small>Mock owner frame</small><a target="_blank" rel="noopener" href="${manager}/">Owner sign-in</a><script>parent.postMessage({type:'mesh-app-status',visibility:'public'},${JSON.stringify(origin)})</script></body></html>`,
    }));
    await page.goto(origin);
    const pill = page.locator('mesh-app-pill');
    const handle = pill.locator('.handle');
    await handle.waitFor({ state: 'visible' });
    await page.waitForFunction(() => { const bounds = document.querySelector('mesh-app-pill').shadowRoot.querySelector('.handle').getBoundingClientRect(); return bounds.left >= 0 && bounds.right <= innerWidth && bounds.top >= 0 && bounds.bottom <= innerHeight; });
    const box = await handle.boundingBox();
    assert(box && box.width >= 44 && box.height >= 44, 'Collapsed dot requires a 44px touch target');
    await handle.click();
    await pill.locator('.controls').waitFor({ state: 'visible' });
    await pill.locator('[aria-label="Owner controls"]').click();
    await page.waitForFunction(() => document.querySelector('mesh-app-pill').shadowRoot.querySelector('.status').textContent === 'Public');
    assert.equal(await pill.locator('iframe').getAttribute('src'), `${manager}/frame?id=7k3d`);
    await page.evaluate(() => {
      for (const message of [
        { origin: location.origin, source: window, data: { type: 'mesh-app-status', visibility: 'private' } },
        { origin: 'https://apps.shaulavo.dev', source: window, data: { type: 'mesh-app-status', visibility: 'private' } },
      ]) window.dispatchEvent(new MessageEvent('message', message));
    });
    assert.equal(await pill.locator('.status').textContent(), 'Public', 'App messages must not impersonate the owner frame');
    if (reducedMotion === 'reduce') {
      assert.equal(await pill.locator('.shell').evaluate(element => getComputedStyle(element).transitionDuration), '0s');
      assert.equal(await pill.locator('.controls').evaluate(element => getComputedStyle(element).animationName), 'none');
    }
    await screenshot(page, `${name}-${reducedMotion}-expanded`);
    await handle.click();
    await pill.locator('.controls').waitFor({ state: 'detached' });
    await handle.hover();
    const dragStart = await handle.boundingBox();
    assert(dragStart);
    await page.mouse.move(dragStart.x + 22, dragStart.y + 22);
    await page.mouse.down();
    await page.mouse.move(25, dragStart.y + 22, { steps: 12 });
    await page.mouse.up();
    await page.waitForTimeout(320);
    const snappedMouse = await handle.boundingBox();
    assert(snappedMouse.x < 40, `Collapsed dot should snap left after dragging: ${JSON.stringify(snappedMouse)}, ${await page.evaluate(() => localStorage.getItem('mesh-app-pill-position'))}`);
    await handle.focus();
    await page.keyboard.press('Alt+ArrowRight');
    await page.waitForTimeout(100);
    assert((await handle.boundingBox()).x > 300, 'Keyboard docking should move the dot right');
    await handle.evaluate(element => {
      const bounds = element.getBoundingClientRect();
      element.dispatchEvent(new PointerEvent('pointerdown', { bubbles: true, composed: true, button: 0, clientX: bounds.x + 22, clientY: bounds.y + 22, pointerType: 'touch' }));
    });
    await page.waitForTimeout(50);
    await page.evaluate(() => window.dispatchEvent(new PointerEvent('pointermove', { clientX: 25, clientY: 180, pointerType: 'touch' })));
    await page.waitForTimeout(120);
    await page.evaluate(() => window.dispatchEvent(new PointerEvent('pointerup', { pointerType: 'touch' })));
    await page.waitForTimeout(320);
    assert((await handle.boundingBox()).x < 40, 'Touch pointer dragging should snap left');
    await handle.focus();
    await page.keyboard.press('Alt+ArrowRight');
    await page.reload();
    await handle.waitFor({ state: 'visible' });
    await page.waitForFunction(() => { const bounds = document.querySelector('mesh-app-pill').shadowRoot.querySelector('.handle').getBoundingClientRect(); return bounds.left >= 0 && bounds.right <= innerWidth && bounds.top >= 0 && bounds.bottom <= innerHeight; });
    assert((await handle.boundingBox()).x > 300, 'Docked position should survive reload');
    const touchTarget = await handle.boundingBox();
    assert(touchTarget);
    await page.touchscreen.tap(touchTarget.x + 22, touchTarget.y + 22);
    await pill.locator('.controls').waitFor({ state: 'visible' });
    await handle.focus();
    await page.keyboard.press('Escape');
    await pill.locator('.controls').waitFor({ state: 'detached' });
    await screenshot(page, `${name}-${reducedMotion}-collapsed`);
    assert.deepEqual(errors, [], 'The pill must not throw browser errors');
    console.log(`${name} ${reducedMotion}: strict CSP, expansion, frame messages, mouse/touch drag, keyboard docking, and persistence passed`);
  } finally {
    await context.close();
  }
}

try {
  for (const [name, engine] of [['chromium', chromium], ['webkit', webkit]]) {
    const options = name === 'chromium' && process.env.MESH_CHROMIUM_EXECUTABLE
      ? { executablePath: process.env.MESH_CHROMIUM_EXECUTABLE }
      : {};
    const browser = await engine.launch({ headless: true, ...options });
    try {
      await checkInteractions(browser, name, 'no-preference');
      await checkInteractions(browser, name, 'reduce');
    } finally {
      await browser.close();
    }
  }
} finally {
  await new Promise((resolveClose, reject) => server.close(error => error ? reject(error) : resolveClose()));
}
