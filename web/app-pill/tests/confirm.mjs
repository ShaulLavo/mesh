import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { join } from 'node:path';
import { pathToFileURL } from 'node:url';

const playwright = await import(process.env.MESH_PLAYWRIGHT_MODULE
  ? pathToFileURL(process.env.MESH_PLAYWRIGHT_MODULE).href : 'playwright');
const directory = process.argv[2];
assert(directory, 'Generate rendered pages with MESH_CONFIRM_BROWSER_DIR and TestConfirmationBrowserPages');
const manager = 'https://apps.shaulavo.dev';

async function fixtureContext(browser, options = {}) {
  const context = await browser.newContext(options);
  let submissions = 0;
  await context.route('**/*', async route => {
    const request = route.request();
    assert.equal(new URL(request.url()).origin, manager, 'The verifier must not reach an external origin');
    if (request.method() === 'POST') {
      assert.equal(new URL(request.url()).pathname, '/action');
      const form = new URLSearchParams(request.postData());
      assert.equal(form.get('id'), '7k3d');
      assert.equal(form.get('csrf'), 'test-csrf');
      if (['public', 'delete'].includes(form.get('action'))) {
        assert.equal(form.get('confirmation'), '7k3d');
      }
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
  return { context, page, submissions: () => submissions };
}

async function verifyDesktop(browser) {
  const { context, page, submissions } = await fixtureContext(browser);
  try {
    await page.goto(`${manager}/confirm?id=7k3d&action=renew`);
    const button = page.getByRole('button', { name: 'Confirm renew' });
    assert(await button.isDisabled(), 'Confirm must be inert on page arrival');
    await page.evaluate(() => {
      document.querySelector('button').click();
      document.querySelector('form').requestSubmit();
    });
    assert.equal(submissions(), 0, 'Immediate synthetic click or submit must not change the app');
    await page.keyboard.press('ArrowDown');
    await page.clock.runFor(750);
    assert(await button.isDisabled(), 'Input during the delay must not arm confirmation');
    await page.evaluate(() => document.dispatchEvent(new PointerEvent('pointermove', { bubbles: true })));
    assert(await button.isDisabled(), 'Synthetic input must not activate confirmation');
    await page.keyboard.press('ArrowDown');
    assert(await button.isEnabled(), 'Trusted input after the delay should activate');

    // Headless engines can report multiple tabs focused; control only the guard's focus input.
    await page.evaluate(() => {
      document.hasFocus = () => false;
      window.dispatchEvent(new Event('blur'));
    });
    assert(await button.isDisabled(), 'Losing focus must deactivate confirmation');
    await page.clock.runFor(2000);
    assert(await button.isDisabled(), 'Background time must not count');
    await page.evaluate(() => {
      document.hasFocus = () => true;
      window.dispatchEvent(new Event('focus'));
    });
    await page.keyboard.press('ArrowDown');
    await page.clock.runFor(749);
    assert(await button.isDisabled());
    await page.clock.runFor(1);
    assert(await button.isDisabled(), 'Early input after focus reset must not count');
    await page.keyboard.press('ArrowDown');
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
    await page.clock.runFor(750);
    assert(await button.isDisabled());
    await page.keyboard.press('ArrowDown');
    await button.click();
    await page.getByText('Confirmed', { exact: true }).waitFor();
    assert.equal(submissions(), 1);

    for (const action of ['public', 'delete']) {
      await page.goto(`${manager}/confirm?id=7k3d&action=${action}`);
      const confirm = page.getByRole('button', { name: `Confirm ${action}` });
      await page.locator('#confirm-id').fill('wrong');
      await page.clock.runFor(750);
      await page.keyboard.press('ArrowDown');
      assert(await confirm.isDisabled(), `${action} requires the exact app ID`);
      await page.locator('#confirm-id').fill('7k3d');
      await confirm.click();
      await page.getByText('Confirmed', { exact: true }).waitFor();
    }
    assert.equal(submissions(), 3);
  } finally {
    await context.close();
  }
}

async function verifyMobile(browser, device) {
  const { context, page, submissions } = await fixtureContext(browser, playwright.devices[device]);
  try {
    for (const action of ['renew', 'private', 'public', 'delete']) {
      await page.goto(`${manager}/confirm?id=7k3d&action=${action}`);
      const button = page.getByRole('button', { name: `Confirm ${action}` });
      if (['public', 'delete'].includes(action)) await page.locator('#confirm-id').fill('7k3d');
      const bounds = await button.boundingBox();
      assert(bounds);
      const tap = () => page.touchscreen.tap(bounds.x + bounds.width / 2, bounds.y + bounds.height / 2);
      const before = submissions();
      await tap();
      await page.clock.runFor(750);
      assert.equal(submissions(), before, 'Early tap must not submit');
      assert(await button.isDisabled(), 'Early tap must not arm a later timer activation');
      await tap();
      await page.getByText('Confirmed', { exact: true }).waitFor({ timeout: 1000 });
      assert.equal(submissions(), before + 1, `${device}: one tap after the delay must submit exactly once`);
    }
  } finally {
    await context.close();
  }
}

for (const engine of (process.env.MESH_CONFIRM_ENGINES || 'chromium,webkit').split(',')) {
  const browser = await playwright[engine].launch({ headless: true,
    ...(engine === 'chromium' && process.env.MESH_CHROMIUM_EXECUTABLE
      ? { executablePath: process.env.MESH_CHROMIUM_EXECUTABLE } : {}),
  });
  try {
    if (process.env.MESH_CONFIRM_CASE !== 'mobile') await verifyDesktop(browser);
    if (process.env.MESH_CONFIRM_CASE !== 'desktop') {
      await verifyMobile(browser, engine === 'webkit' ? 'iPhone 13' : 'Pixel 7');
    }
    console.log(`PASS ${engine}: inert arrival, post-delay trusted input, focus/visibility resets, exact IDs, and single native mobile taps`);
  } finally {
    await browser.close();
  }
}
