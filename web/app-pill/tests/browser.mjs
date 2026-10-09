import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { mkdir, readFile } from 'node:fs/promises';
import { resolve } from 'node:path';
import { chromium, webkit } from 'playwright';
import { fixturePage } from './fixture.mjs';

const assets = new URL('../../../internal/apppill/assets/', import.meta.url);
const artifacts = process.env.MESH_PILL_ARTIFACTS_DIR;
if (artifacts) await mkdir(resolve(artifacts), { recursive: true });

const script = await readFile(new URL('pill.js', assets));
const stylesheet = await readFile(new URL('pill.css', assets));
let origin;
const server = createServer((request, response) => {
  response.setHeader('Cache-Control', 'no-store');
  if (request.url === '/pill.js') {
    response.setHeader('Content-Type', 'application/javascript; charset=utf-8');
    response.end(script);
    return;
  }
  if (request.url === '/pill.css') {
    response.setHeader('Content-Type', 'text/css; charset=utf-8');
    response.end(stylesheet);
    return;
  }
  response.setHeader('Content-Type', 'text/html; charset=utf-8');
  response.setHeader('Content-Security-Policy', `default-src 'none'; script-src 'nonce-test'; style-src ${origin}/pill.css`);
  response.end(fixturePage({ mount: request.url !== '/unmounted' }));
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

async function checkActionAndViewport(page, pill) {
  await pill.getByRole('button', { name: 'Run action', exact: true }).click();
  assert.equal(await page.evaluate(() => window.actionCalls), 1, 'The caller action runs once');
  await page.evaluate(() => {
    Object.defineProperty(visualViewport, 'height', { configurable: true, value: innerHeight - 200 });
    Object.defineProperty(visualViewport, 'offsetTop', { configurable: true, value: 120 });
    visualViewport.dispatchEvent(new Event('resize'));
    visualViewport.dispatchEvent(new Event('scroll'));
  });
  assert.equal(await pill.locator('.shell').evaluate(element => getComputedStyle(element).transitionDuration), '0s', 'Safari viewport changes must not animate position');
  await page.waitForFunction(() => {
    const bounds = document.querySelector('floating-pill').shadowRoot.querySelector('.shell').getBoundingClientRect();
    return bounds.bottom <= visualViewport.offsetTop + visualViewport.height;
  });
  await page.evaluate(() => {
    delete visualViewport.height;
    delete visualViewport.offsetTop;
    visualViewport.dispatchEvent(new Event('resize'));
  });
  await page.waitForFunction(() => !document.querySelector('floating-pill').shadowRoot.querySelector('.shell').classList.contains('resizing'));
}

async function checkLinkDrag(page, pill) {
  await pill.getByRole('link', { name: 'Open details' }).waitFor();
  const result = await page.evaluate(async () => {
    const shadow = document.querySelector('floating-pill').shadowRoot;
    const link = shadow.querySelector('a.action');
    const bounds = link.getBoundingClientRect();
    link.dispatchEvent(new PointerEvent('pointerdown', { bubbles: true, composed: true, button: 0, clientX: bounds.x + bounds.width / 2, clientY: bounds.y + bounds.height / 2 }));
    window.dispatchEvent(new PointerEvent('pointermove', { clientX: bounds.x + 60, clientY: bounds.y - 60 }));
    const dragged = shadow.querySelector('.shell').getBoundingClientRect();
    window.dispatchEvent(new Event('resize'));
    await new Promise(resolve => requestAnimationFrame(resolve));
    const refreshed = shadow.querySelector('.shell').getBoundingClientRect();
    window.dispatchEvent(new PointerEvent('pointerup'));
    return {
      prevented: !link.dispatchEvent(new MouseEvent('click', { bubbles: true, composed: true, cancelable: true })),
      stayedUnderPointer: dragged.x === refreshed.x && dragged.y === refreshed.y,
    };
  });
  assert(result.prevented, 'Releasing a drag on the link must cancel link navigation');
  assert(result.stayedUnderPointer, 'A viewport resize must not redock the pill during dragging');
  await page.waitForFunction(() => !document.querySelector('floating-pill').shadowRoot.querySelector('.shell').classList.contains('snapping'));
  const appURL = page.url();
  assert.equal(await pill.getByRole('link', { name: 'Open details' }).getAttribute('target'), null);
  await pill.getByRole('link', { name: 'Open details' }).click();
  await page.waitForURL(`${origin}/details`);
  await page.goto(appURL);
  await pill.getByRole('button', { name: /^Open floating controls/ }).click();
  await pill.getByRole('link', { name: 'Open details' }).waitFor();
}

async function checkInteractions(browser, name, reducedMotion) {
  const context = await browser.newContext({ viewport: { width: 390, height: 844 }, hasTouch: true, reducedMotion });
  try {
    const page = await context.newPage();
    page.setDefaultTimeout(5000);
    const errors = [];
    page.on('pageerror', error => errors.push(error.message));
    await page.goto(origin);
    const pill = page.locator('floating-pill');
    const dot = pill.locator('.dot-target');
    const isOpen = async () => (await dot.getAttribute('aria-expanded')) === 'true';
    const dotCenter = async () => { const bounds = await dot.boundingBox(); return { x: bounds.x + bounds.width / 2, y: bounds.y + bounds.height / 2 }; };
    await dot.waitFor({ state: 'visible' });
    await page.waitForFunction(() => { const bounds = document.querySelector('floating-pill').shadowRoot.querySelector('.dot-target').getBoundingClientRect(); return bounds.left >= 0 && bounds.right <= innerWidth && bounds.top >= 0 && bounds.bottom <= innerHeight; });
    const box = await dot.boundingBox();
    assert(box, 'The collapsed dot must be visible');
    assert.deepEqual([box.width, box.height], [44, 44], 'The dot keeps a 44px touch target');
    assert.deepEqual(await pill.locator('.dot').evaluate(element => [element.offsetWidth, getComputedStyle(element).backgroundColor]), [14, 'rgb(165, 180, 252)']);
    assert.equal(await pill.locator('[data-react-grab-toolbar-collapse]').count(), 0, 'There is no arrow; the dot is the only toggle');
    assert.equal(await dot.getAttribute('aria-expanded'), 'false');
    const closedCenter = await dotCenter();
    await dot.click();
    await pill.locator('.controls').waitFor({ state: 'visible' });
    assert.equal(await isOpen(), true, 'The dot reports the open pill');
    assert.equal(await pill.locator('[data-react-grab-toolbar-collapse]').count(), 0, 'Opening adds no arrow');
    assert.deepEqual(await dotCenter(), closedCenter, 'The dot stays where it was when the pill opens');
    assert.equal(await pill.locator('.dot').evaluate(element => getComputedStyle(element).opacity), '1', 'The open pill keeps the dot visible');
    assert.equal(await pill.getByRole('button', { name: /^Close floating controls/ }).count(), 1, 'The dot is announced as the close control');
    const openTarget = await dot.boundingBox();
    assert.deepEqual([openTarget.width, openTarget.height], [44, 44], 'The open pill keeps the dot\'s 44px target');
    for (const action of await pill.locator('.action').all()) {
      const bounds = await action.boundingBox();
      assert.deepEqual([bounds.width, bounds.height], [44, 44], 'Every action has a 44px target');
    }
    assert.equal(await pill.getByRole('button').count() + await pill.getByRole('link').count(), 3, 'Dot and two caller actions');
    assert.equal(await pill.locator('iframe').count(), 0, 'The reusable component creates no authorization frame');
    await checkActionAndViewport(page, pill);
    if (reducedMotion === 'reduce') {
      assert.equal(await pill.locator('.shell').evaluate(element => getComputedStyle(element).transitionDuration), '0s');
      assert.equal(await pill.locator('.controls').evaluate(element => getComputedStyle(element).animationName), 'none');
    }
    await page.waitForFunction(() => { const panel = document.querySelector('floating-pill').shadowRoot.querySelector('.panel'); return Math.min(panel.offsetWidth, panel.offsetHeight) === 44; });
    await screenshot(page, `${name}-${reducedMotion}-expanded`);
    await dot.click();
    assert.equal(await isOpen(), false, 'The dot collapses the pill it opened');
    assert.deepEqual(await dotCenter(), closedCenter, 'The dot stays put when the pill collapses');
    await dot.hover();
    const dragStart = await dot.boundingBox();
    assert(dragStart);
    await page.mouse.move(dragStart.x + dragStart.width / 2, dragStart.y + dragStart.height / 2);
    await page.mouse.down();
    await page.mouse.move(25, dragStart.y + dragStart.height / 2, { steps: 12 });
    await page.mouse.up();
    await page.waitForTimeout(320);
    const snappedMouse = await dot.boundingBox();
    assert(snappedMouse.x < 40, `Collapsed dot should snap left after dragging: ${JSON.stringify(snappedMouse)}, ${await page.evaluate(() => localStorage.getItem('floating-pill-position'))}`);
    await dot.focus();
    await page.keyboard.press('Alt+ArrowRight');
    await page.waitForTimeout(100);
    assert((await dot.boundingBox()).x > 300, 'Keyboard docking should move the dot right');
    await dot.evaluate(element => {
      const bounds = element.getBoundingClientRect();
      element.dispatchEvent(new PointerEvent('pointerdown', { bubbles: true, composed: true, button: 0, clientX: bounds.x + bounds.width / 2, clientY: bounds.y + bounds.height / 2, pointerType: 'touch' }));
    });
    await page.waitForTimeout(50);
    await page.evaluate(() => window.dispatchEvent(new PointerEvent('pointermove', { clientX: 25, clientY: 180, pointerType: 'touch' })));
    await page.waitForTimeout(120);
    await page.evaluate(() => window.dispatchEvent(new PointerEvent('pointerup', { pointerType: 'touch' })));
    await page.waitForTimeout(320);
    assert((await dot.boundingBox()).x < 40, 'Touch pointer dragging should snap left');
    await dot.click();
    assert.equal(await isOpen(), true, 'The first deliberate click after a drag must expand');
    await checkLinkDrag(page, pill);
    await dot.click();
    assert.equal(await isOpen(), false);
    await dot.focus();
    await page.keyboard.press('Alt+ArrowRight');
    await page.reload();
    await dot.waitFor({ state: 'visible' });
    await page.waitForFunction(() => { const bounds = document.querySelector('floating-pill').shadowRoot.querySelector('.dot-target').getBoundingClientRect(); return bounds.left >= 0 && bounds.right <= innerWidth && bounds.top >= 0 && bounds.bottom <= innerHeight; });
    assert((await dot.boundingBox()).x > 300, 'Docked position should survive reload');
    const touchTarget = await dot.boundingBox();
    assert(touchTarget);
    await page.touchscreen.tap(touchTarget.x + touchTarget.width / 2, touchTarget.y + touchTarget.height / 2);
    await pill.locator('.controls').waitFor({ state: 'visible' });
    await pill.getByRole('button', { name: 'Run action', exact: true }).focus();
    await page.keyboard.press('Escape');
    assert.equal(await isOpen(), false);
    assert.equal(await page.evaluate(() => document.querySelector('floating-pill').shadowRoot.activeElement?.className), 'dot-target', 'Escape returns focus to the dot');
    await screenshot(page, `${name}-${reducedMotion}-collapsed`);
    assert.deepEqual(errors, [], 'The pill must not throw browser errors');
    console.log(`${name} ${reducedMotion}: strict CSP, dot, expansion, caller actions, mouse/touch drag, keyboard docking, and persistence passed`);
  } finally {
    await context.close();
  }
}

async function checkExplicitMountAndDispose(browser, name) {
  const context = await browser.newContext();
  try {
    const page = await context.newPage();
    await page.goto(`${origin}/unmounted`);
    assert.equal(await page.locator('floating-pill').count(), 0, 'Loading the bundle must not mount controls');
    await page.evaluate(() => {
      window.disposePill = FloatingPill.mountFloatingPill({
        stylesheetHref: '/pill.css', label: 'Example controls', storageKey: 'example-position',
        actions: [{ id: 'example', kind: 'button', label: 'Example action', onSelect: () => {} }],
      });
    });
    await page.getByRole('button', { name: /^Open Example controls/ }).waitFor();
    await page.evaluate(() => window.disposePill());
    assert.equal(await page.locator('floating-pill').count(), 0, 'Disposal removes the host');
    console.log(`${name}: explicit mount and disposal passed`);
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
      await checkExplicitMountAndDispose(browser, name);
    } finally {
      await browser.close();
    }
  }
} finally {
  await new Promise((resolveClose, reject) => server.close(error => error ? reject(error) : resolveClose()));
}
