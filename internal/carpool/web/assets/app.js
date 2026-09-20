import {
  bannerMarkup,
  buttonMarkup,
  dialogContentMarkup,
  emptyStateMarkup,
  escapeHTML,
  loadingMarkup,
  setButtonBusy,
  showToast,
  statusBadgeMarkup,
} from "./ui.js";

const apiBase = "/carpool/api/v1";
const pageLimit = 25;
const maximumSelectorItems = 1000;
const themeStorageKey = "carpool-theme";

function currentTheme() {
  if (typeof document === "undefined") return "light";
  return document.documentElement.dataset.theme === "dark" ? "dark" : "light";
}

function applyTheme(theme, persist = false) {
  if (typeof document === "undefined") return;
  const resolved = theme === "dark" ? "dark" : "light";
  document.documentElement.dataset.theme = resolved;
  document.querySelector('meta[name="theme-color"]')?.setAttribute("content", resolved === "dark" ? "#181714" : "#f5f4ed");
  document.querySelectorAll("[data-theme-toggle]").forEach(button => {
    const target = resolved === "dark" ? "浅色" : "深色";
    button.setAttribute("aria-label", `切换至${target}主题`);
    button.setAttribute("title", `切换至${target}主题`);
    const label = button.querySelector(".theme-toggle-label");
    if (label) label.textContent = target;
  });
  if (!persist || typeof localStorage === "undefined") return;
  try { localStorage.setItem(themeStorageKey, resolved); } catch (_) { /* Theme still applies when storage is unavailable. */ }
}

function initializeTheme() {
  if (typeof document === "undefined") return;
  let theme = "light";
  if (typeof localStorage !== "undefined") {
    try { theme = localStorage.getItem(themeStorageKey) || theme; } catch (_) { /* Use the light default. */ }
  }
  applyTheme(theme);
}

function themeToggleMarkup(extraClass = "") {
  return `<button class="icon-button theme-toggle ${extraClass}" type="button" data-theme-toggle aria-label="切换主题"><svg class="theme-icon-moon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M20 15.2A8 8 0 0 1 8.8 4 8.5 8.5 0 1 0 20 15.2Z"/></svg><svg class="theme-icon-sun" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><circle cx="12" cy="12" r="3.5"/><path d="M12 2v2m0 16v2M4.9 4.9l1.4 1.4m11.4 11.4 1.4 1.4M2 12h2m16 0h2M4.9 19.1l1.4-1.4M17.7 6.3l1.4-1.4"/></svg><span class="theme-toggle-label"></span></button>`;
}

function bindThemeToggle(root = document) {
  root.querySelectorAll("[data-theme-toggle]").forEach(button => button.addEventListener("click", () => {
    applyTheme(currentTheme() === "dark" ? "light" : "dark", true);
  }));
  applyTheme(currentTheme());
}

initializeTheme();

function emptyPage() {
  return { items: [], total: 0, nextCursor: "", loaded: false };
}

const state = {
  session: null,
  csrf: "",
  route: location.hash.slice(1).split("?")[0] || "/",
  period: "today",
  refreshTimer: 0,
  pages: {
    keys: emptyPage(),
    users: emptyPage(),
    cars: emptyPage(),
    audit: emptyPage(),
    usageRequests: emptyPage(),
  },
  requestFilters: "",
  requestFilterValues: {},
  searches: { keys: "", users: "", cars: "" },
  report: {
    rangeMode: "preset",
    period: "today",
    from: "",
    to: "",
    groupBy: "user",
    carRef: "",
    userRef: "",
    accountRef: "",
  },
};

const routeTitles = {
  "/": "概览",
  "/members": "成员用量",
  "/accounts": "账号状态",
  "/admin-accounts": "账号管理",
  "/keys": "API Key",
  "/users": "用户管理",
  "/cars": "车辆管理",
  "/usage": "用量报表",
  "/requests": "请求明细",
  "/pricing": "价格目录",
  "/retention": "数据保留",
  "/audit": "审计记录",
  "/password": "修改密码",
};

async function request(path, options = {}) {
  const headers = new Headers(options.headers || {});
  if (options.body && !(options.body instanceof FormData) && !headers.has("Content-Type")) headers.set("Content-Type", "application/json");
  if (options.method && !["GET", "HEAD"].includes(options.method) && state.csrf) {
    headers.set("X-Carpool-CSRF", state.csrf);
  }
  const response = await fetch(apiBase + path, { credentials: "same-origin", ...options, headers });
  const data = response.status === 204 ? null : await response.json().catch(() => null);
  if (!response.ok) {
    const error = new Error(data?.error?.message || (typeof data?.error === "string" ? data.error : "请求失败"));
    error.code = data?.error?.code || "request_failed";
    error.status = response.status;
    error.data = data;
    throw error;
  }
  return data;
}

function formatNumber(value) {
  return new Intl.NumberFormat("zh-CN").format(Number(value || 0));
}

function formatTime(value) {
  if (!value) return "-";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return "-";
  const options = { dateStyle: "medium", timeStyle: "short" };
  if (state.session?.report_timezone) options.timeZone = state.session.report_timezone;
  try {
    return new Intl.DateTimeFormat("zh-CN", options).format(date);
  } catch (_) {
    delete options.timeZone;
    return new Intl.DateTimeFormat("zh-CN", options).format(date);
  }
}

function toDatetimeLocalValue(date) {
  const local = new Date(date.getTime() - date.getTimezoneOffset() * 60000);
  return local.toISOString().slice(0, 16);
}

function toast(message) {
  showToast(message);
}

const statusLabels = {
  active: "启用",
  disabled: "禁用",
  retired: "已退役",
  healthy: "正常",
  degraded: "受限",
  unavailable: "不可用",
  unknown: "未知",
  allowed: "可用",
  rejected: "已拒绝",
  exceeded: "已达上限",
  observed: "已观测",
  revoked: "已撤销",
  expired: "已过期",
};

function statusText(status) {
  return statusLabels[status] || status || "未知";
}

function statusLabel(status) {
  return statusBadgeMarkup(status, statusText(status));
}

function periodControl() {
  return `<div class="period-control" aria-label="统计周期">
    ${[["today", "今日"], ["7d", "最近 7 天"], ["30d", "最近 30 天"]].map(([value, label]) =>
      `<button type="button" data-period="${value}" aria-pressed="${state.period === value}">${label}</button>`).join("")}
  </div>`;
}

// Resets report, request-filter, and period selections that otherwise linger
// across routes and can be mistaken for unfiltered data.
function resetEphemeralFilters() {
  state.period = "today";
  state.report = { rangeMode: "preset", period: "today", from: "", to: "", groupBy: "user", carRef: "", userRef: "", accountRef: "" };
  state.requestFilters = "";
  state.requestFilterValues = {};
}

function activeFilterCount() {
  return requestFilterKeys.filter(key => String(state.requestFilterValues[key] || "").trim()).length;
}

function activePeriodFilter() {
  const source = new URLSearchParams((state.requestFilters.split("?")[1] || ""));
  const period = source.get("period");
  const from = source.get("from");
  const to = source.get("to");
  if (from || to) return "自定义区间";
  return { today: "今日", "7d": "最近 7 天", "30d": "最近 30 天" }[period] || null;
}

function bindPeriod(content = document) {
  content.querySelectorAll("[data-period]").forEach(button => button.addEventListener("click", () => {
    state.period = button.dataset.period;
    renderRoute();
  }));
}

function clearStatusRefresh() {
  if (state.refreshTimer) window.clearTimeout(state.refreshTimer);
  state.refreshTimer = 0;
}

function scheduleAccountRefresh() {
  clearStatusRefresh();
  state.refreshTimer = window.setTimeout(() => {
    if (state.session?.role === "passenger" && state.route === "/accounts") renderRoute();
  }, 60000);
}

function resetPage(name) {
  state.pages[name] = emptyPage();
  return state.pages[name];
}

async function loadPage(name, path, reset = false) {
  const page = reset ? resetPage(name) : state.pages[name];
  if (!reset && page.loaded && !page.nextCursor) return page;
  const params = new URLSearchParams({ limit: String(pageLimit) });
  if (!reset && page.nextCursor) params.set("cursor", page.nextCursor);
  const separator = path.includes("?") ? "&" : "?";
  const data = await request(`${path}${separator}${params}`);
  page.items = reset ? (data.items || []) : page.items.concat(data.items || []);
  page.total = Number.isFinite(Number(data.total)) ? Number(data.total) : page.items.length;
  page.nextCursor = data.next_cursor || "";
  page.loaded = true;
  return page;
}

async function fetchAllPages(path) {
  const items = [];
  let cursor = "";
  const seen = new Set();
  do {
    const params = new URLSearchParams({ limit: "100" });
    if (cursor) params.set("cursor", cursor);
    const separator = path.includes("?") ? "&" : "?";
    const data = await request(`${path}${separator}${params}`);
    items.push(...(data.items || []));
    const next = data.next_cursor || "";
    if (!next || seen.has(next) || items.length >= maximumSelectorItems) break;
    seen.add(next);
    cursor = next;
  } while (cursor);
  return items.slice(0, maximumSelectorItems);
}

function paginationFooter(page, label = "项") {
  if (!page.loaded || (!page.nextCursor && page.items.length === 0)) return "";
  const total = page.total >= page.items.length ? page.total : page.items.length;
  return `<div class="pagination"><span>已显示 ${formatNumber(page.items.length)} / ${formatNumber(total)} ${escapeHTML(label)}</span>
    ${page.nextCursor ? buttonMarkup({ label: "加载更多", tone: "secondary", size: "compact", attributes: { "data-load-more": true } }) : ""}</div>`;
}

function searchBar(name, placeholder) {
  const value = state.searches[name] || "";
  return `<form class="list-search" data-search-form="${escapeHTML(name)}" role="search">
    <input type="search" name="q" value="${escapeHTML(value)}" placeholder="${escapeHTML(placeholder)}" aria-label="${escapeHTML(placeholder)}" maxlength="80">
    ${buttonMarkup({ label: "搜索", tone: "secondary", size: "compact", type: "submit" })}
    ${value ? buttonMarkup({ label: "清除", tone: "secondary", size: "compact", attributes: { "data-search-clear": name } }) : ""}
  </form>`;
}

function searchPath(name, base) {
  const q = (state.searches[name] || "").trim();
  if (!q) return base;
  return `${base}${base.includes("?") ? "&" : "?"}q=${encodeURIComponent(q)}`;
}

function bindSearch(content, name, rerender) {
  const form = content.querySelector(`[data-search-form="${name}"]`);
  form?.addEventListener("submit", event => {
    event.preventDefault();
    state.searches[name] = String(new FormData(form).get("q") || "").trim();
    resetPage(name);
    rerender(true);
  });
  content.querySelector(`[data-search-clear="${name}"]`)?.addEventListener("click", () => {
    state.searches[name] = "";
    resetPage(name);
    rerender(true);
  });
}

function openDialog(title, body, wide = false) {
  const dialog = document.createElement("dialog");
  if (wide) dialog.className = "dialog-wide";
  dialog.addEventListener("close", () => {
    dialog.querySelectorAll(".secret-box").forEach(node => { node.textContent = ""; });
    dialog.replaceChildren();
    dialog.remove();
  }, { once: true });
  document.body.append(dialog);
  setDialogContent(dialog, title, body);
  dialog.showModal();
  return dialog;
}

function closeEntityPanels(root = document) {
  root.querySelectorAll("dialog.entity-panel").forEach(dialog => {
    dialog.querySelectorAll(".secret-box").forEach(node => { node.textContent = ""; });
    dialog.close();
    dialog.replaceChildren();
    dialog.remove();
  });
}

function openEntityPanel(title, body) {
  const slot = document.querySelector("#content .entity-detail-slot");
  if (!slot) return openDialog(title, body, true);
  const trigger = document.activeElement;
  closeEntityPanels(slot);
  const placeholder = slot.querySelector("[data-entity-empty]");
  if (placeholder) placeholder.hidden = true;
  const dialog = document.createElement("dialog");
  dialog.className = "entity-panel";
  dialog.setAttribute("aria-modal", "false");
  slot.append(dialog);
  dialog.addEventListener("close", () => {
    const current = slot.querySelector("dialog.entity-panel") === dialog;
    dialog.querySelectorAll(".secret-box").forEach(node => { node.textContent = ""; });
    dialog.replaceChildren();
    dialog.remove();
    if (current) {
      if (placeholder) placeholder.hidden = false;
      slot.closest(".entity-workspace")?.querySelectorAll("tr.is-selected").forEach(row => row.classList.remove("is-selected"));
      if (trigger?.isConnected) trigger.focus();
    }
  }, { once: true });
  dialog.addEventListener("keydown", event => {
    if (event.key === "Escape" && !event.defaultPrevented) {
      event.preventDefault();
      dialog.close();
    }
  });
  setDialogContent(dialog, title, body);
  dialog.show();
  dialog.querySelector("[data-dialog-close]").focus();
  return dialog;
}

function selectEntityRow(content, button) {
  content.querySelectorAll(".entity-list tr.is-selected").forEach(row => row.classList.remove("is-selected"));
  button.closest("tr")?.classList.add("is-selected");
}

function entityEmptyMarkup(kind) {
  const title = `选择一${kind === "用户" ? "名用户" : "辆车辆"}`;
  const description = kind === "用户" ? "点击列表中的管理，查看用户资料、重置密码或管理 API Key。" : "点击列表中的管理，调整车辆信息、成员额度与账号分配。";
  return `<aside class="entity-detail-slot" aria-label="${escapeHTML(kind)}详情">${emptyStateMarkup({ title, description, attributes: { "data-entity-empty": true } })}</aside>`;
}

function setDialogContent(dialog, title, body) {
  const restoreFocus = dialog.contains(document.activeElement);
  dialog.setAttribute("aria-label", title);
  dialog.innerHTML = dialogContentMarkup(title, body);
  dialog.querySelectorAll("[data-dialog-close]").forEach(button => button.addEventListener("click", () => dialog.close()));
  if (restoreFocus) dialog.querySelector("[data-dialog-close]").focus();
}

function showOneTimeSecret(dialog, title, secret, warning, onFinish) {
  if (!dialog.isConnected || !dialog.open) return;
  setDialogContent(dialog, title, `${bannerMarkup({ message: warning })}<div class="secret-box" role="status"></div><div class="form-actions">${buttonMarkup({ label: "完成", attributes: { "data-finish": true } })}</div>`);
  dialog.querySelector(".secret-box").textContent = secret;
  dialog.querySelector("[data-finish]").addEventListener("click", () => {
    dialog.close();
    if (onFinish) onFinish();
  });
}

function loginView(message = "") {
  closeAdminAccountDialogs();
  clearStatusRefresh();
  closeEntityPanels();
  document.querySelector("#app").innerHTML = `${themeToggleMarkup("login-theme-toggle")}<main class="login-shell">
    <section class="login-story" aria-labelledby="login-title">
      <h1 id="login-title">来不及解释了，<br>快上车！</h1>
    </section>
    <form class="login-panel" id="login-form">
      <div class="login-intro"><h2>检票口</h2><p>使用售票员下发的专属车票。</p></div>
      ${message ? bannerMarkup({ message, tone: "error", attributes: { role: "alert" } }) : ""}
      <div class="field"><label for="username">用户名</label><input id="username" name="username" autocomplete="username" required minlength="3" maxlength="64"></div>
      <div class="field"><label for="password">密码</label><input id="password" name="password" type="password" autocomplete="current-password" required minlength="12" maxlength="128"></div>
      ${buttonMarkup({ label: "登录", type: "submit" })}
    </form>
  </main>`;
  bindThemeToggle(document.querySelector("#app"));
  document.querySelector("#login-form").addEventListener("submit", async event => {
    event.preventDefault();
    const button = event.currentTarget.querySelector("button[type=submit]");
    const form = new FormData(event.currentTarget);
    setButtonBusy(button, true);
    try {
      const data = await request("/session", { method: "POST", body: JSON.stringify({ username: form.get("username"), password: form.get("password") }) });
      state.session = data;
      state.csrf = data.csrf_token || "";
      resetEphemeralFilters();
      state.route = data.must_change_password ? "/password" : "/";
      location.hash = `#${state.route}`;
      renderShell();
    } catch (error) {
      loginView(error.message);
    } finally {
      setButtonBusy(button, false);
    }
  });
}

function navigation() {
  if (state.session?.must_change_password) return [["/password", "修改密码"]];
  const passenger = [
    ["/", "概览"], ["/members", "成员用量"], ["/accounts", "账号状态"], ["/keys", "API Key"], ["/password", "修改密码"],
  ];
  const admin = [
    ["/", "概览"], ["/cars", "车辆管理"], ["/admin-accounts", "账号管理"], ["/users", "用户管理"], ["/usage", "用量报表"], ["/requests", "请求明细"], ["/pricing", "价格目录"], ["/retention", "数据保留"], ["/audit", "审计记录"], ["/password", "修改密码"],
  ];
  return state.session?.role === "carpool_admin" ? admin : passenger;
}

