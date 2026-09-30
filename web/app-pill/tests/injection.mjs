import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { readFile } from 'node:fs/promises';
import { createServer } from 'node:http';
import { fileURLToPath } from 'node:url';
import { chromium } from 'playwright';

const root = new URL('../../../', import.meta.url);
const assets = new URL('internal/apppill/assets/', root);
const script = await readFile(new URL('pill.js', assets));
const stylesheet = await readFile(new URL('pill.css', assets));
let fixtures;
let frameLoads = 0;
const server = createServer((request, response) => {
  response.setHeader('Cache-Control', 'no-store');
  if (request.url === '/.mesh-app/pill.js') {
    response.setHeader('Content-Type', 'application/javascript');
    response.end(script);
  } else if (request.url === '/.mesh-app/pill.css') {
    response.setHeader('Content-Type', 'text/css');
    response.end(stylesheet);
  } else if (request.url.startsWith('/frame?')) {
    frameLoads++;
    response.setHeader('Content-Type', 'text/html');
    response.end(`<script>parent.postMessage({type:'mesh-app-status',visibility:'public',owns:true},'http://127.0.0.1:${server.address().port}')</script>`);
  } else {
    response.setHeader('Content-Type', 'text/html; charset=utf-8');
    response.end(fixtures[request.url.slice(1)] ?? 'not found');
  }
});
await new Promise((resolve, reject) => {
  server.once('error', reject);
  server.listen(0, '127.0.0.1', resolve);
});
const origin = `http://127.0.0.1:${server.address().port}`;
const manager = `http://localhost:${server.address().port}`;
let browser;
try {
  fixtures = JSON.parse(execFileSync('go', ['run', './internal/apppill/testdata/browser-fixtures.go', manager], {
    cwd: fileURLToPath(root), encoding: 'utf8', maxBuffer: 4 << 20,
  }));
  browser = await chromium.launch({ headless: true, ...(process.env.MESH_CHROMIUM_EXECUTABLE ? { executablePath: process.env.MESH_CHROMIUM_EXECUTABLE } : {}) });
  for (const element of ['script', 'style', 'textarea', 'title'].filter(name => !process.argv[2] || process.argv[2] === name)) {
    const page = await browser.newPage();
    const errors = [];
    page.on('pageerror', error => errors.push(error.message));
    await page.goto(`${origin}/${element}`);
    assert.equal(await page.locator('script[data-mesh-app]').count(), 0, `${element}: overflow must skip the pill`);
    const text = await page.locator(element).first().textContent();
    assert(text.includes('a'.repeat(300 << 10)), `${element}: app token must remain intact`);
    assert(!text.includes('data-mesh-app'), `${element}: pill must not become raw text`);
    if (element === 'script') assert.equal(await page.evaluate(() => window.appOK && window.appData.length === (300 << 10)), true);
    if (element === 'style') assert.equal(await page.locator('body').evaluate(node => getComputedStyle(node).color), 'rgb(1, 2, 3)');
    assert.deepEqual(errors, [], `${element}: app must not throw`);
    await page.close();
    console.log(`Chromium: oversized headless ${element} intact, no unsafe pill`);
  }
  const page = await browser.newPage();
  page.setDefaultTimeout(5000);
  await page.goto(`${origin}/hoisted-csp`);
  const pill = page.locator('mesh-app-pill');
  await pill.locator('[data-react-grab-toolbar-collapse]').click();
  await pill.getByRole('link', { name: 'Make private', exact: true }).waitFor();
  assert.equal(frameLoads, 1, 'Hoisted CSP must allow the actual management frame to load');
  assert.equal(await page.locator('script[data-mesh-app]').evaluate(node => node.parentElement.tagName), 'HEAD');
  await page.close();
  console.log('Chromium: CSP meta after </head> permits management frame and owner controls');
} finally {
  if (browser) await browser.close();
  await new Promise((resolve, reject) => server.close(error => error ? reject(error) : resolve()));
}
