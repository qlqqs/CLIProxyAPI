import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { test } from "node:test";
import vm from "node:vm";

const source = readFileSync(new URL("../internal/carpool/web/assets/app.js", import.meta.url), "utf8");

// Minimal DOM seam for application event handlers; no network, timers or browser dependencies.
class Element {
  constructor() {
    this.children = [];
    this.dataset = {};
    this.attributes = {};
    this.listeners = {};
    this.selectors = new Map();
    this.hidden = false;
    this.disabled = false;
    this.textContent = "";
  }
  set innerHTML(value) { this.markup = value; this.selectors.clear(); }
  get innerHTML() { return this.markup || ""; }
  append(child) { this.children.push(child); child.parent = this; }
  remove() { this.removed = true; if (this.parent) this.parent.children = this.parent.children.filter(node => node !== this); }
  setAttribute(name, value) { this.attributes[name] = value; }
  closest() { return this.parent; }
  focus() { this.focused = true; }
  querySelector(selector) {
    if (this.selectors.has(selector)) return this.selectors.get(selector);
    const attribute = selector.match(/^\[([^\]]+)\]$/)?.[1];
    const child = this.children.find(node => attribute && Object.hasOwn(node.attributes, attribute));
    if (child) return child;
    if ((attribute && this.innerHTML.includes(attribute)) || (selector.startsWith("#") && this.innerHTML.includes(`id="${selector.slice(1)}"`))) {
      const node = new Element();
      this.selectors.set(selector, node);
      return node;
    }
    return null;
  }
  querySelectorAll() { return this.rows || []; }
  addEventListener(type, listener) { this.listeners[type] = listener; }
  async dispatch(type = "click") { await this.listeners[type]?.({ currentTarget: this, preventDefault() {} }); }
}

function harness(respond = async () => null, hash = "") {
  const calls = [];
  const toasts = [];
  const context = vm.createContext({
    location: { hash }, Headers, URLSearchParams,
    window: { addEventListener() {} },
    document: { createElement: () => new Element() },
    fetch: () => new Promise(() => {}),
  });
  vm.runInContext(source, context);
  context.request = async (path, options = {}) => {
    calls.push({ path, options });
    return respond(path, options, calls.length);
  };
  context.toast = message => toasts.push(message);
  return { context, calls, toasts };
}

function preview(operation = "usage_details") {
  return { job_id: "job_stable", operation, expected_count: 1000, in_flight_count: 3, confirmation_required: true, batch_limit: 1000, status: "queued" };
}
function completed(operation = "usage_details", deleted = 7) {
  return { job_id: "job_stable", operation, status: "completed", deleted_count: deleted, in_flight_count: 2, failure_reason: "SECRET_CANARY" };
}
function confirmation(h, value = preview()) {
  const panel = new Element();
  const button = new Element();
  panel.append(button);
  h.context.showRetentionConfirmation(button, value.operation, value);
  const box = panel.children.find(node => node.className === "retention-confirm");
  return { panel, button, box, confirm: box?.querySelector("[data-retention-confirm]"), cancel: box?.querySelector("[data-retention-cancel]"), result: panel.querySelector("[data-retention-result]") };
}
function failure(status) { return Object.assign(new Error("SECRET_CANARY"), { status }); }

for (const [operation, action, unit] of [["usage_details", "删除", "逻辑请求"], ["closed_periods", "删除", "账期"], ["reset_current_period", "重置", "账期"]]) {
  test(`${operation}: confirmation binds preview and shows actual completed count`, async () => {
    const h = harness(async () => completed(operation));
    const ui = confirmation(h, preview(operation));
    assert.match(ui.box.innerHTML, /最多 1,000/);
    await ui.confirm.dispatch();
    assert.equal(h.calls.length, 1);
    assert.equal(h.calls[0].path, "/admin/retention/jobs");
    assert.equal(h.calls[0].options.method, "POST");
    assert.deepEqual(JSON.parse(h.calls[0].options.body), { job_id: "job_stable", operation, confirm: true });
    assert.match(ui.result.textContent, new RegExp(`实际${action} 7 个${unit}`));
    assert.match(ui.result.textContent, /在途或不完整请求 2 项/);
    assert.match(ui.result.textContent, /更多数据需重新预览/);
    assert.doesNotMatch(ui.result.textContent, /SECRET_CANARY/);
    assert.equal(ui.button.hidden, false);
    assert.equal(ui.box.removed, true);
    await ui.confirm.dispatch();
    assert.equal(h.calls.length, 1);
  });
}

test("a completed zero batch is not replaced with the preview estimate", async () => {
  const h = harness(async () => completed("usage_details", 0));
  const ui = confirmation(h);
  await ui.confirm.dispatch();
  assert.match(ui.result.textContent, /实际删除 0 个逻辑请求/);
});