function iconMarkup(route) {
  if (route === "/admin-accounts") route = "/accounts";
  const paths = {
    "/": '<path d="m3 10 9-7 9 7v10H3Z M9 20v-7h6v7"/>',
    "/members": '<circle cx="9" cy="7" r="3"/><path d="M3 21v-3a6 6 0 0 1 12 0v3M17 4a3 3 0 0 1 0 6m1 4a5 5 0 0 1 3 4v3"/>',
    "/accounts": '<rect x="3" y="4" width="18" height="16" rx="3"/><path d="M3 10h18m-13 5h3m3 0h2"/>',
    "/keys": '<circle cx="8" cy="8" r="5"/><path d="m12 12 9 9m-3-3 3-3m-6 0 3-3"/>',
    "/users": '<circle cx="12" cy="7" r="4"/><path d="M4 21v-2a8 8 0 0 1 16 0v2"/>',
    "/cars": '<path d="m5 10 2-6h10l2 6M3 10h18v8H3Zm2 8v3m14-3v3M6 14h2m8 0h2"/>',
    "/usage": '<path d="M4 3v18h17M8 16v-5m5 5V7m5 9V4"/>',
    "/requests": '<rect x="4" y="3" width="16" height="18" rx="2"/><path d="M8 8h8M8 12h8m-8 4h5"/>',
    "/pricing": '<path d="M3 3h8l10 10-8 8L3 11Z"/><circle cx="7.5" cy="7.5" r="1"/>',
    "/retention": '<path d="M4 7h16v14H4ZM3 3h18v4H3Zm6 9h6"/>',
    "/audit": '<path d="m12 3 8 3v6c0 5-8 9-8 9s-8-4-8-9V6Zm-4 9 3 3 5-6"/>',
    "/password": '<rect x="4" y="10" width="16" height="11" rx="2"/><path d="M8 10V7a4 4 0 0 1 8 0v3m-4 5v2"/>',
  };
  return `<svg viewBox="0 0 24 24" width="20" height="20" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false">${Object.hasOwn(paths, route) ? paths[route] : paths["/"]}</svg>`;
}

function renderShell() {
  closeAdminAccountDialogs();
  if (!state.session) return loginView();
  closeEntityPanels();
  const navigationItems = navigation();
  document.querySelector("#app").innerHTML = `<div class="app-shell">
    <a class="skip-link" href="#content">跳到主要内容</a>
    <aside class="app-rail" id="app-rail">
      <div class="rail-brand"><strong>工作台</strong></div>
      <nav class="nav" aria-label="主导航">${navigationItems.map(([route, label]) => `<a href="#${route}" data-route="${route}" aria-current="${state.route === route ? "page" : "false"}"><span class="nav-icon">${iconMarkup(route)}</span><span class="nav-label">${label}</span></a>`).join("")}</nav>
    </aside>
    <div class="app-workspace">
      <header class="topbar">
        <button class="icon-button" id="nav-toggle" type="button" aria-controls="app-rail" aria-expanded="false" aria-label="展开导航"><svg viewBox="0 0 24 24" width="20" height="20" fill="none" stroke="currentColor" stroke-width="1.6" aria-hidden="true"><path d="M4 6h16M4 12h16M4 18h16"/></svg></button>
        <div class="topbar-actions">${themeToggleMarkup()}<span class="tag">${state.session.role === "carpool_admin" ? "管理员" : "乘客"}</span>${statusLabel(state.session.module_status || "unknown")}<span class="user-chip">${escapeHTML(state.session.display_name)}</span><button class="button secondary compact" id="logout" type="button">退出</button></div>
      </header>
      <main class="main"><div class="content" id="content" data-page="${escapeHTML(state.route)}">${loadingMarkup()}</div></main>
    </div>
  </div>`;
  const shell = document.querySelector(".app-shell");
  bindThemeToggle(shell);
  const toggle = document.querySelector("#nav-toggle");
  const setNavOpen = open => {
    shell.classList.toggle("nav-open", open);
    toggle.setAttribute("aria-expanded", String(open));
    toggle.setAttribute("aria-label", open ? "收起导航" : "展开导航");
  };
  shell.querySelector(".skip-link").addEventListener("click", event => {
    event.preventDefault();
    document.querySelector("#content")?.focus();
  });
  toggle.addEventListener("click", () => {
    const open = !shell.classList.contains("nav-open");
    setNavOpen(open);
    if (open) shell.querySelector(".nav a")?.focus();
  });
  shell.querySelectorAll(".nav [data-route]").forEach(link => link.addEventListener("click", event => {
    if (event.ctrlKey || event.metaKey || event.shiftKey || event.altKey) return;
    const wasOpen = shell.classList.contains("nav-open");
    setNavOpen(false);
    if (wasOpen) toggle.focus();
  }));
  shell.addEventListener("keydown", event => {
    if (event.key === "Escape" && shell.classList.contains("nav-open")) {
      setNavOpen(false);
      toggle.focus();
    }
  });
  document.querySelector("#logout").addEventListener("click", async event => {
    closeAdminAccountDialogs();
    setButtonBusy(event.currentTarget, true);
    try { await request("/session", { method: "DELETE" }); } catch (_) { /* Local state still ends. */ }
    state.session = null;
    state.csrf = "";
    resetEphemeralFilters();
    loginView();
  });
  renderRoute();
}

async function renderRoute() {
  if (!state.session) return;
  clearStatusRefresh();
  closeAdminAccountDialogs();
  const allowed = new Set(navigation().map(([route]) => route));
  if (!allowed.has(state.route)) {
    location.hash = state.session.must_change_password ? "#/password" : "#/";
    return;
  }
  renderShellFrameTitle();
  const previous = document.querySelector("#content");
  if (!previous) return;
  closeEntityPanels();
  // Each render owns a node; older requests can only update detached content.
  const content = document.createElement("div");
  content.id = "content";
  content.tabIndex = -1;
  content.className = "content";
  content.dataset.page = state.route;
  content.innerHTML = loadingMarkup();
  previous.replaceWith(content);
  const route = state.route;
  const session = state.session;
  try {
    if (route === "/password") await renderPassword(content);
    else if (session.role === "carpool_admin") await renderAdminRoute(content);
    else await renderPassengerRoute(content);
  } catch (error) {
    if (document.querySelector("#content") !== content || state.session !== session || state.route !== route) return;
    if (error.status === 401) {
      state.session = null;
      state.csrf = "";
      loginView("会话已失效，请重新登录");
      return;
    }
    content.innerHTML = `<section class="workspace-panel">${bannerMarkup({ message: error.message, tone: "error", attributes: { role: "alert" } })}<p class="muted">页面暂时未能加载，请重试。</p><div class="form-actions">${buttonMarkup({ label: "重新加载", tone: "secondary", attributes: { "data-route-retry": true } })}</div></section>`;
    content.querySelector("[data-route-retry]").addEventListener("click", () => renderRoute());
  }
}

function renderShellFrameTitle() {
  document.querySelectorAll("[data-route]").forEach(button => button.setAttribute("aria-current", button.dataset.route === state.route ? "page" : "false"));
}

function updatePageHash(route, query = "") {
  const hash = `#${route}${query ? `?${query}` : ""}`;
  if (location.hash === hash) return false;
  location.hash = hash;
  return true;
}

async function renderPassengerRoute(content) {
  if (state.route === "/") {
    const data = await request("/me/car");
    if (!data?.car) {
      content.innerHTML = `<div class="section-header"><div><h2>我的车辆</h2><p>查看额度、成员用量和上游账号状态</p></div></div><section class="workspace-panel"><div class="empty"><h3>尚未分配车辆</h3><p>请联系管理员分配车辆并设置额度。已有 API Key 可在密钥页面管理。</p><a class="button secondary" href="#/keys">管理 API Key</a></div></section>`;
      return;
    }
    const windows = Array.isArray(data.quota_windows) ? data.quota_windows : [];
    const weekly = windows.find(item => item.kind === "7d") || {};
    const unlimited = weekly.status === "unlimited";
    const notStarted = weekly.status === "not_started";
    const exhausted = weekly.status === "exhausted" || weekly.status === "overage";
    const heroValue = unlimited ? "不限" : moneyMarkup(weekly.remaining_usd);
    content.innerHTML = `<div class="section-header"><div><h2>我的概览</h2><p>${escapeHTML(data.car.name)} · ${escapeHTML(data.car.description || "当前车辆")}</p></div>${statusLabel(data.car.status)}</div>
      <div class="dashboard-layout"><div class="dashboard-main">
        <section class="billing-hero" aria-label="我的 7 天额度"><div><span class="eyebrow">7 天剩余额度 · USD</span><h3 class="balance-value">${heroValue}</h3><p>${unlimited ? `已确认用量 ${moneyMarkup(weekly.used_usd)} · 当前不限额` : `7 天额度 ${moneyMarkup(weekly.limit_usd)} · 已确认用量 ${moneyMarkup(weekly.used_usd)}`}${notStarted ? " · 首次使用后开始计时" : ""}</p></div>${memberShortWindow(data, windows, "7d", "7 天", "weekly_limit_usd", true)}</section>
        ${exhausted ? `<div class="warning-banner">7 天额度已用尽，新的请求将被拒绝。请联系管理员调整额度，或等待窗口重置。</div>` : ""}
        ${data.car.status !== "active" ? `<div class="warning-banner">车辆当前${escapeHTML(statusText(data.car.status))}，请联系管理员确认车辆状态。</div>` : ""}
        ${weekly.data_complete === false ? `<div class="warning-banner">${weekly.unknown_cost_events ? `${formatNumber(weekly.unknown_cost_events)} 条费用未知；` : ""}费用数据不完整，已用金额仅为已确认小计。</div>` : ""}
        <section class="workspace-panel"><div class="panel-heading"><h3>我的 5 小时额度 · USD</h3></div>${memberShortWindow(data, windows, "5h", "5 小时", "five_hour_limit_usd", false)}<p class="muted section-note">本地滚动窗口从首笔可靠费用开始；0 表示不限。已开始的请求仍会结算，可能产生超额。</p></section>
        <section class="workspace-panel"><div class="panel-heading"><h3>当前车辆</h3><a href="#/accounts">查看账号状态</a></div>
          <dl class="overview-stats"><div><dt>当前成员</dt><dd>${formatNumber(data.car.member_count)}</dd></div><div><dt>可用账号</dt><dd>${formatNumber(data.car.available_account_count)}</dd></div><div><dt>席位上限</dt><dd>${data.car.seat_limit ? formatNumber(data.car.seat_limit) : "不限"}</dd></div></dl>
          <p class="muted">车辆启用不代表请求一定可用；请求仍受个人额度与上游账号状态限制。</p>
        </section>
      </div><aside class="dashboard-side">
        <section class="workspace-panel"><div class="panel-heading"><h3>常用操作</h3></div><div class="quick-links"><a href="#/keys">${iconMarkup("/keys")}<span>管理 API Key</span></a><a href="#/members">${iconMarkup("/members")}<span>查看成员用量</span></a><a href="#/accounts">${iconMarkup("/accounts")}<span>查看账号状态</span></a></div></section>
        <section class="workspace-panel"><div class="panel-heading"><h3>我的请求并发</h3></div><p>用户上限：<strong>${concurrencyMarkup(data.concurrency_limit)}</strong></p><p class="muted">所有 API Key 共享此上限，同时受账号并发限制。</p><p class="muted">${concurrencyQueueHelp}</p></section>
        <section class="workspace-panel"><div class="panel-heading"><h3>额度与统计</h3></div><ul class="activity-list"><li><strong>7 天重置</strong><span>${weekly.reset_at ? escapeHTML(formatTime(weekly.reset_at)) : "首次使用后开始计时"}</span></li><li><strong>报表时区</strong><span>${escapeHTML(data.report_timezone || state.session?.report_timezone || "UTC")}</span></li></ul></section>
      </aside></div>`;
    return;
  }
  if (state.route === "/members") {
    const data = await request(`/me/members/usage?period=${encodeURIComponent(state.period)}`);
    content.innerHTML = `<div class="section-header"><div><h2>成员用量</h2><p>${escapeHTML(formatTime(data.data_from))} 至 ${escapeHTML(formatTime(data.data_to))}</p></div>${periodControl()}</div>
      ${memberTable(data.items || [])}`;
    bindPeriod(content);
    return;
  }
  if (state.route === "/accounts") {
    const data = await request(`/me/accounts?period=${encodeURIComponent(state.period)}`);
    content.innerHTML = `<div class="section-header"><div><h2>车辆账号</h2><p>状态更新时间：${escapeHTML(formatTime(new Date()))} · <span class="muted">每 60 秒自动刷新</span></p><p>账号并发由同账号乘客共享；CPA 原生配额不是个人 USD 额度。</p></div>${periodControl()}</div>${accountTable(data.items || [])}`;
    bindPeriod(content);
    if (content.isConnected) scheduleAccountRefresh();
    return;
  }
  if (state.route === "/keys") return renderKeys(content, true);
}

function memberTable(items) {
  if (!items.length) return `<div class="empty">该周期暂无成员用量</div>`;
  return `<div class="table-wrap"><table class="member-usage-table"><thead><tr><th>成员</th><th>个人额度</th><th>并发</th><th class="numeric">请求</th><th class="numeric">成功</th><th class="numeric">失败</th><th class="numeric">Token</th><th class="numeric">未知</th></tr></thead><tbody>
    ${items.map(item => `<tr><td><strong>${escapeHTML(item.display_name)}</strong><br><span class="tag">${item.left ? "已离车" : "当前"}</span></td><td>${memberQuotaRow(item)}</td><td>${item.left ? `<span class="muted">—</span>` : concurrencyBadgeMarkup(item.concurrency_limit)}</td><td class="numeric">${formatNumber(item.logical_requests)}</td><td class="numeric">${formatNumber(item.succeeded)}</td><td class="numeric">${formatNumber(item.failed)}</td><td class="numeric">${formatNumber(item.known_total_tokens)}</td><td class="numeric">${formatNumber(item.unknown_usage_events)}</td></tr>`).join("")}
  </tbody></table></div>`;
}

function moneyMarkup(value) {
  return value == null || value === "" ? "未提供" : `$${escapeHTML(value)}`;
}

const concurrencyQueueHelp = "达到用户或账号并发上限时，请求会排队等待；等待队列已满时返回 HTTP 429。取消请求会退出等待。";

function concurrencyMarkup(value) {
  return value == null ? "不限" : `${escapeHTML(value)} 个请求`;
}

function concurrencyBadgeMarkup(value) {
  return `<span class="concurrency-badge" title="该用户的全部 API Key 共享此并发上限"><svg aria-hidden="true" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path stroke-linecap="round" stroke-linejoin="round" d="M3.75 6A2.25 2.25 0 0 1 6 3.75h2.25A2.25 2.25 0 0 1 10.5 6v2.25a2.25 2.25 0 0 1-2.25 2.25H6A2.25 2.25 0 0 1 3.75 8.25V6Zm0 9.75A2.25 2.25 0 0 1 6 13.5h2.25a2.25 2.25 0 0 1 2.25 2.25V18a2.25 2.25 0 0 1-2.25 2.25H6A2.25 2.25 0 0 1 3.75 18v-2.25ZM13.5 6a2.25 2.25 0 0 1 2.25-2.25H18A2.25 2.25 0 0 1 20.25 6v2.25A2.25 2.25 0 0 1 18 10.5h-2.25a2.25 2.25 0 0 1-2.25-2.25V6Zm0 9.75a2.25 2.25 0 0 1 2.25-2.25H18a2.25 2.25 0 0 1 2.25 2.25V18A2.25 2.25 0 0 1 18 20.25h-2.25A2.25 2.25 0 0 1 13.5 18v-2.25Z"/></svg><span>${concurrencyMarkup(value)}</span></span>`;
}

const memberQuotaStatusLabels = { unlimited: "不限", not_started: "尚未开始", active: "额度可用", exhausted: "额度已用尽", overage: "已超额" };

// One shared card template for every member quota window: heading, meter,
// amounts, details and warnings always sit in the same slots.
function memberQuotaStatusMarkup(status) {
  return `<span class="status ${status}">${memberQuotaStatusLabels[status] || "额度状态未知"}</span>`;
}

function memberQuotaWindowCard(title, statusMarkup, meter, amounts, details, warning) {
  return `<div class="member-quota-window"><div class="member-quota-heading"><strong>${title}</strong>${statusMarkup}</div>${meter}${amounts}${details}${warning}</div>`;
}

const memberShortWindowSpecs = [["5h", "5 小时", "five_hour_limit_usd"], ["7d", "7 天", "weekly_limit_usd"]];

function memberShortWindow(member, windows, kind, label, field, compact, showAmounts = true, showCoverageWarning = true) {
  const window = windows.find(item => item.kind === kind) || {};
  const limit = window.limit_usd ?? member[field] ?? "0";
  const status = Object.hasOwn(memberQuotaStatusLabels, window.status) ? window.status : (limit === "0" ? "unlimited" : "not_started");
  const usedValue = Number.parseFloat(window.used_usd);
  const limitValue = Number.parseFloat(limit);
  const usedPercent = Number.isFinite(usedValue) && Number.isFinite(limitValue) && limitValue > 0 ? (usedValue / limitValue) * 100 : null;
  const meter = usedPercent != null ? `<div class="quota-meter">${percentageMeter(usedPercent, `${label} 已用比例`)}<b>${escapeHTML(formatQuotaPercent(usedPercent))}</b></div>` : "";
  const amounts = !showAmounts ? "" : `<span class="member-quota-amounts">已确认 ${moneyMarkup(window.used_usd || "0")}${status === "unlimited" ? " · 剩余 不限" : ` · 剩余 ${moneyMarkup(window.remaining_usd)}`}${status === "overage" ? ` · 超额 ${moneyMarkup(window.overage_usd)}` : ""}</span>`;
  const details = compact ? "" : `<small>${status === "not_started" ? "首次使用后开始计时" : `重置：${window.reset_at ? escapeHTML(formatTime(window.reset_at)) : "—"}`}</small>`;
  const warning = showCoverageWarning && window.unknown_cost_events ? `<small class="quota-policy-warning">${formatNumber(window.unknown_cost_events)} 条费用未知</small>` : "";
  return memberQuotaWindowCard(`${label} · ${status === "unlimited" ? "不限额" : moneyMarkup(limit)}`, memberQuotaStatusMarkup(status), meter, amounts, details, warning);
}


