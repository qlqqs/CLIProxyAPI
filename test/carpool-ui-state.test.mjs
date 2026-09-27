import assert from "node:assert/strict";
import { test } from "node:test";
import vm from "node:vm";
import { loadCarpoolApp } from "./helpers/carpool-app-harness.mjs";

function deferred() {
  let resolve, reject;
  const promise = new Promise((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}

class Element {
  constructor(tagName = "div") {
    this.tagName = tagName;
    this.listeners = new Map();
    this.nodes = new Map();
    this.children = [];
    this.controls = [];
    this.isConnected = true;
    this.open = true;
    this.disabled = false;
    this.dataset = {};
    this.attributes = {};
    this.values = {};
    this.textContent = "";
  }
  set innerHTML(value) { this.markup = value; this.nodes.clear(); }
  get innerHTML() { return this.markup || ""; }
  querySelector(selector) {
    if (!this.nodes.has(selector)) this.nodes.set(selector, new Element());
    return this.nodes.get(selector);
  }
  querySelectorAll(selector) {
    if (selector === "input, textarea") return this.controls;
    if (selector === ".secret-box") return this.nodes.has(selector) ? [this.nodes.get(selector)] : [];
    return [];
  }
  addEventListener(type, fn) { this.listeners.set(type, fn); }
  dispatchEvent(event) { this.listeners.get(event.type)?.(event); }
  setAttribute(name, value) { this.attributes[name] = value; }
  removeAttribute(name) { delete this.attributes[name]; }
  contains() { return false; }
  focus() {}
  close() { this.open = false; }
  showModal() { this.open = true; }
  show() { this.open = true; }
  append(node) { this.children.push(node); node.parent = this; }
  replaceChildren() { this.children = []; this.nodes.clear(); this.controls = []; }
  remove() {
    this.isConnected = false;
    if (this.parent) this.parent.children = this.parent.children.filter(node => node !== this);
  }
  fire(type = "click") {
    const event = { currentTarget: this, preventDefault() {} };
    const pending = this.listeners.get(type)?.(event);
    // Browsers clear currentTarget as soon as the synchronous dispatch ends.
    event.currentTarget = null;
    return pending;
  }
}

function harness(extra = {}) {
  const body = new Element("body");
  const password = new Element("input");
  password.value = password.defaultValue = "synthetic-password";
  const visibleKey = new Element("code");
  visibleKey.textContent = "synthetic-visible-key";
  const pageNodes = new Map();
  const document = {
    body,
    documentElement: { dataset: {} },
    activeElement: null,
    createElement: tag => new Element(tag),
    querySelector(selector) {
      if (selector.startsWith("meta")) return null;
      if (!pageNodes.has(selector)) pageNodes.set(selector, new Element());
      return pageNodes.get(selector);
    },
    querySelectorAll(selector) {
      if (selector === "dialog") return body.children.filter(node => node.tagName === "dialog");
      if (selector === "dialog.entity-panel") return body.children.filter(node => node.className === "entity-panel");
      if (selector === 'input[type="password"]') return [password];
      if (selector === ".api-key-value code") return [visibleKey];
      return [];
    },
  };
  const app = loadCarpoolApp(vm.createContext({
    document, Headers, URL, URLSearchParams, FormData, Event,
    location: { hash: "" },
    window: { addEventListener() {}, clearTimeout() {} },
    fetch() { throw new Error("unexpected network access"); },
    ...extra,
  }));
  const state = vm.runInContext("state", app);
  const toasts = [];
  app.toast = value => toasts.push(value);
  return { app, state, document, password, visibleKey, toasts };
}

const user = { user_ref: "usr_synthetic", username: "synthetic-user", display_name: "Before", role: "passenger", status: "active" };

test("saved user fields update the list record and survive closing/reopening", async () => {
  const { app } = harness({ FormData: class { constructor(form) { this.get = name => form.values[name]; } } });
  const record = { ...user };
  const dialogs = [];
  const patches = [];
  let updates = 0;
  app.openEntityPanel = () => { const node = new Element("dialog"); dialogs.push(node); return node; };
  app.request = async (path, options) => {
    if (!options) return { items: [], total: 0 };
    const patch = JSON.parse(options.body);
    patches.push(patch);
    return { ...record, display_name: patch.default_display_name, status: patch.status };
  };
  await app.showUserManagement(record, () => updates++);
  const first = dialogs[0];
  first.querySelector("#edit-user-form").values = { default_display_name: "After", status: "disabled" };
  await first.querySelector("#edit-user-form").fire("submit");
  assert.equal(record.display_name, "After");
  assert.equal(record.status, "disabled");
  assert.equal(updates, 1);
  first.close();
  await app.showUserManagement(record);
  assert.match(dialogs[1].innerHTML, /value="After"/);
  assert.match(dialogs[1].innerHTML, /value="disabled" selected/);
  dialogs[1].querySelector("#edit-user-form").values = { default_display_name: "After", status: "active" };
  await dialogs[1].querySelector("#edit-user-form").fire("submit");
  assert.deepEqual(patches[1], { default_display_name: "After", status: "active" });
});

test("session cleanup synchronously scrubs all dialogs, passwords and revealed key cache", () => {
  const { app, state, document, password, visibleKey } = harness();
  state.session = { role: "carpool_admin" };
  state.csrf = "synthetic-csrf";
  const key = { api_key: "synthetic-cached-key" };
  state.pages.keys.items = [key];
  const dialog = app.openDialog("Create user", "");
  app.showOneTimeSecret(dialog, "Created", "synthetic-secret", "Only once");
  const secret = dialog.querySelector(".secret-box");
  const input = new Element("input");
  input.value = input.defaultValue = "synthetic-credential";
  input.attributes.value = input.value;
  dialog.controls.push(input);
  const account = app.accountDialog("OAuth", "");
  let cleanup = false;
  account.addEventListener("account-cleanup", () => { cleanup = true; });
  app.clearPrivateState();
  assert.equal(state.session, null);
  assert.equal(state.csrf, "");
  assert.equal(document.querySelectorAll("dialog").length, 0);
  assert.equal(cleanup, true);
  assert.equal(secret.textContent, "");
  assert.equal(input.value, "");
  assert.equal(input.defaultValue, "");
  assert.equal(input.attributes.value, undefined);
  assert.equal(password.value, "");
  assert.equal(password.defaultValue, "");
  assert.equal(visibleKey.textContent, "");
  assert.equal(key.api_key, "");
  assert.equal(state.pages.keys.items.length, 0);
  app.showOneTimeSecret(dialog, "Late response", "synthetic-late-secret", "Only once");
  assert.equal(document.querySelectorAll("dialog").length, 0);
  assert.equal(secret.textContent, "");
});

test("one-time secret responses are bound to the dialog's owning session", () => {
  const { app, state } = harness();
  const dialog = app.openDialog("Create user", "original content");
  state.sessionGeneration++;
  app.showOneTimeSecret(dialog, "Late", "synthetic-late-secret", "Only once");
  assert.doesNotMatch(dialog.innerHTML, /synthetic-late-secret|secret-box/);
});

for (const code of ["session_required", "session_invalid", "authorization_expired", "invalid_credentials"]) {
  test(`HTTP 401 ${code} only ends the session for session errors`, async () => {
    const { app, state } = harness({ fetch: async () => ({ status: 401, ok: false, json: async () => ({ error: { code, message: "synthetic error" } }) }) });
    state.session = { role: "carpool_admin" };
    let ended = 0;
    app.loginView = () => { ended++; app.clearPrivateState(); };
    await assert.rejects(app.request("/synthetic"), error => error.code === code);
    assert.equal(ended, ["session_required", "session_invalid"].includes(code) ? 1 : 0);
  });
}

test("a late 401 from an old session cannot invalidate a replacement session", async () => {
  const response = deferred();
  const { app, state } = harness({ fetch: () => response.promise });
  state.session = { display_name: "old" };
  let ended = false;
  app.loginView = () => { ended = true; };
  const request = app.request("/synthetic");
  state.sessionGeneration++;
  state.session = { display_name: "replacement" };
  response.resolve({ status: 401, ok: false, json: async () => ({ error: { code: "session_invalid" } }) });
  await assert.rejects(request);
  assert.equal(ended, false);
  assert.equal(state.session.display_name, "replacement");
});

test("late boot and login responses cannot reopen a cleared session", async () => {
  for (const start of ["boot", "login"]) {
    const { app, state, document } = harness({ FormData: class { get() { return "synthetic"; } } });
    const response = deferred();
    app.request = () => response.promise;
    let rendered = false;
    app.renderShell = () => { rendered = true; };
    let pending;
    if (start === "boot") pending = app.boot();
    else {
      app.loginView();
      pending = document.querySelector("#login-form").fire("submit");
    }
    app.clearPrivateState();
    response.resolve({ ...user, csrf_token: "synthetic-csrf" });
    await pending;
    assert.equal(state.session, null);
    assert.equal(rendered, false);
  }
});

for (const [name, render] of [["users", "renderUsers"], ["cars", "renderCars"], ["keys", "renderKeys"], ["audit", "renderAudit"], ["usageRequests", "renderAdminRequests"]]) {
  test(`${name}: a slow obsolete load cannot repaint the latest list or replace its cache`, async () => {
    const { app, state } = harness();
    const old = deferred(), latest = deferred();
    const calls = [];
    app.request = path => { calls.push(path); return calls.length === 1 ? old.promise : latest.promise; };
    const content = new Element();
    state.searches[name] = "old";
    const first = app[render](content, true);
    state.searches[name] = "latest";
    const second = app[render](content, true);
    const latestItem = { ...user, display_name: "Latest result", name: "Latest result", request_id: "latest" };
    latest.resolve({ items: [latestItem], total: 29, next_cursor: "latest-cursor" });
    await second;
    const expectedMarkup = content.innerHTML;
    old.resolve({ items: [{ ...user, display_name: "Obsolete result", name: "Obsolete result", request_id: "old" }], total: 70, next_cursor: "old-cursor" });
    await first;
    assert.equal(content.innerHTML, expectedMarkup);
    assert.equal(state.pages[name].items[0], latestItem);
    assert.equal(state.pages[name].total, 29);
    assert.equal(state.pages[name].nextCursor, "latest-cursor");
  });
}

test("a superseded page failure is ignored and the new cursor is used for pagination", async () => {
  const { app, state } = harness();
  const old = deferred();
  const calls = [];
  app.request = path => {
    calls.push(path);
    if (calls.length === 1) return old.promise;
    return Promise.resolve({ items: [calls.length], total: 2, next_cursor: calls.length === 2 ? "new-cursor" : "" });
  };
  const first = app.loadPage("users", "/users?q=old", true);
  await app.loadPage("users", "/users?q=new", true);
  old.reject(new Error("obsolete failure"));
  assert.equal(await first, null);
  await app.loadPage("users", "/users?q=new", false);
  assert.match(calls[2], /cursor=new-cursor/);
  assert.deepEqual(Array.from(state.pages.users.items), [2, 3]);
});

test("pending key list responses cannot repopulate caches after session cleanup", async () => {
  const { app, state } = harness();
  const response = deferred();
  app.request = () => response.promise;
  const pending = app.loadPage("keys", "/keys", true);
  app.clearPrivateState();
  response.resolve({ items: [{ api_key: "synthetic-late-key" }], total: 1 });
  assert.equal(await pending, null);
  assert.equal(state.pages.keys.items.length, 0);
});

for (const outcome of ["success", "failure"]) {
  test(`load-more ${outcome} releases the captured button after async currentTarget clears`, async () => {
    const { app, toasts } = harness();
    const content = new Element();
    const button = content.querySelector("[data-load-more]");
    const response = deferred();
    let calls = 0;
    app.bindLoadMore(content, () => { calls++; return response.promise; });
    const pending = button.fire();
    assert.equal(button.disabled, true);
    await button.fire();
    assert.equal(calls, 1);
    if (outcome === "success") response.resolve();
    else response.reject(new Error("synthetic retryable error"));
    await pending;
    assert.equal(button.disabled, false);
    assert.equal(button.attributes["aria-busy"], "false");
    assert.equal(toasts.length, outcome === "failure" ? 1 : 0);
  });
}

test("user-management key pagination can retry after a transient failure", async () => {
  const { app } = harness();
  const dialog = new Element("dialog");
  app.openEntityPanel = () => dialog;
  let count = 0;
  app.request = async () => {
    count++;
    if (count === 2) throw new Error("synthetic temporary failure");
    return { items: [{ name: "Synthetic key", key_ref: "key-ref" }], total: 2, next_cursor: count === 1 ? "next" : "" };
  };
  await app.showUserManagement({ ...user });
  const button = dialog.querySelector("[data-load-more]");
  await button.fire();
  assert.equal(button.disabled, false);
  await button.fire();
  assert.equal(count, 3);
});

test("native quota durations override default and model-specific aliases", () => {
  const { app } = harness();
  const weekly = { id: "primary", window_minutes: 10080, used_percent: 67 };
  assert.equal(app.quotaWindowByKind([weekly], "5h"), null);
  assert.equal(app.quotaWindowByKind([weekly], "7d"), weekly);
  const short = { id: "secondary", window_minutes: 300, used_percent: 12 };
  assert.equal(app.quotaWindowByKind([weekly, short], "5h"), short);
  assert.equal(app.quotaWindowByKind([weekly, short], "7d"), weekly);
  assert.equal(app.quotaWindowByKind([{ ...weekly, id: "model-primary" }], "5h"), null);
  const fallback = { id: "primary", used_percent: 5 };
  assert.equal(app.quotaWindowByKind([fallback], "5h"), fallback);
  const markup = app.quotaMarkup({ supported: true, windows: [weekly] });
  assert.equal((markup.match(/usage-window-percent[^>]*>67%/g) || []).length, 1);
  assert.match(markup, /5h 已用 -，重置 未提供/);
});

test("native rejected, status-only and stale observations remain explicit in compact rows", () => {
  const { app } = harness();
  const markup = app.quotaMarkup({ supported: true, stale: true, observed_at: "2026-09-01T00:00:00Z", windows: [
    { id: "5h", used_percent: 53, status: "rejected" },
    { id: "7d", used_percent: null, status: "exceeded" },
  ] });
  assert.match(markup, /usage-window-progress is-danger[^>]*value="53"/);
  assert.match(markup, /usage-window-status is-danger">已拒绝/);
  assert.match(markup, /usage-window-status is-danger">已达上限/);
  assert.match(markup, /配额数据陈旧 · 观测：/);
  assert.doesNotMatch(markup, /is-healthy/);
  const stale = app.quotaMarkup({ supported: true, stale: true, windows: [{ id: "primary", used_percent: 20, status: "observed" }] });
  assert.match(stale, /usage-window-progress is-unknown[^>]*value="20"/);
  assert.doesNotMatch(stale, /is-healthy/);
  const hostile = app.quotaMarkup({ supported: true, windows: [{ id: "primary", status: '<img src=x onerror="bad">' }] });
  assert.doesNotMatch(hostile, /<img/);
  assert.match(hostile, /&lt;img/);
});

test("the idle OAuth zero-percent now contract and shared row design are preserved", () => {
  const { app } = harness();
  const markup = app.quotaMarkup({ supported: true, stale: false, windows: [{ id: "primary", used_percent: 0, status: "allowed", reset_at: "2099-01-01T00:00:00Z" }] });
  assert.match(markup, /5h 已用 0%，重置 现在/);
  assert.equal((markup.match(/data-ui="usage-window"/g) || []).length, 2);
  assert.doesNotMatch(markup, /style=/);
});
