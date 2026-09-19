import assert from 'node:assert/strict';
import { mkdir, readFile, writeFile } from 'node:fs/promises';

const { chromium } = await import(process.env.PLAYWRIGHT_MODULE || 'playwright');
const fixture = JSON.parse(await readFile(process.env.CARPOOL_BROWSER_READY_FILE, 'utf8'));
assert.equal(fixture.synthetic, true);
assert.equal(new URL(fixture.url).hostname, '127.0.0.1');
const output = process.env.CARPOOL_BROWSER_OUTPUT || '/tmp/carpool-accounts-evidence';
await mkdir(output, { recursive: true });
const browser = await chromium.launch({ headless: true, executablePath: process.env.CHROMIUM_PATH, args: ['--no-sandbox'] });
const errors = [];
const checks = [];
const context = await browser.newContext({ viewport: { width: 1440, height: 960 } });
await context.route('**/*', route => new URL(route.request().url()).origin === fixture.url ? route.continue() : route.abort());
const page = await context.newPage();
page.on('pageerror', error => errors.push(error.message));
page.on('dialog', dialog => dialog.accept());
async function login(credentials) {
  await page.goto(fixture.url + '/carpool/');
  await page.locator('#username').fill(credentials.username);
  await page.locator('#password').fill(credentials.password);
  await page.locator('#login-form button[type=submit]').click();
  await page.locator('#logout').waitFor();
}
async function nav(route) {
  const toggle = page.locator('#nav-toggle');
  if (await toggle.isVisible() && await toggle.getAttribute('aria-expanded') !== 'true') await toggle.click();
  await page.locator(`.nav [data-route="${route}"]`).click();
  await page.waitForFunction(route => document.querySelector('#content')?.dataset.page === route && !document.querySelector('#content .loading'), route);
}
async function shot(name) {
  await page.screenshot({ path: `${output}/${name}.png`, fullPage: true });
  assert(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth + 1), `${name}: page overflow`);
}
async function closeDialog() {
  await page.locator('dialog[open] [data-dialog-close]').first().click();
  await page.locator('dialog[open]').waitFor({ state: 'detached' });
}
async function accountList() {
  return page.evaluate(async () => {
    const response = await fetch('/carpool/api/v1/admin/auth-files');
    return { status: response.status, data: await response.json() };
  });
}
try {
  await login(fixture.admin);
  const routes = await page.locator('.nav [data-route]').evaluateAll(nodes => nodes.map(node => node.dataset.route));
  assert.deepEqual(routes.slice(0, 4), ['/', '/cars', '/admin-accounts', '/users']);
  await nav('/admin-accounts');
  await page.locator('[data-account-counts]').filter({ hasText: '共' }).waitFor();
  const initial = await accountList();
  assert.equal(initial.status, 200);
  assert(initial.data.files.every(file => ['codex', 'openai'].includes(file.provider)));
  assert(initial.data.files.every(file => file.quota && Array.isArray(file.quota.windows)));
  assert(!JSON.stringify(initial.data).includes('claude'));
  const emptyQuota = page.locator('.account-table tbody tr').first().locator('td').nth(3);
  await emptyQuota.locator('.quota-meter-compact').first().waitFor();
  assert.equal(await emptyQuota.locator('.quota-window').count(), 2);
  assert.deepEqual(await emptyQuota.locator('.quota-window-compact > strong').allInnerTexts(), ['5h', '7d']);
  assert.deepEqual(await emptyQuota.locator('progress').evaluateAll(nodes => nodes.map(node => node.value)), [0, 0]);
  assert.deepEqual(await emptyQuota.locator('.quota-meter-compact b').allInnerTexts(), ['0%', '0%']);
  assert(!/暂无配额数据|被动更新|账号产生请求且上游返回配额后更新/.test(await emptyQuota.innerText()));
  checks.push('管理员入口顺序、账号配额 5h/7d 双进度和仅 OpenAI/Codex 的真实列表');

  let testRow = page.locator('.account-table tbody tr').filter({ has: page.locator('[data-account-test]:not([disabled])') }).first();
  await testRow.locator('[data-account-test]').click();
  const testDialog = page.locator('dialog[open]').filter({ hasText: '测试账号连接' });
  await testDialog.locator('#account-test-model').fill(fixture.model);
  await testDialog.locator('[data-account-test-submit]').click();
  await testDialog.locator('[data-account-test-status]').filter({ hasText: '连接成功' }).waitFor();
  assert.match(await testDialog.locator('[data-account-test-status]').innerText(), /毫秒[\s\S]*字节/);
  assert(!await testDialog.innerText().then(text => /access_token|refresh_token|SYNTHETIC-ACCESS/.test(text)));
  await shot('account-test-success-desktop');
  await closeDialog();

  await page.route('**/carpool/api/v1/admin/auth-files/test', route => route.fulfill({ status: 429, contentType: 'application/json', body: JSON.stringify({ error: { code: 'rate_limited', message: '账号额度或速率受限，请稍后重试' } }) }));
  testRow = page.locator('.account-table tbody tr').filter({ has: page.locator('[data-account-test]:not([disabled])') }).first();
  await testRow.locator('[data-account-test]').click();
  await page.locator('dialog[open] #account-test-model').fill(fixture.model);
  await page.locator('dialog[open] [data-account-test-submit]').click();
  await page.locator('dialog[open] [data-account-test-status]').filter({ hasText: '账号额度或速率受限' }).waitFor();
  assert.equal(await page.locator('dialog[open] [data-account-test-submit]').innerText(), '重试');
  await page.unroute('**/carpool/api/v1/admin/auth-files/test');
  await page.locator('dialog[open] [data-account-test-submit]').click();
  await page.locator('dialog[open] [data-account-test-status]').filter({ hasText: '连接成功' }).waitFor();
  await closeDialog();
  checks.push('账号测试弹窗成功、限流分类和原位重试');

  const suffix = Date.now();
  const name = `qa-import-${suffix}.json`;
  await page.locator('[data-account-import]').click();
  await page.locator('#account-files').setInputFiles([
    { name, mimeType: 'application/json', buffer: Buffer.from(JSON.stringify({ type: 'codex', email: 'qa-synthetic@example.invalid', access_token: 'SYNTHETIC-ACCESS-NOT-REAL', refresh_token: 'SYNTHETIC-REFRESH-NOT-REAL' })) },
    { name: 'qa-other-provider.json', mimeType: 'application/json', buffer: Buffer.from('{"type":"claude","access_token":"SYNTHETIC-OTHER"}') },
    { name: 'qa-invalid.json', mimeType: 'application/json', buffer: Buffer.from('{broken') },
  ]);
  await page.locator('[data-import-form] button[type=submit]').click();
  await page.locator('.account-import-results').filter({ hasText: `${name} · 导入成功` }).waitFor();
  await page.locator('.account-import-results').filter({ hasText: 'JSON 格式无效' }).waitFor();
  assert.match(await page.locator('.account-import-results').innerText(), /仅支持 OpenAI/);
  await shot('import-results-desktop');
  await closeDialog();
  let listed = await accountList();
  const imported = listed.data.files.find(file => file.name === name);
  assert(imported, 'real upload must reach the live auth manager');
  assert(!JSON.stringify(listed.data).includes('SYNTHETIC-ACCESS'));
  checks.push('真实多文件逐项导入；非法 JSON 与其他供应商拒绝；列表不返回凭据');

  await page.locator('[data-account-import]').click();
  await page.locator('#account-files').setInputFiles({ name, mimeType: 'application/json', buffer: Buffer.from('{"type":"codex","access_token":"SYNTHETIC-REPLACEMENT"}') });
  await page.locator('[data-import-form] button[type=submit]').click();
  await page.locator('.account-import-results').filter({ hasText: '同名账号已存在' }).waitFor();
  await closeDialog();
  assert.equal(JSON.parse(await readFile(`${fixture.auth_dir}/${name}`, 'utf8')).access_token, 'SYNTHETIC-ACCESS-NOT-REAL');
  checks.push('同名文件提示重命名，原始凭据未被替换');

  let row = page.locator('.account-table tbody tr').filter({ hasText: name });
  await row.locator('[data-account-toggle]').click();
  await row.locator('[data-account-toggle]').filter({ hasText: '启用' }).waitFor();
  listed = await accountList();
  assert.equal(listed.data.files.find(file => file.name === name).disabled, true);
  assert.equal(JSON.parse(await readFile(`${fixture.auth_dir}/${name}`, 'utf8')).disabled, true);
  await page.reload();
  row = page.locator('.account-table tbody tr').filter({ hasText: name });
  await row.locator('[data-account-toggle]').filter({ hasText: '启用' }).waitFor();
  await row.locator('[data-account-toggle]').click();
  await row.locator('[data-account-toggle]').filter({ hasText: '禁用' }).waitFor();
  assert.equal((await accountList()).data.files.find(file => file.name === name).disabled, false);
  assert.equal(JSON.parse(await readFile(`${fixture.auth_dir}/${name}`, 'utf8')).disabled, false);
  await row.locator('[data-account-detail]').click();
  assert(!await page.locator('dialog').innerText().then(text => text.includes('SYNTHETIC-ACCESS')));
  await shot('account-detail-desktop');
  await closeDialog();
  checks.push('真实启停写入、刷新后状态保持、无凭据详情');

  let releasePatch, releaseList, patchEntered, listEntered;
  const patchGate = new Promise(resolve => { releasePatch = resolve; });
  const listGate = new Promise(resolve => { releaseList = resolve; });
  const patchStarted = new Promise(resolve => { patchEntered = resolve; });
  const listStarted = new Promise(resolve => { listEntered = resolve; });
  let refreshRequests = 0;
  await page.route('**/carpool/api/v1/admin/auth-files/status', async route => {
    patchEntered();
    await patchGate;
    await route.continue();
  });
  await page.route('**/carpool/api/v1/admin/auth-files', async route => {
    refreshRequests++;
    if (refreshRequests === 1) {
      const oldResponse = await route.fetch();
      listEntered();
      await listGate;
      await route.fulfill({ response: oldResponse });
    } else await route.continue();
  });
  await row.locator('[data-account-toggle]').click();
  await patchStarted;
  await page.locator('[data-account-refresh]').click();
  await listStarted;
  releasePatch();
  await page.waitForFunction(name => {
    const row = Array.from(document.querySelectorAll('.account-table tbody tr')).find(row => row.textContent.includes(name));
    return row && !row.querySelector('[data-account-toggle]').textContent.includes('保存中');
  }, name);
  releaseList();
  await row.locator('[data-account-toggle]').filter({ hasText: '启用' }).waitFor();
  assert(refreshRequests >= 2, 'mutation must queue a trailing refresh instead of accepting a stale snapshot');
  await page.unroute('**/carpool/api/v1/admin/auth-files/status');
  await page.unroute('**/carpool/api/v1/admin/auth-files');
  await row.locator('[data-account-toggle]').click();
  await row.locator('[data-account-toggle]').filter({ hasText: '禁用' }).waitFor();
  checks.push('确定性拦截旧列表响应，验证并发刷新与启停不会显示过期状态');

  await page.locator('#account-search').fill(name);
  await page.locator('.account-filter button[type=submit]').click();
  assert.equal(await page.locator('.account-table tbody tr').count(), 1);
  assert(new URL(page.url()).hash.includes('q='));
  await page.reload();
  await page.locator('#account-search').waitFor();
  assert.equal(await page.locator('#account-search').inputValue(), name);
  await page.locator('[data-account-clear]').click();
  await page.locator('#account-status').selectOption('disabled');
  await page.locator('.account-filter button[type=submit]').click();
  assert.equal(await page.locator('.account-table tbody tr').filter({ hasText: name }).count(), 0);
  await page.locator('[data-account-clear]').click();
  checks.push('搜索、状态筛选、URL 恢复与清除');

  await page.route('**/carpool/api/v1/admin/auth-files', route => route.fulfill({ status: 503, contentType: 'application/json', body: JSON.stringify({ error: { message: '验收模拟临时故障' } }) }));
  await page.locator('[data-account-refresh]').click();
  await page.locator('[data-account-error]').filter({ hasText: '验收模拟临时故障' }).waitFor();
  await page.unroute('**/carpool/api/v1/admin/auth-files');
  await page.locator('[data-account-refresh]').click();
  await page.waitForFunction(() => document.querySelector('[data-account-error]').textContent === '' && document.querySelector('[data-account-results]').getAttribute('aria-busy') === 'false');
  checks.push('刷新失败保留列表并可恢复');

  await page.route('**/carpool/api/v1/admin/auth-files', route => route.fulfill({ contentType: 'application/json', body: '{"files":[]}' }));
  await page.locator('[data-account-refresh]').click();
  await page.locator('[data-account-results]').filter({ hasText: '尚未添加账号' }).waitFor();
  await page.unroute('**/carpool/api/v1/admin/auth-files');
  await page.locator('[data-account-refresh]').click();
  await page.locator('.account-table tbody tr').first().waitFor();
  checks.push('空列表展示导入与 OAuth 下一步入口');

  for (const width of [1440, 390, 360]) {
    await page.setViewportSize({ width, height: width > 760 ? 960 : 844 });
    await shot(`accounts-${width}`);
    if (width <= 760) {
      assert(await page.locator('.account-table [data-account-toggle]').first().evaluate(button => button.getBoundingClientRect().right <= innerWidth), 'mobile account actions must not require horizontal scrolling');
    }
    await page.locator('[data-account-oauth]').click();
    await shot(`oauth-${width}`);
    await closeDialog();
  }
  checks.push('1440/390/360px 列表与 OAuth 弹层截图，无整页横向溢出');
  await page.setViewportSize({ width: 1440, height: 960 });

  let pollCount = 0;
  let submitted = false;
  let oauthOutcome = 'wait';
  const state = 'synthetic-browser-oauth-state';
  await page.route('**/carpool/api/v1/admin/codex-auth-url', async route => {
    assert.equal(route.request().method(), 'POST');
    assert(route.request().headers()['x-carpool-csrf']);
    await route.fulfill({ contentType: 'application/json', body: JSON.stringify({ status: 'ok', state, url: `https://auth.openai.com/oauth/authorize?state=${state}` }) });
  });
  await page.route('**/carpool/api/v1/admin/get-auth-status?*', async route => {
    pollCount++;
    await route.fulfill({ contentType: 'application/json', body: JSON.stringify({ status: oauthOutcome }) });
  });
  await page.route('**/carpool/api/v1/admin/oauth-callback', async route => {
    const body = route.request().postDataJSON();
    assert.equal(body.provider, 'codex');
    assert(new URL(body.redirect_url).searchParams.get('state') === state);
    submitted = true;
    await route.fulfill({ contentType: 'application/json', body: '{"status":"ok"}' });
  });
  await page.locator('[data-account-oauth]').click();
  await page.locator('[data-oauth-start]').click();
  await page.locator('[data-oauth-link] a').waitFor();
  assert.match(await page.locator('[data-oauth-link] a').getAttribute('href'), /^https:\/\/auth\.openai\.com\//);
  await page.locator('#oauth-callback-url').fill(`http://localhost:1455/auth/callback?state=wrong&code=synthetic`);
  await page.locator('[data-oauth-callback] button').click();
  await page.locator('[data-oauth-status]').filter({ hasText: '不属于本次授权' }).waitFor();
  assert.equal(submitted, false);
  await page.locator('#oauth-callback-url').fill(`http://localhost:1455/auth/callback?state=${state}&code=synthetic`);
  await page.locator('[data-oauth-callback] button').click();
  await page.locator('[data-oauth-status]').filter({ hasText: '回调已提交' }).waitFor();
  assert.equal(submitted, true);
  assert(!await page.locator('[data-oauth-status]').innerText().then(text => text.includes('授权成功')));
  oauthOutcome = 'ok';
  await page.locator('[data-oauth-status]').filter({ hasText: '授权成功' }).waitFor();
  await closeDialog();
  checks.push('模拟 OAuth 链接、错误 state 拒绝、手工回调与最终状态确认（未执行真实 OpenAI 登录）');

  oauthOutcome = 'error';
  await page.locator('[data-account-oauth]').click();
  await page.locator('[data-oauth-start]').click();
  await page.locator('[data-oauth-status]').filter({ hasText: '授权失败或已过期' }).waitFor();
  assert.equal(await page.locator('[data-oauth-start]').isEnabled(), true);
  oauthOutcome = 'wait';
  await page.locator('[data-oauth-start]').click();
  await page.locator('[data-oauth-status]').filter({ hasText: '等待 OpenAI' }).waitFor();
  await page.locator('[data-oauth-copy]').click();
  await page.locator('[data-oauth-status]').filter({ hasText: /链接已复制|无法访问剪贴板/ }).waitFor();
  await closeDialog();
  checks.push('模拟 OAuth 失败后的重新授权与复制链接/剪贴板降级');

  oauthOutcome = 'wait';
  await page.clock.install();
  await page.locator('[data-account-oauth]').click();
  await page.locator('[data-oauth-start]').click();
  await page.locator('[data-oauth-status]').filter({ hasText: '等待 OpenAI' }).waitFor();
  await closeDialog();
  const closedCount = pollCount;
  await page.clock.fastForward(10000);
  assert.equal(pollCount, closedCount, 'closed OAuth dialog must stop polling');
  await page.locator('[data-account-oauth]').click();
  await page.locator('[data-oauth-start]').click();
  await page.locator('[data-oauth-status]').filter({ hasText: '等待 OpenAI' }).waitFor();
  await page.evaluate(() => { location.hash = '#/users'; });
  await page.locator('#content[data-page="/users"]').waitFor();
  assert.equal(await page.locator('dialog[open]').count(), 0);
  const navigatedCount = pollCount;
  await page.clock.fastForward(10000);
  assert.equal(pollCount, navigatedCount, 'navigation must stop polling');
  checks.push('受控浏览器时钟验证关闭和离页停止 OAuth 轮询');

  await page.locator('#logout').click();
  await login(fixture.passengers[0]);
  assert.equal(await page.locator('[data-route="/admin-accounts"]').count(), 0);
  const denied = await accountList();
  assert.equal(denied.status, 403);
  checks.push('乘客无入口且后端拒绝访问');
  assert.deepEqual(errors, []);
  await writeFile(`${output}/results.json`, JSON.stringify({ checks, errors, synthetic: true }, null, 2));
  console.log(JSON.stringify({ checks, errors, output }, null, 2));
} catch (error) {
  await page.screenshot({ path: `${output}/failure.png`, fullPage: true }).catch(() => {});
  await writeFile(`${output}/failure.txt`, String(error));
  throw error;
} finally {
  await browser.close();
}