function memberQuotaMarkup(member, compact = false) {
  const windows = Array.isArray(member.quota_windows) ? member.quota_windows : [];
  return `<div class="member-quota-windows${compact ? " compact-quota-windows" : ""}">${memberShortWindowSpecs.map(([kind, label, field]) => memberShortWindow(member, windows, kind, label, field, compact, true, false)).join("")}</div>`;
}

function memberQuotaTableWindow(item, windows, kind, field) {
  const window = windows.find(candidate => candidate.kind === kind) || {};
  const limit = window.limit_usd ?? item[field] ?? "0";
  const status = item.left ? "unavailable" : (Object.hasOwn(memberQuotaStatusLabels, window.status) ? window.status : (String(limit) === "0" ? "unlimited" : "not_started"));
  const usedValue = Number.parseFloat(window.used_usd);
  const limitValue = Number.parseFloat(limit);
  const percentage = status !== "unlimited" && status !== "not_started" && Number.isFinite(usedValue) && usedValue >= 0 && Number.isFinite(limitValue) && limitValue > 0
    ? (usedValue / limitValue) * 100
    : null;
  const label = kind === "5h" ? "5h" : "7d";
  const meter = percentage == null
    ? `<div class="quota-meter member-quota-table-meter"><span class="quota-meter-track member-quota-meter-empty" role="img" aria-label="${label} 已用比例不可用"></span><b>—</b></div>`
    : `<div class="quota-meter member-quota-table-meter">${percentageMeter(percentage, `${label} 已用比例`)}<b>${escapeHTML(formatQuotaPercent(percentage))}</b></div>`;
  return `<div class="member-quota-table-window"><strong>${label}</strong>${meter}</div>`;
}

function memberQuotaRow(item) {
  const windows = Array.isArray(item.quota_windows) ? item.quota_windows : [];
  return `<div class="member-quota-row">${memberShortWindowSpecs.map(([kind, , field]) => memberQuotaTableWindow(item, windows, kind, field)).join("")}</div>`;
}

function optionalConcurrency(value) {
  const text = String(value ?? "").trim();
  if (!text) return null;
  const count = Number(text);
  if (!/^[0-9]+$/.test(text) || !Number.isSafeInteger(count) || count < 1) throw new Error("并发上限请填写正整数，或留空表示不限；不能填写 0。");
  return count;
}

function memberQuotaPatch(form, member) {
  const patch = {};
  for (const field of ["five_hour_limit_usd", "weekly_limit_usd"]) {
    const text = String(form.get(field) ?? "").trim();
    const previous = String(member[field] ?? "").trim();
    if (text === previous) continue;
    if (!/^[0-9]+(\.[0-9]{1,9})?$/.test(text)) throw new Error("额度请填写非负 USD 金额，最多 9 位小数；0 表示不限。");
    patch[field] = text;
  }
  const concurrency = optionalConcurrency(form.get("concurrency_limit"));
  if (concurrency !== (member.concurrency_limit ?? null)) patch.concurrency_limit = concurrency;
  return patch;
}

function bindPolicyForm(editor, path, buildPatch, onSaved) {
  const form = editor.querySelector("form");
  const submit = form.querySelector("button[type=submit]");
  const errorRegion = form.querySelector("[data-policy-error]");
  form.addEventListener("submit", async event => {
    event.preventDefault();
    if (submit.disabled) return;
    errorRegion.hidden = true;
    try {
      const patch = buildPatch(new FormData(form));
      if (!Object.keys(patch).length) {
        editor.close();
        return;
      }
      setButtonBusy(submit, true);
      submit.textContent = "正在保存…";
      await request(path, { method: "PATCH", body: JSON.stringify(patch) });
      if (!editor.isConnected || !editor.open) return;
      editor.close();
      onSaved();
    } catch (error) {
      if (!editor.isConnected || !editor.open) return;
      errorRegion.textContent = `${error.message} 请检查输入后重试；如连接中断，请重新打开表单核对保存结果。`;
      errorRegion.hidden = false;
      errorRegion.focus();
    } finally {
      setButtonBusy(submit, false);
      submit.textContent = "保存设置";
    }
  });
}

function billingMarkup(billing) {
  if (!billing || billing.limit_usd == null || billing.limit_usd === "" || billing.status === "not_configured") return `<span class="quota-missing">待管理员设置额度</span>`;
  const progress = percentageMeter(billing.usage_percent, "已用金额比例");
  const meter = progress ? `<div class="quota-meter">${progress}<b>${escapeHTML(formatQuotaPercent(billing.usage_percent))}</b></div>` : "";
  const status = billing.status === "exhausted" || billing.status === "overage" ? "额度已用尽" : billing.status === "active" ? "额度可用" : "额度状态未知";
  return `<div class="billing-meter"><div class="billing-line"><strong>${moneyMarkup(billing.used_usd)}</strong><span>/ ${moneyMarkup(billing.limit_usd)}</span><em>${escapeHTML(status)}</em></div>${meter}<small>剩余 ${moneyMarkup(billing.remaining_usd)}${billing.status === "overage" ? ` · 超额 ${moneyMarkup(billing.overage_usd)}` : ""} · ${escapeHTML(formatTime(billing.period_to))}${billing.unknown_cost_events ? ` · ${formatNumber(billing.unknown_cost_events)} 条费用未知` : ""}</small></div>`;
}

function accountTable(items) {
  if (!items.length) return `<div class="empty">当前车辆还没有分配账号</div>`;
  return `<div class="table-wrap account-table"><table><thead><tr><th>账号</th><th>供应商</th><th>账号并发</th><th>CPA 原生配额</th><th>状态</th><th>观测时间</th><th class="numeric">请求</th><th class="numeric">Token</th><th class="numeric">未知</th></tr></thead><tbody>
    ${items.map(item => `<tr><td><strong>${escapeHTML(item.label || item.safe_label)}</strong></td><td>${escapeHTML(item.provider)}</td><td>${concurrencyMarkup(item.concurrency_limit)}</td><td>${quotaMarkup(item.quota)}</td><td>${statusLabel(item.stale ? "unknown" : item.status)}${item.stale ? ` <span class="tag">健康数据陈旧</span>` : ""}</td><td>${escapeHTML(formatTime(item.observed_at))}</td><td class="numeric">${formatNumber(item.usage?.logical_requests)}</td><td class="numeric">${formatNumber(item.usage?.known_total_tokens)}</td><td class="numeric">${formatNumber(item.usage?.unknown_usage_events)}</td></tr>`).join("")}
  </tbody></table></div>`;
}

function formatQuotaPercent(value) {
  if (!Number.isFinite(value)) return "";
  return `${value.toLocaleString("zh-CN", { maximumFractionDigits: 1 })}%`;
}

function percentageMeter(percent, label) {
  if (!Number.isFinite(percent)) return "";
  const value = Math.max(0, Math.min(100, percent));
  return `<progress class="quota-meter-track" role="progressbar" aria-label="${escapeHTML(label)}" aria-valuemin="0" aria-valuemax="100" aria-valuenow="${value}" aria-valuetext="${escapeHTML(formatQuotaPercent(percent))}" max="100" value="${value}">${escapeHTML(formatQuotaPercent(percent))}</progress>`;
}

function quotaProgress(window) {
  const percent = window.used_percent;
  const meter = percentageMeter(percent, `${window.label || window.id} 已用比例`);
  if (!meter) return "";
  return `<div class="quota-meter quota-meter-overlay">${meter}<b>${escapeHTML(formatQuotaPercent(percent))}</b></div>`;
}

function quotaWindowMarkup(window, label) {
  if (!window) {
    return `<div class="quota-window quota-window-empty"><div class="quota-window-head"><strong>${escapeHTML(label)}</strong></div><div class="quota-meter quota-meter-overlay">${percentageMeter(0, `${label} 0%`)}<b>0%</b></div></div>`;
  }
  const displayLabel = label || window.label || window.id || "配额窗口";
  const status = window.status && window.status !== "observed" ? statusText(window.status) : "已观测";
  return `<div class="quota-window"><div class="quota-window-head"><strong>${escapeHTML(displayLabel)}</strong><span>${escapeHTML(status)}</span></div>${quotaProgress({...window, label: displayLabel}) || `<div class="quota-status-only">${escapeHTML(status)}</div>`}<small>${window.reset_at ? `重置：${escapeHTML(formatTime(window.reset_at))}` : window.window_minutes ? `窗口：${formatNumber(window.window_minutes)} 分钟` : "重置时间：上游未提供"}</small></div>`;
}

function quotaWindowByKind(windows, kind) {
  const exactIDs = kind === "5h" ? ["5h", "primary"] : ["7d", "secondary"];
  const minutes = kind === "5h" ? 300 : 10080;
  return windows.find(window => exactIDs.includes(String(window?.id || "").toLowerCase())) ||
    windows.find(window => Number(window?.window_minutes) === minutes) ||
    windows.find(window => String(window?.id || "").toLowerCase().endsWith(`-${kind === "5h" ? "primary" : "secondary"}`)) || null;
}

function quotaCompactWindowMarkup(window, label) {
  const percent = Number.isFinite(window?.used_percent) ? window.used_percent : 0;
  return `<div class="quota-window quota-window-compact"><strong>${escapeHTML(label)}</strong><div class="quota-meter quota-meter-compact">${percentageMeter(percent, `${label} 已用比例`)}<b>${escapeHTML(formatQuotaPercent(percent))}</b></div></div>`;
}

function quotaMarkup(quota) {
  const windows = quota?.supported && Array.isArray(quota.windows) ? quota.windows : [];
  const fiveHour = quotaWindowByKind(windows, "5h");
  const sevenDay = quotaWindowByKind(windows, "7d");
  return `<div class="quota-stack quota-stack-compact">${quotaCompactWindowMarkup(fiveHour, "5h")}${quotaCompactWindowMarkup(sevenDay, "7d")}</div>`;
}

function apiKeyValueMarkup(item) {
  if (!item.api_key) return `<span class="muted">旧 Key 不可查看，请重新创建</span>`;
  return `<span class="api-key-value"><code>${escapeHTML(item.api_key)}</code><button class="copy-button" type="button" data-copy-api-key="${escapeHTML(item.key_ref)}" aria-label="复制 ${escapeHTML(item.name)} 的 API Key" title="复制 API Key"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect x="8" y="8" width="11" height="11" rx="2"/><path d="M16 8V6a2 2 0 0 0-2-2H6a2 2 0 0 0-2 2v8a2 2 0 0 0 2 2h2"/></svg></button></span>`;
}

async function copyAPIKey(value) {
  if (navigator.clipboard?.writeText) {
    await navigator.clipboard.writeText(value);
    return;
  }
  const helper = document.createElement("textarea");
  helper.className = "clipboard-helper";
  helper.value = value;
  helper.setAttribute("readonly", "");
  document.body.append(helper);
  helper.select();
  const copied = document.execCommand("copy");
  helper.remove();
  if (!copied) throw new Error("无法访问剪贴板，请手动选择 Key 复制");
}

async function renderKeys(content, reset) {
  const page = await loadPage("keys", searchPath("keys", "/me/api-keys"), reset);
  const items = page.items;
  content.innerHTML = `<div class="section-header"><div><h2>API Key</h2><p>完整 Key 可在列表中复制，请仅提供给受信任的客户端。</p></div><button class="button" id="create-key">创建 Key</button></div>
    ${searchBar("keys", "按名称搜索")}
    ${items.length ? `<div class="table-wrap"><table><thead><tr><th>名称</th><th>Key</th><th>状态</th><th>创建时间</th><th>过期时间</th><th>最近使用</th><th></th></tr></thead><tbody>${items.map(item => `<tr><td>${escapeHTML(item.name)}</td><td>${apiKeyValueMarkup(item)}</td><td>${statusLabel(item.status)}</td><td>${escapeHTML(formatTime(item.created_at))}</td><td>${escapeHTML(formatTime(item.expires_at))}</td><td>${escapeHTML(formatTime(item.last_used_at))}</td><td>${item.status === "active" ? `<button class="button danger compact" data-revoke-key="${escapeHTML(item.key_ref)}">撤销</button>` : ""}</td></tr>`).join("")}</tbody></table></div>` : `<div class="empty">${state.searches.keys ? "没有匹配的 API Key" : "尚未创建 API Key"}</div>`}
    ${paginationFooter(page, "个 Key")}`;
  content.querySelector("#create-key").addEventListener("click", showKeyDialog);
  content.querySelectorAll("[data-copy-api-key]").forEach(button => button.addEventListener("click", async () => {
    const item = items.find(candidate => candidate.key_ref === button.dataset.copyApiKey);
    if (!item?.api_key) return;
    try {
      await copyAPIKey(item.api_key);
      toast("API Key 已复制");
    } catch (error) {
      toast(error.message);
    }
  }));
  bindSearch(content, "keys", async reset => { try { await renderKeys(content, reset); } catch (error) { toast(error.message); } });
  content.querySelector("[data-load-more]")?.addEventListener("click", async event => {
    setButtonBusy(event.currentTarget, true);
    try { await renderKeys(content, false); } catch (error) { toast(error.message); }
  });
  content.querySelectorAll("[data-revoke-key]").forEach(button => button.addEventListener("click", async () => {
    if (!confirm("撤销后使用该 Key 的客户端会立即失效，是否继续？")) return;
    setButtonBusy(button, true);
    try {
      await request(`/me/api-keys/${encodeURIComponent(button.dataset.revokeKey)}`, { method: "DELETE" });
      toast("API Key 已撤销");
      await renderKeys(content, true);
    } catch (error) {
      toast(error.message);
      setButtonBusy(button, false);
    }
  }));
}

function showKeyDialog() {
  const dialog = openDialog("创建 API Key", `<form id="key-form">
    <div class="field"><label for="key-name">名称</label><input id="key-name" name="name" required maxlength="64" placeholder="例如：笔记本电脑"></div>
    <div class="field"><label for="key-expiry">过期时间（可选）</label><input id="key-expiry" name="expires_at" type="datetime-local"></div>
    <div class="form-actions"><button class="button secondary" type="button" data-dialog-close>取消</button><button class="button" type="submit">创建</button></div>
  </form>`);
  dialog.querySelector("form").addEventListener("submit", async event => {
    event.preventDefault();
    const button = event.currentTarget.querySelector("button[type=submit]");
    const form = new FormData(event.currentTarget);
    const expiresAt = form.get("expires_at");
    setButtonBusy(button, true);
    try {
      await request("/me/api-keys", { method: "POST", body: JSON.stringify({ name: form.get("name"), expires_at: expiresAt ? new Date(expiresAt).toISOString() : null }) });
      dialog.close();
      toast("API Key 已创建");
      renderRoute();
    } catch (error) {
      toast(error.message);
      setButtonBusy(button, false);
    }
  });
}

async function renderAdminRoute(content) {
  if (state.route === "/") {
    const [users, cars] = await Promise.all([request("/admin/users?limit=1"), request("/admin/cars?limit=6")]);
    const carItems = cars.items || [];
    const count = value => value == null ? "未提供" : formatNumber(value);
    content.innerHTML = `<div class="section-header"><div><h2>运营概览</h2><p>从车辆和成员开始，管理当前拼车业务。</p></div><a class="button" href="#/cars">管理车辆</a></div>
      <div class="dashboard-layout"><div class="dashboard-main">
        <section class="workspace-panel"><div class="panel-heading"><h3>运营摘要</h3></div><dl class="overview-stats"><div><dt>用户总数</dt><dd>${count(users.total)}</dd></div><div><dt>车辆总数</dt><dd>${count(cars.total)}</dd></div><div><dt>模块状态</dt><dd>${statusLabel(state.session?.module_status || "unknown")}</dd></div></dl></section>
        <section class="workspace-panel"><div class="panel-heading"><div><h3>车辆一览</h3><p class="muted">显示 ${formatNumber(carItems.length)} 辆车辆${cars.total != null ? `，共 ${count(cars.total)} 辆` : ""}</p></div><a href="#/cars">查看全部</a></div>
          ${carItems.length ? `<ul class="activity-list">${carItems.map(car => `<li><div><strong>${escapeHTML(car.name)}</strong><span>${formatNumber(car.member_count)} 名成员 · ${formatNumber(car.account_count)} 个账号 · 席位 ${car.seat_limit ? formatNumber(car.seat_limit) : "不限"}</span></div>${statusLabel(car.status)}</li>`).join("")}</ul>` : `<div class="empty"><h3>尚未创建车辆</h3><p>进入车辆管理创建车辆，再分配成员与上游账号。</p></div>`}
        </section>
      </div><aside class="dashboard-side">
        <section class="workspace-panel"><div class="panel-heading"><h3>工作入口</h3></div><div class="quick-links"><a href="#/users">${iconMarkup("/users")}<span>管理用户与密钥</span></a><a href="#/usage">${iconMarkup("/usage")}<span>查看用量报表</span></a><a href="#/requests">${iconMarkup("/requests")}<span>追溯请求明细</span></a></div></section>
        <section class="workspace-panel"><div class="panel-heading"><h3>统计说明</h3></div><ul class="activity-list"><li><strong>报表时区</strong><span>${escapeHTML(state.session?.report_timezone || "UTC")}</span></li><li><strong>费用与用量</strong><span>费用未知和不完整请求会单独标记，不计作零消费。</span></li></ul><div class="quick-links"><a href="#/pricing">价格目录</a><a href="#/retention">数据保留</a><a href="#/audit">审计记录</a></div></section>
      </aside></div>`;
    return;
  }
  if (state.route === "/users") return renderUsers(content, true);
  if (state.route === "/admin-accounts") return renderAdminAccounts(content);
  if (state.route === "/cars") return renderCars(content, true);
  if (state.route === "/usage") return renderAdminUsage(content);
  if (state.route === "/requests") return renderAdminRequests(content);
  if (state.route === "/pricing") return renderPricing(content);
  if (state.route === "/retention") return renderRetention(content);
  if (state.route === "/audit") return renderAudit(content, true);
}

