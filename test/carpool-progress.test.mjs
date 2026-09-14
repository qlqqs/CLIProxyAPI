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
