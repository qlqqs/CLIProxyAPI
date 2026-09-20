import assert from 'node:assert/strict';
import { readFile, mkdir, writeFile } from 'node:fs/promises';

const { chromium } = await import(process.env.PLAYWRIGHT_MODULE || 'playwright');
const fixture = JSON.parse(await readFile(process.env.CARPOOL_BROWSER_READY_FILE, 'utf8'));
assert.equal(fixture.synthetic, true);
assert.equal(new URL(fixture.url).hostname, '127.0.0.1');
const output = process.env.CARPOOL_BROWSER_OUTPUT || '/tmp/carpool-retention-evidence';
await mkdir(output, { recursive: true });
const browser = await chromium.launch({ headless: true, executablePath: process.env.CHROMIUM_PATH, args: ['--no-sandbox'] });
const errors = [];
const checks = [];
async function pageFor(login) {
  const context = await browser.newContext({ viewport: { width: 1440, height: 900 } });
  await context.route('**/*', route => new URL(route.request().url()).origin === fixture.url ? route.continue() : route.abort());
  const page = await context.newPage();
  page.on('pageerror', error => errors.push(error.message));
  await page.goto(fixture.url + '/carpool/');
  await signIn(page, login);
  return page;
}
async function signIn(page, login) {
  await page.locator('#username').fill(login.username);
  await page.locator('#password').fill(login.password);
  await page.locator('#login-form button[type=submit]').click();
  await page.locator('#logout').waitFor();
}
async function nav(page, route) {
  await page.locator(`[data-route="${route}"]`).click();
  await page.waitForFunction(route => document.querySelector(`[data-route="${route}"]`)?.getAttribute('aria-current') === 'page' && !document.querySelector('#content .loading'), route);
}
async function api(page, path, body, method = 'POST') {
  return page.evaluate(async ({ path, body, method }) => {
    const session = await (await fetch('/carpool/api/v1/session')).json();
    const r = await fetch('/carpool/api/v1' + path, body === undefined ? {} : {
      method, headers: { 'Content-Type': 'application/json', 'X-Carpool-CSRF': session.csrf_token }, body: JSON.stringify(body),
    });
    return { status: r.status, body: await r.json() };
  }, { path, body, method });
}
async function screenshot(page, name) {
  await page.waitForFunction(() => !document.querySelector('.toast'));
  await page.screenshot({ path: `${output}/${name}.png`, fullPage: true, mask: [page.locator('.api-key-value code')] });
  assert(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth + 1), `${name}: overflow`);
}
try {
  const admin = await pageFor(fixture.admin);
  const passenger = await pageFor(fixture.passengers[0]);
  await passenger.locator('#current-password').fill(fixture.passengers[0].password);
  const changed = { username: fixture.passengers[0].username, password: 'Retention-Only-Fake-Password-2026!' };
  await passenger.locator('#new-password').fill(changed.password);
  await passenger.locator('#password-form button').click();
  await signIn(passenger, changed);
  await nav(passenger, '/keys');
  await passenger.locator('#create-key').click();
  await passenger.locator('#key-name').fill('Retention Browser Only');
  await passenger.locator('#key-form button[type=submit]').click();
  await passenger.locator('#key-form').waitFor({ state: 'detached' });
  const key = await passenger.locator('.api-key-value code').first().innerText();
  assert(key.startsWith('cpk_v1_'));
  await nav(admin, '/cars');
  await admin.locator('tr').filter({ hasText: 'QA Shared Car' }).locator('[data-manage-car]').click();
  await admin.locator('[data-edit-member-limit][data-member-name="QA Passenger 1"]').click();
  await admin.locator('#member-five-hour').fill('5');
  await admin.locator('#member-weekly').fill('5');
  await admin.locator('#member-quota-form button[type=submit]').click();
  await admin.locator('#member-quota-form').waitFor({ state: 'detached' });
  async function consume() {
    const status = await passenger.evaluate(async key => (await fetch('/v1/chat/completions', {
      method: 'POST', headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${key}` },
      body: JSON.stringify({ model: 'gpt-4o', messages: [{ role: 'user', content: 'Synthetic retention acceptance' }] }),
    })).status, key);
    assert.equal(status, 200);
  }
  for (let index = 0; index < 30; index++) await consume();
  assert.equal((await api(passenger, '/me/car')).body.quota_windows.find(window => window.kind === '7d').used_usd, '1.35');
  await nav(admin, '/requests');
  await admin.waitForFunction(() => document.querySelectorAll('.request-row').length === 25);
  assert.equal(await admin.locator('.request-row').count(), 25);
  await admin.locator('.request-row').first().locator('summary').click();
  await admin.locator('.request-row').first().locator('.request-event').waitFor();
  const detailText = await admin.locator('.request-row').first().innerText();
  assert(detailText.includes('12,000'));
  assert(detailText.includes('0.045'));
  await admin.locator('[data-load-more]').click();
  await admin.waitForFunction(() => document.querySelectorAll('.request-row').length === 30);
  checks.push('30 次消费、25+5 稳定分页、展开实际加载事件及真实 Token/费用');
  await admin.locator('#request-model').fill('gpt-4o');
  await admin.locator('#request-outcome').selectOption('succeeded');
  await admin.locator('#request-filter button[type=submit]').click();
  await admin.waitForFunction(() => location.hash.includes('model=gpt-4o'));
  await admin.reload();
  await admin.locator('#request-model').waitFor();
  assert.equal(await admin.locator('#request-model').inputValue(), 'gpt-4o');
  assert.equal(await admin.locator('#request-outcome').inputValue(), 'succeeded');
  const first = (await api(admin, '/admin/usage/requests?period=30d')).body;
  assert(first.period.from && first.period.to);
  assert.equal(first.items.length, 25);
  const wrong = await api(admin, '/admin/usage/requests?period=30d&model=changed&cursor=' + encodeURIComponent(first.next_cursor));
  assert.equal(wrong.status, 422);
  await admin.locator('#request-id').fill(first.items[0].request_id);
  await admin.locator('#request-filter button[type=submit]').click();
  await admin.waitForFunction(() => document.querySelectorAll('.request-row').length === 1);
  await admin.locator('.request-row summary').click();
  await admin.locator('.request-event').waitFor();
  await screenshot(admin, 'desktop-filter-details');
  checks.push('模型+结果+精确ID组合过滤、刷新恢复筛选、跨筛选游标拒绝、实际 period');
  await nav(admin, '/retention');
  const noPreview = await api(admin, '/admin/retention/jobs', { operation: 'reset_quota_windows', confirm: true });
  assert.equal(noPreview.status, 422);
  const denied = await api(passenger, '/admin/retention/preview', { operation: 'reset_quota_windows' });
  assert.equal(denied.status, 403);
  const stale = await api(admin, '/admin/retention/preview', { operation: 'reset_quota_windows' });
  assert.equal(stale.status, 200);
  await consume();
  const rejected = await api(admin, '/admin/retention/jobs', { operation: 'reset_quota_windows', job_id: stale.body.job_id, confirm: true });
  assert.equal(rejected.status, 409);
  assert.equal((await api(passenger, '/me/car')).body.quota_windows.find(window => window.kind === '7d').used_usd, '1.395');
  checks.push('无预览拒绝、乘客越权拒绝、预览后新增消费使旧确认失效且不扣除新消费');
  await admin.locator('[data-retention-op="reset_quota_windows"]').click();
  await admin.locator('[data-retention-confirm]').waitFor();
  await screenshot(admin, 'desktop-reset-preview');
  const complete = admin.waitForResponse(r => r.url().endsWith('/admin/retention/jobs') && r.request().method() === 'POST');
  await admin.locator('[data-retention-confirm]').click();
  const job = await (await complete).json();
  assert.equal(job.status, 'completed');
  assert(job.deleted_count > 0);
  assert.equal((await api(passenger, '/me/car')).body.quota_windows.find(window => window.kind === '7d').used_usd, '0');
  await consume();
  const duplicate = await api(admin, '/admin/retention/jobs', { operation: job.operation, job_id: job.job_id, confirm: true });
  assert.equal(duplicate.status, 202);
  assert.equal(duplicate.body.job_id, job.job_id);
  assert.equal((await api(passenger, '/me/car')).body.quota_windows.find(window => window.kind === '7d').used_usd, '0.045');
  assert.equal((await api(admin, '/admin/retention/jobs/' + job.job_id)).body.status, 'completed');
  checks.push('页面确认原子重置；重复确认返回同作业，不重置之后的 $0.045');
  await admin.setViewportSize({ width: 360, height: 844 });
  await nav(admin, '/retention');
  await admin.locator('input[name=days][value="0"]').check();
  const updated = admin.waitForResponse(r => r.url().endsWith('/admin/retention') && r.request().method() === 'GET');
  await admin.locator('#retention-form button[type=submit]').click();
  await updated;
  await admin.locator('[data-retention-op="usage_details"]').click();
  await admin.locator('[data-retention-cancel]').click();
  assert.equal((await api(admin, '/admin/usage/requests?period=30d')).body.total, 32);
  await admin.locator('[data-retention-op="usage_details"]').click();
  await admin.locator('[data-retention-confirm]').waitFor();
  await screenshot(admin, 'mobile-cleanup-preview');
  const cleaned = admin.waitForResponse(r => r.url().endsWith('/admin/retention/jobs') && r.request().method() === 'POST');
  await admin.locator('[data-retention-confirm]').click();
  const cleanup = await (await cleaned).json();
  assert.equal(cleanup.deleted_count, 32);
  assert.equal((await api(admin, '/admin/usage/requests?period=30d')).body.total, 0);
  assert.equal((await api(passenger, '/me/car')).body.quota_windows.find(window => window.kind === '7d').used_usd, '0.045');
  checks.push('360 移动端清理取消不删数据、确认真实删除32条、金额不变');
  assert.deepEqual(errors, []);
  await writeFile(`${output}/results.json`, JSON.stringify({ checks, errors }, null, 2));
  console.log(JSON.stringify({ output, checks }, null, 2));
} finally {
  await browser.close();
}