async function renderPricing(content) {
  const status = await request("/admin/pricing");
  const loaded = formatTime(status.loaded_at);
  const checked = formatTime(status.last_checked_at);
  const success = formatTime(status.last_success_at);
  content.innerHTML = `<div class="section-header"><div><h2>价格目录</h2><p>乘客金额按已保存的 USD 价格快照计算；目录更新不会改写历史事件。</p></div>${status.failure_reason ? `<span class="status degraded">最近检查失败</span>` : `<span class="status healthy">目录可用</span>`}</div><section class="pricing-panel"><div class="pricing-grid"><div><span class="eyebrow">当前版本</span><code>${escapeHTML(status.hash || "未加载")}</code></div><div><span class="eyebrow">来源</span><strong>${escapeHTML(status.source || "未提供")}</strong></div><div><span class="eyebrow">载入时间</span><strong>${escapeHTML(loaded)}</strong></div><div><span class="eyebrow">最近成功</span><strong>${escapeHTML(success)}</strong></div><div><span class="eyebrow">最近检查</span><strong>${escapeHTML(checked)}</strong></div><div><span class="eyebrow">远程地址</span><span class="code-ref">${escapeHTML(status.catalog_url || "未配置")}</span></div></div>${status.failure_reason ? `<div class="warning-banner">最近一次远程检查未更新目录：${escapeHTML(status.failure_reason)}。当前仍使用最后一份有效快照。</div>` : `<p class="muted">后台每天检查一次远程目录；离线时继续使用内置或最后有效快照。</p>`}</section>`;
}

async function renderUsers(content, reset) {
  const page = await loadPage("users", searchPath("users", "/admin/users"), reset);
  const items = page.items;
  closeEntityPanels(content);
  content.innerHTML = `<div class="section-header"><div><h2>用户管理</h2><p>共 ${formatNumber(page.total)} 名用户 · 选择用户后在右侧管理</p></div><button class="button" id="create-user">创建用户</button></div>
    <div class="entity-workspace"><section class="entity-list workspace-panel" aria-label="用户列表">
      ${searchBar("users", "按用户名、展示名或引用搜索")}
      ${items.length ? `<div class="table-wrap"><table><thead><tr><th>用户</th><th>状态</th><th></th></tr></thead><tbody>${items.map(item => `<tr><td><div class="entity-title"><strong>${escapeHTML(item.username)}</strong>${item.display_name && item.display_name !== item.username ? `<span class="muted">${escapeHTML(item.display_name)}</span>` : ""}</div><div class="entity-meta"><span>${item.role === "carpool_admin" ? "管理员" : "乘客"}</span><span>创建于 ${escapeHTML(formatTime(item.created_at))}</span></div></td><td class="entity-status">${statusLabel(item.status)}</td><td><button class="button secondary compact" data-manage-user="${escapeHTML(item.user_ref)}" aria-label="管理用户 ${escapeHTML(item.username)}">管理</button></td></tr>`).join("")}</tbody></table></div>` : `<div class="empty">${state.searches.users ? "没有匹配的用户" : "暂无用户，请先创建用户。"}</div>`}
      ${paginationFooter(page, "名用户")}</section>${entityEmptyMarkup("用户")}</div>`;
  content.querySelector("#create-user").addEventListener("click", showUserDialog);
  bindSearch(content, "users", async reset => { try { await renderUsers(content, reset); } catch (error) { toast(error.message); } });
  content.querySelector("[data-load-more]")?.addEventListener("click", async event => {
    setButtonBusy(event.currentTarget, true);
    try { await renderUsers(content, false); } catch (error) { toast(error.message); }
  });
  content.querySelectorAll("[data-manage-user]").forEach(button => button.addEventListener("click", () => {
    const user = items.find(item => item.user_ref === button.dataset.manageUser);
    if (user) {
      showUserManagement(user).catch(error => toast(error.message));
      selectEntityRow(content, button);
    }
  }));
}

function showUserDialog() {
  const dialog = openDialog("创建用户", `<form id="user-form">
    <div class="form-row"><div class="field"><label for="new-username">用户名</label><input id="new-username" name="username" required minlength="3" maxlength="64"></div><div class="field"><label for="new-display-name">默认展示名</label><input id="new-display-name" name="display_name" required maxlength="64"></div></div>
    <div class="field"><label for="new-role">角色</label><select id="new-role" name="role"><option value="passenger">乘客</option><option value="carpool_admin">拼车管理员</option></select></div>
    <div class="form-actions"><button class="button secondary" type="button" data-dialog-close>取消</button><button class="button" type="submit">创建</button></div>
  </form>`);
  dialog.querySelector("form").addEventListener("submit", async event => {
    event.preventDefault();
    const button = event.currentTarget.querySelector("button[type=submit]");
    const form = new FormData(event.currentTarget);
    setButtonBusy(button, true);
    try {
      const data = await request("/admin/users", { method: "POST", body: JSON.stringify({ username: form.get("username"), display_name: form.get("display_name"), role: form.get("role") }) });
      showOneTimeSecret(dialog, "用户已创建", data.temporary_password, "请通过安全渠道把临时密码交给用户。", renderRoute);
    } catch (error) {
      toast(error.message);
      setButtonBusy(button, false);
    }
  });
}

async function showUserManagement(initialUser) {
  let user = initialUser;
  let keyPage = emptyPage();

  async function loadKeys(reset) {
    if (user.role !== "passenger") {
      keyPage = emptyPage();
      keyPage.loaded = true;
      return;
    }
    const params = new URLSearchParams({ limit: String(pageLimit) });
    if (!reset && keyPage.nextCursor) params.set("cursor", keyPage.nextCursor);
    const data = await request(`/admin/users/${encodeURIComponent(user.user_ref)}/api-keys?${params}`);
    keyPage.items = reset ? (data.items || []) : keyPage.items.concat(data.items || []);
    keyPage.total = Number(data.total || keyPage.items.length);
    keyPage.nextCursor = data.next_cursor || "";
    keyPage.loaded = true;
  }

  const dialog = openEntityPanel("管理用户", `<div class="loading" role="status">正在加载用户...</div>`);
  try { await loadKeys(true); } catch (error) { dialog.close(); throw error; }
  if (!dialog.isConnected || !dialog.open) return;

  function paint() {
    if (!dialog.isConnected || !dialog.open) return;
    const keyRows = keyPage.items.map(key => `<tr><td>${escapeHTML(key.name)}</td><td class="code-ref">${escapeHTML(key.key_ref)}</td><td>${statusLabel(key.status)}</td><td>${escapeHTML(formatTime(key.expires_at))}</td><td>${escapeHTML(formatTime(key.last_used_at))}</td><td>${key.status === "active" ? `<button class="button danger compact" data-admin-revoke-key="${escapeHTML(key.key_ref)}">撤销</button>` : ""}</td></tr>`).join("");
    setDialogContent(dialog, "管理用户", `<div class="entity-summary"><div class="entity-summary-head"><strong>${escapeHTML(user.username)}</strong>${statusLabel(user.status)}</div><span>${user.role === "carpool_admin" ? "拼车管理员" : "乘客"}${user.display_name && user.display_name !== user.username ? ` · ${escapeHTML(user.display_name)}` : ""} · <span class="code-ref">${escapeHTML(user.user_ref)}</span></span></div>
      <form id="edit-user-form">
        <div class="form-row"><div class="field"><label for="edit-display-name">默认展示名</label><input id="edit-display-name" name="default_display_name" value="${escapeHTML(user.display_name)}" required maxlength="64"></div><div class="field"><label for="edit-user-status">状态</label><select id="edit-user-status" name="status"><option value="active"${user.status === "active" ? " selected" : ""}>启用</option><option value="disabled"${user.status === "disabled" ? " selected" : ""}>禁用</option></select></div></div>
        <div class="form-actions"><button class="button secondary" type="button" data-reset-password>重置密码</button><button class="button" type="submit">保存用户</button></div>
      </form>
      <div class="subsection-head"><h3>API Key<span class="subsection-count">${formatNumber(keyPage.total)}</span></h3>${user.role === "passenger" ? `<button class="button danger compact" type="button" data-revoke-all-keys>撤销全部</button>` : ""}</div>
      ${keyRows ? `<div class="table-wrap"><table><thead><tr><th>名称</th><th>Key 引用</th><th>状态</th><th>过期时间</th><th>最近使用</th><th></th></tr></thead><tbody>${keyRows}</tbody></table></div>` : `<div class="empty compact-empty">该用户没有 API Key</div>`}
      ${paginationFooter(keyPage, "个 Key")}`);

    dialog.querySelector("#edit-user-form").addEventListener("submit", async event => {
      event.preventDefault();
      const form = new FormData(event.currentTarget);
      const button = event.currentTarget.querySelector("button[type=submit]");
      setButtonBusy(button, true);
      try {
        user = await request(`/admin/users/${encodeURIComponent(user.user_ref)}`, { method: "PATCH", body: JSON.stringify({ default_display_name: form.get("default_display_name"), status: form.get("status") }) });
        toast("用户已更新");
        paint();
      } catch (error) {
        toast(error.message);
        setButtonBusy(button, false);
      }
    });

    dialog.querySelector("[data-reset-password]").addEventListener("click", async event => {
      if (!confirm("重置密码会撤销该用户全部网页登录会话，是否继续？")) return;
      setButtonBusy(event.currentTarget, true);
      try {
        const data = await request(`/admin/users/${encodeURIComponent(user.user_ref)}/reset-password`, { method: "POST" });
        showOneTimeSecret(dialog, "密码已重置", data.temporary_password, "临时密码只显示一次，请通过安全渠道交给用户。", renderRoute);
      } catch (error) {
        toast(error.message);
        setButtonBusy(event.currentTarget, false);
      }
    });

    dialog.querySelector("[data-revoke-all-keys]")?.addEventListener("click", async event => {
      if (!confirm("该用户的全部 API Key 都会立即失效，是否继续？")) return;
      setButtonBusy(event.currentTarget, true);
      try {
        await request(`/admin/users/${encodeURIComponent(user.user_ref)}/api-keys`, { method: "DELETE" });
        await loadKeys(true);
        toast("全部 API Key 已撤销");
        paint();
      } catch (error) {
        toast(error.message);
        setButtonBusy(event.currentTarget, false);
      }
    });

    dialog.querySelectorAll("[data-admin-revoke-key]").forEach(button => button.addEventListener("click", async () => {
      if (!confirm("撤销后该 Key 会立即失效，是否继续？")) return;
      setButtonBusy(button, true);
      try {
        await request(`/admin/users/${encodeURIComponent(user.user_ref)}/api-keys/${encodeURIComponent(button.dataset.adminRevokeKey)}`, { method: "DELETE" });
        await loadKeys(true);
        toast("API Key 已撤销");
        paint();
      } catch (error) {
        toast(error.message);
        setButtonBusy(button, false);
      }
    }));

    dialog.querySelector("[data-load-more]")?.addEventListener("click", async event => {
      setButtonBusy(event.currentTarget, true);
      try { await loadKeys(false); paint(); } catch (error) { toast(error.message); setButtonBusy(event.currentTarget, false); }
    });
  }

  paint();
}

async function renderCars(content, reset) {
  const page = await loadPage("cars", searchPath("cars", "/admin/cars"), reset);
  const items = page.items;
  closeEntityPanels(content);
  content.innerHTML = `<div class="section-header"><div><h2>车辆管理</h2><p>共 ${formatNumber(page.total)} 辆车辆 · 选择车辆后在右侧管理</p></div><button class="button" id="create-car">创建车辆</button></div>
    <div class="entity-workspace"><section class="entity-list workspace-panel" aria-label="车辆列表">
      ${searchBar("cars", "按名称、说明或引用搜索")}
      ${items.length ? `<div class="table-wrap"><table><thead><tr><th>车辆</th><th>状态</th><th></th></tr></thead><tbody>${items.map(item => `<tr><td><div class="entity-title"><strong>${escapeHTML(item.name)}</strong>${item.description ? `<span class="muted">${escapeHTML(item.description)}</span>` : ""}</div><div class="entity-meta"><span>${formatNumber(item.member_count)} 名成员</span><span>${formatNumber(item.account_count)} 个账号</span><span>席位 ${item.seat_limit ? formatNumber(item.seat_limit) : "不限"}</span></div></td><td class="entity-status">${statusLabel(item.status)}</td><td><button class="button secondary compact" data-manage-car="${escapeHTML(item.car_ref)}" aria-label="管理车辆 ${escapeHTML(item.name)}">管理</button></td></tr>`).join("")}</tbody></table></div>` : `<div class="empty">${state.searches.cars ? "没有匹配的车辆" : "暂无车辆，请先创建车辆。"}</div>`}
      ${paginationFooter(page, "辆车辆")}</section>${entityEmptyMarkup("车辆")}</div>`;
  content.querySelector("#create-car").addEventListener("click", showCarDialog);
  bindSearch(content, "cars", async reset => { try { await renderCars(content, reset); } catch (error) { toast(error.message); } });
  content.querySelector("[data-load-more]")?.addEventListener("click", async event => {
    setButtonBusy(event.currentTarget, true);
    try { await renderCars(content, false); } catch (error) { toast(error.message); }
  });
  content.querySelectorAll("[data-manage-car]").forEach(button => button.addEventListener("click", () => {
    const car = items.find(item => item.car_ref === button.dataset.manageCar);
    if (car) {
      showCarManagement(car).catch(error => toast(error.message));
      selectEntityRow(content, button);
    }
  }));
}

function showCarDialog() {
  const dialog = openDialog("创建车辆", `<form id="car-form">
    <div class="field"><label for="car-name">名称</label><input id="car-name" name="name" required maxlength="80"></div>
    <div class="field"><label for="car-description">说明</label><textarea id="car-description" name="description" maxlength="500"></textarea></div>
    <div class="field"><label for="seat-limit">席位上限（留空表示不限）</label><input id="seat-limit" name="seat_limit" type="number" min="1"></div>
    <div class="form-actions"><button class="button secondary" type="button" data-dialog-close>取消</button><button class="button" type="submit">创建</button></div>
  </form>`);
  dialog.querySelector("form").addEventListener("submit", async event => {
    event.preventDefault();
    const button = event.currentTarget.querySelector("button[type=submit]");
    const form = new FormData(event.currentTarget);
    setButtonBusy(button, true);
    try {
      await request("/admin/cars", { method: "POST", body: JSON.stringify({ name: form.get("name"), description: form.get("description"), seat_limit: form.get("seat_limit") ? Number(form.get("seat_limit")) : null }) });
      dialog.close();
      toast("车辆已创建");
      renderRoute();
    } catch (error) {
      toast(error.message);
      setButtonBusy(button, false);
    }
  });
}

