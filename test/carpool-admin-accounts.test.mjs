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

test("sub2api v1 validates every OpenAI OAuth account before upload", () => {
  const c = harness();
  const account = {platform: "openai", type: "oauth", credentials: {access_token: "synthetic"}, extra: {recovery: {login_password: "not-for-import", totp_secret: "not-for-import"}}};
  const envelope = {type: "sub2api-data", version: 1, accounts: [account]};
  assert.equal(c.accountImportError(envelope), "");
  assert.equal(c.accountImportError({...envelope, accounts: [account, account]}), "");
  for (const invalid of [
    {...envelope, version: 2}, {...envelope, version: "1"}, {...envelope, accounts: []},
    {...envelope, accounts: {}}, {...envelope, accounts: Array(101).fill(account)},
    {...envelope, accounts: [null]}, {...envelope, accounts: [account, {...account, platform: "claude"}]},
    {...envelope, accounts: [{...account, type: "api_key"}]},
    {...envelope, accounts: [{...account, credentials: {refresh_token: "synthetic"}}]},
  ]) assert(c.accountImportError(invalid));
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
