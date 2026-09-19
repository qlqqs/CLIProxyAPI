import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { test } from "node:test";
import vm from "node:vm";

const source = readFileSync(new URL("../internal/carpool/web/assets/app.js", import.meta.url), "utf8");
function harness(fetch = () => new Promise(() => {})) {
  const context = vm.createContext({
    location: { hash: "" }, URL, URLSearchParams, Headers, FormData, Event,
    window: { addEventListener() {} }, fetch,
  });
  vm.runInContext(source.replace(/boot\(\);\s*$/, ""), context);
  return context;
}

test("admin navigation places account management between cars and users without changing passenger accounts", () => {
  const c = harness();
  vm.runInContext('state.session = {role: "carpool_admin"}', c);
  assert.deepEqual(Array.from(c.navigation(), item => item[0]).slice(0, 4), ["/", "/cars", "/admin-accounts", "/users"]);
  vm.runInContext('state.session = {role: "passenger"}', c);
  assert.ok(c.navigation().some(([route]) => route === "/accounts"));
  assert.ok(!c.navigation().some(([route]) => route === "/admin-accounts"));
});

test("multipart uploads retain browser boundary, same-origin cookies and CSRF", async () => {
  const calls = [];
  const c = harness(async (url, options) => { calls.push({url, options}); return {ok: true, status: 200, json: async () => ({status: "ok"})}; });
  vm.runInContext('state.csrf = "test-csrf"', c);
  await c.request("/admin/auth-files", {method: "POST", body: new FormData()});
  assert.equal(calls[0].url, "/carpool/api/v1/admin/auth-files");
  assert.equal(calls[0].options.credentials, "same-origin");
  assert.equal(calls[0].options.headers.get("X-Carpool-CSRF"), "test-csrf");
  assert.equal(calls[0].options.headers.has("Content-Type"), false);
  await c.request("/admin/auth-files/status", {method: "PATCH", body: "{}"});
  assert.equal(calls[1].options.headers.get("Content-Type"), "application/json");
});

test("original string errors and structured carpool errors are preserved", async () => {
  for (const error of ["invalid JSON", {message: "invalid JSON", code: "bad_file"}]) {
    const c = harness(async () => ({ok: false, status: 400, json: async () => ({error})}));
    await assert.rejects(c.request("/admin/auth-files"), err => err.message === "invalid JSON" && err.status === 400);
  }
});

test("account proxy accepts HTTP and SOCKS URLs and rejects unsupported schemes", () => {
  const c = harness();
  for (const value of ["http://127.0.0.1:7890", "https://proxy.example:8443", "socks5://user:pass@proxy.example:1080", "socks5h://proxy.example:1080"]) assert.equal(c.accountProxyURL(value), value);
  assert.equal(c.accountProxyURL("  "), "");
  for (const value of ["ftp://proxy.example", "proxy.example:8080", "http://", "javascript:alert(1)"]) assert.throws(() => c.accountProxyURL(value));
});

test("import and OAuth forms submit the selected proxy", () => {
  const importSection = source.slice(source.indexOf("function openAccountImport"), source.indexOf("function openAccountOAuth"));
  assert.match(importSection, /body\.append\("proxy_url", proxyURL\)/);
  const oauthSection = source.slice(source.indexOf("function openAccountOAuth"), source.indexOf("function renderPassword"));
  assert.match(oauthSection, /JSON\.stringify\(\{ proxy_url: proxyURL \}\)/);
  assert.match(oauthSection, /浏览器自身网络/);
});

test("OAuth authorization links reject unsafe schemes, foreign hosts and credentials", () => {
  const c = harness();
  const valid = "https://auth.openai.com/oauth/authorize?state=example";
  assert.equal(c.safeOAuthURL(valid), valid);
  for (const value of ["javascript:alert(1)", "http://auth.openai.com/", "https://auth.openai.com.evil.test/", "https://evil.test/", "https://a:b@auth.openai.com/", "https://auth.openai.com:999/", "//auth.openai.com/"]) assert.equal(c.safeOAuthURL(value), "");
});

test("callback requires HTTP URL, matching state, and code or upstream error", () => {
  const c = harness();
  assert.equal(c.validCallbackURL("http://localhost:1455/auth/callback?state=s&code=c", "s"), true);
  for (const value of ["javascript:alert(1)", "https://localhost/?state=other&code=c", "https://localhost/?state=s", "https://u:p@localhost/?state=s&code=c"]) assert.equal(c.validCallbackURL(value, "s"), false);
});