async function showCarManagement(car) {
  const dialog = openEntityPanel("管理车辆", `<div class="loading" role="status">正在加载车辆...</div>`);
  const panelIsCurrent = () => dialog.isConnected && dialog.open;
  const [members, accounts, users] = await Promise.all([
    request(`/admin/cars/${encodeURIComponent(car.car_ref)}/members`),
    request(`/admin/cars/${encodeURIComponent(car.car_ref)}/accounts`),
    fetchAllPages("/admin/users"),
  ]).catch(error => {
    if (!panelIsCurrent()) return [];
    dialog.close();
    throw error;
  });
  if (!panelIsCurrent()) return;
  const passengers = users.filter(user => user.role === "passenger" && user.status === "active");
  const candidates = (accounts.candidates || []).filter(candidate => !candidate.assigned_to_current_car);
  const passengerOptions = passengers.map(user => `<option value="${escapeHTML(user.user_ref)}" data-display-name="${escapeHTML(user.display_name)}">${escapeHTML(user.display_name)} · ${escapeHTML(user.username)}</option>`).join("");
  const candidateOptions = candidates.map(candidate => {
    const assignment = candidate.assigned ? "（将从其他车辆移入）" : "";
    return `<option value="${escapeHTML(candidate.candidate_ref)}">${escapeHTML(candidate.provider)} · ${escapeHTML(candidate.candidate_ref.slice(-8))} ${assignment}</option>`;
  }).join("");
  setDialogContent(dialog, "管理车辆", `<div class="entity-summary"><div class="entity-summary-head"><strong>${escapeHTML(car.name)}</strong>${statusLabel(car.status)}</div><span class="code-ref">${escapeHTML(car.car_ref)}</span></div>
    <form id="edit-car-form" class="entity-form">
      <div class="form-row-4"><div class="field"><label for="edit-car-name">名称</label><input id="edit-car-name" name="name" value="${escapeHTML(car.name)}" required maxlength="80"></div><div class="field"><label for="edit-car-status">状态</label><select id="edit-car-status" name="status"><option value="active"${car.status === "active" ? " selected" : ""}>启用</option><option value="disabled"${car.status === "disabled" ? " selected" : ""}>禁用</option><option value="retired"${car.status === "retired" ? " selected" : ""}>退役</option></select></div><div class="field"><label for="edit-seat-limit">席位上限</label><input id="edit-seat-limit" name="seat_limit" type="number" min="1" placeholder="不限" value="${car.seat_limit || ""}"></div><div class="field"><label for="edit-car-description">说明</label><input id="edit-car-description" name="description" maxlength="500" value="${escapeHTML(car.description || "")}"></div></div>
      <div class="form-actions"><button class="button" type="submit">保存车辆</button></div>
    </form>
    <div class="subsection-head"><h3>成员<span class="subsection-count">${formatNumber(members.total)}</span></h3></div>
    ${(members.items || []).length ? `<div class="compact-list member-policy-list">${members.items.map(item => `<div><div><strong>${escapeHTML(item.display_name)}</strong><div class="compact-meta">用户并发：${concurrencyMarkup(item.concurrency_limit)}<br>上车：${escapeHTML(formatTime(item.started_at))}</div></div><div class="compact-meta member-policy">${memberQuotaRow(item)}</div><span class="inline-actions"><button class="button secondary compact" data-edit-member-limit="${escapeHTML(item.member_ref)}" data-member-name="${escapeHTML(item.display_name)}" aria-label="编辑 ${escapeHTML(item.display_name)} 的额度与并发">编辑</button><button class="button danger compact" data-remove-member="${escapeHTML(item.member_ref)}">移除</button></span></div>`).join("")}</div>` : `<div class="empty compact-empty">暂无成员</div>`}
    ${passengerOptions && car.status !== "retired" ? `<details class="add-form"><summary>+ 添加成员</summary><form id="add-member" class="inline-editor"><div class="field"><label for="member-user">乘客</label><select id="member-user" name="user_ref" required><option value="">选择乘客</option>${passengerOptions}</select></div><div class="field"><label for="member-display-name">车内展示名</label><input id="member-display-name" name="display_name" required maxlength="64"></div><div class="form-row"><div class="field"><label for="member-five-hour-new">5 小时额度（USD）</label><input id="member-five-hour-new" name="five_hour_limit_usd" inputmode="decimal" pattern="[0-9]+(\\.[0-9]{1,9})?" value="0" required></div><div class="field"><label for="member-weekly-new">7 天额度（USD）</label><input id="member-weekly-new" name="weekly_limit_usd" inputmode="decimal" pattern="[0-9]+(\\.[0-9]{1,9})?" value="0" required></div></div><small>0 表示不限；金额最多 9 位小数。</small><button class="button compact" type="submit">加入或换入</button></form></details>` : `<p class="muted section-note">${car.status === "retired" ? "退役车辆不能接收新成员" : "没有可分配的启用乘客"}</p>`}
    <div class="subsection-head"><h3>账号<span class="subsection-count">${formatNumber(accounts.total)}</span></h3></div>
    ${(accounts.items || []).length ? `<div class="compact-list">${accounts.items.map(item => `<div><strong>${escapeHTML(item.safe_label)}</strong><span class="compact-meta">${escapeHTML(item.provider)} · 并发 ${concurrencyMarkup(item.concurrency_limit)}</span><span class="compact-meta code-ref">${escapeHTML(item.account_ref)}</span><span class="inline-actions"><button class="button secondary compact" data-edit-account-concurrency="${escapeHTML(item.account_ref)}" aria-label="设置 ${escapeHTML(item.safe_label)} 的账号并发">设置并发</button><button class="button danger compact" data-remove-account="${escapeHTML(item.account_ref)}">撤销</button></span></div>`).join("")}</div>` : `<div class="empty compact-empty">暂无账号</div>`}
    ${candidateOptions && car.status !== "retired" ? `<details class="add-form"><summary>+ 添加账号</summary><form id="add-account" class="inline-editor"><div class="field"><label for="account-candidate">上游账号</label><select id="account-candidate" name="candidate_ref" required><option value="">选择账号</option>${candidateOptions}</select></div><div class="field"><label for="account-label">安全标签</label><input id="account-label" name="safe_label" required maxlength="80"></div><button class="button compact" type="submit">分配或移入</button></form></details>` : `<p class="muted section-note">${car.status === "retired" ? "退役车辆不能接收新账号" : "当前没有可分配的运行时账号"}</p>`}`);

  dialog.querySelector("#member-user")?.addEventListener("change", event => {
    const option = event.currentTarget.selectedOptions[0];
    const display = dialog.querySelector("#member-display-name");
    if (display && option?.dataset.displayName) display.value = option.dataset.displayName;
  });

  dialog.querySelector("#edit-car-form").addEventListener("submit", async event => {
    event.preventDefault();
    const button = event.currentTarget.querySelector("button[type=submit]");
    const form = new FormData(event.currentTarget);
    const nextStatus = form.get("status");
    if (nextStatus === "retired" && car.status !== "retired" && !confirm("车辆退役后不能接收新成员或账号，是否继续？")) return;
    setButtonBusy(button, true);
    try {
      await request(`/admin/cars/${encodeURIComponent(car.car_ref)}`, { method: "PATCH", body: JSON.stringify({ name: form.get("name"), description: form.get("description"), seat_limit: form.get("seat_limit") ? Number(form.get("seat_limit")) : null, status: nextStatus }) });
      if (!panelIsCurrent()) return;
      dialog.close();
      toast("车辆已更新");
      renderRoute();
    } catch (error) {
      if (!panelIsCurrent()) return;
      toast(error.message);
      setButtonBusy(button, false);
    }
  });

  dialog.querySelector("#add-member")?.addEventListener("submit", async event => {
    event.preventDefault();
    const button = event.currentTarget.querySelector("button[type=submit]");
    const form = new FormData(event.currentTarget);
    setButtonBusy(button, true);
    try {
      await request(`/admin/cars/${encodeURIComponent(car.car_ref)}/members`, { method: "POST", body: JSON.stringify({ user_ref: form.get("user_ref"), display_name: form.get("display_name"), five_hour_limit_usd: String(form.get("five_hour_limit_usd") || "0").trim(), weekly_limit_usd: String(form.get("weekly_limit_usd") || "0").trim() }) });
      if (!panelIsCurrent()) return;
      dialog.close();
      toast("成员已加入或换入");
      renderRoute();
    } catch (error) {
      if (!panelIsCurrent()) return;
      toast(error.message);
      setButtonBusy(button, false);
    }
  });

  const policySaved = () => {
    toast("设置已保存；不终止已开始的请求");
    if (!panelIsCurrent()) return;
    dialog.close();
    renderRoute();
  };
  dialog.querySelectorAll("[data-edit-member-limit]").forEach(button => button.addEventListener("click", () => {
    const member = (members.items || []).find(item => item.member_ref === button.dataset.editMemberLimit);
    if (!member) return;
    const editor = openDialog("调整额度与用户并发", `<form id="member-quota-form"><p class="muted">${escapeHTML(member.display_name)} · 修改后立即应用于新请求。调低额度到已用金额以下会拒绝新请求；不终止在途请求。</p>
      <div class="form-row"><div class="field"><label for="member-five-hour">5 小时额度（USD）</label><input id="member-five-hour" name="five_hour_limit_usd" inputmode="decimal" pattern="[0-9]+(\\.[0-9]{1,9})?" value="${escapeHTML(member.five_hour_limit_usd ?? "0")}" required aria-describedby="member-window-help"></div><div class="field"><label for="member-weekly">7 天额度（USD）</label><input id="member-weekly" name="weekly_limit_usd" inputmode="decimal" pattern="[0-9]+(\\.[0-9]{1,9})?" value="${escapeHTML(member.weekly_limit_usd ?? "0")}" required aria-describedby="member-window-help"></div></div>
      <p class="muted section-note" id="member-window-help">0 表示不限；5 小时与 7 天为本地滚动额度，金额最多 9 位小数。</p>
      <div class="field"><label for="member-concurrency">用户并发上限（可选）</label><input id="member-concurrency" name="concurrency_limit" type="number" min="1" step="1" max="9007199254740991" value="${escapeHTML(member.concurrency_limit ?? "")}" placeholder="不限" aria-describedby="member-concurrency-help"><small id="member-concurrency-help">留空表示不限，仅接受正整数；此用户的全部 API Key 共享上限。</small></div><p class="muted">${concurrencyQueueHelp}</p>
      <div class="error-banner" data-policy-error role="alert" tabindex="-1" hidden></div><div class="form-actions"><button class="button secondary" type="button" data-dialog-close>取消</button><button class="button" type="submit">保存设置</button></div></form>`);
    bindPolicyForm(editor, `/admin/cars/${encodeURIComponent(car.car_ref)}/members/${encodeURIComponent(member.member_ref)}/quota`, form => memberQuotaPatch(form, member), policySaved);
  }));

  dialog.querySelectorAll("[data-edit-account-concurrency]").forEach(button => button.addEventListener("click", () => {
    const account = (accounts.items || []).find(item => item.account_ref === button.dataset.editAccountConcurrency);
    if (!account) return;
    const editor = openDialog("设置账号并发", `<form id="account-concurrency-form"><p class="muted">${escapeHTML(account.safe_label)} · 使用此账号的拼车用户共享此上限。调低上限不会中断在途请求。</p><div class="field"><label for="account-concurrency">账号并发上限（可选）</label><input id="account-concurrency" name="concurrency_limit" type="number" min="1" step="1" max="9007199254740991" value="${escapeHTML(account.concurrency_limit ?? "")}" placeholder="不限" aria-describedby="account-concurrency-help"><small id="account-concurrency-help">留空表示不限，仅接受正整数；不能填写 0。</small></div><p class="muted">${concurrencyQueueHelp}</p><div class="error-banner" data-policy-error role="alert" tabindex="-1" hidden></div><div class="form-actions"><button class="button secondary" type="button" data-dialog-close>取消</button><button class="button" type="submit">保存设置</button></div></form>`);
    bindPolicyForm(editor, `/admin/cars/${encodeURIComponent(car.car_ref)}/accounts/${encodeURIComponent(account.account_ref)}/concurrency`, form => {
      const value = optionalConcurrency(form.get("concurrency_limit"));
      return value === (account.concurrency_limit ?? null) ? {} : { concurrency_limit: value };
    }, policySaved);
  }));

  dialog.querySelector("#add-account")?.addEventListener("submit", async event => {
    event.preventDefault();
    const button = event.currentTarget.querySelector("button[type=submit]");
    const form = new FormData(event.currentTarget);
    setButtonBusy(button, true);
    try {
      await request(`/admin/cars/${encodeURIComponent(car.car_ref)}/accounts`, { method: "POST", body: JSON.stringify({ candidate_ref: form.get("candidate_ref"), safe_label: form.get("safe_label") }) });
      if (!panelIsCurrent()) return;
      dialog.close();
      toast("账号已分配或移入");
      renderRoute();
    } catch (error) {
      if (!panelIsCurrent()) return;
      toast(error.message);
      setButtonBusy(button, false);
    }
  });

  dialog.querySelectorAll("[data-remove-member]").forEach(button => button.addEventListener("click", async () => {
    if (!confirm("移除后该乘客的新请求将无法使用本车账号。本期已确认用量与在途请求不受影响、仍会计费。是否继续？")) return;
    setButtonBusy(button, true);
    try {
      await request(`/admin/cars/${encodeURIComponent(car.car_ref)}/members/${encodeURIComponent(button.dataset.removeMember)}`, { method: "DELETE" });
      if (!panelIsCurrent()) return;
      dialog.close();
      toast("成员已移除");
      renderRoute();
    } catch (error) {
      if (!panelIsCurrent()) return;
      toast(error.message);
      setButtonBusy(button, false);
    }
  }));

  dialog.querySelectorAll("[data-remove-account]").forEach(button => button.addEventListener("click", async () => {
    if (!confirm("撤销后该账号不会再参与本车的新请求。已产生用量与在途请求不受影响、仍会计费。是否继续？")) return;
    setButtonBusy(button, true);
    try {
      await request(`/admin/cars/${encodeURIComponent(car.car_ref)}/accounts/${encodeURIComponent(button.dataset.removeAccount)}`, { method: "DELETE" });
      if (!panelIsCurrent()) return;
      dialog.close();
      toast("账号分配已撤销");
      renderRoute();
    } catch (error) {
      if (!panelIsCurrent()) return;
      toast(error.message);
      setButtonBusy(button, false);
    }
  }));
}

async function reportOptions() {
  const [users, cars] = await Promise.all([fetchAllPages("/admin/users"), fetchAllPages("/admin/cars")]);
  const accountResponses = await Promise.all(cars.map(car => request(`/admin/cars/${encodeURIComponent(car.car_ref)}/accounts`)));
  const accounts = accountResponses.flatMap((response, index) => (response.items || []).map(account => ({ ...account, car_name: cars[index].name })));
  return { users, cars, accounts };
}

function adminReportPath() {
  const report = state.report;
  const params = new URLSearchParams({ group_by: report.groupBy });
  if (report.rangeMode === "custom") {
    params.set("from", new Date(report.from).toISOString());
    params.set("to", new Date(report.to).toISOString());
  } else {
    params.set("period", report.period);
  }
  if (report.carRef) params.set("car_ref", report.carRef);
  if (report.userRef) params.set("user_ref", report.userRef);
  if (report.accountRef) params.set("account_ref", report.accountRef);
  return `/admin/usage?${params}`;
}

function adminUsageTable(data) {
  const items = data.items || [];
  if (!items.length) return `<div class="empty">当前条件下暂无用量</div>`;
  const groupBy = data.group_by || state.report.groupBy;
  const heading = groupBy === "car" ? "车辆" : groupBy === "account" ? "账号" : "用户";
  return `<div class="table-wrap"><table><thead><tr><th>${heading}</th><th class="numeric">请求</th><th class="numeric">成功</th><th class="numeric">失败</th><th class="numeric">拒绝</th><th class="numeric">取消</th><th class="numeric">不完整</th><th class="numeric">Token</th><th class="numeric">未知</th></tr></thead><tbody>${items.map(item => {
    let label = item.display_name || item.name || item.safe_label || "未归属";
    let reference = item.user_ref || item.car_ref || item.account_ref || "";
    if (groupBy === "account" && item.provider) label += ` · ${item.provider}`;
    if (item.unattributed) { label = "未归属"; reference = ""; }
    return `<tr><td><strong>${escapeHTML(label)}</strong>${reference ? `<br><span class="muted code-ref">${escapeHTML(reference)}</span>` : ""}</td><td class="numeric">${formatNumber(item.logical_requests)}</td><td class="numeric">${formatNumber(item.succeeded)}</td><td class="numeric">${formatNumber(item.failed)}</td><td class="numeric">${formatNumber(item.rejected)}</td><td class="numeric">${formatNumber(item.canceled)}</td><td class="numeric">${formatNumber(item.incomplete)}</td><td class="numeric">${formatNumber(item.known_total_tokens)}</td><td class="numeric">${formatNumber(item.unknown_usage_events)}</td></tr>`;
  }).join("")}</tbody></table></div>`;
}

