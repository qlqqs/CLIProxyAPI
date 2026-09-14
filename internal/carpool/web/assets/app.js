const apiBase = "/carpool/api/v1";
const pageLimit = 25;
const maximumSelectorItems = 1000;

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
  "/requests": "请求明细",
  "/pricing": "价格目录",
  "/retention": "数据保留",
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
      <div class="brand"><div class="brand-mark">cpa</div><div><strong>拼车工作台</strong><span>CLIProxyAPI · 安全登录</span></div></div>
      <div class="login-intro"><h1>欢迎回来</h1><p>登录后查看车辆、成员和上游账号状态。</p></div>
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
    ["/", "概览"], ["/users", "用户管理"], ["/cars", "车辆管理"], ["/usage", "用量报表"], ["/requests", "请求明细"], ["/pricing", "价格目录"], ["/retention", "数据保留"], ["/audit", "审计记录"], ["/password", "修改密码"],
  ];
  return state.session?.role === "carpool_admin" ? admin : passenger;
}

function renderShell() {
  if (!state.session) return loginView();
  const navigationItems = navigation();
  document.querySelector("#app").innerHTML = `<div class="app-shell">
    <header class="topbar">
      <div class="topbar-brand"><div class="brand-mark">cpa</div><div><strong>拼车工作台</strong><span>CLIProxyAPI</span></div></div>
      <div class="topbar-title"><span>${escapeHTML(routeTitles[state.route] || "拼车管理")}</span></div><nav class="nav" aria-label="主导航">${navigationItems.map(([route, label]) => `<button type="button" data-route="${route}" aria-current="${state.route === route ? "page" : "false"}">${label}</button>`).join("")}</nav>
      <div class="topbar-actions"><span class="user-chip"><i aria-hidden="true"></i>${escapeHTML(state.session.display_name)}</span><button class="button secondary compact" id="logout">退出</button></div>
    </header>
    <main class="main"><div class="content" id="content"><div class="loading">正在加载...</div></div></main>
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
  const title = document.querySelector(".topbar-title span");
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
      ${data.billing ? `<section class="billing-hero"><div><span class="eyebrow">本期乘客额度</span><h3>$${escapeHTML(data.billing.used_usd || "0")} <small>/ $${escapeHTML(data.billing.limit_usd || "0")}</small></h3><p>账期至 ${escapeHTML(formatTime(data.billing.period_to))} · 剩余 $${escapeHTML(data.billing.remaining_usd || "0")}</p></div>${billingMarkup(data.billing)}</section>` : ""}
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
  return `<div class="table-wrap"><table><thead><tr><th>成员</th><th>额度进度</th><th class="numeric">请求</th><th class="numeric">成功</th><th class="numeric">失败</th><th class="numeric">Token</th><th class="numeric">未知</th></tr></thead><tbody>
    ${items.map(item => `<tr><td><strong>${escapeHTML(item.display_name)}</strong><br><span class="tag">${item.left ? "已离车" : "当前"}</span></td><td>${billingMarkup(item.billing)}</td><td class="numeric">${formatNumber(item.logical_requests)}</td><td class="numeric">${formatNumber(item.succeeded)}</td><td class="numeric">${formatNumber(item.failed)}</td><td class="numeric">${formatNumber(item.known_total_tokens)}</td><td class="numeric">${formatNumber(item.unknown_usage_events)}</td></tr>`).join("")}
  </tbody></table></div>`;
}

function billingMarkup(billing) {
  if (!billing || billing.limit_usd == null || billing.limit_usd === "") return `<span class="quota-missing">待管理员设置额度</span>`;
  const progress = percentageMeter(billing.usage_percent, "已用金额比例");
  const meter = progress ? `<div class="quota-meter">${progress}<b>${escapeHTML(formatQuotaPercent(billing.usage_percent))}</b></div>` : "";
  const status = billing.status === "exhausted" || billing.status === "overage" ? "额度已用尽" : "可用";
  return `<div class="billing-meter"><div class="billing-line"><strong>$${escapeHTML(billing.used_usd || "0")}</strong><span>/ $${escapeHTML(billing.limit_usd)}</span><em>${escapeHTML(status)}</em></div>${meter}<small>剩余 $${escapeHTML(billing.remaining_usd || "0")}${billing.status === "overage" ? ` · 超额 $${escapeHTML(billing.overage_usd || "0")}` : ""} · ${escapeHTML(formatTime(billing.period_to))}${billing.unknown_cost_events ? ` · ${formatNumber(billing.unknown_cost_events)} 条费用未知` : ""}</small></div>`;
}

function accountTable(items) {
  if (!items.length) return `<div class="empty">当前车辆还没有分配账号</div>`;
  return `<div class="table-wrap account-table"><table><thead><tr><th>账号</th><th>供应商</th><th>CPA 原生配额</th><th>状态</th><th>观测时间</th><th class="numeric">请求</th><th class="numeric">Token</th><th class="numeric">未知</th></tr></thead><tbody>
    ${items.map(item => `<tr><td><strong>${escapeHTML(item.label || item.safe_label)}</strong></td><td>${escapeHTML(item.provider)}</td><td>${quotaMarkup(item.quota)}</td><td>${statusLabel(item.stale ? "unknown" : item.status)}${item.stale ? ` <span class="tag">健康数据陈旧</span>` : ""}</td><td>${escapeHTML(formatTime(item.observed_at))}</td><td class="numeric">${formatNumber(item.usage?.logical_requests)}</td><td class="numeric">${formatNumber(item.usage?.known_total_tokens)}</td><td class="numeric">${formatNumber(item.usage?.unknown_usage_events)}</td></tr>`).join("")}
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
  return `<div class="quota-meter">${meter}<b>${escapeHTML(formatQuotaPercent(percent))}</b></div>`;
}