test("stale 409 clears obsolete confirmation and restores actionable preview", async () => {
  const h = harness(async () => { throw failure(409); });
  const ui = confirmation(h);
  await ui.confirm.dispatch();
  assert.equal(h.calls.length, 1);
  assert.equal(ui.box.removed, true);
  assert.equal(ui.button.hidden, false);
  assert.equal(ui.button.disabled, false);
  assert.match(ui.result.textContent, /预览已失效.*重新预览/);
  assert.doesNotMatch(ui.result.textContent, /SECRET_CANARY|已完成/);
  await ui.confirm.dispatch();
  assert.equal(h.calls.length, 1);
});

test("ambiguous network failure queries the same job once without resubmitting", async () => {
  const h = harness(async (_path, _options, n) => { if (n === 1) throw failure(); return completed(); });
  const ui = confirmation(h);
  await ui.confirm.dispatch();
  assert.equal(h.calls.length, 2);
  assert.equal(h.calls[1].path, "/admin/retention/jobs/job_stable");
  assert.equal(h.calls[1].options.method, undefined);
  assert.match(ui.result.textContent, /已完成.*实际删除 7/);
});

test("failed result lookup retains the reference for explicit retry only", async () => {
  const h = harness(async (_path, _options, n) => { if (n < 3) throw failure(); return completed(); });
  const ui = confirmation(h);
  await ui.confirm.dispatch();
  assert.equal(h.calls.length, 2);
  assert.equal(ui.button.hidden, true);
  assert.equal(ui.confirm.disabled, false);
  assert.equal(ui.cancel.disabled, false);
  assert.equal(ui.confirm.textContent, "重试确认");
  assert.match(ui.result.textContent, /暂时无法查询操作结果.*同一预览.*不会自动执行/);
  assert.doesNotMatch(ui.result.textContent, /SECRET_CANARY/);
  await ui.confirm.dispatch();
  assert.equal(h.calls.length, 3);
  assert.equal(h.calls[2].options.body, h.calls[0].options.body);
  assert.match(ui.result.textContent, /已完成/);
});

for (const status of ["queued", "failed", "running"]) {
  test(`HTTP success with status ${status} cannot announce completion or strand retry`, async () => {
    const h = harness(async () => ({ ...completed(), status }));
    const ui = confirmation(h);
    await ui.confirm.dispatch();
    assert.doesNotMatch(ui.result.textContent, /已完成|SECRET_CANARY/);
    assert.equal(ui.confirm.disabled, false);
    assert.equal(ui.cancel.disabled, false);
    assert.equal(ui.confirm.textContent, "重试确认");
    assert.equal(h.calls.filter(call => call.options.method === "POST").length, 1);
  });
}

test("missing result count cannot fabricate zero completion", async () => {
  const h = harness(async () => ({ ...completed(), deleted_count: null }));
  const ui = confirmation(h);
  await ui.confirm.dispatch();
  assert.doesNotMatch(ui.result.textContent, /已完成|实际删除 0/);
  assert.equal(h.calls.length, 2);
});

test("cancel and invalid preview do not submit destructive requests", async () => {
  const h = harness();
  const ui = confirmation(h);
  await ui.cancel.dispatch();
  assert.equal(ui.box.removed, true);
  assert.equal(ui.button.hidden, false);
  assert.equal(h.calls.length, 0);
  const invalid = confirmation(h, { ...preview(), job_id: undefined });
  assert.equal(invalid.box, undefined);
  assert.equal(invalid.button.hidden, false);
  assert.match(h.toasts[0], /预览结果无效/);
});

test("pending confirmation blocks duplicate clicks and cancellation", async () => {
  let resolve;
  const pending = new Promise(done => { resolve = done; });
  const h = harness(() => pending);
  const ui = confirmation(h);
  const first = ui.confirm.dispatch();
  assert.equal(ui.cancel.disabled, true);
  await ui.confirm.dispatch();
  await ui.cancel.dispatch();
  assert.equal(h.calls.length, 1);
  assert.equal(ui.box.removed, undefined);
  resolve(completed());
  await first;
});

function requestRows() {
  const content = new Element();
  const row = new Element();
  row.dataset = { requestId: "request/safe", requestLoaded: "false" };
  row.innerHTML = "<div data-request-events></div>";
  content.rows = [row];
  return { content, row, region: row.querySelector("[data-request-events]") };
}
const event = { safe_label: "fixture upstream", model: "test-model", usage_known: true, input_tokens: 12, output_tokens: 0, cached_tokens: null, cache_read_tokens: 0, cache_write_tokens: null, reasoning_tokens: null, total_tokens: 12, cost_usd: "0.25", pricing_status: "priced", authorization: "SECRET_CANARY" };

test("list rows defer fetching and distinguish unavailable detail from real empty events", () => {
  const h = harness();
  const markup = h.context.requestDetailMarkup({ request_id: '\"<request>', billed_usd: "0.25", unknown_cost_events: 1, event_count: 2 });
  assert.match(markup, /data-request-id="&quot;&lt;request&gt;"/);
  assert.match(markup, /展开后加载上游事件/);
  assert.doesNotMatch(markup, /没有上游事件/);
  assert.match(markup, /\$0\.25 · 已确认小计，费用不完整/);
  assert.match(h.context.requestDetailMarkup({ events: [] }), /没有上游事件/);
});

