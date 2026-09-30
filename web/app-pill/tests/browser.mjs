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

async function checkCopyAndViewport(page, pill) {
  await page.evaluate(() => {
    window.copyWrites = [];
    window.rejectCopy = false;
    window.prompt = () => { throw new Error('Copy must never open a native input'); };
    Object.defineProperty(navigator, 'clipboard', { configurable: true, value: {
      writeText: async text => {
        if (window.rejectCopy) throw new Error('Clipboard unavailable');
        window.copyWrites.push(text);
      },
    } });
  });
  await pill.getByRole('button', { name: 'Copy link', exact: true }).click();
  await pill.getByRole('button', { name: 'Link copied', exact: true }).waitFor();
  assert.deepEqual(await page.evaluate(() => window.copyWrites), [origin]);
  await page.evaluate(() => { window.rejectCopy = true; });
  await pill.getByRole('button', { name: 'Link copied', exact: true }).click();
  await pill.getByText('Couldn’t copy. Tap to try again.', { exact: true }).waitFor();
  await page.evaluate(() => {
    Object.defineProperty(visualViewport, 'height', { configurable: true, value: innerHeight - 200 });
    Object.defineProperty(visualViewport, 'offsetTop', { configurable: true, value: 120 });
    visualViewport.dispatchEvent(new Event('resize'));
    visualViewport.dispatchEvent(new Event('scroll'));
  });
  assert.equal(await pill.locator('.shell').evaluate(element => getComputedStyle(element).transitionDuration), '0s', 'Safari viewport changes must not animate position');
  await page.waitForFunction(() => {
    const bounds = document.querySelector('mesh-app-pill').shadowRoot.querySelector('.shell').getBoundingClientRect();
    return bounds.bottom <= visualViewport.offsetTop + visualViewport.height;
  });
  await page.evaluate(() => {
    delete visualViewport.height;
    delete visualViewport.offsetTop;
    visualViewport.dispatchEvent(new Event('resize'));
  });
  await pill.getByText('Couldn’t copy. Tap to try again.', { exact: true }).waitFor({ state: 'hidden' });
}

