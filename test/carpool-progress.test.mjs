import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { test } from "node:test";
import vm from "node:vm";

// Run the actual application functions without a DOM or network. Boot remains
// pending so it cannot render a page or affect the pure markup assertions.
const context = vm.createContext({
  location: { hash: "" },
  window: { addEventListener() {} },
  Headers,
  fetch: () => new Promise(() => {}),
});
vm.runInContext(readFileSync(new URL("../internal/carpool/web/assets/app.js", import.meta.url), "utf8"), context);

for (const value of [null, undefined, NaN, Infinity, "", "0", false]) {
  test(`missing or nonnumeric quota ${String(value)} is status-only`, () => {
    const markup = context.quotaWindowMarkup({ id: "7d", status: "rejected", used_percent: value });
    assert.doesNotMatch(markup, /progressbar|<progress|0%/);
    assert.match(markup, /quota-status-only/);
  });
}

for (const percent of [0, 53, 81, 125]) {
  test(`native quota progress preserves ${percent}% without inline CSS`, () => {
    const markup = context.quotaProgress({ id: "5h", used_percent: percent });
    const clamped = Math.min(100, percent);
    assert.match(markup, /<progress /);
    assert.ok(markup.includes(`max="100" value="${clamped}"`));
    assert.ok(markup.includes(`aria-valuenow="${clamped}"`));
    assert.ok(markup.includes(`<b>${percent}%</b>`));
    assert.doesNotMatch(markup, /style=|--quota-width/);
  });
}

const billing = { limit_usd: "1", used_usd: "1.25", remaining_usd: "0", overage_usd: "0.25", status: "overage" };
for (const percent of [null, undefined, NaN, Infinity]) {
  test(`missing billing percentage ${String(percent)} does not fabricate progress`, () => {
    const markup = context.billingMarkup({ ...billing, usage_percent: percent });
    assert.doesNotMatch(markup, /progressbar|<progress|aria-valuenow/);
    assert.match(markup, /\$1\.25/);
  });
}

test("zero quota retains exhausted state without a percentage", () => {
  const markup = context.billingMarkup({ ...billing, limit_usd: "0", usage_percent: null, status: "exhausted" });
  assert.match(markup, /额度已用尽/);
  assert.doesNotMatch(markup, /待管理员|progressbar|aria-valuenow/);
});

test("known zero billing progress remains zero", () => {
  const markup = context.billingMarkup({ ...billing, usage_percent: 0 });
  assert.match(markup, /aria-valuenow="0"/);
  assert.match(markup, /max="100" value="0"/);
});

test("over-limit billing clamps the meter, not the real amount or accessible percentage", () => {
  const markup = context.billingMarkup({ ...billing, usage_percent: 125 });
  assert.match(markup, /aria-valuenow="100"/);
  assert.match(markup, /aria-valuetext="125%"/);
  assert.match(markup, /<b>125%<\/b>/);
  assert.match(markup, /超额 \$0\.25/);
  assert.match(markup, /\$1\.25/);
  assert.doesNotMatch(markup, /style=|--quota-width/);
});

const shortWindow = { kind: "5h", limit_usd: "2", used_usd: "1.25", remaining_usd: "0.75", overage_usd: "0", status: "active", period_from: "2026-09-15T00:00:00Z", reset_at: "2026-09-15T05:00:00Z", coverage_from: "2026-09-15T01:00:00Z", unknown_cost_events: 2, data_complete: false };

test("short USD windows display real amounts, reset, coverage and incomplete cost", () => {
  const markup = context.memberQuotaMarkup({ quota_windows: [shortWindow] });
  for (const text of ["5 小时", "$2", "$1.25", "$0.75", "重置：", "周期起点：", "统计覆盖起点：", "2 条费用未知", "统计覆盖不完整"]) assert.ok(markup.includes(text), text);
  assert.doesNotMatch(markup, /<progress|style=/);
});

