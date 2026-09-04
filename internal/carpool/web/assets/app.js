const apiBase = "/carpool/api/v1";
const pageLimit = 25;
const maximumSelectorItems = 1000;

function emptyPage() {
  return { items: [], total: 0, nextCursor: "", loaded: false };
}

const state = {
  session: null,
  csrf: "",
  route: location.hash.slice(1) || "/",
  period: "today",
  refreshTimer: 0,
  pages: {
    keys: emptyPage(),
    users: emptyPage(),
    cars: emptyPage(),
    audit: emptyPage(),
  },
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
  "/keys": "API Key",
  "/users": "用户管理",
  "/cars": "车辆管理",
  "/usage": "用量报表",
  "/audit": "审计记录",
  "/password": "修改密码",
};

async function request(path, options = {}) {
  const headers = new Headers(options.headers || {});
  if (options.body && !headers.has("Content-Type")) headers.set("Content-Type", "application/json");
  if (options.method && !["GET", "HEAD"].includes(options.method) && state.csrf) {
    headers.set("X-Carpool-CSRF", state.csrf);
  }
  const response = await fetch(apiBase + path, { credentials: "same-origin", ...options, headers });
  const data = response.status === 204 ? null : await response.json().catch(() => null);
  if (!response.ok) {
    const error = new Error(data?.error?.message || "请求失败");
    error.code = data?.error?.code || "request_failed";
    error.status = response.status;
    throw error;
  }
  return data;
}

