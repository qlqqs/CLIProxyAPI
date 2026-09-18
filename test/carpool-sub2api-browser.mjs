import assert from 'node:assert/strict';
import { mkdir, readFile, writeFile } from 'node:fs/promises';

const { chromium } = await import(process.env.PLAYWRIGHT_MODULE || 'playwright');
const fixture = JSON.parse(await readFile(process.env.CARPOOL_BROWSER_READY_FILE, 'utf8'));
assert.equal(fixture.synthetic, true);
assert.equal(new URL(fixture.url).hostname, '127.0.0.1');
const output = process.env.CARPOOL_BROWSER_OUTPUT || '/tmp/carpool-sub2api-evidence';
await mkdir(output, {recursive: true});
const browser = await chromium.launch({headless: true, executablePath: process.env.CHROMIUM_PATH, args: ['--no-sandbox']});
const context = await browser.newContext({viewport: {width: 1440, height: 960}});
await context.route('**/*', route => new URL(route.request().url()).origin === fixture.url ? route.continue() : route.abort());
const page = await context.newPage();
const errors = [];
const checks = [];
page.on('pageerror', error => errors.push(error.message));
const account = index => ({
  name: `Synthetic account ${index}`, platform: 'openai', type: 'oauth',
  credentials: {access_token: `SYNTHETIC-SUB2-${index}`, refresh_token: `SYNTHETIC-REFRESH-${index}`, chatgpt_account_id: `synthetic-account-${index}`, expires_at: 2000000000, expires_in: 3600, organization_id: 'synthetic-org', plan_type: 'plus'},
  extra: {email: `qa-sub2-${index}@example.invalid`, display_name: 'Synthetic', recovery: {email: 'recovery@example.invalid', login_password: 'NEVER-PERSIST-PASSWORD', totp_secret: 'NEVER-PERSIST-TOTP', credential_line: 'NEVER-PERSIST-RECOVERY'}},
  concurrency: 10, priority: 1, rate_multiplier: 1, auto_pause_on_expired: true, plan_type: 'plus',
});
const envelope = accounts => ({type: 'sub2api-data', version: 1, exported_at: '2026-09-18T00:00:00Z', proxies: [], accounts});
async function upload(name, data) {
  await page.locator('[data-account-import]').click();
  await page.locator('#account-files').setInputFiles({name, mimeType: 'application/json', buffer: Buffer.from(JSON.stringify(data))});
  await page.locator('[data-import-form] button[type=submit]').click();
  await page.waitForFunction(() => {
    const button = document.querySelector('[data-import-form] button[type=submit]');
    return button && !button.disabled && document.querySelector('.account-import-results')?.textContent;
  });
  return page.locator('.account-import-results').innerText();
}
async function close() {
  await page.locator('dialog[open] [data-dialog-close]').first().click();
  await page.locator('dialog[open]').waitFor({state: 'detached'});
}
try {
  await page.goto(fixture.url + '/');
  await page.locator('#username').fill(fixture.admin.username);
  await page.locator('#password').fill(fixture.admin.password);
  await page.locator('#login-form button[type=submit]').click();
  await page.locator('[data-route="/admin-accounts"]').click();
  await page.locator('[data-account-counts]').filter({hasText: '共'}).waitFor();
  const stamp = Date.now();
  const single = `sub2api-single-${stamp}.json`;
  assert.match(await upload(single, envelope([account(1)])), /导入成功/);
  await close();
  const persisted = JSON.parse(await readFile(`${fixture.auth_dir}/${single}`, 'utf8'));
  assert.equal(persisted.type, 'codex');
  assert.equal(persisted.account_id, 'synthetic-account-1');
  assert.equal(persisted.access_token, 'SYNTHETIC-SUB2-1');
  assert.equal(persisted.refresh_token, 'SYNTHETIC-REFRESH-1');
  assert.equal(persisted.email, 'qa-sub2-1@example.invalid');
  assert.equal(persisted.expired, new Date(2000000000 * 1000).toISOString().replace('.000Z', 'Z'));
  assert(!JSON.stringify(persisted).includes('NEVER-PERSIST'));
  for (const field of ['extra', 'recovery', 'concurrency', 'rate_multiplier', 'proxies']) assert.equal(Object.hasOwn(persisted, field), false);
  await page.locator('.account-table tbody tr').filter({hasText: single}).waitFor();
  checks.push('单账号 sub2api 原文件上传、Codex 字段映射、落盘及列表展示');
  checks.push('密码、TOTP、恢复信息与供应商调度字段不持久化');

  const base = `sub2api-multiple-${stamp}`;
  assert.match(await upload(`${base}-001.json`, {type: 'codex', access_token: 'SYNTHETIC-ORIGINAL'}), /导入成功/);
  await close();
  const result = await upload(`${base}.json`, envelope([account(2), account(3)]));
  assert.match(result, /已导入 1 个账号，失败 1 个/);
  assert(result.includes(`${base}-001.json`) && result.includes(`${base}-002.json`));
  await page.screenshot({path: `${output}/partial-results-desktop.png`, fullPage: true});
  await page.setViewportSize({width: 390, height: 844});
  await page.screenshot({path: `${output}/partial-results-mobile.png`, fullPage: true});
  assert(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth + 1));
  await close();
  assert.equal(JSON.parse(await readFile(`${fixture.auth_dir}/${base}-001.json`, 'utf8')).access_token, 'SYNTHETIC-ORIGINAL');
  assert.equal(JSON.parse(await readFile(`${fixture.auth_dir}/${base}-002.json`, 'utf8')).account_id, 'synthetic-account-3');
  checks.push('多账号分别导入、同名不覆盖、207 部分失败逐项展示，桌面/手机无溢出');

  const conflicts = await upload(`${base}.json`, envelope([account(2), account(3)]));
  assert(conflicts.includes(`${base}-001.json`) && conflicts.includes(`${base}-002.json`));
  assert.match(conflicts, /同名账号文件已存在/);
  assert(!conflicts.includes('导入成功'));
  await close();
  checks.push('全部重名时展示每个失败文件，不误报成功');

  const invalid = `sub2api-mixed-${stamp}.json`;
  assert.match(await upload(invalid, envelope([account(4), {...account(5), platform: 'claude'}])), /第 2 个账号不是 OpenAI OAuth/);
  await close();
  await assert.rejects(readFile(`${fixture.auth_dir}/${invalid}`, 'utf8'), error => error.code === 'ENOENT');
  const response = await page.evaluate(async () => {
    const r = await fetch('/carpool/api/v1/admin/auth-files');
    return {status: r.status, data: await r.json()};
  });
  assert.equal(response.status, 200);
  assert(!JSON.stringify(response.data).includes('SYNTHETIC-SUB2'));
  assert(!JSON.stringify(response.data).includes('NEVER-PERSIST'));
  checks.push('混入其他供应商时阻止上传，列表不返回令牌或恢复信息');
  assert.deepEqual(errors, []);
  await writeFile(`${output}/results.json`, JSON.stringify({checks, errors, synthetic: true}, null, 2));
  console.log(JSON.stringify({checks, errors, output}, null, 2));
} catch (error) {
  await page.screenshot({path: `${output}/failure.png`, fullPage: true}).catch(() => {});
  throw error;
} finally {
  await browser.close();
}