test("account status does not imply healthy for missing or unsupported status", () => {
  const c = harness();
  assert.equal(c.accountStatus({disabled: true, status: "active"}), "disabled");
  assert.equal(c.accountStatus({unavailable: true, status: "active"}), "unavailable");
  assert.equal(c.accountStatus({status: "active"}), "active");
  assert.equal(c.accountStatus({}), "unknown");
  assert.equal(c.accountStatus({status: "mystery"}), "unknown");
  assert.equal(c.accountProvider({type: "Codex"}), "codex");
});

test("cleanup immediately notifies and closes every account dialog", () => {
  const c = harness();
  vm.runInContext(`globalThis.events = []; adminAccountDialogs.add({dispatchEvent(event) { events.push(event.type); }, close() { events.push("close"); }});`, c);
  c.closeAdminAccountDialogs();
  assert.deepEqual(Array.from(c.events), ["account-cleanup", "close"]);
  assert.equal(vm.runInContext("adminAccountDialogs.size", c), 0);
  c.closeAdminAccountDialogs();
  assert.equal(c.events.length, 2);
});

test("account metadata and imported filenames are escaped or rendered through textContent", () => {
  const c = harness();
  assert.equal(c.escapeHTML('<img src=x onerror="bad()">'), "&lt;img src=x onerror=&quot;bad()&quot;&gt;");
  const section = source.slice(source.indexOf("async function renderAdminAccounts"), source.indexOf("function openAccountImport"));
  assert.doesNotMatch(section, /JSON\.stringify\(item\)|item\.(access_token|refresh_token|id_token|path|metadata)/);
});

test("import uses one original file field per request and prevents accidental replacement", () => {
  const section = source.slice(source.indexOf("function openAccountImport"), source.indexOf("function openAccountOAuth"));
  assert.match(section, /for \(const file of selected\)/);
  assert.match(section, /body\.append\("file", file, file\.name\)/);
  assert.match(section, /existingNames\.includes\(file\.name\)/);
  assert.match(section, /同名账号已存在/);
  assert.doesNotMatch(section, /同名文件会覆盖|确认替换/);
  assert.match(section, /controller\.signal\.aborted/);
});