function escapeHTML(value) {
  return String(value ?? "").replace(/[&<>'"]/g, char => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", "'": "&#39;", '"': "&quot;" })[char]);
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
  const region = document.querySelector("#toast-region");
  const node = document.createElement("div");
  node.className = "toast";
  node.textContent = message;
  region.append(node);
  window.setTimeout(() => node.remove(), 3200);
}

const statusLabels = {
  active: "启用",
  disabled: "禁用",
  retired: "已退役",
  healthy: "正常",
  degraded: "受限",
  unavailable: "不可用",
  unknown: "未知",
  revoked: "已撤销",
  expired: "已过期",
};

function statusText(status) {
  return statusLabels[status] || status || "未知";
}

function statusLabel(status) {
  return `<span class="status ${escapeHTML(status)}">${escapeHTML(statusText(status))}</span>`;
}

function periodControl() {
  return `<div class="period-control" aria-label="统计周期">
    ${[["today", "今日"], ["7d", "最近 7 天"], ["30d", "最近 30 天"]].map(([value, label]) =>
      `<button type="button" data-period="${value}" aria-pressed="${state.period === value}">${label}</button>`).join("")}
  </div>`;
}

function bindPeriod() {
  document.querySelectorAll("[data-period]").forEach(button => button.addEventListener("click", () => {
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
    ${page.nextCursor ? `<button class="button secondary compact" type="button" data-load-more>加载更多</button>` : ""}</div>`;
}

function setButtonBusy(button, busy) {
  if (!button) return;
  button.disabled = busy;
  button.setAttribute("aria-busy", String(busy));
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

function setDialogContent(dialog, title, body) {
  dialog.innerHTML = `<div class="dialog-head"><h2>${escapeHTML(title)}</h2><button class="icon-button" aria-label="关闭" title="关闭" type="button" data-dialog-close>×</button></div><div class="dialog-body">${body}</div>`;
  dialog.querySelectorAll("[data-dialog-close]").forEach(button => button.addEventListener("click", () => dialog.close()));
}

function showOneTimeSecret(dialog, title, secret, warning, onFinish) {
  setDialogContent(dialog, title, `<div class="warning-banner">${escapeHTML(warning)}</div><div class="secret-box" role="status"></div><div class="form-actions"><button class="button" type="button" data-finish>完成</button></div>`);
  dialog.querySelector(".secret-box").textContent = secret;
  dialog.querySelector("[data-finish]").addEventListener("click", () => {
    dialog.close();
    if (onFinish) onFinish();
  });
}

function loginView(message = "") {
  clearStatusRefresh();
  document.querySelector("#app").innerHTML = `<main class="login-shell">
    <form class="login-panel" id="login-form">
      <div class="brand"><div class="brand-mark">CPA</div><div><strong>拼车管理</strong><span>CLIProxyAPI</span></div></div>
      ${message ? `<div class="error-banner">${escapeHTML(message)}</div>` : ""}
      <div class="field"><label for="username">用户名</label><input id="username" name="username" autocomplete="username" required minlength="3" maxlength="64"></div>
      <div class="field"><label for="password">密码</label><input id="password" name="password" type="password" autocomplete="current-password" required minlength="12" maxlength="128"></div>
      <button class="button" type="submit">登录</button>
    </form>
  </main>`;
  document.querySelector("#login-form").addEventListener("submit", async event => {
    event.preventDefault();
    const button = event.currentTarget.querySelector("button[type=submit]");
    const form = new FormData(event.currentTarget);
    setButtonBusy(button, true);
    try {
      const data = await request("/session", { method: "POST", body: JSON.stringify({ username: form.get("username"), password: form.get("password") }) });
      state.session = data;
      state.csrf = data.csrf_token || "";
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
    ["/", "概览"], ["/users", "用户管理"], ["/cars", "车辆管理"], ["/usage", "用量报表"], ["/audit", "审计记录"], ["/password", "修改密码"],
  ];
  return state.session?.role === "carpool_admin" ? admin : passenger;
}

function renderShell() {
  if (!state.session) return loginView();
  const navigationItems = navigation();
  document.querySelector("#app").innerHTML = `<div class="app-shell">
    <aside class="sidebar">
      <div class="brand"><div class="brand-mark">CPA</div><div><strong>拼车管理</strong><span>独立权限域</span></div></div>
      <nav class="nav">${navigationItems.map(([route, label]) => `<button type="button" data-route="${route}" aria-current="${state.route === route ? "page" : "false"}">${label}</button>`).join("")}</nav>
      <div class="sidebar-footer"><strong>${escapeHTML(state.session.display_name)}</strong><span>${state.session.role === "carpool_admin" ? "拼车管理员" : "乘客"}</span></div>
    </aside>
    <main class="main">
      <header class="topbar"><h1>${escapeHTML(routeTitles[state.route] || "拼车管理")}</h1><button class="button secondary compact" id="logout">退出登录</button></header>
      <div class="content" id="content"><div class="loading">正在加载...</div></div>
    </main>
  </div>`;
  document.querySelectorAll("[data-route]").forEach(button => button.addEventListener("click", () => { location.hash = `#${button.dataset.route}`; }));
  document.querySelector("#logout").addEventListener("click", async event => {
    setButtonBusy(event.currentTarget, true);
    try { await request("/session", { method: "DELETE" }); } catch (_) { /* Local state still ends. */ }
    state.session = null;
    state.csrf = "";
    loginView();
  });
  renderRoute();
}

async function renderRoute() {
  if (!state.session) return;
  clearStatusRefresh();
  const allowed = new Set(navigation().map(([route]) => route));
  if (!allowed.has(state.route)) {
    location.hash = state.session.must_change_password ? "#/password" : "#/";
    return;
  }
  renderShellFrameTitle();
  const content = document.querySelector("#content");
  content.innerHTML = `<div class="loading">正在加载...</div>`;
  try {
    if (state.route === "/password") return renderPassword(content);
    if (state.session.role === "carpool_admin") return renderAdminRoute(content);
    return renderPassengerRoute(content);
  } catch (error) {
    if (error.status === 401) {
      state.session = null;
      state.csrf = "";
      loginView("会话已失效，请重新登录");
      return;
    }
    content.innerHTML = `<div class="error-banner">${escapeHTML(error.message)}</div>`;
  }
}

function renderShellFrameTitle() {
  const title = document.querySelector(".topbar h1");
  if (title) title.textContent = routeTitles[state.route] || "拼车管理";
  document.querySelectorAll("[data-route]").forEach(button => button.setAttribute("aria-current", button.dataset.route === state.route ? "page" : "false"));
}

async function renderPassengerRoute(content) {
  if (state.route === "/") {
    const data = await request("/me/car");
    if (!data?.car) {
      content.innerHTML = `<div class="empty">当前没有可用车辆</div>`;
      return;
    }
    content.innerHTML = `<div class="section-header"><div><h2>${escapeHTML(data.car.name)}</h2><p>${escapeHTML(data.car.description || "当前车辆")}</p></div>${statusLabel(data.car.status)}</div>
      <div class="summary-grid">
        <div class="metric"><span>当前成员</span><strong>${formatNumber(data.car.member_count)}</strong></div>
        <div class="metric"><span>可用账号</span><strong>${formatNumber(data.car.available_account_count)}</strong></div>
        <div class="metric"><span>席位上限</span><strong>${data.car.seat_limit ? formatNumber(data.car.seat_limit) : "不限"}</strong></div>
        <div class="metric"><span>报表时区</span><strong>${escapeHTML(data.report_timezone || state.session.report_timezone || "UTC")}</strong></div>
      </div>`;
    return;
  }
  if (state.route === "/members") {
    const data = await request(`/me/members/usage?period=${encodeURIComponent(state.period)}`);
    content.innerHTML = `<div class="section-header"><div><h2>成员用量</h2><p>${escapeHTML(formatTime(data.data_from))} 至 ${escapeHTML(formatTime(data.data_to))}</p></div>${periodControl()}</div>
      ${usageCoverage(data.coverage)}${memberTable(data.items || [])}`;
    bindPeriod();
    return;
  }
  if (state.route === "/accounts") {
    const data = await request(`/me/accounts?period=${encodeURIComponent(state.period)}`);
    content.innerHTML = `<div class="section-header"><div><h2>车辆账号</h2><p>状态更新时间：${escapeHTML(formatTime(new Date()))}</p></div>${periodControl()}</div>${accountTable(data.items || [])}`;
    bindPeriod();
    scheduleAccountRefresh();
    return;
  }
  if (state.route === "/keys") return renderKeys(content, true);
}

function usageCoverage(coverage) {
  if (!coverage || (!coverage.unknown_usage_events && !coverage.incomplete_requests)) return "";
  return `<div class="warning-banner">存在 ${formatNumber(coverage.unknown_usage_events)} 条未知用量事件和 ${formatNumber(coverage.incomplete_requests)} 个不完整请求。</div>`;
}

function memberTable(items) {
  if (!items.length) return `<div class="empty">该周期暂无成员用量</div>`;
  return `<div class="table-wrap"><table><thead><tr><th>成员</th><th>状态</th><th class="numeric">请求</th><th class="numeric">成功</th><th class="numeric">失败</th><th class="numeric">Token</th><th class="numeric">未知</th></tr></thead><tbody>
    ${items.map(item => `<tr><td>${escapeHTML(item.display_name)}</td><td><span class="tag">${item.left ? "已离车" : "当前"}</span></td><td class="numeric">${formatNumber(item.logical_requests)}</td><td class="numeric">${formatNumber(item.succeeded)}</td><td class="numeric">${formatNumber(item.failed)}</td><td class="numeric">${formatNumber(item.known_total_tokens)}</td><td class="numeric">${formatNumber(item.unknown_usage_events)}</td></tr>`).join("")}
  </tbody></table></div>`;
}

function accountTable(items) {
  if (!items.length) return `<div class="empty">当前车辆还没有分配账号</div>`;
  return `<div class="table-wrap"><table><thead><tr><th>账号</th><th>供应商</th><th>状态</th><th>观测时间</th><th class="numeric">请求</th><th class="numeric">Token</th><th class="numeric">未知</th></tr></thead><tbody>
    ${items.map(item => `<tr><td>${escapeHTML(item.label || item.safe_label)}</td><td>${escapeHTML(item.provider)}</td><td>${statusLabel(item.stale ? "unknown" : item.status)}${item.stale ? ` <span class="tag">陈旧</span>` : ""}</td><td>${escapeHTML(formatTime(item.observed_at))}</td><td class="numeric">${formatNumber(item.usage?.logical_requests)}</td><td class="numeric">${formatNumber(item.usage?.known_total_tokens)}</td><td class="numeric">${formatNumber(item.usage?.unknown_usage_events)}</td></tr>`).join("")}
  </tbody></table></div>`;
}

async function renderKeys(content, reset) {
  const page = await loadPage("keys", "/me/api-keys", reset);
  const items = page.items;
  content.innerHTML = `<div class="section-header"><div><h2>API Key</h2><p>密钥只在创建时显示一次</p></div><button class="button" id="create-key">创建 Key</button></div>
    ${items.length ? `<div class="table-wrap"><table><thead><tr><th>名称</th><th>Key 引用</th><th>状态</th><th>创建时间</th><th>过期时间</th><th>最近使用</th><th></th></tr></thead><tbody>${items.map(item => `<tr><td>${escapeHTML(item.name)}</td><td class="code-ref">${escapeHTML(item.key_ref)}</td><td>${statusLabel(item.status)}</td><td>${escapeHTML(formatTime(item.created_at))}</td><td>${escapeHTML(formatTime(item.expires_at))}</td><td>${escapeHTML(formatTime(item.last_used_at))}</td><td>${item.status === "active" ? `<button class="button danger compact" data-revoke-key="${escapeHTML(item.key_ref)}">撤销</button>` : ""}</td></tr>`).join("")}</tbody></table></div>` : `<div class="empty">尚未创建 API Key</div>`}
    ${paginationFooter(page, "个 Key")}`;
  content.querySelector("#create-key").addEventListener("click", showKeyDialog);
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
      const data = await request("/me/api-keys", { method: "POST", body: JSON.stringify({ name: form.get("name"), expires_at: expiresAt ? new Date(expiresAt).toISOString() : null }) });
      showOneTimeSecret(dialog, "API Key 已创建", data.api_key, "关闭后无法再次查看，请立即配置客户端。", renderRoute);
    } catch (error) {
      toast(error.message);
      setButtonBusy(button, false);
    }
  });
}

async function renderAdminRoute(content) {
  if (state.route === "/") {
    const [users, cars] = await Promise.all([request("/admin/users?limit=1"), request("/admin/cars?limit=1")]);
    content.innerHTML = `<div class="section-header"><div><h2>运营概览</h2><p>当前拼车业务状态</p></div></div>
      <div class="summary-grid"><div class="metric"><span>用户总数</span><strong>${formatNumber(users.total)}</strong></div><div class="metric"><span>车辆总数</span><strong>${formatNumber(cars.total)}</strong></div><div class="metric"><span>模块状态</span><strong>${escapeHTML(statusText(state.session.module_status || "healthy"))}</strong></div><div class="metric"><span>报表时区</span><strong>${escapeHTML(state.session.report_timezone || "UTC")}</strong></div></div>`;
    return;
  }
  if (state.route === "/users") return renderUsers(content, true);
  if (state.route === "/cars") return renderCars(content, true);
  if (state.route === "/usage") return renderAdminUsage(content);
  if (state.route === "/audit") return renderAudit(content, true);
}

async function renderUsers(content, reset) {
  const page = await loadPage("users", "/admin/users", reset);
  const items = page.items;
  content.innerHTML = `<div class="section-header"><div><h2>用户</h2><p>共 ${formatNumber(page.total)} 名用户</p></div><button class="button" id="create-user">创建用户</button></div>
    ${items.length ? `<div class="table-wrap"><table><thead><tr><th>用户名</th><th>展示名</th><th>角色</th><th>状态</th><th>首次改密</th><th>创建时间</th><th></th></tr></thead><tbody>${items.map(item => `<tr><td>${escapeHTML(item.username)}</td><td>${escapeHTML(item.display_name)}</td><td>${item.role === "carpool_admin" ? "管理员" : "乘客"}</td><td>${statusLabel(item.status)}</td><td>${item.must_change_password ? "待完成" : "已完成"}</td><td>${escapeHTML(formatTime(item.created_at))}</td><td><button class="button secondary compact" data-manage-user="${escapeHTML(item.user_ref)}">管理</button></td></tr>`).join("")}</tbody></table></div>` : `<div class="empty">暂无用户</div>`}
    ${paginationFooter(page, "名用户")}`;
  content.querySelector("#create-user").addEventListener("click", showUserDialog);
  content.querySelector("[data-load-more]")?.addEventListener("click", async event => {
    setButtonBusy(event.currentTarget, true);
    try { await renderUsers(content, false); } catch (error) { toast(error.message); }
  });
  content.querySelectorAll("[data-manage-user]").forEach(button => button.addEventListener("click", () => {
    const user = items.find(item => item.user_ref === button.dataset.manageUser);
    if (user) showUserManagement(user).catch(error => toast(error.message));
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

  await loadKeys(true);
  const dialog = openDialog("管理用户", "", true);

  function paint() {
    const keyRows = keyPage.items.map(key => `<tr><td>${escapeHTML(key.name)}</td><td class="code-ref">${escapeHTML(key.key_ref)}</td><td>${statusLabel(key.status)}</td><td>${escapeHTML(formatTime(key.expires_at))}</td><td>${escapeHTML(formatTime(key.last_used_at))}</td><td>${key.status === "active" ? `<button class="button danger compact" data-admin-revoke-key="${escapeHTML(key.key_ref)}">撤销</button>` : ""}</td></tr>`).join("");
    setDialogContent(dialog, "管理用户", `<div class="entity-summary"><strong>${escapeHTML(user.username)}</strong><span>${user.role === "carpool_admin" ? "拼车管理员" : "乘客"} · ${escapeHTML(user.user_ref)}</span></div>
      <form id="edit-user-form">
        <div class="form-row"><div class="field"><label for="edit-display-name">默认展示名</label><input id="edit-display-name" name="default_display_name" value="${escapeHTML(user.display_name)}" required maxlength="64"></div><div class="field"><label for="edit-user-status">状态</label><select id="edit-user-status" name="status"><option value="active"${user.status === "active" ? " selected" : ""}>启用</option><option value="disabled"${user.status === "disabled" ? " selected" : ""}>禁用</option></select></div></div>
        <div class="form-actions"><button class="button secondary" type="button" data-reset-password>重置密码</button><button class="button" type="submit">保存用户</button></div>
      </form>
      <div class="subsection-head"><div><h3>API Key</h3><p>${formatNumber(keyPage.total)} 个记录</p></div>${user.role === "passenger" ? `<button class="button danger compact" type="button" data-revoke-all-keys>撤销全部</button>` : ""}</div>
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
  const page = await loadPage("cars", "/admin/cars", reset);
  const items = page.items;
  content.innerHTML = `<div class="section-header"><div><h2>车辆</h2><p>共 ${formatNumber(page.total)} 辆车辆</p></div><button class="button" id="create-car">创建车辆</button></div>
    ${items.length ? `<div class="table-wrap"><table><thead><tr><th>车辆</th><th>状态</th><th class="numeric">成员</th><th class="numeric">账号</th><th>席位</th><th></th></tr></thead><tbody>${items.map(item => `<tr><td><strong>${escapeHTML(item.name)}</strong><br><span class="muted">${escapeHTML(item.description || "")}</span></td><td>${statusLabel(item.status)}</td><td class="numeric">${formatNumber(item.member_count)}</td><td class="numeric">${formatNumber(item.account_count)}</td><td>${item.seat_limit ? formatNumber(item.seat_limit) : "不限"}</td><td><button class="button secondary compact" data-manage-car="${escapeHTML(item.car_ref)}">管理</button></td></tr>`).join("")}</tbody></table></div>` : `<div class="empty">暂无车辆</div>`}
    ${paginationFooter(page, "辆车辆")}`;
  content.querySelector("#create-car").addEventListener("click", showCarDialog);
  content.querySelector("[data-load-more]")?.addEventListener("click", async event => {
    setButtonBusy(event.currentTarget, true);
    try { await renderCars(content, false); } catch (error) { toast(error.message); }
  });
  content.querySelectorAll("[data-manage-car]").forEach(button => button.addEventListener("click", () => {
    const car = items.find(item => item.car_ref === button.dataset.manageCar);
    if (car) showCarManagement(car).catch(error => toast(error.message));
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
  const [members, accounts, users] = await Promise.all([
    request(`/admin/cars/${encodeURIComponent(car.car_ref)}/members`),
    request(`/admin/cars/${encodeURIComponent(car.car_ref)}/accounts`),
    fetchAllPages("/admin/users"),
  ]);
  const passengers = users.filter(user => user.role === "passenger" && user.status === "active");
  const candidates = (accounts.candidates || []).filter(candidate => !candidate.assigned_to_current_car);
  const passengerOptions = passengers.map(user => `<option value="${escapeHTML(user.user_ref)}" data-display-name="${escapeHTML(user.display_name)}">${escapeHTML(user.display_name)} · ${escapeHTML(user.username)}</option>`).join("");
  const candidateOptions = candidates.map(candidate => {
    const assignment = candidate.assigned ? "（将从其他车辆移入）" : "";
    return `<option value="${escapeHTML(candidate.candidate_ref)}">${escapeHTML(candidate.provider)} · ${escapeHTML(candidate.candidate_ref.slice(-8))} ${assignment}</option>`;
  }).join("");
  const dialog = openDialog("管理车辆", `<div class="entity-summary"><strong>${escapeHTML(car.name)}</strong><span>${escapeHTML(car.car_ref)}</span></div>
    <form id="edit-car-form">
      <div class="form-row"><div class="field"><label for="edit-car-name">名称</label><input id="edit-car-name" name="name" value="${escapeHTML(car.name)}" required maxlength="80"></div><div class="field"><label for="edit-car-status">状态</label><select id="edit-car-status" name="status"><option value="active"${car.status === "active" ? " selected" : ""}>启用</option><option value="disabled"${car.status === "disabled" ? " selected" : ""}>禁用</option><option value="retired"${car.status === "retired" ? " selected" : ""}>退役</option></select></div></div>
      <div class="field"><label for="edit-car-description">说明</label><textarea id="edit-car-description" name="description" maxlength="500">${escapeHTML(car.description || "")}</textarea></div>
      <div class="field"><label for="edit-seat-limit">席位上限（留空表示不限）</label><input id="edit-seat-limit" name="seat_limit" type="number" min="1" value="${car.seat_limit || ""}"></div>
      <div class="form-actions"><button class="button" type="submit">保存车辆</button></div>
    </form>
    <div class="subsection-head"><div><h3>成员</h3><p>${formatNumber(members.total)} 名当前成员</p></div></div>
    ${(members.items || []).length ? `<div class="compact-list">${members.items.map(item => `<div><span><strong>${escapeHTML(item.display_name)}</strong><small>${escapeHTML(formatTime(item.started_at))}</small></span><button class="button danger compact" data-remove-member="${escapeHTML(item.member_ref)}">移除</button></div>`).join("")}</div>` : `<div class="empty compact-empty">暂无成员</div>`}
    ${passengerOptions && car.status !== "retired" ? `<form id="add-member" class="inline-editor"><div class="field"><label for="member-user">乘客</label><select id="member-user" name="user_ref" required><option value="">选择乘客</option>${passengerOptions}</select></div><div class="field"><label for="member-display-name">车内展示名</label><input id="member-display-name" name="display_name" required maxlength="64"></div><button class="button compact" type="submit">加入或换入</button></form>` : `<p class="muted section-note">${car.status === "retired" ? "退役车辆不能接收新成员" : "没有可分配的启用乘客"}</p>`}
    <div class="subsection-head"><div><h3>账号</h3><p>${formatNumber(accounts.total)} 个当前账号</p></div></div>
    ${(accounts.items || []).length ? `<div class="compact-list">${accounts.items.map(item => `<div><span><strong>${escapeHTML(item.safe_label)}</strong><small>${escapeHTML(item.provider)} · ${escapeHTML(item.account_ref)}</small></span><button class="button danger compact" data-remove-account="${escapeHTML(item.account_ref)}">撤销</button></div>`).join("")}</div>` : `<div class="empty compact-empty">暂无账号</div>`}
    ${candidateOptions && car.status !== "retired" ? `<form id="add-account" class="inline-editor"><div class="field"><label for="account-candidate">上游账号</label><select id="account-candidate" name="candidate_ref" required><option value="">选择账号</option>${candidateOptions}</select></div><div class="field"><label for="account-label">安全标签</label><input id="account-label" name="safe_label" required maxlength="80"></div><button class="button compact" type="submit">分配或移入</button></form>` : `<p class="muted section-note">${car.status === "retired" ? "退役车辆不能接收新账号" : "当前没有可分配的运行时账号"}</p>`}`, true);

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
      dialog.close();
      toast("车辆已更新");
      renderRoute();
    } catch (error) {
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
      await request(`/admin/cars/${encodeURIComponent(car.car_ref)}/members`, { method: "POST", body: JSON.stringify({ user_ref: form.get("user_ref"), display_name: form.get("display_name") }) });
      dialog.close();
      toast("成员已加入或换入");
      renderRoute();
    } catch (error) {
      toast(error.message);
      setButtonBusy(button, false);
    }
  });

  dialog.querySelector("#add-account")?.addEventListener("submit", async event => {
    event.preventDefault();
    const button = event.currentTarget.querySelector("button[type=submit]");
    const form = new FormData(event.currentTarget);
    setButtonBusy(button, true);
    try {
      await request(`/admin/cars/${encodeURIComponent(car.car_ref)}/accounts`, { method: "POST", body: JSON.stringify({ candidate_ref: form.get("candidate_ref"), safe_label: form.get("safe_label") }) });
      dialog.close();
      toast("账号已分配或移入");
      renderRoute();
    } catch (error) {
      toast(error.message);
      setButtonBusy(button, false);
    }
  });

  dialog.querySelectorAll("[data-remove-member]").forEach(button => button.addEventListener("click", async () => {
    if (!confirm("移除后该乘客的新请求将无法使用本车账号，是否继续？")) return;
    setButtonBusy(button, true);
    try {
      await request(`/admin/cars/${encodeURIComponent(car.car_ref)}/members/${encodeURIComponent(button.dataset.removeMember)}`, { method: "DELETE" });
      dialog.close();
      toast("成员已移除");
      renderRoute();
    } catch (error) { toast(error.message); setButtonBusy(button, false); }
  }));

  dialog.querySelectorAll("[data-remove-account]").forEach(button => button.addEventListener("click", async () => {
    if (!confirm("撤销后该账号不会再参与本车的新请求，是否继续？")) return;
    setButtonBusy(button, true);
    try {
      await request(`/admin/cars/${encodeURIComponent(car.car_ref)}/accounts/${encodeURIComponent(button.dataset.removeAccount)}`, { method: "DELETE" });
      dialog.close();
      toast("账号分配已撤销");
      renderRoute();
    } catch (error) { toast(error.message); setButtonBusy(button, false); }
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
      <div class="field"><label for="report-range">时间范围</label><select id="report-range" name="range_mode"><option value="preset"${report.rangeMode === "preset" ? " selected" : ""}>预设周期</option><option value="custom"${report.rangeMode === "custom" ? " selected" : ""}>自定义 UTC 区间</option></select></div>
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
    ${usageCoverage(data.coverage)}${adminUsageTable(data)}`;
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

function renderPassword(content) {
  content.innerHTML = `<div class="section-header"><div><h2>修改密码</h2><p>更新后需要重新登录</p></div></div><form id="password-form" class="login-panel">
    <div class="field"><label for="current-password">当前密码</label><input id="current-password" name="current_password" type="password" autocomplete="current-password" required></div>
    <div class="field"><label for="new-password">新密码</label><input id="new-password" name="new_password" type="password" autocomplete="new-password" required minlength="12" maxlength="128"></div>
    <button class="button" type="submit">更新密码</button>
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
  state.route = location.hash.slice(1) || "/";
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