function quotaWindowMarkup(window) {
  const status = window.status && window.status !== "observed" ? statusText(window.status) : "已观测";
  return `<div class="quota-window"><div class="quota-window-head"><strong>${escapeHTML(window.label || window.id || "配额窗口")}</strong><span>${escapeHTML(status)}</span></div>${quotaProgress(window) || `<div class="quota-status-only">${escapeHTML(status)}</div>`}<small>${window.reset_at ? `重置：${escapeHTML(formatTime(window.reset_at))}` : window.window_minutes ? `窗口：${formatNumber(window.window_minutes)} 分钟` : "重置时间：上游未提供"}</small></div>`;
}

function quotaMarkup(quota) {
  if (!quota || !quota.supported || !Array.isArray(quota.windows) || quota.windows.length === 0) {
    return `<span class="quota-missing">上游未提供</span>`;
  }
  const windows = quota.windows;
  const visible = windows.slice(0, 2).map(quotaWindowMarkup).join("");
  const rest = windows.length > 2 ? `<details class="quota-more"><summary>其余 ${formatNumber(windows.length - 2)} 个窗口</summary><div>${windows.slice(2).map(quotaWindowMarkup).join("")}</div></details>` : "";
  return `<div class="quota-stack">${quota.stale ? `<span class="quota-stale">数据陈旧 · ${escapeHTML(formatTime(quota.observed_at))}</span>` : ""}${visible}${rest}</div>`;
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
    ${(members.items || []).length ? `<div class="compact-list">${members.items.map(item => `<div><span><strong>${escapeHTML(item.display_name)}</strong><small>${escapeHTML(formatTime(item.started_at))} · ${item.billing && item.billing.status !== "not_configured" ? `本期 $${escapeHTML(item.billing.used_usd || "0")} / $${escapeHTML(item.billing.limit_usd || "0")} · 剩余 $${escapeHTML(item.billing.remaining_usd || "0")}` : (item.monthly_limit_usd == null ? "额度待设置" : `额度 $${escapeHTML(item.monthly_limit_usd)}`)}</small></span><span class="inline-actions"><button class="button secondary compact" data-edit-member-limit="${escapeHTML(item.member_ref)}" data-member-name="${escapeHTML(item.display_name)}" data-member-limit="${escapeHTML(item.monthly_limit_usd || "")}">调额</button><button class="button danger compact" data-remove-member="${escapeHTML(item.member_ref)}">移除</button></span></div>`).join("")}</div>` : `<div class="empty compact-empty">暂无成员</div>`}
    ${passengerOptions && car.status !== "retired" ? `<form id="add-member" class="inline-editor"><div class="field"><label for="member-user">乘客</label><select id="member-user" name="user_ref" required><option value="">选择乘客</option>${passengerOptions}</select></div><div class="field"><label for="member-display-name">车内展示名</label><input id="member-display-name" name="display_name" required maxlength="64"></div><div class="field"><label for="member-limit">月度额度（USD）</label><input id="member-limit" name="monthly_limit_usd" inputmode="decimal" pattern="[0-9]+(\\.[0-9]{1,9})?" placeholder="例如 25.00" required><small>必须填写；输入 0 会暂时禁止新请求。</small></div><button class="button compact" type="submit">加入或换入</button></form>` : `<p class="muted section-note">${car.status === "retired" ? "退役车辆不能接收新成员" : "没有可分配的启用乘客"}</p>`}
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
      const limit = String(form.get("monthly_limit_usd") || "").trim();
      await request(`/admin/cars/${encodeURIComponent(car.car_ref)}/members`, { method: "POST", body: JSON.stringify({ user_ref: form.get("user_ref"), display_name: form.get("display_name"), monthly_limit_usd: limit }) });
      dialog.close();
      toast("成员已加入或换入");
      renderRoute();
    } catch (error) {
      toast(error.message);
      setButtonBusy(button, false);
    }
  });

  dialog.querySelectorAll("[data-edit-member-limit]").forEach(button => button.addEventListener("click", () => {
    const editor = openDialog("调整月度额度", `<form id="member-quota-form"><p class="muted">${escapeHTML(button.dataset.memberName)} · 当前账期立即生效。调低到已用金额以下会暂停新请求，输入 0 可暂时停用。</p><div class="field"><label for="member-quota-value">月度额度（USD）</label><input id="member-quota-value" name="monthly_limit_usd" inputmode="decimal" pattern="[0-9]+(\\.[0-9]{1,9})?" value="${escapeHTML(button.dataset.memberLimit || "")}" placeholder="例如 25.00" required></div><div class="form-actions"><button class="button secondary" type="button" data-dialog-close>取消</button><button class="button" type="submit">保存额度</button></div></form>`);
    editor.querySelector("#member-quota-form").addEventListener("submit", async event => {
      event.preventDefault();
      const form = new FormData(event.currentTarget);
      const value = String(form.get("monthly_limit_usd") || "").trim();
      const submit = event.currentTarget.querySelector("button[type=submit]");
      setButtonBusy(submit, true);
      try {
        await request(`/admin/cars/${encodeURIComponent(car.car_ref)}/members/${encodeURIComponent(button.dataset.editMemberLimit)}/quota`, { method: "PATCH", body: JSON.stringify({ monthly_limit_usd: value }) });
        editor.close();
        dialog.close();
        toast("额度已立即生效");
        renderRoute();
      } catch (error) {
        toast(error.message);
        setButtonBusy(submit, false);
      }
    });
  }));

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
  reset_current_period: "重置本期用量",
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
  const impact = operation === "reset_current_period" ? "本期已确认累计会归零，限额和账期边界不变；在途请求完成后可能再次增加用量。" : operation === "closed_periods" ? "已结束账期汇总和关联引用会被删除。" : "本期已确认金额和费用未知数量不会改变；进行中及不完整请求不会删除。";
  confirmation.innerHTML = `<strong>确认${escapeHTML(retentionOperationLabels[operation] || "操作")}</strong><p>本批预计影响 ${formatNumber(preview.expected_count)} 项，在途或不完整请求 ${formatNumber(preview.in_flight_count)} 项。每批最多 1,000 个逻辑请求或账期，更多数据需重新预览。执行后不可撤销；${impact}</p><div class="retention-confirm-actions"><button class="button secondary compact" type="button" data-retention-cancel>取消</button><button class="button danger compact" type="button" data-retention-confirm>确认执行</button></div>`;
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
      const action = operation === "reset_current_period" ? "重置" : "删除";
      const unit = operation === "usage_details" ? "个逻辑请求" : "个账期";
      result.textContent = `${retentionOperationLabels[operation]}已完成：实际${action} ${formatNumber(job.deleted_count)} ${unit}；在途或不完整请求 ${formatNumber(job.in_flight_count)} 项。${operation === "reset_current_period" ? "在途请求完成后可能再次增加用量。" : "进行中及不完整请求仍受保护。"}每批最多 1,000 项，更多数据需重新预览。`;
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
  content.innerHTML = `<div class="section-header"><div><h2>请求明细</h2><p>一行一个逻辑请求，展开查看全部 CPA 上游事件。默认最近 30 天。</p></div></div>${filters}<div class="request-list">${page.items.length ? page.items.map(requestDetailMarkup).join("") : `<div class="empty">当前条件下没有请求记录</div>`}</div>${paginationFooter(page, "个逻辑请求")}`;
  bindRequestDetails(content, page.items);
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
  content.innerHTML = `<div class="section-header"><div><h2>数据保留</h2><p>当前明细保留：${escapeHTML(current)} · 来源：${escapeHTML(settings.source === "database" ? "管理端设置" : "配置文件默认")}</p></div></div><div class="retention-layout"><section class="retention-panel"><h3>明细保留期限</h3><p class="muted">仅影响请求和上游事件明细；账期金额汇总独立保存。</p><form id="retention-form"><label><input type="radio" name="days" value="90"${settings.effective_days === 90 ? " checked" : ""}>90 天</label><label><input type="radio" name="days" value="180"${settings.effective_days === 180 ? " checked" : ""}>半年（180 天）</label><label><input type="radio" name="days" value="365"${settings.effective_days === 365 ? " checked" : ""}>一年（365 天）</label><label><input type="radio" name="days" value="0"${settings.effective_days === 0 ? " checked" : ""}>永久保留</label><div class="form-actions"><button class="button" type="submit">保存设置</button><button class="button secondary" id="restore-retention" type="button">恢复配置默认</button></div></form></section><section class="retention-panel"><h3>明细清理</h3><p class="muted">删除已完成请求及关联事件，不改变账期已确认金额。</p><button class="button danger" data-retention-op="usage_details">预览并清理明细</button></section><section class="retention-panel"><h3>已结束账期</h3><p class="muted">删除账期汇总前请确认历史金额不再需要核对。</p><button class="button danger" data-retention-op="closed_periods">预览并删除账期</button></section><section class="retention-panel"><h3>重置本期用量</h3><p class="muted">归零当前期已确认累计，限额和账期边界保持不变；在途请求完成后可能再次增加。</p><button class="button danger" data-retention-op="reset_current_period">预览并重置</button></section></div>`;
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