test("OAuth only declares success after polling and explains mandatory manual callback", () => {
  const section = source.slice(source.indexOf("function openAccountOAuth"), source.indexOf("function renderPassword"));
  assert.match(section, /必须复制地址栏中的完整 localhost 回调 URL/);
  assert.match(section, /result\?\.status === "ok"/);
  assert.match(section, /window\.setTimeout\(poll, 2000\)/);
  const submission = section.slice(section.indexOf('callback.addEventListener("submit"'));
  assert.doesNotMatch(submission, /onSaved\(/);
  assert.match(submission, /回调已提交，等待确认/);
});


test("account rows retain filename identity without duplicating the display label", () => {
  const c = harness();
  assert.equal(c.accountSecondaryText({name: "imported.json", label: "owner@example.invalid", email: "owner@example.invalid"}), "imported.json");
  assert.equal(c.accountSecondaryText({name: "runtime-key"}), "");
  assert.equal(c.accountSecondaryText({name: "account.json", label: "Work", email: "owner@example.invalid"}), "account.json · owner@example.invalid");
});


test("import accepts executable Codex OAuth files and rejects unsupported key-only shapes", () => {
  const c = harness();
  assert.equal(c.accountImportError({type: "codex", access_token: "synthetic"}), "");
  for (const file of [null, [], {type: "openai", api_key: "synthetic"}, {type: "claude", access_token: "synthetic"}, {type: "codex", api_key: "synthetic"}, {type: "codex", refresh_token: "synthetic"}, {type: "codex", access_token: " "}]) {
    assert(c.accountImportError(file));
  }
});

test("sub2api envelope recognition supports v1 and fully headerless exports only", () => {
  const c = harness();
  const account = {platform: "openai", type: "oauth", credentials: {access_token: "synthetic"}, extra: {recovery: {login_password: "not-for-import", totp_secret: "not-for-import"}}};
  const versioned = {type: "sub2api-data", version: 1, exported_at: "synthetic", proxies: [], accounts: [account]};
  const headerless = {exported_at: "synthetic", proxies: [], accounts: [account]};
  assert.equal(c.isSub2APIEnvelope(versioned), true);
  assert.equal(c.isSub2APIEnvelope(headerless), true);
  assert.equal(c.accountImportError(versioned), "");
  assert.equal(c.accountImportError(headerless), "");
  assert.equal(c.accountImportError({...headerless, accounts: [account, account]}), "");
  for (const invalidHeader of [
    {type: "sub2api-data", accounts: [account]},
    {version: 1, accounts: [account]},
    {...versioned, type: "other"},
    {...versioned, type: "codex", access_token: "native-looking"},
    {...versioned, version: 2},
    {...versioned, version: "1"},
  ]) {
    assert.equal(c.isSub2APIEnvelope(invalidHeader), false);
    assert(c.accountImportError(invalidHeader));
  }
  for (const invalid of [
    {...versioned, accounts: []}, {...versioned, accounts: {}}, {...versioned, accounts: Array(101).fill(account)},
    {...versioned, accounts: [null]}, {...versioned, accounts: [account, {...account, platform: "claude"}]},
    {...versioned, accounts: [{...account, type: "api_key"}]},
    {...versioned, accounts: [{...account, credentials: {refresh_token: "synthetic"}}]},
  ]) assert(c.accountImportError(invalid));
});

test("sub2api validation and duplicate-name handling share envelope recognition", () => {
  const section = source.slice(source.indexOf("function openAccountImport"), source.indexOf("function openAccountOAuth"));
  assert.match(section, /const validationError = accountImportError\(parsed\)/);
  assert.match(section, /isSub2APIEnvelope\(parsed\) && parsed\.accounts\.length > 1/);
  assert.doesNotMatch(section, /parsed\.type === "sub2api-data"/);
});

test("batch results preserve individual success and failure instead of claiming full success", () => {
  const c = harness();
  const native = c.accountImportOutcome({status: "ok"}, "original.json");
  assert.deepEqual(Array.from(native.names), ["original.json"]);
  const partial = c.accountImportOutcome({status: "partial", uploaded: 1, files: ["export-002.json"], failed: [{name: "export-001.json", error: "同名文件已存在"}]}, "export.json");
  assert.deepEqual(Array.from(partial.names), ["export-002.json"]);
  assert.equal(partial.failures.length, 1);
  assert.equal(partial.failures[0].name, "export-001.json");
  assert.throws(() => c.accountImportOutcome({status: "partial"}, "export.json"));
  assert.throws(() => c.accountImportOutcome({status: "error"}, "export.json"));
});

test("account rows expose a disabled-aware connection test action", () => {
  const section = source.slice(source.indexOf("async function renderAdminAccounts"), source.indexOf("function accountTestFailureMessage"));
  assert.match(section, /data-account-test=/);
  assert.match(section, /accountStatus\(item\) === "disabled"[^\n]+disabled/);
  assert.match(section, /openAccountTest\(item, refresh\)/);
});

test("account connection test aborts on dialog cleanup and prevents duplicate submission", () => {
  const section = source.slice(source.indexOf("function openAccountTest"), source.indexOf("function openAccountImport"));
  assert.match(section, /new AbortController\(\)/);
  assert.match(section, /account-cleanup[^\n]+controller\.abort\(\)/);
  assert.match(section, /if \(running\) return/);
  assert.match(section, /setButtonBusy\(submit, true\)/);
  assert.match(section, /signal: controller\.signal/);
  assert.match(section, /submit\.textContent = "重试"/);
});

test("account connection output uses safe classifications without dangerous fields", () => {
  const c = harness();
  assert.equal(c.accountTestFailureMessage({status: 401, message: "SECRET_TOKEN /private/path raw body"}), "账号授权已失效，请重新授权后重试。");
  assert.equal(c.accountTestFailureMessage({status: 429}), "账号额度或速率受限，请稍后重试。");
  assert.equal(c.accountTestFailureMessage({status: 503}), "上游暂不可用，请稍后重试。");
  const section = source.slice(source.indexOf("function openAccountTest"), source.indexOf("function openAccountImport"));
  assert.doesNotMatch(section, /error\.message|access_token|refresh_token|id_token|\.path|raw_body|response\.body/);
  assert.match(section, /status\.textContent/);
  assert.match(section, /response_bytes/);
  assert.match(section, /if \(onUpdated\) onUpdated\(\)/);
});


test("account quota renders passive observations and a zero meter before data exists", () => {
  const c = harness();
  const missing = c.quotaMarkup(null);
  assert.match(missing, /暂无配额数据/);
  assert.match(missing, /value="0"/);
  assert.match(missing, />0%<\/b>/);
  assert.match(missing, /账号产生请求且上游返回配额后更新/);

  const observed = c.quotaMarkup({supported: true, stale: false, windows: [{id: "primary", label: "主窗口", used_percent: 37, status: "observed"}]});
  assert.match(observed, /value="37"/);
  assert.match(observed, />37%<\/b>/);
  assert.doesNotMatch(observed, /暂无配额数据/);

  const section = source.slice(source.indexOf("async function renderAdminAccounts"), source.indexOf("function accountTestFailureMessage"));
  assert.match(section, /账号配额/);
  assert.match(section, /quotaMarkup\(item\.quota\)/);
});
