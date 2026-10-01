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
const DOT_LINE = 38;
const TARGET = 44;
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
  return {
    shell: gaps(shell), closed: shell.classList.contains('closed'), dot: gaps(dot), panel: gaps(root.querySelector('.panel')),
    panelWidth: root.querySelector('.panel').offsetWidth, panelHeight: root.querySelector('.panel').offsetHeight,
    target: gaps(root.querySelector('.dot-target')),
    dotVisible: getComputedStyle(dot).opacity === '1', chevrons: root.querySelectorAll('[data-react-grab-toolbar-collapse]').length,
    // Every touch target, the dot's and each action's, as rendered rectangles.
    targets: [root.querySelector('.dot-target'), ...root.querySelectorAll('.action')].map(element => {
      const box = element.getBoundingClientRect();
      return { left: box.left, top: box.top, right: box.right, bottom: box.bottom, width: box.width, height: box.height };
    }),
  };
});

const settle = page => page.waitForTimeout(450);
const center = async page => { const { shell } = await measure(page); return { x: shell.x, y: shell.y }; };
// Expanded drags start on the chevron so no link or copy action sits under the pointer.
const grip = async (page, expanded) => {
  if (!expanded) return center(page);
  const box = await page.locator('mesh-app-pill .dot-target').boundingBox();
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
    const target = root.querySelector('.dot-target');
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
  await page.locator('mesh-app-pill .dot-target').focus();
  await page.keyboard.press(`Alt+${{ top: 'ArrowUp', bottom: 'ArrowDown', left: 'ArrowLeft', right: 'ArrowRight' }[edge]}`);
  await settle(page);
}

async function setExpanded(page, expanded) {
  const { closed } = await measure(page);
  if (closed === !expanded) return;
  await page.locator('mesh-app-pill .dot-target').click();
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
        assert(Math.abs(rest.target[edge] + TARGET / 2 - DOT_LINE) < 0.5, `${label}: dot center ${rest.target[edge] + TARGET / 2}px from the edge, want ${DOT_LINE}`);
        assert(Math.abs(rest.dot.x - rest.target.x) < 0.5 && Math.abs(rest.dot.y - rest.target.y) < 0.5, `${label}: the dot is centered in its target`);
        assert.deepEqual([rest.targets[0].width, rest.targets[0].height], [TARGET, TARGET], `${label}: the dot keeps a ${TARGET}px target`);
        assert.equal(rest.chevrons, 0, `${label}: no arrow`);
        if (expanded) {
          assert(Math.abs(rest.panel[edge] - GAP) < 0.5, `${label}: pill ${rest.panel[edge]}px from the edge, want ${GAP}`);
          assert(rest.dotVisible, `${label}: the open pill keeps its dot`);
          const panelThickness = edge === 'top' || edge === 'bottom' ? rest.panelHeight : rest.panelWidth;
          assert.equal(panelThickness, TARGET, `${label}: the open pill is one ${TARGET}px target thick`);
          for (const [index, target] of rest.targets.entries()) {
            assert.deepEqual([target.width, target.height], [TARGET, TARGET], `${label}: target ${index} is ${target.width}x${target.height}, want ${TARGET}px`);
            for (const other of rest.targets.slice(index + 1)) {
              const apart = Math.max(other.left - target.right, target.left - other.right, other.top - target.bottom, target.top - other.bottom);
              assert(apart >= -0.5, `${label}: touch targets overlap by ${-apart}px`);
            }
          }
        }
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
    assert(Math.abs(shifted.target.bottom + TARGET / 2 - DOT_LINE) < 0.5, `${engine} ${orientation}: a Safari toolbar keeps the dot ${DOT_LINE}px above the visible bottom, got ${shifted.target.bottom + TARGET / 2}`);
    assert.deepEqual(errors, [], 'The pill must not throw browser errors');
    console.log(`${engine} ${orientation}: one rest position per edge and state after drag, flick, keyboard, resize and reload`);
  } finally {
    await context.close();
  }
}