async function renderAdminUsage(content) {
  if (!state.report.from || !state.report.to) {
    const now = new Date();
    state.report.to = toDatetimeLocalValue(now);
    state.report.from = toDatetimeLocalValue(new Date(now.getTime() - 24 * 60 * 60 * 1000));
  }
  const [options, data] = await Promise.all([reportOptions(), request(adminReportPath())]);
  const report = state.report;
  content.innerHTML = `<form id="report-form" class="report-toolbar">
      <div class="field"><label for="report-range">时间范围</label><select id="report-range" name="range_mode"><option value="preset"${report.rangeMode === "preset" ? " selected" : ""}>预设周期</option><option value="custom"${report.rangeMode === "custom" ? " selected" : ""}>自定义区间（本地时间）</option></select></div>
      <div class="field" data-preset-range${report.rangeMode === "custom" ? " hidden" : ""}><label for="report-period">周期</label><select id="report-period" name="period"><option value="today"${report.period === "today" ? " selected" : ""}>今日</option><option value="7d"${report.period === "7d" ? " selected" : ""}>最近 7 天</option><option value="30d"${report.period === "30d" ? " selected" : ""}>最近 30 天</option></select></div>
      <div class="field" data-custom-range${report.rangeMode !== "custom" ? " hidden" : ""}><label for="report-from">开始（本地时间）</label><input id="report-from" name="from" type="datetime-local" value="${escapeHTML(report.from)}"></div>
      <div class="field" data-custom-range${report.rangeMode !== "custom" ? " hidden" : ""}><label for="report-to">结束（本地时间）</label><input id="report-to" name="to" type="datetime-local" value="${escapeHTML(report.to)}"></div>
      <div class="field"><label for="report-group">分组</label><select id="report-group" name="group_by"><option value="user"${report.groupBy === "user" ? " selected" : ""}>用户</option><option value="car"${report.groupBy === "car" ? " selected" : ""}>车辆</option><option value="account"${report.groupBy === "account" ? " selected" : ""}>账号</option></select></div>
      <div class="field"><label for="report-car">车辆筛选</label><select id="report-car" name="car_ref"><option value="">全部车辆</option>${options.cars.map(car => `<option value="${escapeHTML(car.car_ref)}"${report.carRef === car.car_ref ? " selected" : ""}>${escapeHTML(car.name)}</option>`).join("")}</select></div>
      <div class="field"><label for="report-user">用户筛选</label><select id="report-user" name="user_ref"><option value="">全部用户</option>${options.users.map(user => `<option value="${escapeHTML(user.user_ref)}"${report.userRef === user.user_ref ? " selected" : ""}>${escapeHTML(user.display_name)} · ${escapeHTML(user.username)}</option>`).join("")}</select></div>
      <div class="field"><label for="report-account">账号筛选</label><select id="report-account" name="account_ref"><option value="">全部账号</option>${options.accounts.map(account => `<option value="${escapeHTML(account.account_ref)}"${report.accountRef === account.account_ref ? " selected" : ""}>${escapeHTML(account.safe_label)} · ${escapeHTML(account.car_name)}</option>`).join("")}</select></div>
      <button class="button" type="submit">查询</button>
    </form>
    <div class="section-header report-heading"><div><h2>聚合用量</h2><p>${escapeHTML(formatTime(data.data_from))} 至 ${escapeHTML(formatTime(data.data_to))}</p></div><div class="report-meta">数据保留起点 ${escapeHTML(formatTime(data.retention_cutoff))}</div></div>
    ${adminUsageTable(data)}`;
  const rangeSelect = content.querySelector("#report-range");
  rangeSelect.addEventListener("change", () => {
    const custom = rangeSelect.value === "custom";
    content.querySelector("[data-preset-range]").hidden = custom;
    content.querySelectorAll("[data-custom-range]").forEach(element => { element.hidden = !custom; });
  });
  content.querySelector("#report-form").addEventListener("submit", async event => {
    event.preventDefault();
    const form = new FormData(event.currentTarget);
    const from = String(form.get("from") || "");
    const to = String(form.get("to") || "");
    if (form.get("range_mode") === "custom" && (!from || !to || new Date(from) >= new Date(to))) {
      toast("自定义时间范围必须包含有效且递增的开始与结束时间");
      return;
    }
    state.report = {
      rangeMode: String(form.get("range_mode")),
      period: String(form.get("period")),
      from,
      to,
      groupBy: String(form.get("group_by")),
      carRef: String(form.get("car_ref")),
      userRef: String(form.get("user_ref")),
      accountRef: String(form.get("account_ref")),
    };
    const button = event.currentTarget.querySelector("button[type=submit]");
    setButtonBusy(button, true);
    try { await renderAdminUsage(content); } catch (error) { toast(error.message); setButtonBusy(button, false); }
  });
}

function requestDetailEventMarkup(event) {
  const tokens = [["输入", "input_tokens"], ["输出", "output_tokens"], ["缓存", "cached_tokens"], ["缓存读取", "cache_read_tokens"], ["缓存写入", "cache_write_tokens"], ["推理", "reasoning_tokens"], ["合计", "total_tokens"]].map(([label, key]) => `<span>${label} Token：${event.usage_known === true && Number.isSafeInteger(event[key]) && event[key] >= 0 ? formatNumber(event[key]) : "上游未提供"}</span>`).join("");
  const cost = event.cost_usd == null ? "费用未知" : `$${escapeHTML(event.cost_usd)}`;
  return `<div class="request-event"><div><strong>${escapeHTML(event.safe_label || event.provider || "上游尝试")}</strong><span>${escapeHTML(event.model || "模型未提供")} · ${escapeHTML(formatTime(event.requested_at))}</span><span>账号 ${escapeHTML(event.account_ref || "-")}</span></div><div class="request-event-numbers">${tokens}<span>${cost}</span><span>${escapeHTML(event.pricing_status || "未计价")}</span></div></div>`;
}

function requestDetailMarkup(item) {
  const incomplete = item.billing_status === "unknown" || Number(item.unknown_cost_events || 0) > 0;
  const amount = item.billed_usd == null ? (incomplete ? "费用未知 · 费用不完整" : "未计价") : `$${escapeHTML(item.billed_usd)}${incomplete ? " · 已确认小计，费用不完整" : ""}`;
  const loaded = Array.isArray(item.events);
  return `<details class="request-row" data-request-id="${escapeHTML(item.request_id)}" data-request-loaded="${loaded}"><summary><span class="request-main"><strong>${escapeHTML(item.display_name || item.user_ref || "未归属")}</strong><span class="muted">${escapeHTML(item.model || "模型未提供")} · ${escapeHTML(formatTime(item.started_at))}</span></span><span class="request-status">${statusLabel(item.outcome)}</span><span class="request-amount">${amount}<small>${formatNumber(item.event_count)} 个上游事件</small></span></summary><div class="request-detail"><div class="request-meta"><span>请求 ID <code>${escapeHTML(item.request_id)}</code></span><span>车辆 ${escapeHTML(item.car_ref || "-")}</span><span>API Key ${escapeHTML(item.api_key_ref || "-")}</span><span>结果 ${escapeHTML(item.status_class || item.reason_code || "-")}</span></div><div data-request-events role="status">${loaded ? (item.events.length ? item.events.map(requestDetailEventMarkup).join("") : `<div class="empty compact-empty">没有上游事件</div>`) : "展开后加载上游事件"}</div></div></details>`;
}

function bindRequestDetails(content, items) {
  content.querySelectorAll("[data-request-id]").forEach(row => {
    const region = row.querySelector("[data-request-events]");
    let loading = false;
    let failed = false;
    const load = async () => {
      if (loading || row.dataset.requestLoaded === "true") return;
      loading = true;
      region.textContent = "正在加载上游事件...";
      try {
        const detail = await request(`/admin/usage/requests/${encodeURIComponent(row.dataset.requestId)}`);
        if (detail?.request_id !== row.dataset.requestId || !Array.isArray(detail.events)) throw new Error("invalid_request_detail");
        region.innerHTML = detail.events.length ? detail.events.map(requestDetailEventMarkup).join("") : `<div class="empty compact-empty">没有上游事件</div>`;
        row.dataset.requestLoaded = "true";
        const item = items.find(item => item.request_id === detail.request_id);
        if (item) item.events = detail.events;
      } catch (_) {
        failed = true;
        region.innerHTML = `<p>上游事件加载失败，请手动重试。</p><button class="button secondary compact" type="button" data-request-retry>重试加载</button>`;
        region.querySelector("[data-request-retry]").addEventListener("click", load);
      } finally {
        loading = false;
      }
    };
    row.addEventListener("toggle", async () => {
      if (row.open && !failed) await load();
    });
  });
}

const requestFilterKeys = ["model", "outcome", "billing_status", "user_ref", "car_ref", "account_ref", "api_key_ref", "request_id"];

function restoreRequestFilters() {
  const query = location.hash.slice(1).split("?").slice(1).join("?");
  const source = new URLSearchParams(query);
  const params = new URLSearchParams();
  // Keep only supported filters; cursors belong to page state, not the URL.
  if (source.has("from") || source.has("to")) {
    params.set("from", source.get("from") || "");
    params.set("to", source.get("to") || "");
  } else params.set("period", source.get("period") || "30d");
  const values = {};
  for (const key of requestFilterKeys) {
    values[key] = (source.get(key) || "").trim();
    if (values[key]) params.set(key, values[key]);
  }
  const path = `/admin/usage/requests?${params.toString()}`;
  if (state.requestFilters !== path) state.pages.usageRequests = emptyPage();
  state.requestFilterValues = values;
  state.requestFilters = path;
}

const retentionOperationLabels = {
  usage_details: "清理用量明细",
  closed_periods: "删除已结束账期",
  reset_quota_windows: "重置 5h / 7d 用量",
};

function validRetentionCount(value) {
  return Number.isSafeInteger(value) && value >= 0;
}

function showRetentionConfirmation(button, operation, preview) {
  const panel = button.closest(".retention-panel");
  if (!panel) return;
  if (!preview || typeof preview.job_id !== "string" || !preview.job_id.trim() || preview.operation !== operation || preview.confirmation_required !== true || !validRetentionCount(preview.expected_count) || !validRetentionCount(preview.in_flight_count)) {
    toast("预览结果无效，请重新预览；未执行任何操作。");
    return;
  }
  const jobID = preview.job_id;
  panel.querySelector("[data-retention-result]")?.remove();
  button.hidden = true;
  const confirmation = document.createElement("div");
  confirmation.className = "retention-confirm";
  const impact = operation === "reset_quota_windows" ? "5 小时与 7 天已确认用量和窗口起点会同时清零；限额、并发和历史请求费用不变；后续首笔可靠费用重新开始窗口。" : operation === "closed_periods" ? "已结束账期汇总和关联引用会被删除。" : "本期已确认金额和费用未知数量不会改变；进行中及不完整请求不会删除。";
  confirmation.innerHTML = `<strong>确认${escapeHTML(retentionOperationLabels[operation] || "操作")}</strong><p>本批预计影响 ${formatNumber(preview.expected_count)} 项，在途或不完整请求 ${formatNumber(preview.in_flight_count)} 项。每批最多 1,000 个逻辑请求、账期或额度窗口，更多数据需重新预览。执行后不可撤销；${impact}</p><div class="retention-confirm-actions"><button class="button secondary compact" type="button" data-retention-cancel>取消</button><button class="button danger compact" type="button" data-retention-confirm>确认执行</button></div>`;
  panel.append(confirmation);
  const result = document.createElement("p");
  result.setAttribute("data-retention-result", "");
  result.setAttribute("role", "status");
  panel.append(result);
  const cancelButton = confirmation.querySelector("[data-retention-cancel]");
  const confirmButton = confirmation.querySelector("[data-retention-confirm]");
  let active = true;
  let busy = false;
  const closeConfirmation = () => {
    active = false;
    confirmation.remove();
    button.hidden = false;
    button.focus();
  };
  const stalePreview = () => {
    result.textContent = "预览已失效或数据已变化，请重新预览后确认。";
    closeConfirmation();
  };
  const showResult = job => {
    if (!job || job.job_id !== jobID || job.operation !== operation) return false;
    if (job.status === "completed" && validRetentionCount(job.deleted_count) && validRetentionCount(job.in_flight_count)) {
      const action = operation === "reset_quota_windows" ? "重置" : "删除";
      const unit = operation === "usage_details" ? "个逻辑请求" : operation === "reset_quota_windows" ? "个额度窗口" : "个账期";
      result.textContent = `${retentionOperationLabels[operation]}已完成：实际${action} ${formatNumber(job.deleted_count)} ${unit}；在途或不完整请求 ${formatNumber(job.in_flight_count)} 项。${operation === "reset_quota_windows" ? "在途请求完成后可能再次增加用量。" : "进行中及不完整请求仍受保护。"}每批最多 1,000 项，更多数据需重新预览。`;
      closeConfirmation();
      return true;
    }
    if (job.status === "queued" || job.status === "failed") {
      result.textContent = "本批尚未完成，可点击“重试确认”使用同一预览重试；不会自动执行。";
      return true;
    }
    return false;
  };
  cancelButton.addEventListener("click", () => {
    if (!active || busy) return;
    result.remove();
    closeConfirmation();
  });
  confirmButton.addEventListener("click", async () => {
    if (!active || busy) return;
    busy = true;
    setButtonBusy(confirmButton, true);
    setButtonBusy(cancelButton, true);
    result.textContent = "正在确认本批操作，请勿重复提交。";
    try {
      try {
        const job = await request("/admin/retention/jobs", { method: "POST", body: JSON.stringify({ job_id: jobID, operation, confirm: true }) });
        if (showResult(job)) return;
      } catch (error) {
        if (error.status === 409) {
          stalePreview();
          return;
        }
      }
      // Resolve an ambiguous response once using a safe GET, never another POST.
      try {
        const job = await request(`/admin/retention/jobs/${encodeURIComponent(jobID)}`);
        if (!showResult(job)) result.textContent = "暂时无法确认操作结果；可点击“重试确认”使用同一预览重试，不会自动执行。";
      } catch (error) {
        if (error.status === 409 || error.status === 404) stalePreview();
        else result.textContent = "暂时无法查询操作结果；可点击“重试确认”使用同一预览重试，不会自动执行。";
      }
    } finally {
      busy = false;
      if (active) {
        confirmButton.textContent = "重试确认";
        setButtonBusy(confirmButton, false);
        setButtonBusy(cancelButton, false);
      }
    }
  });
  confirmButton.focus();
}

async function renderAdminRequests(content, reset = true) {
  restoreRequestFilters();
  const path = state.requestFilters || "/admin/usage/requests?period=30d";
  const page = await loadPage("usageRequests", path, reset);
  const values = state.requestFilterValues;
  const filters = `<form id="request-filter" class="report-toolbar request-filter"><div class="field"><label for="request-model">模型</label><input id="request-model" name="model" placeholder="请求或实际模型" value="${escapeHTML(values.model || "")}"></div><div class="field"><label for="request-outcome">结果</label><select id="request-outcome" name="outcome"><option value="">全部</option><option value="succeeded"${values.outcome === "succeeded" ? " selected" : ""}>成功</option><option value="failed"${values.outcome === "failed" ? " selected" : ""}>失败</option><option value="rejected"${values.outcome === "rejected" ? " selected" : ""}>拒绝</option><option value="incomplete"${values.outcome === "incomplete" ? " selected" : ""}>不完整</option></select></div><div class="field"><label for="request-billing">计价状态</label><select id="request-billing" name="billing_status"><option value="">全部</option><option value="priced"${values.billing_status === "priced" ? " selected" : ""}>已计价</option><option value="unknown"${values.billing_status === "unknown" ? " selected" : ""}>费用未知</option><option value="legacy_unpriced"${values.billing_status === "legacy_unpriced" ? " selected" : ""}>历史未计价</option></select></div><div class="field"><label for="request-user">用户引用</label><input id="request-user" name="user_ref" placeholder="usr_..." value="${escapeHTML(values.user_ref || "")}"></div><div class="field"><label for="request-car">车辆引用</label><input id="request-car" name="car_ref" placeholder="car_..." value="${escapeHTML(values.car_ref || "")}"></div><div class="field"><label for="request-account">账号引用</label><input id="request-account" name="account_ref" placeholder="acct_..." value="${escapeHTML(values.account_ref || "")}"></div><div class="field"><label for="request-key">API Key 引用</label><input id="request-key" name="api_key_ref" placeholder="cpk_..." value="${escapeHTML(values.api_key_ref || "")}"></div><div class="field"><label for="request-id">请求 ID</label><input id="request-id" name="request_id" value="${escapeHTML(values.request_id || "")}"></div><button class="button" type="submit">筛选</button></form>`;
  const activeCount = activeFilterCount();
  const activePeriod = activePeriodFilter();
  const applied = [
    activePeriod ? `周期：${escapeHTML(activePeriod)}` : "",
    activeCount ? `${activeCount} 个条件` : "",
  ].filter(Boolean).join(" · ");
  const filterNote = applied ? `<div class="filter-status" role="status">已应用筛选：${applied}<button class="button secondary compact" type="button" data-clear-filters>清除</button></div>` : "";
  content.innerHTML = `<div class="section-header"><div><h2>请求明细</h2><p>一行一个逻辑请求，展开查看全部 CPA 上游事件。默认最近 30 天。</p></div></div>${filterNote}${filters}<div class="request-list">${page.items.length ? page.items.map(requestDetailMarkup).join("") : `<div class="empty">当前条件下没有请求记录</div>`}</div>${paginationFooter(page, "个逻辑请求")}`;
  bindRequestDetails(content, page.items);
  content.querySelector("[data-clear-filters]")?.addEventListener("click", () => {
    const cleared = `#/requests?${new URLSearchParams({ period: "30d" }).toString()}`;
    if (location.hash !== cleared) location.hash = cleared;
    else {
      state.pages.usageRequests = emptyPage();
      state.requestFilters = "";
      renderAdminRequests(content, true).catch(error => toast(error.message));
    }
  });
  content.querySelector("#request-filter").addEventListener("submit", async event => {
    event.preventDefault();
    const form = new FormData(event.currentTarget);
    const params = new URLSearchParams(state.requestFilters.split("?")[1]);
    for (const key of requestFilterKeys) {
      const value = String(form.get(key) || "").trim();
      if (value) params.set(key, value);
      else params.delete(key);
    }
    const hash = `#/requests?${params.toString()}`;
    if (location.hash !== hash) location.hash = hash;
    else {
      state.pages.usageRequests = emptyPage();
      try { await renderAdminRequests(content, true); } catch (error) { toast(error.message); }
    }
  });
  content.querySelector("[data-load-more]")?.addEventListener("click", async event => { setButtonBusy(event.currentTarget, true); try { await renderAdminRequests(content, false); } catch (error) { toast(error.message); setButtonBusy(event.currentTarget, false); } });
}