async function checkLinkDrag(page, pill) {
  await pill.getByRole('link', { name: 'Make private' }).waitFor();
  const result = await page.evaluate(async () => {
    const shadow = document.querySelector('mesh-app-pill').shadowRoot;
    const link = shadow.querySelector('a.action');
    const bounds = link.getBoundingClientRect();
    link.dispatchEvent(new PointerEvent('pointerdown', { bubbles: true, composed: true, button: 0, clientX: bounds.x + bounds.width / 2, clientY: bounds.y + bounds.height / 2 }));
    window.dispatchEvent(new PointerEvent('pointermove', { clientX: bounds.x + 60, clientY: bounds.y - 60 }));
    const dragged = shadow.querySelector('.shell').getBoundingClientRect();
    window.dispatchEvent(new MessageEvent('message', {
      origin: 'https://apps.shaulavo.dev', source: shadow.querySelector('iframe').contentWindow,
      data: { type: 'mesh-app-status', visibility: 'public', owns: true },
    }));
    await new Promise(resolve => requestAnimationFrame(resolve));
    const refreshed = shadow.querySelector('.shell').getBoundingClientRect();
    window.dispatchEvent(new PointerEvent('pointerup'));
    return {
      prevented: !link.dispatchEvent(new MouseEvent('click', { bubbles: true, composed: true, cancelable: true })),
      stayedUnderPointer: dragged.x === refreshed.x && dragged.y === refreshed.y,
    };
  });
  assert(result.prevented, 'Releasing a drag on the lock must cancel link navigation');
  assert(result.stayedUnderPointer, 'An ownership response must not redock the pill during dragging');
  await page.waitForFunction(() => !document.querySelector('mesh-app-pill').shadowRoot.querySelector('.shell').classList.contains('snapping'));
  const popupRequested = page.waitForEvent('popup');
  await pill.getByRole('link', { name: 'Make private' }).click();
  const popup = await popupRequested;
  await popup.close();
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
      body: `<!doctype html><html><head><style>body{margin:0;padding:16px;color:#eef0f2;font:13px system-ui;background:#18181b}a{color:#a5b4fc}small{display:block;margin-bottom:12px}</style></head><body><small>Mock owner frame</small><a target="_blank" rel="noopener" href="${manager}/">Owner sign-in</a><script>parent.postMessage({type:'mesh-app-status',visibility:'public',owns:true},${JSON.stringify(origin)})</script></body></html>`,
    }));
    await page.goto(origin);
    const pill = page.locator('mesh-app-pill');
    const handle = pill.locator('[data-react-grab-toolbar-collapse]');
    await handle.waitFor({ state: 'visible' });
    await page.waitForFunction(() => { const bounds = document.querySelector('mesh-app-pill').shadowRoot.querySelector('[data-react-grab-toolbar-collapse]').getBoundingClientRect(); return bounds.left >= 0 && bounds.right <= innerWidth && bounds.top >= 0 && bounds.bottom <= innerHeight; });
    const box = await handle.boundingBox();
    assert(box, 'Collapse control must be visible');
    assert.equal(await handle.evaluate(element => getComputedStyle(element, '::before').minWidth), '24px');
    await handle.click();
    await pill.locator('.controls').waitFor({ state: 'visible' });
    await pill.locator('[aria-label="Make private"]').waitFor({ state: 'visible' });
    assert.equal(await pill.locator('[aria-label="Make private"]').getAttribute('href'), `${manager}/confirm?id=7k3d&action=private`);
    assert.equal(await pill.locator('button, a').count(), 3, 'Expanded pill has exactly three direct actions');
    assert.equal(await pill.locator('iframe').isVisible(), false, 'Authorization must never show a blank panel');
    assert.equal(await pill.locator('.status').count(), 0, 'No generic App label');
    assert.equal(await pill.locator('iframe').getAttribute('src'), `${manager}/frame?id=7k3d`);
    await checkCopyAndViewport(page, pill);
    await page.evaluate(() => {
      for (const message of [
        { origin: location.origin, source: window, data: { type: 'mesh-app-status', visibility: 'private', owns: true } },
        { origin: 'https://apps.shaulavo.dev', source: window, data: { type: 'mesh-app-status', visibility: 'private', owns: true } },
      ]) window.dispatchEvent(new MessageEvent('message', message));
    });
    assert.equal(await pill.locator('[aria-label="Make private"]').count(), 1, 'App messages must not impersonate the owner frame');
    if (reducedMotion === 'reduce') {
      assert.equal(await pill.locator('.shell').evaluate(element => getComputedStyle(element).transitionDuration), '0s');
      assert.equal(await pill.locator('.controls').evaluate(element => getComputedStyle(element).animationName), 'none');
    }
    await page.waitForFunction(() => { const panel = document.querySelector('mesh-app-pill').shadowRoot.querySelector('[data-react-grab-toolbar-panel]'); return Math.min(panel.offsetWidth, panel.offsetHeight) === 26; });
    await screenshot(page, `${name}-${reducedMotion}-expanded`);
    await handle.click();
    assert.equal(await handle.getAttribute('aria-expanded'), 'false');
    await handle.hover();
    const dragStart = await handle.boundingBox();
    assert(dragStart);
    await page.mouse.move(dragStart.x + dragStart.width / 2, dragStart.y + dragStart.height / 2);
    await page.mouse.down();
    await page.mouse.move(25, dragStart.y + dragStart.height / 2, { steps: 12 });
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
      element.dispatchEvent(new PointerEvent('pointerdown', { bubbles: true, composed: true, button: 0, clientX: bounds.x + bounds.width / 2, clientY: bounds.y + bounds.height / 2, pointerType: 'touch' }));
    });
    await page.waitForTimeout(50);
    await page.evaluate(() => window.dispatchEvent(new PointerEvent('pointermove', { clientX: 25, clientY: 180, pointerType: 'touch' })));
    await page.waitForTimeout(120);
    await page.evaluate(() => window.dispatchEvent(new PointerEvent('pointerup', { pointerType: 'touch' })));
    await page.waitForTimeout(320);
    assert((await handle.boundingBox()).x < 40, 'Touch pointer dragging should snap left');
    await handle.click();
    assert.equal(await handle.getAttribute('aria-expanded'), 'true', 'The first deliberate click after a drag must expand');
    await checkLinkDrag(page, pill);
    await handle.click();
    assert.equal(await handle.getAttribute('aria-expanded'), 'false');
    await handle.focus();
    await page.keyboard.press('Alt+ArrowRight');
    await page.reload();
    await handle.waitFor({ state: 'visible' });
    await page.waitForFunction(() => { const bounds = document.querySelector('mesh-app-pill').shadowRoot.querySelector('[data-react-grab-toolbar-collapse]').getBoundingClientRect(); return bounds.left >= 0 && bounds.right <= innerWidth && bounds.top >= 0 && bounds.bottom <= innerHeight; });
    assert((await handle.boundingBox()).x > 300, 'Docked position should survive reload');
    const touchTarget = await handle.boundingBox();
    assert(touchTarget);
    await page.touchscreen.tap(touchTarget.x + touchTarget.width / 2, touchTarget.y + touchTarget.height / 2);
    await pill.locator('.controls').waitFor({ state: 'visible' });
    await handle.focus();
    await page.keyboard.press('Escape');
    assert.equal(await handle.getAttribute('aria-expanded'), 'false');
    await screenshot(page, `${name}-${reducedMotion}-collapsed`);
    assert.deepEqual(errors, [], 'The pill must not throw browser errors');
    console.log(`${name} ${reducedMotion}: strict CSP, expansion, frame messages, mouse/touch drag, keyboard docking, and persistence passed`);
  } finally {
    await context.close();
  }
}

