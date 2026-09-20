import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { test } from "node:test";
import vm from "node:vm";

const source = readFileSync(new URL("../internal/carpool/web/assets/app.js", import.meta.url), "utf8");
const context = vm.createContext({
  location: { hash: "" },
  window: { addEventListener() {} },
  Headers,
  fetch: () => new Promise(() => {}),
});
vm.runInContext(source, context);

test("usage coverage banner helper, calls and phrase are removed", () => {
  assert.doesNotMatch(source, /usageCoverage|条未知用量事件和/);
});

test("member and account tables retain unknown usage counts", () => {
  const member = context.memberTable([{ display_name: "Member", unknown_usage_events: 23 }]);
  const account = context.accountTable([{ label: "Account", usage: { unknown_usage_events: 23 } }]);
  for (const markup of [member, account]) {
    assert.match(markup, /<th class="numeric">未知<\/th>/);
    assert.match(markup, /<td class="numeric">23<\/td>/);
  }
});

test("admin usage details retain incomplete and unknown statistics", () => {
  const markup = context.adminUsageTable({
    group_by: "user",
    items: [{ display_name: "Member", incomplete: 17, unknown_usage_events: 23 }],
  });
  assert.match(markup, /<th class="numeric">不完整<\/th>/);
  assert.match(markup, /<th class="numeric">未知<\/th>/);
  assert.match(markup, /<td class="numeric">17<\/td>/);
  assert.match(markup, /<td class="numeric">23<\/td>/);
});

test("billing completeness and exhaustion warnings remain", () => {
  assert.match(source, /费用数据不完整，已用金额仅为已确认小计。/);
  assert.match(source, /7 天额度已用尽，新的请求将被拒绝。/);
  const markup = context.billingMarkup({ limit_usd: "10", used_usd: "1", unknown_cost_events: 2 });
  assert.match(markup, /2 条费用未知/);
});
