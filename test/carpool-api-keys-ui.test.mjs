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

test("API key cells show the complete key with a small copy control", () => {
  const markup = context.apiKeyValueMarkup({ name: "Laptop", key_ref: "key-ref", api_key: "cpk_v1_key-ref.full-secret" });
  assert.match(markup, /cpk_v1_key-ref\.full-secret/);
  assert.match(markup, /class="copy-button"/);
  assert.match(markup, /data-copy-api-key="key-ref"/);
  assert.match(markup, /aria-label="复制 Laptop 的 API Key"/);
});

test("legacy keys without a persisted token explain that they must be recreated", () => {
  const markup = context.apiKeyValueMarkup({ name: "Old", key_ref: "old-ref" });
  assert.match(markup, /旧 Key 不可查看，请重新创建/);
  assert.doesNotMatch(markup, /copy-button/);
});

test("key creation refreshes the list instead of showing a one-time secret dialog", () => {
  const section = source.slice(source.indexOf("function showKeyDialog"), source.indexOf("async function renderAdminRoute"));
  assert.match(section, /dialog\.close\(\)/);
  assert.match(section, /toast\("API Key 已创建"\)/);
  assert.doesNotMatch(section, /showOneTimeSecret|关闭后无法再次查看/);
});
