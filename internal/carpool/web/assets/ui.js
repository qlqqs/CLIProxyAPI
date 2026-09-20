const attributeNamePattern = /^(?:[a-z][a-z0-9-]*|aria-[a-z0-9-]+|data-[a-z0-9-]+)$/;
const buttonTones = new Set(["primary", "secondary", "danger"]);
const buttonTypes = new Set(["button", "submit", "reset"]);
const bannerTones = new Set(["error", "warning", "success"]);

export function escapeHTML(value) {
  return String(value ?? "").replace(/[&<>'"]/g, character => ({
    "&": "&amp;",
    "<": "&lt;",
    ">": "&gt;",
    "'": "&#39;",
    '"': "&quot;",
  })[character]);
}

function classNames(...values) {
  return values.flat().filter(Boolean).join(" ");
}

function attributesMarkup(attributes = {}) {
  return Object.entries(attributes).map(([name, value]) => {
    if (!attributeNamePattern.test(name)) throw new Error(`Unsupported UI attribute: ${name}`);
    if (value === false || value === null || value === undefined) return "";
    if (value === true) return ` ${name}`;
    return ` ${name}="${escapeHTML(value)}"`;
  }).join("");
}

export function buttonMarkup({ label, tone = "primary", size = "default", type = "button", className = "", attributes = {} }) {
  if (!buttonTones.has(tone)) throw new Error(`Unsupported button tone: ${tone}`);
  if (!buttonTypes.has(type)) throw new Error(`Unsupported button type: ${type}`);
  const toneClass = tone === "primary" ? "" : tone;
  const sizeClass = size === "compact" ? "compact" : "";
  return `<button class="${escapeHTML(classNames("button", toneClass, sizeClass, className))}" type="${escapeHTML(type)}" data-ui="button"${attributesMarkup(attributes)}>${escapeHTML(label)}</button>`;
}

export function iconButtonMarkup({ label, content, title = label, className = "", attributes = {} }) {
  return `<button class="${escapeHTML(classNames("icon-button", className))}" type="button" data-ui="icon-button" aria-label="${escapeHTML(label)}" title="${escapeHTML(title)}"${attributesMarkup(attributes)}>${content}</button>`;
}

export function linkButtonMarkup({ label, href, tone = "primary", size = "default", className = "", attributes = {} }) {
  if (!buttonTones.has(tone)) throw new Error(`Unsupported link button tone: ${tone}`);
  const toneClass = tone === "primary" ? "" : tone;
  const sizeClass = size === "compact" ? "compact" : "";
  return `<a class="${escapeHTML(classNames("button", toneClass, sizeClass, className))}" href="${escapeHTML(href)}" data-ui="link-button"${attributesMarkup(attributes)}>${escapeHTML(label)}</a>`;
}

export function formFieldMarkup({ id, label, controlHTML, help = "", className = "" }) {
  const helpMarkup = help ? `<small>${escapeHTML(help)}</small>` : "";
  return `<div class="${escapeHTML(classNames("field", className))}" data-ui="field"><label for="${escapeHTML(id)}">${escapeHTML(label)}</label>${controlHTML}${helpMarkup}</div>`;
}

export function formActionsMarkup(actionsHTML, className = "") {
  return `<div class="${escapeHTML(classNames("form-actions", className))}" data-ui="form-actions">${actionsHTML}</div>`;
}

export function sectionHeaderMarkup({ title, description = "", descriptionHTML = "", actionsHTML = "" }) {
  const details = descriptionHTML || (description ? escapeHTML(description) : "");
  const descriptionMarkup = details ? `<p>${details}</p>` : "";
  return `<div class="section-header" data-ui="section-header"><div><h2>${escapeHTML(title)}</h2>${descriptionMarkup}</div>${actionsHTML}</div>`;
}

export function segmentedControlMarkup({ label, items }) {
  const buttons = items.map(item => `<button type="button"${attributesMarkup(item.attributes)} aria-pressed="${item.pressed ? "true" : "false"}">${escapeHTML(item.label)}</button>`).join("");
  return `<div class="period-control" data-ui="segmented-control" aria-label="${escapeHTML(label)}">${buttons}</div>`;
}

export function tableMarkup({ headings, rowsHTML, className = "", wrapperClassName = "", caption = "" }) {
  const headingMarkup = headings.map(heading => {
    const definition = typeof heading === "string" ? { label: heading } : heading;
    return `<th${definition.className ? ` class="${escapeHTML(definition.className)}"` : ""}>${escapeHTML(definition.label || "")}</th>`;
  }).join("");
  const captionMarkup = caption ? `<caption>${escapeHTML(caption)}</caption>` : "";
  return `<div class="${escapeHTML(classNames("table-wrap", wrapperClassName))}" data-ui="table"><table class="${escapeHTML(className)}">${captionMarkup}<thead><tr>${headingMarkup}</tr></thead><tbody>${rowsHTML}</tbody></table></div>`;
}

export function statusBadgeMarkup(status, label) {
  return `<span class="${escapeHTML(classNames("status", status))}" data-ui="status">${escapeHTML(label)}</span>`;
}

export function bannerMarkup({ message, tone = "warning", attributes = {} }) {
  if (!bannerTones.has(tone)) throw new Error(`Unsupported banner tone: ${tone}`);
  return `<div class="${escapeHTML(classNames("banner", `banner-${tone}`, `${tone}-banner`))}" data-ui="banner"${attributesMarkup(attributes)}>${escapeHTML(message)}</div>`;
}

export function emptyStateMarkup({ title = "", description = "", compact = false, actionHTML = "", attributes = {} }) {
  const titleMarkup = title ? `<h3>${escapeHTML(title)}</h3>` : "";
  const descriptionMarkup = description ? `<p>${escapeHTML(description)}</p>` : "";
  return `<div class="${escapeHTML(classNames("empty", compact && "compact-empty"))}" data-ui="empty"${attributesMarkup(attributes)}>${titleMarkup}${descriptionMarkup}${actionHTML}</div>`;
}

export function loadingMarkup(message = "正在加载…") {
  return `<div class="loading" data-ui="loading" role="status"><span class="loading-indicator" aria-hidden="true"></span><span>${escapeHTML(message)}</span></div>`;
}

export function dialogContentMarkup(title, body) {
  return `<div class="dialog-head" data-ui="dialog-head"><h2>${escapeHTML(title)}</h2>${iconButtonMarkup({ label: "关闭", content: "×", attributes: { "data-dialog-close": true } })}</div><div class="dialog-body">${body}</div>`;
}

export function setButtonBusy(button, busy) {
  if (!button) return;
  button.disabled = busy;
  button.setAttribute("aria-busy", String(busy));
}

export function showToast(message, tone = "neutral") {
  const region = document.querySelector("#toast-region");
  if (!region) return;
  const node = document.createElement("div");
  node.className = classNames("toast", tone !== "neutral" && `toast-${tone}`);
  node.setAttribute("role", tone === "danger" ? "alert" : "status");
  node.textContent = message;
  region.append(node);
  window.setTimeout(() => node.remove(), 3200);
}
