import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { test } from "node:test";

const css = readFileSync(new URL("../internal/carpool/web/assets/app.css", import.meta.url), "utf8");
const js = readFileSync(new URL("../internal/carpool/web/assets/app.js", import.meta.url), "utf8");
const html = readFileSync(new URL("../internal/carpool/web/assets/index.html", import.meta.url), "utf8");

test("admin, passenger and login surfaces share a persistent theme toggle", () => {
  assert.match(js, /themeStorageKey = "carpool-theme"/);
  assert.match(js, /themeToggleMarkup\("login-theme-toggle"\)/);
  assert.match(js, /<div class="topbar-actions">\$\{themeToggleMarkup\(\)\}/);
  assert.match(js, /localStorage\.setItem\(themeStorageKey, resolved\)/);
  assert.match(js, /切换至\$\{target\}主题/);
});

test("dark theme uses warm Claude-style semantic tokens", () => {
  assert.match(css, /:root\[data-theme="dark"\]/);
  for (const token of ["--bg: #181714", "--surface: #24211d", "--accent: #d97757", "--ink: rgba(255, 255, 255, .88)"]) assert.ok(css.includes(token), token);
  assert.match(html, /name="color-scheme" content="light dark"/);
});
