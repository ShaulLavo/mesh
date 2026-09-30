import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { join } from 'node:path';
import { pathToFileURL } from 'node:url';

const { chromium } = await import(process.env.MESH_PLAYWRIGHT_MODULE
  ? pathToFileURL(process.env.MESH_PLAYWRIGHT_MODULE).href : 'playwright');
const directory = process.argv[2];
assert(directory, 'Generate rendered pages with MESH_CONFIRM_BROWSER_DIR and TestConfirmationBrowserPages');
const manager = 'https://apps.shaulavo.dev';
const browser = await chromium.launch({ headless: true,
  ...(process.env.MESH_CHROMIUM_EXECUTABLE ? { executablePath: process.env.MESH_CHROMIUM_EXECUTABLE } : {}),
});
try {
  const context = await browser.newContext();
  let submissions = 0;
  await context.route('**/*', async route => {
    const request = route.request();
    assert.equal(new URL(request.url()).origin, manager, 'The verifier must not reach an external origin');
    if (request.method() === 'POST') {
      assert.equal(new URL(request.url()).pathname, '/action');
      const form = new URLSearchParams(request.postData());
      assert.equal(form.get('id'), '7k3d');
      assert.equal(form.get('csrf'), 'test-csrf');
      submissions++;
      await route.fulfill({ contentType: 'text/html', body: '<p>Confirmed</p>' });
      return;
    }
    const action = new URL(request.url()).searchParams.get('action');
    const body = await readFile(join(directory, `${action}.html`), 'utf8');
    await route.fulfill({ contentType: 'text/html', body,
      headers: { 'Content-Security-Policy': "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'" },
    });
  });
  const page = await context.newPage();
  await page.bringToFront();
  const epoch = new Date('2026-10-01T00:00:00Z');
  await page.clock.install({ time: epoch });
  await page.clock.pauseAt(new Date(epoch.getTime() + 1000));
  await page.goto(`${manager}/confirm?id=7k3d&action=renew`);
  const button = page.getByRole('button', { name: 'Confirm renew' });
  assert(await button.isDisabled(), 'Confirm must be inert on page arrival');
  await page.evaluate(() => {
    document.querySelector('button').click();
    document.querySelector('form').requestSubmit();
  });
  assert.equal(submissions, 0, 'Immediate synthetic click or submit must not change the app');
  await page.clock.runFor(750);
  assert(await button.isDisabled(), 'Elapsed time alone must not activate confirmation');
  await page.evaluate(() => document.dispatchEvent(new PointerEvent('pointermove', { bubbles: true })));
  assert(await button.isDisabled(), 'Synthetic input must not activate confirmation');
  await page.keyboard.press('ArrowDown');
  assert(await button.isEnabled(), 'Deliberate keyboard input after the delay should activate');

  // Headless Chromium reports focus on both tabs. Control that input without replacing the page's guard.
  await page.evaluate(() => {
    document.hasFocus = () => false;
    window.dispatchEvent(new Event('blur'));
  });
  assert(await button.isDisabled(), 'Losing focus must deactivate confirmation');
  await page.clock.runFor(2000);
  assert(await button.isDisabled(), 'Background time must not count toward activation');
  await page.evaluate(() => {
    document.hasFocus = () => true;
    window.dispatchEvent(new Event('focus'));
  });
  await page.keyboard.press('ArrowDown');
  await page.clock.runFor(749);
  assert(await button.isDisabled(), 'Refocusing requires a fresh 750 ms delay');
  await page.clock.runFor(1);
  assert(await button.isEnabled());
  await page.evaluate(() => {
    Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'hidden' });
    document.dispatchEvent(new Event('visibilitychange'));
  });
  await page.clock.runFor(2000);
  assert(await button.isDisabled(), 'Hidden time must not activate confirmation');
  await page.evaluate(() => {
    Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'visible' });
    document.dispatchEvent(new Event('visibilitychange'));
  });
  await page.keyboard.press('ArrowDown');
  await page.clock.runFor(749);
  assert(await button.isDisabled(), 'Becoming visible requires a fresh delay');
  await page.clock.runFor(1);
  assert(await button.isEnabled());
  await button.click();
  await page.getByText('Confirmed', { exact: true }).waitFor();
  assert.equal(submissions, 1, 'An activated confirmation should submit exactly once');

  for (const action of ['public', 'delete']) {
    await page.goto(`${manager}/confirm?id=7k3d&action=${action}`);
    const confirm = page.getByRole('button', { name: `Confirm ${action}` });
    assert(await confirm.isDisabled());
    await page.locator('#confirm-id').fill('wrong');
    await page.clock.runFor(750);
    await page.keyboard.press('ArrowDown');
    assert(await confirm.isDisabled(), `${action} requires the exact app ID`);
    await page.locator('#confirm-id').fill('7k3d');
    assert(await confirm.isEnabled());
    await confirm.click();
    await page.getByText('Confirmed', { exact: true }).waitFor();
  }
  assert.equal(submissions, 3);
  console.log('PASS: rendered /confirm blocks immediate and synthetic submits, resets on blur, waits 750 ms in focus, and requires the app ID for public/delete');
  await context.close();
} finally {
  await browser.close();
}
