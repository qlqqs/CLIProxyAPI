import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import vm from "node:vm";

const assets = new URL("../../internal/carpool/web/assets/", import.meta.url);
export const appSource = readFileSync(new URL("app.js", assets), "utf8");
const uiSource = readFileSync(new URL("ui.js", assets), "utf8");

// The browser owns module loading in production. Unit tests keep an isolated,
// synchronous VM per case without requiring Node's experimental VM-module flag.
// Resolve only the application's single named UI import, using the real UI
// implementations in that same sandbox; fail closed if the module graph changes.
const uiImport = appSource.match(/^import\s*\{([^}]+)\}\s*from "\.\/ui\.js";/);
assert.ok(uiImport, "app.js must explicitly import its shared UI module");
const importedNames = uiImport[1].split(",").map(name => name.trim()).filter(Boolean);
const exportedNames = Array.from(uiSource.matchAll(/^export function (\w+)\(/gm), match => match[1]);
for (const name of importedNames) {
  assert.match(name, /^\w+$/, "only named UI imports are supported");
  assert.ok(exportedNames.includes(name), `missing UI export: ${name}`);
}
const uiScript = uiSource.replace(/^export (?=function )/gm, "");
const appScript = appSource.replace(uiImport[0], "");
assert.doesNotMatch(uiScript, /^\s*(?:import|export)\b/m, "unsupported UI module dependency");
assert.doesNotMatch(appScript, /^\s*(?:import|export)\b/m, "unsupported application module dependency");

export function loadCarpoolApp(context, { boot = false } = {}) {
  const ui = vm.runInContext(`(() => {\n${uiScript}\nreturn { ${importedNames.join(", ")} };\n})()`, context, { filename: "carpool/ui.js" });
  Object.assign(context, ui);
  vm.runInContext(boot ? appScript : appScript.replace(/\nboot\(\);\s*$/, "\n"), context, { filename: "carpool/app.js" });
  return context;
}