async function renderRetention(content) {
  const settings = await request("/admin/retention");
  const current = settings.effective_days === 0 ? "永久" : `${settings.effective_days} 天`;
  content.innerHTML = `<div class="section-header"><div><h2>数据保留</h2><p>当前明细保留：${escapeHTML(current)} · 来源：${escapeHTML(settings.source === "database" ? "管理端设置" : "配置文件默认")}</p></div></div><div class="retention-layout"><section class="retention-panel"><h3>明细保留期限</h3><p class="muted">仅影响请求和上游事件明细；账期金额汇总独立保存。</p><form id="retention-form"><label><input type="radio" name="days" value="90"${settings.effective_days === 90 ? " checked" : ""}>90 天</label><label><input type="radio" name="days" value="180"${settings.effective_days === 180 ? " checked" : ""}>半年（180 天）</label><label><input type="radio" name="days" value="365"${settings.effective_days === 365 ? " checked" : ""}>一年（365 天）</label><label><input type="radio" name="days" value="0"${settings.effective_days === 0 ? " checked" : ""}>永久保留</label><div class="form-actions"><button class="button" type="submit">保存设置</button><button class="button secondary" id="restore-retention" type="button">恢复配置默认</button></div></form></section><section class="retention-panel"><h3>明细清理</h3><p class="muted">删除已完成请求及关联事件，不改变账期已确认金额。</p><button class="button danger" data-retention-op="usage_details">预览并清理明细</button></section><section class="retention-panel"><h3>已结束账期</h3><p class="muted">删除账期汇总前请确认历史金额不再需要核对。</p><button class="button danger" data-retention-op="closed_periods">预览并删除账期</button></section><section class="retention-panel"><h3>重置 5h / 7d 用量</h3><p class="muted">同时清零两个本地窗口的用量与起点；不修改限额、并发或历史请求费用。</p><button class="button danger" data-retention-op="reset_quota_windows">预览并重置额度窗口</button></section></div>`;
  content.querySelector("#retention-form").addEventListener("submit", async event => { event.preventDefault(); const selected = new FormData(event.currentTarget).get("days"); if (selected === null) { toast("请选择保留期限后保存。"); return; } const days = Number(selected); try { await request("/admin/retention", { method: "PATCH", body: JSON.stringify({ days }) }); toast("保留设置已保存"); renderRetention(content); } catch (error) { toast(error.message); } });
  content.querySelector("#restore-retention").addEventListener("click", async () => { try { await request("/admin/retention", { method: "PATCH", body: JSON.stringify({ days: null }) }); toast("已恢复配置默认"); renderRetention(content); } catch (error) { toast(error.message); } });
  content.querySelectorAll("[data-retention-op]").forEach(button => button.addEventListener("click", async () => {
    const operation = button.dataset.retentionOp;
    setButtonBusy(button, true);
    try {
      const preview = await request("/admin/retention/preview", { method: "POST", body: JSON.stringify({ operation }) });
      setButtonBusy(button, false);
      showRetentionConfirmation(button, operation, preview);
    } catch (error) {
      toast("预览失败，请稍后重新预览；未执行任何操作。");
      setButtonBusy(button, false);
    }
  }));
}

const auditActionLabels = {
  bootstrap_admin: "初始化管理员",
  login: "登录",
  logout: "退出登录",
  change_password: "修改密码",
  reset_password: "重置密码",
  create_user: "创建用户",
  update_user: "更新用户",
  create_api_key: "创建 API Key",
  revoke_api_key: "撤销 API Key",
  revoke_all_api_keys: "撤销全部 API Key",
  create_car: "创建车辆",
  update_car: "更新车辆",
  move_member: "调整成员",
  remove_member: "移除成员",
  move_account: "调整账号",
  remove_account: "撤销账号分配",
  authentication_reject: "拒绝用户 Key 认证",
  authorization_reject: "拒绝代理授权",
};

async function renderAudit(content, reset) {
  const page = await loadPage("audit", "/admin/audit-events", reset);
  const items = page.items;
  content.innerHTML = `<div class="section-header"><div><h2>审计记录</h2><p>安全操作与授权结果</p></div></div>${items.length ? `<div class="table-wrap"><table><thead><tr><th>时间</th><th>操作</th><th>执行者</th><th>目标</th><th>结果</th><th>原因</th></tr></thead><tbody>${items.map(item => `<tr><td>${escapeHTML(formatTime(item.occurred_at))}</td><td>${escapeHTML(auditActionLabels[item.action] || item.action)}</td><td>${escapeHTML(item.actor_type)} ${escapeHTML(item.actor_ref || "-")}</td><td>${escapeHTML(item.target_type)} ${escapeHTML(item.target_ref || "-")}</td><td>${item.result === "success" ? "成功" : item.result === "failure" ? "失败" : escapeHTML(item.result)}</td><td>${escapeHTML(item.reason_code || "-")}</td></tr>`).join("")}</tbody></table></div>` : `<div class="empty">暂无审计记录</div>`}
    ${paginationFooter(page, "条记录")}`;
  content.querySelector("[data-load-more]")?.addEventListener("click", async event => {
    setButtonBusy(event.currentTarget, true);
    try { await renderAudit(content, false); } catch (error) { toast(error.message); }
  });
}

// Keep credential files and OAuth material out of persistent application state.
const adminAccountDialogs = new Set();

function closeAdminAccountDialogs() {
  for (const dialog of adminAccountDialogs) {
    dialog.dispatchEvent(new Event("account-cleanup"));
    dialog.close();
  }
  adminAccountDialogs.clear();
}

function accountDialog(title, body, wide = false) {
  const dialog = openDialog(title, body, wide);
  adminAccountDialogs.add(dialog);
  dialog.addEventListener("close", () => {
    dialog.dispatchEvent(new Event("account-cleanup"));
    adminAccountDialogs.delete(dialog);
  }, { once: true });
  return dialog;
}

function accountProvider(item) {
  return String(item.provider || item.type || "").trim().toLowerCase();
}

function accountStatus(item) {
  if (item.disabled === true || item.status === "disabled") return "disabled";
  if (item.unavailable === true || ["error", "unavailable"].includes(item.status)) return "unavailable";
  return item.status === "active" ? "active" : "unknown";
}

function accountSecondaryText(item) {
  const primary = item.label || item.name;
  return [item.name !== primary ? item.name : "", item.email !== primary ? item.email : ""].filter(Boolean).join(" · ");
}

function isSub2APIEnvelope(file) {
  if (!file || Array.isArray(file) || typeof file !== "object" || !Object.hasOwn(file, "accounts")) return false;
  const hasType = Object.hasOwn(file, "type");
  const hasVersion = Object.hasOwn(file, "version");
  if (!hasType && !hasVersion) return true;
  return hasType && hasVersion && file.type === "sub2api-data" && file.version === 1;
}

function accountImportError(file) {
  if (isSub2APIEnvelope(file)) {
    if (!Array.isArray(file.accounts) || !file.accounts.length || file.accounts.length > 100) return "sub2api 文件须包含 1–100 个账号";
    for (let index = 0; index < file.accounts.length; index++) {
      const account = file.accounts[index];
      if (account?.platform !== "openai" || account.type !== "oauth") return `第 ${index + 1} 个账号不是 OpenAI OAuth 账号，请单独导出支持的账号`;
      if (typeof account.credentials?.access_token !== "string" || !account.credentials.access_token.trim()) return `第 ${index + 1} 个账号缺少 access_token，请重新导出完整凭据`;
    }
    return "";
  }
  if (file && !Array.isArray(file) && typeof file === "object" && Object.hasOwn(file, "accounts")) return "sub2api 文件头无效，仅支持 v1 或同时缺少 type/version 的新版导出文件";
  if (!file || file.type !== "codex") return "仅支持 OpenAI / Codex OAuth JSON 或兼容的 sub2api 导出文件；API Key 请使用原版管理配置";
  if (typeof file.access_token !== "string" || !file.access_token.trim()) return "授权文件缺少 access_token，请重新导出完整的 Codex 授权 JSON";
  return "";
}

function accountImportOutcome(result, originalName) {
  if (result?.error || result?.errors?.length || !["ok", "partial"].includes(result?.status)) throw new Error(typeof result?.error === "string" ? result.error : "文件导入失败，请检查文件后重试");
  const names = Array.isArray(result.files) ? result.files.filter(name => typeof name === "string") : result.status === "ok" ? [originalName] : [];
  const failures = Array.isArray(result.failed) ? result.failed.filter(item => item && typeof item.name === "string") : [];
  if (result.status === "partial" && !failures.length) throw new Error("导入结果不完整，请刷新账号列表后核对");
  return { names, failures };
}

function accountProxyURL(value) {
  const trimmed = String(value || "").trim();
  if (!trimmed) return "";
  try {
    const url = new URL(trimmed);
    if (!["http:", "https:", "socks5:", "socks5h:"].includes(url.protocol) || !url.hostname) throw new Error();
    return trimmed;
  } catch (_) { throw new Error("代理地址格式无效，请使用 http://、https://、socks5:// 或 socks5h://"); }
}

function safeOAuthURL(value) {
  try {
    const url = new URL(value);
    if (url.protocol !== "https:" || url.hostname !== "auth.openai.com" || url.username || url.password || (url.port && url.port !== "443")) return "";
    return url.href;
  } catch (_) { return ""; }
}

function validCallbackURL(value, expectedState) {
  try {
    const url = new URL(value);
    return ["http:", "https:"].includes(url.protocol) && !url.username && !url.password &&
      url.searchParams.get("state") === expectedState && Boolean(url.searchParams.get("code") || url.searchParams.get("error"));
  } catch (_) { return false; }
}

async function renderAdminAccounts(content) {
  const session = state.session;
  const current = () => content.isConnected && state.session === session && state.route === "/admin-accounts";
  const params = new URLSearchParams(location.hash.split("?")[1] || "");
  let query = params.get("q") || "";
  let filter = params.get("status") || "";
  let files = [];
  let loaded = false;
  let loading = false;
  let refreshQueued = false;
  let message = "";
  const busy = new Set();
  content.innerHTML = `<div class="section-header"><div><h2>账号管理</h2><p>管理 OpenAI / Codex 上游账号；车辆分配仍在车辆管理中完成。</p></div><div class="form-actions"><button class="button secondary" data-account-import>导入 JSON</button><button class="button" data-account-oauth>添加 OAuth 账号</button></div></div>
    <form class="request-filter account-filter" role="search"><div class="field"><label for="account-search">搜索账号</label><input id="account-search" name="q" type="search" maxlength="160" value="${escapeHTML(query)}" placeholder="名称、邮箱或标签"></div><div class="field account-provider-field"><label for="account-provider">提供商</label><input id="account-provider" value="OpenAI / Codex" readonly></div><div class="field"><label for="account-status">状态</label><select id="account-status" name="status"><option value="">全部状态</option>${["active", "disabled", "unavailable", "unknown"].map(value => `<option value="${value}"${filter === value ? " selected" : ""}>${statusText(value)}</option>`).join("")}</select></div><div class="form-actions"><button class="button secondary" type="submit">筛选</button><button class="button secondary" type="button" data-account-clear>清除</button></div></form>
    <div class="toolbar"><span data-account-counts class="muted" aria-live="polite"></span><button class="button secondary compact" data-account-refresh>刷新</button></div><div data-account-error role="alert"></div><div data-account-results aria-live="polite"></div>`;
  const draw = () => {
    if (!current()) return;
    content.querySelector("[data-account-error]").innerHTML = message ? `<div class="error-banner">${escapeHTML(message)} 请点击刷新重试。</div>` : "";
    const refreshButton = content.querySelector("[data-account-refresh]");
    refreshButton.disabled = loading;
    refreshButton.textContent = loading ? "刷新中…" : "刷新";
    content.querySelector("[data-account-counts]").textContent = loaded ? `共 ${files.length} 个 · 启用 ${files.filter(x => accountStatus(x) === "active").length} · 禁用 ${files.filter(x => accountStatus(x) === "disabled").length} · 不可用 ${files.filter(x => accountStatus(x) === "unavailable").length} · 未知 ${files.filter(x => accountStatus(x) === "unknown").length}` : "";
    const results = content.querySelector("[data-account-results]");
    results.setAttribute("aria-busy", String(loading));
    if (!loaded) {
      results.innerHTML = loading ? '<div class="loading" role="status">正在加载账号…</div>' : "";
      return;
    }
    const shown = files.map((item, index) => ({ item, index })).filter(({ item }) => (!filter || accountStatus(item) === filter) && [item.name, item.email, item.label, item.auth_index].some(value => String(value || "").toLowerCase().includes(query.toLowerCase())));
    results.innerHTML = shown.length ? `<div class="table-wrap"><table class="account-table"><thead><tr><th>账号</th><th>提供商</th><th>状态</th><th>账号配额</th><th>最近更新</th><th>操作</th></tr></thead><tbody>${shown.map(({ item, index }) => `<tr><td><strong>${escapeHTML(item.label || item.name || "未命名账号")}</strong><div class="muted">${escapeHTML(accountSecondaryText(item))}</div></td><td>OpenAI / Codex</td><td>${statusLabel(accountStatus(item))}</td><td>${quotaMarkup(item.quota)}</td><td>${escapeHTML(formatTime(item.updated_at || item.modtime))}</td><td><div class="form-actions"><button class="button secondary compact" data-account-detail="${index}">详情</button><button class="button secondary compact" data-account-test="${index}"${accountStatus(item) === "disabled" || busy.has(item.name) || loading ? " disabled" : ""}${accountStatus(item) === "disabled" ? ' title="禁用账号不能测试"' : ""}>测试</button><button class="button secondary compact" data-account-toggle="${index}"${busy.has(item.name) || loading ? " disabled" : ""}>${busy.has(item.name) ? "保存中…" : accountStatus(item) === "disabled" ? "启用" : "禁用"}</button><button class="button danger compact" data-account-delete="${index}"${!item.deletable || busy.has(item.name) || loading ? " disabled" : ""}${!item.deletable ? ' title="配置管理的账号不能在此删除"' : ""}>删除</button></div></td></tr>`).join("")}</tbody></table></div>` : `<div class="empty"><h3>${files.length ? "没有匹配的账号" : "尚未添加账号"}</h3><p>${files.length ? "调整搜索或清除筛选后重试。" : "导入 OpenAI / Codex JSON 文件，或通过 OAuth 添加账号。"}</p></div>`;
    results.querySelectorAll("[data-account-detail]").forEach(button => button.addEventListener("click", () => {
      const item = files[Number(button.dataset.accountDetail)];
      const fields = [["名称", item.name], ["标签", item.label], ["邮箱", item.email], ["提供商", "OpenAI / Codex"], ["账号索引", item.auth_index], ["套餐", item.plan_type], ["状态", statusText(accountStatus(item))], ["创建时间", formatTime(item.created_at)], ["更新时间", formatTime(item.updated_at || item.modtime)], ["最近刷新", formatTime(item.last_refresh)], ["下次重试", formatTime(item.next_retry_after)]];
      accountDialog("账号详情", `<p class="muted">仅展示账号元数据，不展示令牌、凭据内容或文件路径。</p><dl class="account-metadata">${fields.map(([label, value]) => `<dt>${label}</dt><dd>${escapeHTML(value || "—")}</dd>`).join("")}</dl>`);
    }));
    results.querySelectorAll("[data-account-test]").forEach(button => button.addEventListener("click", () => {
      const item = files[Number(button.dataset.accountTest)];
      if (accountStatus(item) !== "disabled") openAccountTest(item, refresh);
    }));
    results.querySelectorAll("[data-account-delete]").forEach(button => button.addEventListener("click", async () => {
      const item = files[Number(button.dataset.accountDelete)];
      if (!item?.deletable || busy.has(item.name)) return;
      if (!window.confirm(`确定删除 ${item.label || item.name}？此操作会移除账号授权文件，且无法撤销。`)) return;
      busy.add(item.name);
      draw();
      try {
        const query = new URLSearchParams({name: item.name, auth_index: item.auth_index || ""});
        await request(`/admin/auth-files?${query}`, {method: "DELETE"});
        if (current()) { toast("账号已删除"); await refresh(); }
      } catch (error) { if (current()) message = error.message; }
      finally { busy.delete(item.name); draw(); }
    }));
    results.querySelectorAll("[data-account-toggle]").forEach(button => button.addEventListener("click", async () => {
      const item = files[Number(button.dataset.accountToggle)];
      const disabled = accountStatus(item) !== "disabled";
      if (disabled && !window.confirm(`禁用 ${item.label || item.name}？该账号将不再接收新请求，可随时重新启用。`)) return;
      busy.add(item.name);
      draw();
      try {
        await request("/admin/auth-files/status", { method: "PATCH", body: JSON.stringify({ name: item.name, auth_index: item.auth_index, disabled }) });
        if (current()) { toast(disabled ? "账号已禁用" : "账号已启用"); await refresh(); }
      } catch (error) { if (current()) message = error.message; }
      finally { busy.delete(item.name); draw(); }
    }));
  };
  const refresh = async () => {
    if (!current()) return;
    if (loading) { refreshQueued = true; return; }
    do {
      refreshQueued = false;
      loading = true;
      message = "";
      draw();
      try {
        const data = await request("/admin/auth-files");
        if (!current()) return;
        if (!Array.isArray(data?.files)) throw new Error("账号列表响应无效");
        // A mutation finishing during this request invalidates its snapshot.
        if (!refreshQueued) {
          files = data.files.filter(item => item && ["codex", "openai"].includes(accountProvider(item)));
          loaded = true;
        }
      } catch (error) { if (current() && !refreshQueued) message = error.message; }
      finally { loading = false; if (!refreshQueued) draw(); }
    } while (refreshQueued && current());
  };
  const applyFilters = () => {
    const form = content.querySelector("form");
    query = form.elements.q.value.trim();
    filter = form.elements.status.value;
    const next = new URLSearchParams();
    if (query) next.set("q", query);
    if (filter) next.set("status", filter);
    history.replaceState(null, "", `#/admin-accounts${next.size ? `?${next}` : ""}`);
    draw();
  };
  content.querySelector("form").addEventListener("submit", event => { event.preventDefault(); applyFilters(); });
  content.querySelector("[data-account-clear]").addEventListener("click", () => {
    content.querySelector("form").elements.q.value = "";
    content.querySelector("form").elements.status.value = "";
    applyFilters();
  });
  content.querySelector("[data-account-refresh]").addEventListener("click", refresh);
  content.querySelector("[data-account-import]").addEventListener("click", () => openAccountImport(refresh, files.map(item => item.name)));
  content.querySelector("[data-account-oauth]").addEventListener("click", () => openAccountOAuth(refresh));
  await refresh();
}


