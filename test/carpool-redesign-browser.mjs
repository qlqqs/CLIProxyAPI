import assert from 'node:assert/strict';
import { mkdir, readFile, writeFile } from 'node:fs/promises';

const { chromium } = await import(process.env.PLAYWRIGHT_MODULE || 'playwright');
const fixture = JSON.parse(await readFile(process.env.CARPOOL_BROWSER_READY_FILE, 'utf8'));
assert.equal(fixture.synthetic, true);
assert.equal(new URL(fixture.url).hostname, '127.0.0.1');
const output = process.env.CARPOOL_BROWSER_OUTPUT || '/tmp/carpool-redesign-evidence';
await mkdir(output, { recursive: true });
const browser = await chromium.launch({ headless: true, executablePath: process.env.CHROMIUM_PATH, args: ['--no-sandbox'] });
const errors = [];
const checks = [];
async function newPage() {
  const context = await browser.newContext({ viewport: { width: 1440, height: 1000 } });
  await context.route('**/*', route => new URL(route.request().url()).origin === fixture.url ? route.continue() : route.abort());
  const page = await context.newPage();
  page.on('pageerror', error => errors.push(error.message));
  await page.goto(fixture.url + '/carpool/');
  return page;
}
async function login(page, credentials) {
  await page.locator('#username').fill(credentials.username);
  await page.locator('#password').fill(credentials.password);
  await page.locator('#login-form button[type=submit]').click();
  await page.locator('#logout').waitFor();
}
async function nav(page, route) {
  const toggle = page.locator('#nav-toggle');
  if (await toggle.isVisible() && await toggle.getAttribute('aria-expanded') !== 'true') await toggle.click();
  await page.locator(`.nav [data-route="${route}"]`).click();
  await page.waitForFunction(route => document.querySelector('#content')?.dataset.page === route && !document.querySelector('#content .loading'), route);
}
async function screenshot(page, name) {
  assert.equal(await page.locator('.secret-box').count(), 0, 'never screenshot one-time credentials');
  await page.evaluate(() => { window.scrollTo(0, 0); const main = document.querySelector('.main'); if (main) main.scrollTop = 0; });
  await page.screenshot({ path: `${output}/${name}.png`, fullPage: true, mask: [page.locator('.api-key-value code')] });
  assert(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth + 1), `${name}: page overflow`);
}
try {
  const admin = await newPage();
  await screenshot(admin, 'login-desktop');
  await admin.locator('[data-theme-toggle]').click();
  assert.equal(await admin.evaluate(() => document.documentElement.dataset.theme), 'dark');
  assert.equal(await admin.locator('meta[name="theme-color"]').getAttribute('content'), '#181714');
  await screenshot(admin, 'login-dark-desktop');
  await admin.reload();
  assert.equal(await admin.evaluate(() => document.documentElement.dataset.theme), 'dark');
  await admin.setViewportSize({ width: 390, height: 844 });
  await screenshot(admin, 'login-mobile');
  await admin.setViewportSize({ width: 1440, height: 1000 });
  await login(admin, fixture.admin);
  assert.equal(await admin.evaluate(() => document.documentElement.dataset.theme), 'dark');
  assert.equal(await admin.locator('.topbar [data-theme-toggle]').getAttribute('aria-label'), '切换至浅色主题');
  await screenshot(admin, 'admin-dark-overview');
  await admin.locator('.topbar [data-theme-toggle]').click();
  assert.equal(await admin.evaluate(() => document.documentElement.dataset.theme), 'light');
  await admin.locator('.skip-link').focus();
  await admin.keyboard.press('Enter');
  assert.equal(await admin.locator('#content').evaluate(node => node === document.activeElement), true);
  assert.equal(new URL(admin.url()).hash, '#/');
  const routes = ['/', '/users', '/cars', '/usage', '/requests', '/pricing', '/retention', '/audit', '/password'];
  for (const width of [1440, 1024, 390, 360]) {
    await admin.setViewportSize({ width, height: width > 760 ? 1000 : 844 });
    if (width <= 760) {
      await admin.locator('#nav-toggle').click();
      assert.equal(await admin.locator('.nav a').first().evaluate(node => node === document.activeElement), true);
      await admin.keyboard.press('Escape');
      assert.equal(await admin.locator('#nav-toggle').getAttribute('aria-expanded'), 'false');
      assert.equal(await admin.locator('#nav-toggle').evaluate(node => node === document.activeElement), true);
    }
    for (const route of routes) {
      await nav(admin, route);
      assert.equal(await admin.locator('#content .error-banner').count(), 0, `${route}: failed loading`);
      await screenshot(admin, `admin-${width}-${route.slice(1) || 'overview'}`);
    }
    for (const [route, selector] of [['/users', '[data-manage-user]'], ['/cars', '[data-manage-car]']]) {
      await nav(admin, route);
      await admin.locator(selector).first().click();
      await admin.locator('dialog.entity-panel .entity-summary').waitFor();
      assert.equal(await admin.locator('dialog.entity-panel').evaluate(dialog => dialog.matches(':modal')), false);
      await screenshot(admin, `admin-${width}-${route.slice(1)}-detail`);
      await admin.locator('dialog.entity-panel [data-dialog-close]').first().click();
      await admin.locator('dialog.entity-panel').waitFor({ state: 'detached' });
    }
  }
  checks.push('管理端明暗主题切换与持久化、全部九入口与用户/车辆非模态详情，1440/1024/390/360px 无整页溢出');
  await admin.setViewportSize({ width: 1440, height: 1000 });
  await nav(admin, '/cars');
  await admin.locator('[data-manage-car]').first().click();
  await admin.locator('#edit-car-form').waitFor();
  let finishMutation;
  const mutationGate = new Promise(resolve => { finishMutation = resolve; });
  let mutationStarted;
  const mutationEntered = new Promise(resolve => { mutationStarted = resolve; });
  await admin.route('**/carpool/api/v1/admin/cars/*', async route => {
    if (route.request().method() !== 'PATCH') return route.continue();
    mutationStarted();
    await mutationGate;
    await route.fulfill({ status: 200, contentType: 'application/json', body: '{}' });
  });
  await admin.locator('#edit-car-form button[type=submit]').click();
  await mutationEntered;
  await admin.locator('[data-manage-car]').nth(1).click();
  await admin.locator('#edit-car-form').waitFor();
  await admin.locator('#edit-car-name').fill('Synthetic unsaved car B draft');
  const oldMutation = admin.waitForResponse(response => response.request().method() === 'PATCH');
  finishMutation();
  await oldMutation;
  await admin.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
  assert.equal(await admin.locator('#edit-car-name').inputValue(), 'Synthetic unsaved car B draft');
  await admin.unroute('**/carpool/api/v1/admin/cars/*');
  checks.push('旧车辆保存响应不会关闭另一辆车的详情或丢弃未保存输入');
  const passenger = await newPage();
  const initial = fixture.passengers[1];
  await login(passenger, initial);
  assert.equal(await passenger.locator('.nav [data-route]').count(), 1);
  await screenshot(passenger, 'forced-password');
  const credentials = { username: initial.username, password: 'Fake-Redesign-Second-Passenger-2026!' };
  await passenger.locator('#current-password').fill(initial.password);
  await passenger.locator('#new-password').fill(credentials.password);
  await passenger.locator('#password-form button').click();
  await passenger.locator('#login-form').waitFor();
  await login(passenger, credentials);
  await passenger.locator('.topbar [data-theme-toggle]').click();
  assert.equal(await passenger.evaluate(() => document.documentElement.dataset.theme), 'dark');
  await screenshot(passenger, 'passenger-dark-overview');
  await passenger.locator('.topbar [data-theme-toggle]').click();
  for (const width of [1440, 390, 360]) {
    await passenger.setViewportSize({ width, height: width > 760 ? 1000 : 844 });
    for (const route of ['/', '/members', '/accounts', '/keys', '/password']) {
      await nav(passenger, route);
      assert.equal(await passenger.locator('#content .error-banner').count(), 0);
      await screenshot(passenger, `passenger-${width}-${route.slice(1) || 'overview'}`);
    }
  }
  checks.push('用户端明暗主题切换、五入口、登录、首次改密门禁、空 Key 与账号状态，桌面和双手机宽度');
  await admin.setViewportSize({ width: 1440, height: 1000 });
  await admin.route('**/carpool/api/v1/admin/pricing', route => route.fulfill({ status: 503, contentType: 'application/json', body: JSON.stringify({ error: { message: '测试服务暂不可用', code: 'qa_unavailable' } }) }));
  await nav(admin, '/pricing');
  await admin.locator('#content .error-banner').waitFor();
  assert((await admin.locator('#content').innerText()).includes('重试'));
  await screenshot(admin, 'error-recovery');
  await admin.unroute('**/carpool/api/v1/admin/pricing');
  await admin.locator('[data-route-retry]').click();
  await admin.locator('.pricing-panel').waitFor();
  checks.push('异步页面失败由路由捕获，重试可恢复');
  await nav(admin, "/audit");
  let release;
  const gate = new Promise(resolve => { release = resolve; });
  let started;
  const entered = new Promise(resolve => { started = resolve; });
  await admin.route('**/carpool/api/v1/admin/pricing', async route => { started(); await gate; await route.fulfill({ status: 503, contentType: 'application/json', body: JSON.stringify({ error: { message: '迟到的旧错误' } }) }); });
  await admin.locator('.nav [data-route="/pricing"]').click();
  await entered;
  await nav(admin, '/password');
  const lateResponse = admin.waitForResponse(response => response.url().endsWith('/admin/pricing'));
  release();
  await lateResponse;
  assert.equal(await admin.locator('#password-form').count(), 1);
  assert.equal(await admin.locator('#content .error-banner').count(), 0);
  await admin.unroute('**/carpool/api/v1/admin/pricing');
  checks.push('迟到的旧路由响应不能覆盖当前页');
  await passenger.setViewportSize({ width: 1440, height: 1000 });
  await passenger.evaluate(() => { document.documentElement.style.zoom = '2'; });
  await nav(passenger, '/');
  await screenshot(passenger, 'passenger-200-percent');
  assert.deepEqual(errors, []);
  await writeFile(`${output}/results.json`, JSON.stringify({ checks, errors }, null, 2));
  console.log(JSON.stringify({ output, checks }, null, 2));
} finally {
  await browser.close();
}