const ownerFrame = route => route.fulfill({ status: 200, contentType: 'text/html; charset=utf-8', body: `<!doctype html><script>parent.postMessage({type:'mesh-app-status',visibility:'public',owns:true},${JSON.stringify(origin)})</script>` });
const anchorOf = page => page.evaluate(() => {
  const box = document.querySelector('mesh-app-pill').shadowRoot.querySelector('.dot-target').getBoundingClientRect();
  return { x: box.x + box.width / 2, y: box.y + box.height / 2 };
});
const isOpen = async page => (await page.locator('mesh-app-pill .dot-target').getAttribute('aria-expanded')) === 'true';
const near = (a, b) => Math.abs(a.x - b.x) <= 1.5 && Math.abs(a.y - b.y) <= 1.5;
// Holds the pill's running transitions, so a tap lands at a known moment of the motion.
// Already held ones stay where they are; rewinding them would resize the pill under the test.
const hold = (page, at) => page.evaluate(at => {
  for (const animation of document.querySelector('mesh-app-pill').shadowRoot.querySelector('.shell').getAnimations({ subtree: true })) {
    if (animation.playState !== 'running') continue;
    animation.pause();
    if (at !== undefined) animation.currentTime = at;
  }
}, at);
const release = page => page.evaluate(() => {
  for (const animation of document.querySelector('mesh-app-pill').shadowRoot.querySelector('.shell').getAnimations({ subtree: true })) animation.play();
});

