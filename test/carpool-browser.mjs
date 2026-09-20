import assert from 'node:assert/strict';
import { readFile, mkdir, writeFile } from 'node:fs/promises';

const { chromium } = await import(process.env.PLAYWRIGHT_MODULE || 'playwright');
const fixture = JSON.parse(await readFile(process.env.CARPOOL_BROWSER_READY_FILE, 'utf8'));
assert.equal(fixture.synthetic, true);
assert.equal(new URL(fixture.url).hostname, '127.0.0.1');
const output = process.env.CARPOOL_BROWSER_OUTPUT || '/tmp/carpool-browser-evidence';
await mkdir(output, { recursive: true });
const browser = await chromium.launch({ headless: true, executablePath: process.env.CHROMIUM_PATH, args: ['--no-sandbox'] });
const errors = [];
const checks = [];
async function context(width = 1440, height = 900) {
  const ctx = await browser.newContext({ viewport: { width, height } });
  await ctx.route('**/*', route => new URL(route.request().url()).origin === fixture.url ? route.continue() : route.abort());
  const page = await ctx.newPage();
  page.on('pageerror', error => errors.push(error.message));
  page.on('dialog', dialog => dialog.accept());
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
  await page.locator(`[data-route="${route}"]`).click();
  await page.locator('#content .loading').waitFor({ state: 'detached' });
}
async function shot(page, name) {
  await page.waitForFunction(() => !document.querySelector('.toast'));
  await page.screenshot({ path: `${output}/${name}.png`, fullPage: true, mask: [page.locator('.api-key-value code')] });
  assert(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth + 1), `${name}: page overflow`);
}
async function api(page, path) {
  return page.evaluate(async path => { const r = await fetch('/carpool/api/v1' + path); return { status: r.status, body: await r.json() }; }, path);
}
async function generate(page, key, model = fixture.model, stream = false) {
  return page.evaluate(async ({ key, model, stream }) => {
    const r = await fetch('/v1/chat/completions', { method: 'POST', headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${key}` }, body: JSON.stringify({ model, stream, messages: [{ role: 'user', content: 'Synthetic QA only' }] }) });
    return { status: r.status, body: await r.text() };
  }, { key, model, stream });
}
async function manageCar(admin) {
  await nav(admin, '/cars');
  await admin.locator('tr').filter({ hasText: 'QA Shared Car' }).locator('[data-manage-car]').click();
  await admin.locator('#add-member').waitFor();
}
async function quota(admin, value) {
  await manageCar(admin);
  await admin.locator('[data-edit-member-limit][data-member-name="QA Passenger 1"]').click();
  await admin.locator('#member-five-hour').fill(value);
  await admin.locator('#member-weekly').fill(value);
  await admin.locator('#member-quota-form button[type=submit]').click();
  await admin.locator('#member-quota-form').waitFor({ state: 'detached' });
}
try {
  const admin = await context();
  await login(admin, fixture.admin);
  const passenger = await context();
  await login(passenger, fixture.passengers[0]);
  await passenger.locator('#password-form').waitFor();
  assert.equal((await api(passenger, '/me/car')).status, 403);
  await shot(passenger, 'desktop-forced-password');
  const credentials = { username: fixture.passengers[0].username, password: 'Fake-Browser-Changed-Only-2026!' };
  await passenger.locator('#current-password').fill(fixture.passengers[0].password);
  await passenger.locator('#new-password').fill(credentials.password);
  await passenger.locator('#password-form button').click();
  await login(passenger, credentials);
  await nav(passenger, '/');
  checks.push('首次强制改密、改密后重新登录');
  await nav(admin, '/users');
  await admin.locator('#create-user').click();
  await admin.locator('#new-username').fill('qa-new-member');
  await admin.locator('#new-display-name').fill('QA New Member');
  await admin.locator('#user-form button[type=submit]').click();
  await admin.locator('.secret-box').waitFor();
  await admin.locator('[data-finish]').click();
  await manageCar(admin);
  await admin.locator('#member-user').selectOption({ label: 'QA New Member · qa-new-member' });
  for (const selector of ['#member-five-hour-new', '#member-weekly-new']) {
    await admin.locator(selector).fill('');
    assert.equal(await admin.locator(selector).evaluate(el => el.checkValidity()), false);
    await admin.locator(selector).fill('-1');
    assert.equal(await admin.locator(selector).evaluate(el => el.checkValidity()), false);
    await admin.locator(selector).fill('0');
  }
  await admin.locator('#add-member button[type=submit]').click();
  await admin.locator('#add-member').waitFor({ state: 'detached' });
  checks.push('创建用户、上车必填/负数校验、零额度上车');
  await nav(passenger, '/keys');
  await passenger.locator('#create-key').click();
  await passenger.locator('#key-name').fill('Synthetic Browser QA');
  await passenger.locator('#key-form button[type=submit]').click();
  await passenger.locator('#key-form').waitFor({ state: 'detached' });
  const key = await passenger.locator('.api-key-value code').first().innerText();
  assert(key.startsWith('cpk_v1_'));
  const models = await passenger.evaluate(async key => (await (await fetch('/v1/models', { headers: { Authorization: `Bearer ${key}` } })).json()), key);
  assert(!JSON.stringify(models).includes('qa-isolated-only'));
  assert.equal((await generate(passenger, key)).status, 200);
  assert.equal((await api(passenger, '/me/car')).body.quota_windows.find(window => window.kind === '7d').used_usd, '0.045');
  assert.equal((await generate(passenger, key, fixture.model, true)).status, 200);
  assert.equal((await api(passenger, '/me/car')).body.quota_windows.find(window => window.kind === '7d').used_usd, '0.09');
  const calls = () => passenger.evaluate(async () => (await (await fetch('/__fixture/calls')).json()).calls);
  const before = await calls();
  const missing = await generate(passenger, key, fixture.missing_model);
  assert.equal(missing.status, 422);
  assert(missing.body.includes('model_price_not_configured'));
  assert.equal(await calls(), before);
  checks.push('Key 创建、车辆模型隔离、普通/流式消费各 $0.045、缺价 422 且上游零调用');
  await quota(admin, '0.01');
  assert.equal((await generate(passenger, key)).status, 403);
  assert.equal(await calls(), before);
  await nav(passenger, '/');
  await shot(passenger, 'desktop-overage');
  await quota(admin, '2');
  assert.equal((await generate(passenger, key)).status, 200);
  await nav(passenger, '/');
  await shot(passenger, 'desktop-billing');
  checks.push('调低立即拦截、调高立即恢复');
  await nav(passenger, '/accounts');
  await shot(passenger, 'desktop-accounts');
  const statusOnly = passenger.locator('.quota-window').filter({ hasText: '已拒绝' });
  assert.equal(await statusOnly.locator('[role=progressbar]').count(), 0, 'status-only quota must not invent zero percent');
  checks.push('数值配额、仅状态配额、缺失及陈旧观测');
  await nav(admin, '/requests');
  await admin.locator('.request-row').first().locator('summary').click();
  await shot(admin, 'desktop-requests');
  assert((await admin.locator('#content').innerText()).includes('gpt-4o'));
  await nav(admin, '/retention');
  async function retentionDays(days) {
    await admin.locator(`input[name=days][value="${days}"]`).check();
    const refreshed = admin.waitForResponse(response => response.url().endsWith('/admin/retention') && response.request().method() === 'GET');
    await admin.locator('#retention-form button[type=submit]').click();
    await refreshed;
    await admin.getByText(`当前明细保留：${days === 0 ? "永久" : `${days} 天`} · 来源：管理端设置`, { exact: true }).waitFor();
  }
  await retentionDays(180);
  // Permanent retention disables automatic expiry; manual cleanup still removes completed details.
  await retentionDays(0);
  const billed = (await api(passenger, '/me/car')).body.quota_windows.find(window => window.kind === '7d').used_usd;
  const previewResponse = admin.waitForResponse(response => response.url().endsWith('/admin/retention/preview') && response.request().method() === 'POST');
  await admin.locator('[data-retention-op="usage_details"]').click();
  assert((await (await previewResponse).json()).expected_count > 0, 'manual detail cleanup must preview real records');
  await admin.locator('[data-retention-confirm]').waitFor();
  await shot(admin, 'desktop-retention-preview');
  const cleanupResponse = admin.waitForResponse(response => response.url().endsWith('/admin/retention/jobs') && response.request().method() === 'POST');
  await admin.locator('[data-retention-confirm]').click();
  const cleanup = await (await cleanupResponse).json();
  assert.equal(cleanup.status, 'completed');
  assert(cleanup.deleted_count > 0, 'manual detail cleanup must delete real records');
  await admin.locator('[data-retention-confirm]').waitFor({ state: 'detached' });
  assert.equal((await api(passenger, '/me/car')).body.quota_windows.find(window => window.kind === '7d').used_usd, billed);
  assert.equal((await api(admin, '/admin/usage/requests?period=30d')).body.total, 0);
  // Keep a nonzero detail-retention policy while exercising current-period reset.
  await retentionDays(180);
  checks.push('请求及事件明细、保留设置、清理预览/执行且金额不变');
  await nav(admin, '/users');
  await admin.locator('tr').filter({ hasText: 'qa-passenger-1' }).locator('[data-manage-user]').click();
  await admin.locator('[data-reset-password]').click();
  await admin.locator('.secret-box').waitFor();
  const temporary = await admin.locator('.secret-box').innerText();
  await admin.locator('[data-finish]').click();
  await passenger.reload();
  await login(passenger, { username: credentials.username, password: temporary });
  await passenger.locator('#password-form').waitFor();
  assert((await passenger.locator('.password-notice').innerText()).includes('管理员'));
  assert.equal((await api(passenger, '/me/car')).status, 403);
  await passenger.locator('#current-password').fill(temporary);
  await passenger.locator('#new-password').fill(credentials.password);
  await passenger.locator('#password-form button').click();
  await login(passenger, credentials);
  await nav(passenger, '/');
  assert.equal((await api(passenger, '/me/car')).body.quota_windows.find(window => window.kind === '7d').used_usd, billed);
  assert.equal((await generate(passenger, key)).status, 200);
  checks.push('管理员重置密码、旧会话失效、强制改密期间工作台拦截、改密后Key及金额保留');
  for (const width of [390, 360]) {
    await passenger.setViewportSize({ width, height: 844 });
    for (const route of ['/', '/members', '/accounts', '/keys']) {
      await nav(passenger, route);
      await shot(passenger, `mobile-${width}-${route.slice(1) || 'overview'}`);
    }
    await admin.setViewportSize({ width, height: 844 });
    await manageCar(admin);
    await admin.locator('[data-edit-member-limit][data-member-name="QA Passenger 1"]').click();
    await shot(admin, `mobile-${width}-quota-dialog`);
    await admin.keyboard.press('Escape');
    await admin.keyboard.press('Escape');
    assert.equal(await admin.locator('dialog[open]').count(), 0);
    await quota(admin, '3');
    assert.equal((await generate(passenger, key)).status, 200);
    await nav(admin, '/requests');
    await admin.locator('.request-row').first().locator('summary').click();
    await shot(admin, `mobile-${width}-requests`);
    await nav(admin, '/retention');
    await admin.locator('[data-retention-op="reset_quota_windows"]').click();
    await shot(admin, `mobile-${width}-reset-preview`);
    await admin.locator('[data-retention-confirm]').click();
    await admin.locator('[data-retention-confirm]').waitFor({ state: 'detached' });
    assert.equal((await api(passenger, '/me/car')).body.quota_windows.find(window => window.kind === '7d').used_usd, '0');
    assert.equal((await api(passenger, '/me/car')).body.quota_windows.find(window => window.kind === '7d').limit_usd, '3');
  }
  await passenger.setViewportSize({ width: 1440, height: 900 });
  await passenger.evaluate(() => { document.documentElement.style.zoom = '2'; });
  await nav(passenger, '/');
  await shot(passenger, 'desktop-200-percent');
  checks.push('390/360 移动端导航、调额保存、消费、明细与本期重置；Esc 关闭、200% 缩放无整页溢出');
  assert.deepEqual(errors, []);
  await writeFile(`${output}/results.json`, JSON.stringify({ checks, errors }, null, 2));
  console.log(JSON.stringify({ output, checks }, null, 2));
} finally {
  await browser.close();
}