test("pending short window never fabricates zero amounts or dates", () => {
  const markup = context.memberQuotaMarkup({ five_hour_limit_usd: "2", quota_windows: [{ ...shortWindow, status: "pending_sync", used_usd: null, remaining_usd: null, period_from: null, reset_at: null, coverage_from: null }] });
  assert.match(markup, /周期待同步/);
  assert.match(markup, /暂不执行此项短周期限制；月额度与并发限制仍生效/);
  assert.match(markup, /重置：待同步/);
  assert.match(markup, /统计覆盖起点：未提供/);
  assert.doesNotMatch(markup, /\$0|<progress|1970/);
});

test("configured short limit without a window is pending, not unlimited", () => {
  const markup = context.memberQuotaMarkup({ five_hour_limit_usd: "0", weekly_limit_usd: "10" });
  assert.match(markup, /5 小时 · \$0/);
  assert.match(markup, /7 天 · \$10/);
  assert.equal((markup.match(/class="status pending_sync"/g) || []).length, 2);
});

test("unlimited windows and user concurrency remain explicitly unlimited", () => {
  const markup = context.memberQuotaMarkup({ five_hour_limit_usd: null, weekly_limit_usd: null });
  assert.equal((markup.match(/不限额/g) || []).length, 2);
  assert.doesNotMatch(markup, /\$0/);
  assert.equal(context.concurrencyMarkup(null), "不限");
  assert.equal(context.concurrencyMarkup(1), "1 个请求");
});

test("short overage and exhausted zero limits preserve actual money", () => {
  const markup = context.memberQuotaMarkup({ quota_windows: [{ ...shortWindow, limit_usd: "0", status: "overage", used_usd: "0.000000001", remaining_usd: "0", overage_usd: "0.000000001" }] });
  assert.match(markup, /5 小时 · \$0/);
  assert.match(markup, /已超额/);
  assert.match(markup, /超额 \$0\.000000001/);
  const exhausted = context.memberQuotaMarkup({ quota_windows: [{ ...shortWindow, limit_usd: "0", status: "exhausted" }] });
  assert.match(exhausted, /额度已用尽/);
});

test("short quota values and unknown statuses cannot inject markup", () => {
  const hostile = '\"><img src=x onerror=alert(1)>';
  const markup = context.memberQuotaMarkup({ quota_windows: [{ ...shortWindow, limit_usd: hostile, used_usd: hostile, status: hostile }] });
  assert.doesNotMatch(markup, /<img/);
  assert.match(markup, /&lt;img/);
  assert.match(markup, /额度状态未知/);
  assert.doesNotMatch(context.concurrencyMarkup(hostile), /<img/);
});

function quotaForm(values) {
  return { get: name => values[name] ?? "" };
}
const policy = { monthly_limit_usd: "25", five_hour_limit_usd: "1", weekly_limit_usd: null, concurrency_limit: 2 };
const inputPolicy = { ...policy, weekly_limit_usd: "", concurrency_limit: "2" };
function patch(values, previous = policy) {
  return JSON.parse(JSON.stringify(context.memberQuotaPatch(quotaForm(values), previous)));
}

test("unchanged quota form yields no write and independent weekly change omits monthly", () => {
  assert.deepEqual(patch(inputPolicy), {});
  assert.deepEqual(patch({ ...inputPolicy, weekly_limit_usd: "3.000000001" }), { weekly_limit_usd: "3.000000001" });
});

test("blank new limits remove only those limits while zero money is retained", () => {
  assert.deepEqual(patch({ ...inputPolicy, five_hour_limit_usd: "", concurrency_limit: "" }), { five_hour_limit_usd: null, concurrency_limit: null });
  assert.deepEqual(patch({ ...inputPolicy, five_hour_limit_usd: "0", weekly_limit_usd: "0", monthly_limit_usd: "0" }), { monthly_limit_usd: "0", five_hour_limit_usd: "0", weekly_limit_usd: "0" });
});

for (const value of ["-1", "1e3", "NaN", "Infinity", "1.1234567890", "1,2"]) {
  test(`invalid short money ${value} is rejected without conversion`, () => {
    assert.throws(() => patch({ ...inputPolicy, weekly_limit_usd: value }), /非负 USD/);
  });
}

