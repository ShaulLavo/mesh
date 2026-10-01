import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { mkdir, readFile } from 'node:fs/promises';
import { resolve } from 'node:path';
import { chromium, webkit } from 'playwright';

const assets = process.env.MESH_PILL_ASSETS ? new URL(`file://${resolve(process.env.MESH_PILL_ASSETS)}/`) : new URL('../../../internal/apppill/assets/', import.meta.url);
const artifacts = process.env.MESH_PILL_ARTIFACTS_DIR;
if (artifacts) await mkdir(resolve(artifacts), { recursive: true });
const manager = 'https://apps.shaulavo.dev';
const script = await readFile(new URL('pill.js', assets));
const stylesheet = await readFile(new URL('pill.css', assets));
const DOT_LINE = 29;
const GAP = 16;

let origin;
const server = createServer((request, response) => {
  response.setHeader('Cache-Control', 'no-store');
  if (request.url === '/.mesh-app/pill.js') { response.setHeader('Content-Type', 'application/javascript'); response.end(script); return; }
  if (request.url === '/.mesh-app/pill.css') { response.setHeader('Content-Type', 'text/css'); response.end(stylesheet); return; }
  response.setHeader('Content-Type', 'text/html; charset=utf-8');
  response.setHeader('Content-Security-Policy', `default-src 'none'; script-src 'nonce-test'; style-src ${origin}/.mesh-app/pill.css; frame-src ${manager}`);
  response.end(`<!doctype html><html><head><meta name="viewport" content="width=device-width,initial-scale=1"><script defer nonce="test" data-mesh-app="7k3d" data-mesh-manager="${manager}" src="/.mesh-app/pill.js"></script></head><body><h1>Mesh pill snap harness</h1></body></html>`);
});
await new Promise((resolveListen, reject) => { server.once('error', reject); server.listen(0, '127.0.0.1', resolveListen); });
origin = `http://127.0.0.1:${server.address().port}`;

const measure = page => page.evaluate(() => {
  const root = document.querySelector('mesh-app-pill').shadowRoot;
  const viewport = { left: visualViewport.offsetLeft, top: visualViewport.offsetTop, right: visualViewport.offsetLeft + visualViewport.width, bottom: visualViewport.offsetTop + visualViewport.height };
  const gaps = element => {
    const box = element.getBoundingClientRect();
    return { top: box.top - viewport.top, bottom: viewport.bottom - box.bottom, left: box.left - viewport.left, right: viewport.right - box.right, x: box.left + box.width / 2, y: box.top + box.height / 2 };
  };
  const dot = root.querySelector('.dot');
  const shell = root.querySelector('.shell');
  return { shell: gaps(shell), closed: shell.classList.contains('closed'), dot: dot ? gaps(dot) : undefined, panel: gaps(root.querySelector('[data-react-grab-toolbar-panel]')) };
});

const settle = page => page.waitForTimeout(450);
const center = async page => { const { shell } = await measure(page); return { x: shell.x, y: shell.y }; };
// Expanded drags start on the chevron so no link or copy action sits under the pointer.
const grip = async (page, expanded) => {
  if (!expanded) return center(page);
  const box = await page.locator('mesh-app-pill [data-react-grab-toolbar-collapse]').boundingBox();
  return { x: box.x + box.width / 2, y: box.y + box.height / 2 };
};
const targets = (size) => ({ top: { x: size.width / 2, y: 4 }, bottom: { x: size.width / 2, y: size.height - 4 }, left: { x: 4, y: size.height / 2 }, right: { x: size.width - 4, y: size.height / 2 } });

async function drag(page, edge, expanded, size) {
  const from = await grip(page, expanded);
  const to = targets(size)[edge];
  await page.mouse.move(from.x, from.y);
  await page.mouse.down();
  await page.mouse.move(to.x, to.y, { steps: 12 });
  await page.waitForTimeout(160);
  await page.mouse.up();
  await settle(page);
}

// A fake clock fixes the release velocity at 15px per 8ms, whatever the machine's load.
async function flick(page, edge, expanded) {
  await page.evaluate(({ edge, expanded }) => {
    const root = document.querySelector('mesh-app-pill').shadowRoot;
    const target = root.querySelector(expanded ? '[data-react-grab-toolbar-collapse]' : '.dot-target') ?? root.querySelector('[data-react-grab-toolbar-collapse]');
    const bounds = target.getBoundingClientRect();
    const now = performance.now;
    let clock = now.call(performance);
    performance.now = () => clock;
    try {
      const at = (type, x, y, elapsed) => { clock += elapsed; (type === 'pointerdown' ? target : window).dispatchEvent(new PointerEvent(type, { bubbles: true, composed: true, button: 0, clientX: x, clientY: y })); };
      const x = bounds.x + bounds.width / 2;
      const y = bounds.y + bounds.height / 2;
      const middle = { x: innerWidth / 2, y: innerHeight / 2 };
      at('pointerdown', x, y, 0);
      for (let step = 1; step <= 6; step++) at('pointermove', x + (middle.x - x) * step / 6, y + (middle.y - y) * step / 6, 16);
      const [dx, dy] = { top: [0, -1], bottom: [0, 1], left: [-1, 0], right: [1, 0] }[edge];
      at('pointermove', middle.x, middle.y, 200);
      for (let step = 1; step <= 4; step++) at('pointermove', middle.x + dx * 15 * step, middle.y + dy * 15 * step, 8);
      at('pointerup', middle.x + dx * 60, middle.y + dy * 60, 8);
    } finally {
      performance.now = now;
    }
  }, { edge, expanded });
  await settle(page);
}