function accountTestFailureMessage(error) {
  const messages = {
    account_not_found: "未找到指定账号，请刷新列表后重试。",
    account_ambiguous: "账号标识不唯一，请刷新列表后重试。",
    account_disabled: "账号已禁用，无法测试。",
    account_not_testable: "该账号不支持连接测试。",
    authorization_expired: "账号授权已失效，请重新授权后重试。",
    upstream_forbidden: "上游拒绝访问，请检查账号权限。",
    rate_limited: "账号额度或速率受限，请稍后重试。",
    upstream_unavailable: "上游暂不可用，请稍后重试。",
    invalid_upstream_response: "上游暂不可用，请稍后重试。",
    accounts_unavailable: "上游暂不可用，请稍后重试。",
  };
  if (messages[error?.code]) return messages[error.code];
  if (error?.status === 401) return messages.authorization_expired;
  if (error?.status === 403) return messages.upstream_forbidden;
  if (error?.status === 429) return messages.rate_limited;
  if (error?.status >= 500) return messages.upstream_unavailable;
  return "账号连接测试失败，请检查输入后重试。";
}

function openAccountTest(item, onUpdated) {
  if (!item || accountStatus(item) === "disabled") return null;
  const displayName = item.label || item.name || "未命名账号";
  const dialog = accountDialog("测试账号连接", `<div class="account-test-summary"><div><span>账号</span><strong>${escapeHTML(displayName)}</strong></div><div><span>状态</span>${statusLabel(accountStatus(item))}</div></div><form data-account-test-form><div class="field"><label for="account-test-model">模型</label><input id="account-test-model" name="model" value="gpt-5.6-sol" required maxlength="128" autocomplete="off" spellcheck="false"><small>请输入该账号可访问的模型，最多 128 个字符。</small></div><div class="field"><label for="account-test-prompt">提示词</label><textarea id="account-test-prompt" name="prompt" required maxlength="2000" rows="3">请只回复 OK。</textarea><small>仅用于本次连接测试，最多 2000 个字符。</small></div><div class="account-test-terminal" data-account-test-status role="status" aria-live="polite">等待开始测试…</div><div class="form-actions"><button class="button" type="submit" data-account-test-submit>开始测试</button><button class="button secondary" type="button" data-dialog-close>关闭</button></div></form>`);
  const controller = new AbortController();
  let running = false;
  dialog.addEventListener("account-cleanup", () => controller.abort(), { once: true });
  const form = dialog.querySelector("[data-account-test-form]");
  const submit = dialog.querySelector("[data-account-test-submit]");
  const status = dialog.querySelector("[data-account-test-status]");
  form.addEventListener("submit", async event => {
    event.preventDefault();
    if (running) return;
    const model = form.elements.model.value.trim();
    const prompt = form.elements.prompt.value.trim();
    if (!model || !prompt) {
      status.dataset.state = "error";
      status.textContent = "输入无效\n请填写模型和提示词后重试。";
      return;
    }
    running = true;
    setButtonBusy(submit, true);
    submit.textContent = "测试中…";
    status.dataset.state = "running";
    status.textContent = `正在连接…\n模型：${model}\n等待上游响应。`;
    try {
      const result = await request("/admin/auth-files/test", {
        method: "POST",
        signal: controller.signal,
        body: JSON.stringify({ name: item.name, auth_index: item.auth_index || "", model, prompt }),
      });
      if (controller.signal.aborted || !dialog.isConnected) return;
      status.dataset.state = "success";
      status.textContent = `连接成功\n模型：${result.model}\n耗时：${formatNumber(result.duration_ms)} 毫秒\n已收到有效响应（${formatNumber(result.response_bytes)} 字节）`;
      submit.textContent = "再次测试";
      if (onUpdated) onUpdated();
    } catch (error) {
      if (controller.signal.aborted || error?.name === "AbortError" || !dialog.isConnected) return;
      status.dataset.state = "error";
      status.textContent = `测试失败\n${accountTestFailureMessage(error)}`;
      submit.textContent = "重试";
    } finally {
      running = false;
      if (!controller.signal.aborted && dialog.isConnected) setButtonBusy(submit, false);
    }
  });
  form.elements.model.focus();
  return dialog;
}

function openAccountImport(onSaved, existingNames = []) {
  const dialog = accountDialog("导入 OpenAI / Codex 账号", `<p class="muted">支持原版 Codex OAuth JSON、sub2api-data v1 和新版无 type/version 头导出文件（仅 OpenAI OAuth）。sub2api 中的多个账号将分别导入；不会读取导出文件中的密码、TOTP、恢复信息或代理配置。填写代理后，导入账号的刷新及后续请求都会使用该代理。API Key 请使用原版管理配置。每个文件单独上传并显示结果；同名文件不会覆盖已有账号，请先重命名文件再导入。每个文件须小于 1 MiB（含上传封装）。</p><form data-import-form><div class="field"><label for="account-import-proxy">代理地址（可选）</label><input id="account-import-proxy" name="proxy_url" type="text" inputmode="url" autocomplete="off" spellcheck="false" placeholder="http://127.0.0.1:7890 或 socks5://127.0.0.1:1080"><p class="muted">支持 HTTP、HTTPS、SOCKS5 和 SOCKS5H，可包含代理用户名和密码。</p></div><div class="field"><label for="account-files">JSON 文件</label><input id="account-files" name="files" type="file" accept=".json,application/json" multiple required></div><div class="form-actions"><button class="button" type="submit">开始导入</button><button class="button secondary" type="button" data-dialog-close>关闭</button></div></form><ul class="account-import-results" aria-live="polite"></ul>`);
  const controller = new AbortController();
  dialog.addEventListener("account-cleanup", () => controller.abort(), { once: true });
  dialog.querySelector("form").addEventListener("submit", async event => {
    event.preventDefault();
    const form = event.currentTarget;
    const selected = Array.from(form.elements.files.files);
    if (!selected.length) return;
    let proxyURL;
    try { proxyURL = accountProxyURL(form.elements.proxy_url.value); }
    catch (error) { toast(error.message); form.elements.proxy_url.focus(); return; }
    const results = dialog.querySelector("ul");
    results.replaceChildren();
    form.querySelector("button[type=submit]").disabled = true;
    form.elements.files.disabled = true;
    form.elements.proxy_url.disabled = true;
    let saved = false;
    for (const file of selected) {
      if (controller.signal.aborted) break;
      const row = document.createElement("li");
      row.textContent = `${file.name} · 导入中…`;
      results.append(row);
      try {
        if (!file.name.toLowerCase().endsWith(".json")) throw new Error("请选择 .json 文件");
        if (file.size > 1024 * 1024 - 4096) throw new Error("文件过大，请使用小于 1020 KiB 的 JSON 文件");
        let parsed;
        try { parsed = JSON.parse(await file.text()); } catch (_) { throw new Error("JSON 格式无效，请修正文件后重试"); }
        const validationError = accountImportError(parsed);
        if (validationError) throw new Error(validationError);
        if (!(isSub2APIEnvelope(parsed) && parsed.accounts.length > 1) && existingNames.includes(file.name)) throw new Error("同名账号已存在，请重命名文件后再导入");
        parsed = null;
        if (controller.signal.aborted) break;
        const body = new FormData();
        body.append("file", file, file.name);
        body.append("proxy_url", proxyURL);
        const result = await request("/admin/auth-files", { method: "POST", body, signal: controller.signal });
        const outcome = accountImportOutcome(result, file.name);
        row.textContent = outcome.failures.length
          ? `${file.name} · 已导入 ${outcome.names.length} 个账号，失败 ${outcome.failures.length} 个`
          : `${file.name} · 导入成功${outcome.names.length > 1 ? `（${outcome.names.length} 个账号）` : ""}`;
        if (outcome.names.length > 1 || outcome.failures.length) {
          const details = document.createElement("ul");
          for (const name of outcome.names) {
            const entry = document.createElement("li");
            entry.textContent = `${name} · 导入成功`;
            details.append(entry);
          }
          for (const failure of outcome.failures) {
            const entry = document.createElement("li");
            entry.textContent = `${failure.name} · ${typeof failure.error === "string" ? failure.error : "导入失败，请重试"}`;
            details.append(entry);
          }
          row.append(details);
        }
        if (outcome.names.length) saved = true;
        for (const name of outcome.names) if (!existingNames.includes(name)) existingNames.push(name);
      } catch (error) {
        if (controller.signal.aborted) break;
        row.textContent = `${file.name} · ${error.message}`;
        if (Array.isArray(error.data?.failed)) {
          const details = document.createElement("ul");
          for (const failure of error.data.failed) {
            if (!failure || typeof failure.name !== "string") continue;
            const entry = document.createElement("li");
            entry.textContent = `${failure.name} · ${typeof failure.error === "string" ? failure.error : "导入失败，请重试"}`;
            details.append(entry);
          }
          row.append(details);
        }
      }
    }
    if (saved && !controller.signal.aborted) await onSaved();
    if (dialog.isConnected) {
      form.elements.files.value = "";
      form.elements.files.disabled = false;
      form.elements.proxy_url.disabled = false;
      form.querySelector("button[type=submit]").disabled = false;
    }
  });
}

function openAccountOAuth(onSaved) {
  const dialog = accountDialog("添加 OpenAI / Codex 账号", `<p class="muted">在 OpenAI 授权页面完成登录后，必须复制地址栏中的完整 localhost 回调 URL 并在下方提交。即使浏览器显示无法连接，也请复制完整地址。关闭此窗口将停止查询。</p><div class="field"><label for="account-oauth-proxy">代理地址（可选）</label><input id="account-oauth-proxy" data-oauth-proxy type="text" inputmode="url" autocomplete="off" spellcheck="false" placeholder="http://127.0.0.1:7890 或 socks5://127.0.0.1:1080"><p class="muted">支持 HTTP、HTTPS、SOCKS5 和 SOCKS5H。服务端换取、刷新令牌及该账号后续请求都会使用此代理；浏览器打开授权页时仍使用浏览器自身网络。</p></div><div data-oauth-status role="status" aria-live="polite"></div><div data-oauth-link></div><form data-oauth-callback hidden><div class="field"><label for="oauth-callback-url">手动提交回调 URL</label><input id="oauth-callback-url" name="redirect_url" type="url" autocomplete="off" spellcheck="false" required placeholder="粘贴授权后浏览器地址栏中的完整 URL"></div><p class="muted">请粘贴完整 localhost 地址（包含 code 和 state 参数）。回调提交成功不代表登录完成，请等待授权成功提示。不要将回调链接分享给他人。</p><button class="button secondary" type="submit">提交回调</button></form><div class="form-actions"><button class="button" data-oauth-start>开始授权</button><button class="button secondary" data-dialog-close>取消</button></div>`);
  let controller = null;
  let timer = 0;
  let generation = 0;
  let oauthState = "";
  const status = dialog.querySelector("[data-oauth-status]");
  const start = dialog.querySelector("[data-oauth-start]");
  const callback = dialog.querySelector("form");
  const link = dialog.querySelector("[data-oauth-link]");
  const proxyInput = dialog.querySelector("[data-oauth-proxy]");
  const stop = () => { generation++; window.clearTimeout(timer); controller?.abort(); oauthState = ""; callback.reset(); proxyInput.disabled = false; };
  dialog.addEventListener("account-cleanup", stop, { once: true });
  start.addEventListener("click", async () => {
    let proxyURL;
    try { proxyURL = accountProxyURL(proxyInput.value); }
    catch (error) { status.textContent = error.message; proxyInput.focus(); return; }
    stop();
    const version = generation;
    controller = new AbortController();
    proxyInput.disabled = true;
    const active = () => version === generation && dialog.isConnected && dialog.open && !controller.signal.aborted;
    start.disabled = true;
    start.textContent = "重新授权";
    callback.hidden = true;
    link.replaceChildren();
    status.textContent = "正在创建授权链接…";
    const fail = message => {
      if (!active()) return;
      stop();
      callback.hidden = true;
      callback.reset();
      link.replaceChildren();
      oauthState = "";
      status.textContent = `${message} 可点击重新授权重试。`;
      start.disabled = false;
    };
    const poll = async () => {
      if (!active()) return;
      try {
        const result = await request(`/admin/get-auth-status?state=${encodeURIComponent(oauthState)}`, { signal: controller.signal });
        if (!active()) return;
        if (result?.status === "ok") {
          stop();
          link.replaceChildren();
          callback.hidden = true;
          start.disabled = false;
          start.textContent = "添加另一个账号";
          status.textContent = "授权成功，账号已添加。";
          await onSaved();
        } else if (result?.status === "wait") {
          status.textContent = "等待 OpenAI 授权完成…";
          timer = window.setTimeout(poll, 2000);
        } else fail(typeof result?.error === "string" ? result.error : "授权失败或已过期");
      } catch (error) { fail(error.message); }
    };
    try {
      const result = await request("/admin/codex-auth-url", { method: "POST", body: JSON.stringify({ proxy_url: proxyURL }), signal: controller.signal });
      if (!active()) return;
      const url = safeOAuthURL(result?.url);
      if (result?.status !== "ok" || !url || typeof result.state !== "string" || !result.state) throw new Error("授权链接无效，请重试");
      oauthState = result.state;
      link.innerHTML = `<div class="form-actions"><a class="button" href="${escapeHTML(url)}" target="_blank" rel="noopener noreferrer" referrerpolicy="no-referrer">打开 OpenAI 授权页</a><button class="button secondary" type="button" data-oauth-copy>复制链接</button></div><p class="muted">授权状态标识：<code>${escapeHTML(oauthState)}</code></p>`;
      link.querySelector("button").addEventListener("click", async () => {
        try { await navigator.clipboard.writeText(url); if (active()) status.textContent = "链接已复制，等待授权完成…"; }
        catch (_) { if (active()) status.textContent = "无法访问剪贴板，请使用打开授权页，或右键复制链接。"; }
      });
      callback.hidden = false;
      start.disabled = false;
      await poll();
    } catch (error) { fail(error.message); }
  });
  callback.addEventListener("submit", async event => {
    event.preventDefault();
    const redirect = callback.elements.redirect_url.value.trim();
    if (!oauthState || !validCallbackURL(redirect, oauthState)) { status.textContent = "回调 URL 无效或不属于本次授权，请复制完整地址。"; return; }
    const version = generation;
    const button = callback.querySelector("button");
    button.disabled = true;
    try {
      await request("/admin/oauth-callback", { method: "POST", body: JSON.stringify({ provider: "codex", redirect_url: redirect }), signal: controller.signal });
      if (version === generation && dialog.isConnected) { callback.reset(); status.textContent = "回调已提交，等待确认…"; }
    } catch (error) { if (version === generation && dialog.isConnected) status.textContent = `${error.message} 请检查回调地址后重试。`; }
    finally { button.disabled = false; }
  });
}

function renderPassword(content) {
  const forced = Boolean(state.session?.must_change_password);
  const reason = state.session?.password_change_reason || "";
  const resetNotice = reason === "admin_reset"
    ? "管理员刚刚重置了你的密码。车辆、用量和 API Key 都仍然保留；请使用临时密码设置新密码，完成后重新登录即可恢复工作台。"
    : "这是你的首次临时密码。设置新密码后重新登录，车辆、用量和 API Key 会继续保留。";
  content.innerHTML = `<div class="section-header"><div><h2>${forced ? "先设置新密码" : "修改密码"}</h2><p>${forced ? "完成后请重新登录" : "更新后需要重新登录"}</p></div></div>${forced ? `<div class="password-notice"><strong>需要先完成一次安全设置</strong><p>${escapeHTML(resetNotice)}</p></div>` : ""}<form id="password-form" class="password-panel">
    <div class="field"><label for="current-password">当前密码</label><input id="current-password" name="current_password" type="password" autocomplete="current-password" required></div>
    <div class="field"><label for="new-password">新密码</label><input id="new-password" name="new_password" type="password" autocomplete="new-password" required minlength="12" maxlength="128"></div>
    <div class="password-hint">至少 12 个字符。提交后当前会话会退出，这是为了让新密码立即生效。</div><button class="button" type="submit">${forced ? "设置并继续" : "更新密码"}</button>
  </form>`;
  document.querySelector("#password-form").addEventListener("submit", async event => {
    event.preventDefault();
    const button = event.currentTarget.querySelector("button[type=submit]");
    const form = new FormData(event.currentTarget);
    setButtonBusy(button, true);
    try {
      await request("/me/password", { method: "POST", body: JSON.stringify({ current_password: form.get("current_password"), new_password: form.get("new_password") }) });
      state.session = null;
      state.csrf = "";
      loginView("密码已更新，请重新登录");
    } catch (error) {
      toast(error.message);
      setButtonBusy(button, false);
    }
  });
}

window.addEventListener("hashchange", () => {
  state.route = location.hash.slice(1).split("?")[0] || "/";
  if (state.session) renderShell();
});

async function boot() {
  try {
    const data = await request("/session");
    state.session = data;
    state.csrf = data.csrf_token || "";
    if (data.must_change_password) {
      state.route = "/password";
      location.hash = "#/password";
    }
    renderShell();
  } catch (_) {
    loginView();
  }
}

boot();