test("monthly quota cannot be cleared", () => {
  assert.throws(() => patch({ ...inputPolicy, monthly_limit_usd: "" }), /月度额度不能留空/);
});

for (const value of ["0", "-1", "1.5", "1e2", "Infinity", "9007199254740992"]) {
  test(`invalid concurrency ${value} cannot become a queue-blocking policy`, () => {
    assert.throws(() => context.optionalConcurrency(value), /正整数/);
  });
}

test("positive concurrency and blank unlimited are distinct", () => {
  assert.equal(context.optionalConcurrency("1"), 1);
  assert.equal(context.optionalConcurrency("  "), null);
  assert.deepEqual(patch({ ...inputPolicy, concurrency_limit: "3" }), { concurrency_limit: 3 });
});

function policyHarness(respond) {
  const calls = [];
  let saved = 0;
  const app = vm.createContext({
    location: { hash: "" }, Headers,
    window: { addEventListener() {} },
    fetch: () => new Promise(() => {}),
    FormData: class { constructor(form) { this.get = name => form.values[name]; } },
  });
  vm.runInContext(readFileSync(new URL("../internal/carpool/web/assets/app.js", import.meta.url), "utf8"), app);
  app.request = async (path, options) => { calls.push({ path, options }); return respond?.(); };
  const submit = { disabled: false, textContent: "保存设置", setAttribute(name, value) { this[name] = value; } };
  const error = { hidden: true, focus() { this.focused = true; } };
  const form = {
    values: { ...inputPolicy, weekly_limit_usd: "3" },
    querySelector: selector => selector === "button[type=submit]" ? submit : error,
    addEventListener(type, callback) { this[type] = callback; },
  };
  const editor = { isConnected: true, open: true, querySelector: () => form, close() { this.open = false; } };
  app.bindPolicyForm(editor, "/quota", values => app.memberQuotaPatch(values, policy), () => { saved++; });
  return { calls, submit, error, form, editor, save: () => form.submit({ preventDefault() {} }), saved: () => saved };
}

test("policy editor sends one independent PATCH and suppresses duplicate in-flight submissions", async () => {
  let release;
  const pending = new Promise(resolve => { release = resolve; });
  const ui = policyHarness(() => pending);
  const save = ui.save();
  assert.equal(ui.submit.disabled, true);
  assert.equal(ui.submit["aria-busy"], "true");
  assert.equal(ui.submit.textContent, "正在保存…");
  await ui.save();
  assert.equal(ui.calls.length, 1);
  assert.equal(ui.calls[0].options.method, "PATCH");
  assert.deepEqual(JSON.parse(ui.calls[0].options.body), { weekly_limit_usd: "3" });
  release();
  await save;
  assert.equal(ui.saved(), 1);
  assert.equal(ui.editor.open, false);
});

test("policy save failure preserves input with persistent accessible error and allows retry", async () => {
  let fail = true;
  const ui = policyHarness(() => { if (fail) throw new Error("暂时不可用"); });
  await ui.save();
  assert.equal(ui.editor.open, true);
  assert.equal(ui.form.values.weekly_limit_usd, "3");
  assert.equal(ui.error.hidden, false);
  assert.equal(ui.error.focused, true);
  assert.match(ui.error.textContent, /暂时不可用.*重试/);
  assert.equal(ui.submit.disabled, false);
  assert.equal(ui.submit["aria-busy"], "false");
  assert.equal(ui.saved(), 0);
  fail = false;
  await ui.save();
  assert.equal(ui.saved(), 1);
});

test("unchanged policy editor closes without a write or success claim", async () => {
  const ui = policyHarness();
  ui.form.values = inputPolicy;
  await ui.save();
  assert.equal(ui.calls.length, 0);
  assert.equal(ui.editor.open, false);
  assert.equal(ui.saved(), 0);
});

test("closed policy editor cannot refresh a different view after an in-flight save", async () => {
  let release;
  const pending = new Promise(resolve => { release = resolve; });
  const ui = policyHarness(() => pending);
  const save = ui.save();
  ui.editor.close();
  release();
  await save;
  assert.equal(ui.saved(), 0);
});