test("event rendering preserves token dimensions, known zero and missing values without secrets", () => {
  const h = harness();
  const markup = h.context.requestDetailEventMarkup(event);
  assert.match(markup, /输入 Token：12/);
  assert.match(markup, /输出 Token：0/);
  assert.match(markup, /缓存 Token：上游未提供/);
  assert.match(markup, /缓存读取 Token：0/);
  assert.match(markup, /缓存写入 Token：上游未提供/);
  assert.match(markup, /推理 Token：上游未提供/);
  assert.doesNotMatch(markup, /SECRET_CANARY/);
  assert.doesNotMatch(h.context.requestDetailEventMarkup({ ...event, usage_known: false }), /Token：[0-9]/);
});

test("first open fetches real detail once and caches safe events for pagination rerender", async () => {
  const h = harness(async () => ({ request_id: "request/safe", events: [event] }));
  const ui = requestRows();
  const items = [{ request_id: "request/safe" }];
  h.context.bindRequestDetails(ui.content, items);
  assert.equal(h.calls.length, 0);
  ui.row.open = true;
  await ui.row.dispatch("toggle");
  assert.equal(h.calls[0].path, "/admin/usage/requests/request%2Fsafe");
  assert.match(ui.region.innerHTML, /fixture upstream.*test-model/);
  assert.doesNotMatch(ui.region.innerHTML, /SECRET_CANARY/);
  assert.equal(items[0].events.length, 1);
  await ui.row.dispatch("toggle");
  assert.equal(h.calls.length, 1);
});

test("detail loading is explicit and retries only after a manual click", async () => {
  let reject;
  const pending = new Promise((_resolve, fail) => { reject = fail; });
  const h = harness((_path, _options, n) => n === 1 ? pending : { request_id: "request/safe", events: [] });
  const ui = requestRows();
  h.context.bindRequestDetails(ui.content, []);
  ui.row.open = true;
  const first = ui.row.dispatch("toggle");
  assert.equal(ui.region.textContent, "正在加载上游事件...");
  await ui.row.dispatch("toggle");
  assert.equal(h.calls.length, 1);
  reject(failure(500));
  await first;
  assert.match(ui.region.innerHTML, /加载失败，请手动重试/);
  assert.doesNotMatch(ui.region.innerHTML, /SECRET_CANARY/);
  await ui.row.dispatch("toggle");
  assert.equal(h.calls.length, 1);
  await ui.region.querySelector("[data-request-retry]").dispatch();
  assert.equal(h.calls.length, 2);
  assert.match(ui.region.innerHTML, /没有上游事件/);
});

test("request route restores whitelisted URL filters and resets stale pagination", () => {
  const h = harness(undefined, "#/requests?model=one&user_ref=usr_1&cursor=unsafe&authorization=SECRET_CANARY");
  assert.equal(vm.runInContext("state.route", h.context), "/requests");
  h.context.restoreRequestFilters();
  assert.equal(vm.runInContext("state.requestFilters", h.context), "/admin/usage/requests?period=30d&model=one&user_ref=usr_1");
  vm.runInContext('state.pages.usageRequests.nextCursor = "next"', h.context);
  h.context.restoreRequestFilters();
  assert.equal(vm.runInContext("state.pages.usageRequests.nextCursor", h.context), "next");
  h.context.location.hash = "#/requests?model=two";
  h.context.restoreRequestFilters();
  assert.equal(vm.runInContext("state.requestFilterValues.model", h.context), "two");
  assert.equal(vm.runInContext("state.pages.usageRequests.nextCursor", h.context), "");
  h.context.location.hash = "#/requests?model=one&user_ref=usr_1";
  h.context.restoreRequestFilters();
  assert.equal(vm.runInContext("state.requestFilterValues.model", h.context), "one");
});


test("custom configured retention without a selected radio cannot silently become permanent", async () => {
  const h = harness(async () => ({ effective_days: 62, source: "config" }));
  h.context.FormData = class { get() { return null; } };
  const content = new Element();
  await h.context.renderRetention(content);
  await content.querySelector("#retention-form").dispatch("submit");
  assert.equal(h.calls.length, 1);
  assert.equal(h.calls[0].options.method, undefined);
  assert.equal(h.toasts[0], "请选择保留期限后保存。");
});

test("submitting request filters writes restorable URL values instead of only memory", async () => {
  const h = harness(async () => ({ items: [], total: 0 }), "#/requests");
  h.context.FormData = class { get(key) { return { model: "gpt-4o", outcome: "succeeded", request_id: "exact/request" }[key] || ""; } };
  const content = new Element();
  await h.context.renderAdminRequests(content);
  await content.querySelector("#request-filter").dispatch("submit");
  assert.equal(h.context.location.hash, "#/requests?period=30d&model=gpt-4o&outcome=succeeded&request_id=exact%2Frequest");
  h.context.restoreRequestFilters();
  assert.equal(vm.runInContext("state.requestFilterValues.request_id", h.context), "exact/request");
  assert.equal(vm.runInContext("state.requestFilterValues.outcome", h.context), "succeeded");
});
