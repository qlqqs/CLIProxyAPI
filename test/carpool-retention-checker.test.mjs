import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { test } from "node:test";
import vm from "node:vm";

const source = readFileSync(new URL("../internal/carpool/web/assets/app.js", import.meta.url), "utf8");
const start = source.indexOf("function requestDetailMarkup(");
const end = source.indexOf("\nfunction bindRequestDetails(", start);
assert.ok(start >= 0 && end > start);
const context = vm.createContext({
  escapeHTML: value => String(value ?? ""),
  formatNumber: value => String(value ?? 0),
  formatTime: value => String(value ?? ""),
  statusLabel: value => String(value ?? ""),
});
vm.runInContext(source.slice(start, end), context);

for (const billed of ["0.225", "0", null]) {
  test(`persisted unknown billing remains incomplete after event pruning: ${billed}`, () => {
    const markup = context.requestDetailMarkup({
      request_id: "checker-request", outcome: "succeeded", billing_status: "unknown",
      billed_usd: billed, unknown_cost_events: 0, event_count: 1,
    });
    assert.match(markup, /费用不完整/);
    if (billed !== null) assert.match(markup, /已确认小计/);
  });
}

test("fully priced zero is not marked as unknown", () => {
  const markup = context.requestDetailMarkup({
    request_id: "checker-request", outcome: "succeeded", billing_status: "priced",
    billed_usd: "0", unknown_cost_events: 0, event_count: 1,
  });
  assert.match(markup, /\$0/);
  assert.doesNotMatch(markup, /费用不完整|费用未知/);
});