async function checkAnchoring(browser, engine, reducedMotion) {
  const context = await browser.newContext({ viewport: { width: 390, height: 844 }, hasTouch: true, reducedMotion });
  try {
    const page = await context.newPage();
    page.setDefaultTimeout(5000);
    const errors = [];
    page.on('pageerror', error => errors.push(error.message));
    const confirms = [];
    page.on('request', request => { if (new URL(request.url()).pathname === '/confirm') confirms.push(request.url()); });
    await page.route(`${manager}/**`, ownerFrame);
    await page.goto(origin);
    const label = `${engine} ${reducedMotion}`;
    const dot = page.locator('mesh-app-pill .dot-target');
    const collapse = dot;
    await dot.click();
    await page.locator('mesh-app-pill').getByRole('link', { name: 'Make private' }).waitFor();
    await collapse.click();
    await settle(page);

    for (const [edge, ratio] of [['bottom', 0], ['bottom', 1], ['top', 0], ['top', 1], ['left', 0], ['left', 1], ['right', 0], ['right', 1]]) {
      await page.evaluate(dock => localStorage.setItem('mesh-app-pill-position', JSON.stringify(dock)), { edge, ratio });
      await page.reload();
      await dot.waitFor();
      await settle(page);
      const before = await anchorOf(page);
      await dot.click();
      await page.locator('mesh-app-pill').getByRole('link', { name: 'Make private' }).waitFor();
      await settle(page);
      assert(near(await anchorOf(page), before), `${label} ${edge} ${ratio}: opening moved the dot from ${JSON.stringify(before)} to ${JSON.stringify(await anchorOf(page))}`);
      const grip = await collapse.boundingBox();
      const from = { x: grip.x + grip.width / 2, y: grip.y + grip.height / 2 };
      const [dx, dy] = edge === 'top' || edge === 'bottom' ? [10, 0] : [0, 10];
      await page.mouse.move(from.x, from.y);
      await page.mouse.down();
      await page.mouse.move(from.x + dx, from.y + dy, { steps: 4 });
      await page.mouse.move(from.x, from.y, { steps: 4 });
      await page.waitForTimeout(160);
      await page.mouse.up();
      await settle(page);
      await collapse.click();
      await settle(page);
      assert.equal(await isOpen(page), false, `${label} ${edge} ${ratio}: collapsed`);
      const after = await anchorOf(page);
      assert(near(after, before), `${label} ${edge} ${ratio}: a zero-net drag of the open pill moved the dot from ${JSON.stringify(before)} to ${JSON.stringify(after)}`);
      await page.reload();
      await dot.waitFor();
      await settle(page);
      assert(near(await anchorOf(page), before), `${label} ${edge} ${ratio}: the dot moved after reload`);
    }

    await page.evaluate(() => localStorage.setItem('mesh-app-pill-position', JSON.stringify({ edge: 'bottom', ratio: 0.5 })));
    await page.reload();
    await dot.waitFor();
    await settle(page);
    await dot.click();
    await page.locator('mesh-app-pill').getByRole('link', { name: 'Make private' }).waitFor();
    await collapse.click();
    await settle(page);
    if (reducedMotion !== 'reduce') {
      const start = await anchorOf(page);
      await page.mouse.move(start.x, start.y);
      await page.mouse.down();
      await page.mouse.move(220, 420, { steps: 10 });
      await page.waitForTimeout(160);
      await page.mouse.up();
      await hold(page, 35);
      const flying = await anchorOf(page);
      assert(flying.x > 230 && flying.x < 350, `${label}: the dot is mid-snap at ${JSON.stringify(flying)}`);
      await page.touchscreen.tap(flying.x, flying.y);
      await hold(page, 0);
      assert.equal(await isOpen(page), true, `${label}: a tap during a snap opens the pill`);
      const opening = await anchorOf(page);
      assert(near(opening, flying), `${label}: opening mid-snap moved the anchor from ${JSON.stringify(flying)} to ${JSON.stringify(opening)}`);
      await release(page);
      await settle(page);
      const rest = await measure(page);
      assert(Math.abs(rest.panel.right - GAP) < 0.5, `${label}: the opened pill finishes its snap ${rest.panel.right}px from the right edge`);
      await collapse.click();
      await settle(page);
      await page.keyboard.press('Tab');
      await dot.focus();
      await page.keyboard.press('Alt+ArrowDown');
      await settle(page);
    }

    const center = await anchorOf(page);
    await page.touchscreen.tap(center.x, center.y);
    await hold(page);
    await page.touchscreen.tap(center.x, center.y);
    await settle(page);
    assert.deepEqual(confirms, [], `${label}: a quick second tap must not activate an action that is still appearing`);
    assert.equal(await isOpen(page), false, `${label}: a quick second tap reverses the morph`);
    await release(page);
    await settle(page);
    await page.touchscreen.tap(center.x, center.y);
    await settle(page);
    const lock = await page.locator('mesh-app-pill').getByRole('link', { name: 'Make private' }).boundingBox();
    await page.touchscreen.tap(lock.x + lock.width / 2, lock.y + lock.height / 2);
    await page.waitForURL(`${manager}/confirm?**`);
    assert.deepEqual(errors, [], 'The pill must not throw browser errors');
    console.log(`${label}: corner drags keep the dot, a tap mid-snap keeps the anchor, and a quick second tap reverses the morph`);
  } finally {
    await context.close();
  }
}

try {
  for (const [engine, type] of [['chromium', chromium], ['webkit', webkit]]) {
    const options = engine === 'chromium' && process.env.MESH_CHROMIUM_EXECUTABLE ? { executablePath: process.env.MESH_CHROMIUM_EXECUTABLE } : {};
    const browser = await type.launch({ headless: true, ...options });
    try {
      await checkAnchoring(browser, engine, 'no-preference');
      await checkAnchoring(browser, engine, 'reduce');
      await checkOrientation(browser, engine, 'portrait', { width: 390, height: 844 });
      await checkOrientation(browser, engine, 'landscape', { width: 844, height: 390 });
    } finally {
      await browser.close();
    }
  }
} finally {
  await new Promise((resolveClose, reject) => server.close(error => error ? reject(error) : resolveClose()));
}