async function keyboardDock(page, edge, expanded) {
  const control = expanded ? 'mesh-app-pill [data-react-grab-toolbar-collapse]' : 'mesh-app-pill button[aria-expanded="false"]';
  await page.locator(control).focus();
  await page.keyboard.press(`Alt+${{ top: 'ArrowUp', bottom: 'ArrowDown', left: 'ArrowLeft', right: 'ArrowRight' }[edge]}`);
  await settle(page);
}

async function setExpanded(page, expanded) {
  const { closed } = await measure(page);
  if (closed === !expanded) return;
  await page.locator(expanded ? 'mesh-app-pill button[aria-expanded="false"]' : 'mesh-app-pill [data-react-grab-toolbar-collapse]').click();
  await settle(page);
}

async function checkOrientation(browser, engine, orientation, size) {
  const context = await browser.newContext({ viewport: size, hasTouch: true, deviceScaleFactor: 2 });
  try {
    const page = await context.newPage();
    page.setDefaultTimeout(5000);
    const errors = [];
    page.on('pageerror', error => errors.push(error.message));
    await page.route(`${manager}/**`, route => route.abort());
    await page.goto(origin);
    await page.locator('mesh-app-pill .shell').waitFor();
    await settle(page);
    for (const expanded of [false, true]) {
      const state = expanded ? 'expanded' : 'collapsed';
      await setExpanded(page, expanded);
      for (const edge of ['top', 'bottom', 'left', 'right']) {
        const rests = [];
        const record = async path => {
          const result = await measure(page);
          assert.equal(result.closed, !expanded, `${engine} ${orientation} ${state} ${edge} ${path}: state unchanged`);
          rests.push([path, result]);
        };
        await drag(page, edge, expanded, size); await record('drag');
        await flick(page, edge, expanded); await record('flick');
        await keyboardDock(page, edge, expanded); await record('keyboard');
        await page.setViewportSize({ width: size.width - 1, height: size.height }); await settle(page);
        await page.setViewportSize(size); await settle(page); await record('resize');
        await page.reload(); await page.locator('mesh-app-pill .shell').waitFor(); await settle(page);
        await setExpanded(page, expanded); await record('reload');
        const label = `${engine} ${orientation} ${state} ${edge}`;
        const [, first] = rests[0];
        for (const [path, result] of rests) {
          assert(Math.abs(result.shell[edge] - first.shell[edge]) < 0.5, `${label}: ${path} rests ${result.shell[edge]}px from the edge, drag rests ${first.shell[edge]}px`);
          assert(Math.min(result.shell.top, result.shell.bottom, result.shell.left, result.shell.right) === result.shell[edge], `${label}: ${path} docks to ${edge}`);
        }
        const [, rest] = rests.at(-1);
        if (rest.dot && !expanded) assert(Math.abs(rest.dot[edge] + 6 - DOT_LINE) < 0.5, `${label}: dot center ${rest.dot[edge] + 6}px from the edge, want ${DOT_LINE}`);
        if (rest.dot && expanded) assert(Math.abs(rest.panel[edge] - GAP) < 0.5, `${label}: pill ${rest.panel[edge]}px from the edge, want ${GAP}`);
        if (artifacts && (edge === 'right' || (orientation === 'portrait' && edge === 'bottom'))) await page.screenshot({ path: resolve(artifacts, `${engine}-${orientation}-${state}-${edge}.png`) });
      }
    }
    await setExpanded(page, false);
    await keyboardDock(page, 'bottom', false);
    await page.evaluate(() => {
      Object.defineProperty(visualViewport, 'height', { configurable: true, value: innerHeight - 120 });
      Object.defineProperty(visualViewport, 'offsetTop', { configurable: true, value: 40 });
      visualViewport.dispatchEvent(new Event('resize'));
    });
    await settle(page);
    const shifted = await measure(page);
    if (shifted.dot) assert(Math.abs(shifted.dot.bottom + 6 - DOT_LINE) < 0.5, `${engine} ${orientation}: a Safari toolbar keeps the dot ${DOT_LINE}px above the visible bottom, got ${shifted.dot.bottom + 6}`);
    assert.deepEqual(errors, [], 'The pill must not throw browser errors');
    console.log(`${engine} ${orientation}: one rest position per edge and state after drag, flick, keyboard, resize and reload`);
  } finally {
    await context.close();
  }
}

try {
  for (const [engine, type] of [['chromium', chromium], ['webkit', webkit]]) {
    const options = engine === 'chromium' && process.env.MESH_CHROMIUM_EXECUTABLE ? { executablePath: process.env.MESH_CHROMIUM_EXECUTABLE } : {};
    const browser = await type.launch({ headless: true, ...options });
    try {
      await checkOrientation(browser, engine, 'portrait', { width: 390, height: 844 });
      await checkOrientation(browser, engine, 'landscape', { width: 844, height: 390 });
    } finally {
      await browser.close();
    }
  }
} finally {
  await new Promise((resolveClose, reject) => server.close(error => error ? reject(error) : resolveClose()));
}