async function checkUnavailableOrVisitor(browser, name, unavailable) {
  const context = await browser.newContext({ viewport: { width: 390, height: 844 } });
  try {
    const page = await context.newPage();
    let resolveFrame;
    let owns = false;
    const frameRequested = new Promise(resolve => { resolveFrame = resolve; });
    await page.route(`${manager}/**`, async route => {
      if (unavailable) await route.abort('failed');
      else await route.fulfill({contentType: 'text/html', body: `<script>parent.postMessage({type:'mesh-app-status',visibility:'public',owns:${owns}},${JSON.stringify(origin)})</script>`});
      resolveFrame();
    });
    await page.goto(origin);
    const pill = page.locator('mesh-app-pill');
    await pill.locator('[data-react-grab-toolbar-collapse]').click();
    await frameRequested;
    await pill.getByRole('button', { name: 'Copy link', exact: true }).waitFor();
    assert.equal(await pill.locator('iframe').isVisible(), false);
    assert.equal(await pill.locator('[aria-label="Owner controls"]').count(), 0);
    assert.equal(await pill.locator('[aria-label="Make private"]').count(), 0);
    if (unavailable) assert.equal(await pill.locator('a').count(), 0, 'Unavailable management must not leave a dead action');
    else await pill.getByRole('link', { name: 'Pair owner browser' }).waitFor();
    await screenshot(page, `${name}-${unavailable ? 'unavailable' : 'visitor'}`);
    if (!unavailable) {
      owns = true;
      await page.evaluate(() => document.dispatchEvent(new Event('visibilitychange')));
      await pill.getByRole('link', { name: 'Make private' }).waitFor();
      owns = false;
      await page.evaluate(() => window.dispatchEvent(new PageTransitionEvent('pageshow', { persisted: true })));
      await pill.getByRole('link', { name: 'Pair owner browser' }).waitFor();
    }
    await pill.locator('[data-react-grab-toolbar-collapse]').click();
    assert.equal(await pill.locator('[data-react-grab-toolbar-collapse]').getAttribute('aria-expanded'), 'false');
    console.log(`${name}: ${unavailable ? 'unavailable manager' : 'visitor pairing'} remains compact and usable`);
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
      await checkUnavailableOrVisitor(browser, name, false);
      await checkUnavailableOrVisitor(browser, name, true);
    } finally {
      await browser.close();
    }
  }
} finally {
  await new Promise((resolveClose, reject) => server.close(error => error ? reject(error) : resolveClose()));
}
