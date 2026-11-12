"use strict";
const $ = (selector, root = document) => root.querySelector(selector);
const $$ = (selector, root = document) => [...root.querySelectorAll(selector)];
const state = {
  csrf: "",
  session: null,
  accounts: [],
  status: {},
  view: "dashboard",
  offset: 0,
  total: 0,
  limit: 50,
  editingAccount: null,
  version: "",
};
const esc = (value) =>
  String(value ?? "").replace(
    /[&<>"']/g,
    (c) =>
      ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[
        c
      ],
  );
const number = (value) => Number(value || 0).toLocaleString("zh-CN");
// Delegated handlers must not cancel native interactions such as toggling a
// <summary>. Only form submits and button clicks keep the old preventDefault.
const shouldPreventDefault = (e) =>
  !!e && (e.type === "submit" || (e.type === "click" && !!e.target?.closest?.("button")));

// Re-render a list of <details data-key> while keeping what the user expanded
// or collapsed and the keyboard focus, so polling does not fight the user.
// A focused control is found again by its record key plus its data-*
// attributes (for example data-task-cancel="<run id>"). If that control is
// gone (e.g. the run finished), focus falls back to the record's summary, and
// if the record itself is gone, to the list container.
const detailsMemory = new WeakMap();
const controlIdentity = (el) =>
  [...el.attributes].filter((a) => a.name.startsWith("data-")).map((a) => `${a.name}=${a.value}`).sort().join("&");
function renderDetails(container, html) {
  let memory = detailsMemory.get(container);
  if (!memory) {
    memory = new Map();
    detailsMemory.set(container, memory);
    container.addEventListener("toggle", (e) => {
      const d = e.target;
      if (d instanceof HTMLDetailsElement && d.dataset.key) memory.set(d.dataset.key, d.open);
    }, true);
  }
  const active = document.activeElement;
  const inside = active && active !== container && container.contains(active);
  const record = inside ? active.closest("details[data-key]") : null;
  const focus = inside ? {
    key: record?.dataset.key || "",
    summary: active.tagName === "SUMMARY",
    control: active.tagName !== "SUMMARY" ? controlIdentity(active) : "",
  } : null;
  container.innerHTML = html;
  for (const d of container.querySelectorAll("details[data-key]")) {
    if (memory.has(d.dataset.key)) d.open = memory.get(d.dataset.key);
  }
  if (!focus) return;
  const d = [...container.querySelectorAll("details[data-key]")].find((x) => x.dataset.key === focus.key);
  let target = null;
  if (d && focus.control) {
    target = [...d.querySelectorAll("button,[tabindex]")].find((el) => controlIdentity(el) === focus.control && !el.disabled) || null;
  }
  if (!target && d) target = d.querySelector("summary");
  if (!target) {
    if (!container.hasAttribute("tabindex")) container.setAttribute("tabindex", "-1");
    target = container;
  }
  target.focus({preventScroll: true});
}
const date = (value) =>
  value
    ? new Date(value * 1000).toLocaleString("zh-CN", { hour12: false })
    : "尚未同步";
function folderLabel(folder) {
  return String(folder).toLowerCase() === "inbox" ? "收件箱" : folder;
}
const providerNames = { qq: "QQ 邮箱", netease: "网易邮箱", icloud: "iCloud" };
let noticeTimer;
let syncPollTimer;
function notify(message, error = false) {
  const n = $("#notice");
  n.textContent = message;
  n.classList.toggle("error", error);
  n.hidden = false;
  clearTimeout(noticeTimer);
  noticeTimer = setTimeout(() => (n.hidden = true), error ? 10000 : 6000);
}
async function api(path, options = {}) {
  const method = options.method || "GET";
  const response = await fetch(path, {
    credentials: "same-origin",
    ...options,
    headers: {
      ...(method === "GET"
        ? {}
        : { "Content-Type": "application/json", "X-CSRF-Token": state.csrf }),
      ...options.headers,
    },
  });
  let data;
  try {
    data = await response.json();
  } catch {
    throw new Error("服务响应异常，请稍后重试");
  }
  if (!response.ok) {
    if (response.status === 401 && path !== "/api/login") {
      state.session = null;
      await checkSession();
    }
    throw new Error(data.error || "请求未完成，请重试");
  }
  return data;
}
async function busy(button, label, task) {
  const old = button.innerHTML;
  button.disabled = true;
  button.textContent = label;
  try {
    return await task();
  } finally {
    button.disabled = false;
    button.innerHTML = old;
  }
}
function hideDialogs() {
  $$("dialog[open]").forEach((d) => d.close());
}
async function checkSession() {
  const data = await api("/api/session");
  $("#loading").hidden = true;
  if (data.version) {
    state.version = data.version;
    $$(".app-version-label").forEach((el) => {
      el.textContent = `v${data.version}`;
    });
  }
  state.csrf = data.authenticated ? data.session.csrf : data.csrf;
  state.session = data.authenticated ? data.session : null;
  const normal = state.session && !state.session.must_change_credentials;
  $("#workspace").hidden = !normal;
  $("#auth-view").hidden = !!normal;
  $("#login-form").hidden = !!state.session;
  $("#setup-form").hidden = !state.session || normal;
  if (!normal) {
    if (window.clearMailSelection) window.clearMailSelection();
    clearTimeout(syncPollTimer);
    resetCredentials();
    hideDialogs();
    return;
  }
  $("#current-user").textContent = state.session.username;
  resetCredentials();
  await refresh();
  await showView(state.view || "dashboard");
  if (location.hash.startsWith("#task-run=") && window.openTaskRun) {
    await window.openTaskRun(decodeURIComponent(location.hash.slice(10)));
  }
  if (window.checkOAuthConsent) await window.checkOAuthConsent();
}
async function refresh() {
  const [accounts, status] = await Promise.all([
    api("/api/accounts"),
    api("/api/status"),
  ]);
  const accountsChanged = JSON.stringify(state.accounts) !== JSON.stringify(accounts);
  const indexChanged = accountsChanged || state.status.index_revision !== status.index_revision || state.status.messages !== status.messages || state.status.unread !== status.unread;
  state.accounts = accounts;
  state.status = status;

  if (status.version) {
    state.version = status.version;
    $$(".app-version-label").forEach((el) => {
      el.textContent = `v${status.version}`;
    });
  }

  const accText = `${number(status.accounts)} <small>个</small>`;
  const msgText = `${number(status.messages)} <small>封</small>`;
  const unreadText = `${number(status.unread)} <small>封</small>`;
  const syncText = status.last_sync ? `最近同步 ${date(status.last_sync)}` : "还没有同步记录";

  if ($("#stat-accounts")) $("#stat-accounts").innerHTML = accText;
  if ($("#stat-messages")) $("#stat-messages").innerHTML = msgText;
  if ($("#stat-unread")) $("#stat-unread").innerHTML = unreadText;
  if ($("#sync-time")) $("#sync-time").textContent = syncText;

  if ($("#stat-inbox-accounts")) $("#stat-inbox-accounts").innerHTML = accText;
  if ($("#stat-inbox-messages")) $("#stat-inbox-messages").innerHTML = msgText;
  if ($("#stat-inbox-unread")) $("#stat-inbox-unread").innerHTML = unreadText;
  if ($("#sync-inbox-time")) $("#sync-inbox-time").textContent = syncText;

  $("#nav-count").textContent = number(status.messages);
  $("#retention-note").textContent =
    "同步全部历史邮件元数据，不限制总封数；正文按需读取，附件仅显示信息。文件夹名称跟随原邮箱；彻底删除后自动清理索引并回收空闲空间。";
  $("#retention-value").textContent = "全部历史邮件元数据";

  $("#side-accounts").innerHTML = accounts.length
    ? accounts
        .map(
          (a) =>
            `<div class="side-account" title="${esc(a.username)}"><span class="side-account-dot"></span><span>${esc(a.name)}</span></div>`,
        )
        .join("")
    : '<p class="muted small side-accounts-empty">还没有添加邮箱</p>';

  const old = $("#account-filter").value;
  $("#account-filter").innerHTML =
    '<option value="">全部邮箱</option>' +
    accounts
      .map((a) => `<option value="${esc(a.id)}">${esc(a.name)}</option>`)
      .join("");
  if (accounts.some((a) => a.id === old)) $("#account-filter").value = old;
  updateFolderFilter();

  if (accountsChanged || !$("#account-list").children.length) {
    renderAccounts();
  } else {
    updateAccountCardStatuses();
  }
  renderAutomatic();
  renderDashboard();

  const jobs = status.sync_jobs || [];
  const progress = $("#sync-progress");
  progress.hidden = !jobs.length;
  progress.textContent = jobs.map((job) => {
    const account = accounts.find((a) => a.id === job.account_id);
    const labels = {queued: "等待同步", running: "同步中", completed: "同步完成", failed: "同步失败，可重试", interrupted: "同步中断，可重试"};
    return `${account?.name || "邮箱"}：${labels[job.state] || job.state}，${number(job.messages)} 封，${job.folders_done}/${job.folders_total} 个文件夹${["running", "failed", "interrupted"].includes(job.state) && job.folder ? `（${folderLabel(job.folder)}）` : ""}${job.error ? `；${job.error}` : ""}`;
  }).join(" · ");

  clearTimeout(syncPollTimer);
  if (jobs.some((job) => ["queued", "running"].includes(job.state))) syncPollTimer = setTimeout(pollSync, 2000);
  else if (status.automatic_sync) syncPollTimer = setTimeout(pollSync, 5000);
  return indexChanged;
}

function getAccountStatus(account, autoItem, automaticEnabled) {
  if (autoItem && automaticEnabled) {
    const labels = {
      checking: "正在检查…",
      connecting: "正在连接 TLS…",
      syncing: "正在校对 · TLS 已连接",
      watching: "正在监控 · TLS 已连接",
      waiting: "等待读取",
      retrying: "连接中断 · 重连中",
    };
    const badgeClasses = {
      checking: "badge",
      connecting: "badge",
      syncing: "badge badge-warning",
      watching: "badge badge-success",
      waiting: "badge",
      retrying: "badge badge-danger",
    };
    const dotClasses = {
      checking: "status-dot status-dot-blue",
      connecting: "status-dot status-dot-blue",
      syncing: "status-dot status-dot-amber",
      watching: "status-dot status-dot-green",
      waiting: "status-dot status-dot-gray",
      retrying: "status-dot status-dot-red",
    };
    const state = autoItem.state || "";
    const mode = autoItem.mode === "idle" ? "收件箱推送 + 全文件夹校对" : autoItem.mode === "poll" ? "定期校对" : "增量更新";
    const checked = autoItem.checked_at ? `最近更新 ${date(autoItem.checked_at)}` : "待首次校对";
    return {
      label: labels[state] || state || "等待同步",
      badgeClass: badgeClasses[state] || "badge",
      dotClass: dotClasses[state] || "status-dot status-dot-gray",
      modeText: mode,
      checkedText: checked,
      error: autoItem.error || "",
    };
  }

  if (account?.synced_folders && account.synced_folders.length > 0) {
    return {
      label: `已同步 ${account.synced_folders.length} 个文件夹`,
      badgeClass: "badge badge-success",
      dotClass: "status-dot status-dot-green",
      modeText: "手动同步模式",
      checkedText: "索引已就绪",
      error: "",
    };
  }

  return {
    label: "配置已保存 · 尚未验证连接",
    badgeClass: "badge",
    dotClass: "status-dot status-dot-gray",
    modeText: "尚未同步",
    checkedText: "待首次连接诊断",
    error: "",
  };
}

function updateAccountCardStatuses() {
  const automatic = state.status.automatic;
  const autoMap = Object.fromEntries((automatic?.accounts || []).map((a) => [a.account_id, a]));
  for (const acc of state.accounts) {
    const card = $(`[data-account="${acc.id}"]`);
    if (!card) continue;
    const autoItem = autoMap[acc.id];
    const st = getAccountStatus(acc, autoItem, automatic?.enabled);
    const badgeEl = $(".account-status-badge", card);
    if (badgeEl) {
      badgeEl.className = `account-status-badge ${st.badgeClass}`;
      badgeEl.textContent = st.label;
    }
  }
}

function renderDashboard() {
  const container = $("#dashboard-account-status");
  if (!container) return;
  const automatic = state.status.automatic;
  const autoMap = Object.fromEntries((automatic?.accounts || []).map((a) => [a.account_id, a]));

  const engineBadge = $("#dashboard-engine-badge");
  const engineSummary = $("#dashboard-engine-summary");
  const syncModeNote = $("#dashboard-sync-mode-note");

  if (automatic?.enabled) {
    if (engineBadge) {
      engineBadge.textContent = "自动同步已开启";
      engineBadge.className = "badge badge-success";
    }
    if (engineSummary) {
      engineSummary.textContent = `收件箱优先使用推送，全部文件夹每轮完成后 ${automatic.interval_seconds} 秒再次校对；延迟受服务器和网络影响。`;
    }
    if (syncModeNote) {
      syncModeNote.textContent = `后台实时校对中（每 ${automatic.interval_seconds}s）`;
    }
  } else {
    if (engineBadge) {
      engineBadge.textContent = "自动同步已关闭";
      engineBadge.className = "badge";
    }
    if (engineSummary) {
      engineSummary.textContent = "自动同步已关闭，后台不会自动增量校对；可点击右上角「同步邮箱」或各邮箱卡片更新索引。";
    }
    if (syncModeNote) {
      syncModeNote.textContent = "自动同步未开启";
    }
  }

  if (!state.accounts.length) {
    container.innerHTML = '<p class="muted small empty-accounts-note">尚未添加任何邮箱。点击右上角「＋ 添加邮箱」开始使用。</p>';
    return;
  }

  container.innerHTML = state.accounts.map((acc) => {
    const item = autoMap[acc.id];
    const st = getAccountStatus(acc, item, automatic?.enabled);

    return `
      <div class="dash-account-card">
        <div class="dash-account-main">
          <span class="dash-account-avatar">
            ${esc(acc.provider === "icloud" ? "iC" : acc.provider === "qq" ? "QQ" : "易")}
          </span>
          <div class="dash-account-info">
            <div class="dash-account-header">
              <strong class="dash-account-name">${esc(acc.name)}</strong>
              <span class="badge badge-xs">${esc(providerNames[acc.provider] || acc.provider)}</span>
            </div>
            <p class="muted small dash-account-meta">
              ${esc(acc.username)} · ${esc(acc.host)}:${esc(acc.port)}
            </p>
          </div>
        </div>

        <div class="dash-account-status-side">
          <div class="dash-account-status-badge">
            <span class="${st.dotClass}" aria-hidden="true"></span>
            <span>${esc(st.label)}</span>
          </div>
          <div class="muted small dash-account-sub">
            ${esc(st.modeText)} · ${esc(st.checkedText)}
          </div>
        </div>
      </div>
    `;
  }).join("");
}

function renderAutomatic() {
  const automatic = state.status.automatic;
  const line = $("#automatic-status");
  const description = automatic?.enabled
    ? `自动同步已开启 · 收件箱优先使用推送，全部文件夹每轮完成后 ${automatic.interval_seconds} 秒再次校对；延迟受服务器和网络影响。`
    : "自动同步已关闭，可使用「同步邮箱」更新索引。";
  $("#automatic-setting").textContent = description;
  if (!automatic?.enabled) {
    for (const card of $$("[data-account]")) {
      const el = $(".automatic-account", card);
      if (el) el.textContent = "";
    }
    line.textContent = description;
    return;
  }
  const labels = {connecting: "正在连接", syncing: "正在校对", watching: "正在监控", waiting: "等待读取", retrying: "连接中断，自动重连中"};
  const details = (automatic?.accounts || []).map((item) => {
    const account = state.accounts.find((a) => a.id === item.account_id);
    const mode = item.mode === "idle" ? "收件箱推送 + 全文件夹校对" : item.mode === "poll" ? "定期校对" : "";
    const text = `${labels[item.state] || item.state}${mode ? ` · ${mode}` : ""}${item.folder ? ` · ${folderLabel(item.folder)}` : ""}${item.checked_at ? ` · 最近更新 ${date(item.checked_at)}` : ""}${item.error ? ` · ${item.error}` : ""}`;
    const card = $$("[data-account]").find((c) => c.dataset.account === item.account_id);
    if (card) $(".automatic-account", card).textContent = text;
    return `${account?.name || "邮箱"}：${text}`;
  });
  line.textContent = [description, ...details].join("\n");
}

async function pollSync() {
  if (!state.session || state.session.must_change_credentials) return;
  try {
    const changed = await refresh();
    if (changed && state.view === "inbox") await search();
  } catch (e) {
    notify(e.message, true);
    if (state.session) syncPollTimer = setTimeout(pollSync, 5000);
  }
}

function updateFolderFilter() {
  const old = $("#folder-filter").value,
    selected = $("#account-filter").value;
  const folders = [
    ...new Set(
      state.accounts
        .filter((a) => !selected || a.id === selected)
        .flatMap((a) => [...a.folders, ...(a.synced_folders || [])]),
    ),
  ];
  $("#folder-filter").innerHTML =
    '<option value="">全部文件夹</option>' +
    folders.map((f) => `<option value="${esc(f)}">${esc(folderLabel(f))}</option>`).join("");
  if (folders.includes(old)) $("#folder-filter").value = old;
}

const views = {
  dashboard: [
    "SYSTEM DASHBOARD",
    "系统仪表盘",
    "全景掌控所有邮箱的运行状态、实时同步进度与自动化任务。",
  ],
  inbox: ["MAIL INDEX", "邮件索引", "在一个地方，查看不同邮箱的来信。"],
  accounts: [
    "MAILBOX CONNECTIONS",
    "邮箱连接",
    "管理连接与访问范围，原始邮件保留在邮箱中。",
  ],
  rules: ["MAIL RULES", "整理规则", "配置原邮箱的分类规则与任务授权范围。"],
  tasks: ["MAIL TASKS", "定时与任务", "选择内置定时或外部 MCP 触发，统一查看原邮箱整理结果。"],
  organizer: ["MAIL ORGANIZER", "整理与 MCP", "配置原邮箱文件夹分类、整理任务和 MCP 连接。"],
  notifications: ["TASK NOTIFICATIONS", "TG 通知", "按需接收任务结果，成功时可以保持安静。"],
  audit: ["ACTIVITY LOG", "操作记录", "查看登录、同步和配置变更的足迹。"],
  system: [
    "SYSTEM SETTINGS",
    "系统设置",
    "管理登录账号与密码，查看版本信息和备份恢复方法。",
  ],
};

async function showView(view) {
  state.view = view;
  const [eyebrow, title, description] = views[view] || views.dashboard;
  $("#page-eyebrow").textContent = eyebrow;
  $("#page-title").textContent = title;
  $("#page-description").textContent = description;
  $("#breadcrumb").textContent = `工作台 / ${title}`;
  $(".head-actions").hidden = view === "system";
  if (view !== "system") resetCredentials();

  Object.keys(views).forEach((v) => {
    const pane = $(`#${v}-pane`);
    if (pane) pane.hidden = v !== view;
  });

  $$(".nav-button").forEach((b) =>
    b.classList.toggle("active", b.dataset.view === view),
  );

  if (view === "dashboard") renderDashboard();
  if (view === "inbox") await search();
  if (view === "audit") await loadAudits();
  if (view === "organizer" && window.loadOrganizer) await window.loadOrganizer();
  if (view === "rules" && window.loadRules) await window.loadRules();
  if (view === "tasks" && window.loadTasks) await window.loadTasks();
  if (view === "notifications" && window.loadNotifications) await window.loadNotifications();
}

function empty(title, description, action) {
  return `<div class="empty-state"><div class="empty-envelope" aria-hidden="true">✉</div><h3>${esc(title)}</h3><p>${esc(description)}</p>${action ? `<button class="primary" data-empty-action="${esc(action)}">${action === "add" ? "＋ 添加第一个邮箱" : "↻ 同步邮箱"}</button>` : ""}</div>`;
}

async function search() {
  const list = $("#message-list");
  if (!state.accounts.length) {
    list.innerHTML = empty(
      "工作台已就绪，接入你的第一个邮箱",
      "支持 QQ、网易和 iCloud。添加邮箱后自动同步来信，在这里开始浏览。",
      "add",
    );
    $("#pagination").hidden = true;
    $("#result-count").textContent = "0 封";
    return;
  }
  const params = new URLSearchParams();
  const data = new FormData($("#search-form"));
  for (const [key, value] of data) {
    if (value) params.set(key, key === "unread" ? "true" : value);
  }
  params.set("offset", state.offset);
  params.set("limit", state.limit);
  list.innerHTML = '<div class="loading">正在查询本地索引…</div>';
  try {
    const result = await api(`/api/messages?${params}`);
    if (state.offset > 0 && state.offset >= result.total) {
      state.offset = Math.max(0, Math.floor((result.total - 1) / state.limit) * state.limit);
      return search();
    }
    state.total = result.total;
    $("#result-count").textContent = `${number(result.total)} 封`;
    const names = Object.fromEntries(state.accounts.map((a) => [a.id, a.name]));
    state.pageMessages = result.messages;
    list.innerHTML = result.messages.length
      ? result.messages
          .map(
            (m) =>
              `<div class="mail-select-row">
                <input type="checkbox" class="message-select" data-select-ref="${esc(m.ref)}" aria-label="选择 ${esc(m.subject)}" />
                <button type="button" class="mail-row ${m.unread ? "unread" : ""}" data-ref="${esc(m.ref)}">
                  <span class="unread-mark" aria-label="${m.unread ? "未读" : "已读"}"></span>
                  <span class="mail-sender" title="${esc(m.sender)}">${esc(m.sender || "未知发件人")}</span>
                  <span class="mail-preview">
                    <span class="mail-subject">${esc(m.subject || "（无主题）")}</span>
                    <span class="mail-origin">
                      <span>${esc(names[m.account_id] || "邮箱")}</span>
                      <span>/</span>
                      <span>${esc(m.folder_label || folderLabel(m.folder))}</span>
                    </span>
                  </span>
                  <span class="mail-date">${esc(new Date(m.sent_at * 1000).toLocaleDateString("zh-CN"))}<br>${number(Math.ceil(m.size / 1024))} KiB</span>
                </button>
              </div>`,
          )
          .join("")
      : empty(
          state.status.messages ? "没有符合条件的邮件" : "还没有同步邮件",
          state.status.messages
            ? "试试其他关键词、日期或文件夹。"
            : "同步只读取元数据，邮件正文会在打开时读取。",
          state.status.messages ? "" : "sync",
        );
    if (window.restoreMailSelection) window.restoreMailSelection();
    $("#pagination").hidden = !result.total;
    $("#page-info").textContent =
      `${result.total ? state.offset + 1 : 0}–${Math.min(state.offset + state.limit, result.total)} / ${number(result.total)} 封`;
    $("#prev-page").disabled = state.offset === 0;
    $("#next-page").disabled = state.offset + state.limit >= result.total;
  } catch (e) {
    list.innerHTML = `<p class="error-inline">${esc(e.message)}</p>`;
    $("#pagination").hidden = true;
  }
}

function renderAccounts() {
  const autoMap = Object.fromEntries((state.status.automatic?.accounts || []).map((a) => [a.account_id, a]));
  $("#account-list").innerHTML = state.accounts.length
    ? state.accounts
        .map((a) => {
          const autoItem = autoMap[a.id];
          const st = getAccountStatus(a, autoItem, state.status.automatic?.enabled);

          return `
            <article class="panel account-card" data-account="${esc(a.id)}">
              <header>
                <span class="account-icon">${esc(a.provider === "icloud" ? "iC" : a.provider === "qq" ? "QQ" : "易")}</span>
                <div>
                  <h3>${esc(a.name)}</h3>
                  <span class="email">${esc(a.username)}</span>
                </div>
                <span class="account-status-badge ${st.badgeClass}">${esc(st.label)}</span>
              </header>
              <dl>
                <div><dt>服务商</dt><dd>${esc(providerNames[a.provider] || a.provider)}</dd></div>
                <div><dt>连接地址</dt><dd>${esc(a.host)}:${esc(a.port)} · TLS</dd></div>
                <div><dt>允许文件夹</dt><dd>${a.folders.length ? a.folders.map((f) => esc(folderLabel(f))).join(" / ") : "全部可读取文件夹"}</dd></div>
                <div><dt>凭据加密</dt><dd>${a.credential_source === "external" ? `外部 secret · ${esc(a.secret_file)}` : "本地加密保存"}</dd></div>
              </dl>
              <p class="automatic-account muted small" aria-live="polite"></p>
              <div class="account-actions">
                <button class="secondary" data-action="edit">编辑配置</button>
                <button class="secondary" data-action="diagnose">连接诊断</button>
                <button class="secondary" data-action="sync">同步邮件</button>
                <button class="danger" data-action="delete">移除连接</button>
              </div>
              <div class="diagnosis" hidden></div>
            </article>
          `;
        })
        .join("")
    : empty(
        "从第一个邮箱开始",
        "开启 IMAP 后，使用邮箱授权码或应用专用密码建立安全连接。",
        "add",
      );
}

async function syncAll(button) {
  if (!state.accounts.length) { openAccount(); return; }
  await busy(button, "提交同步…", async () => {
    for (const account of state.accounts) {
      await api(`/api/accounts/${account.id}/sync`, {method: "POST", body: "{}"});
    }
    await refresh();
    notify("已提交全部邮箱的历史同步；任务在服务器后台继续，可刷新页面查看进度");
  });
}

async function loadAudits() {
  const labels = {
    "admin.initialize": "初始化管理员",
    "admin.setup": "完成首次设置",
    "admin.recover": "本地恢复账号",
    "admin.credentials": "修改登录账号或密码",
    "auth.login": "管理员登录",
    "account.create": "添加邮箱",
    "account.update": "修改邮箱配置",
    "account.delete": "移除邮箱",
    "account.diagnose": "连接诊断",
    "mail.sync": "同步邮件",
    "mail.read": "读取正文",
  };
  const data = await api("/api/audits");
  $("#audit-list").innerHTML = data.length
    ? data
        .map(
          (a) =>
            `<tr><td>${esc(date(a.at))}</td><td>${esc(labels[a.action] || a.action)}</td><td class="ref">${esc(a.target)}</td><td>${esc({ ok: "完成", failed: "失败", denied: "拒绝" }[a.outcome] || a.outcome)}</td></tr>`,
        )
        .join("")
    : '<tr><td colspan="4">暂无操作记录</td></tr>';
}

function updateProvider() {
  const hosts = {
    qq: ["imap.qq.com"],
    netease: ["imap.163.com", "imap.126.com", "imap.yeah.net"],
    icloud: ["imap.mail.me.com"],
  };
  $("#host").innerHTML = (hosts[$("#provider").value] || [])
    .map((h) => `<option>${h}</option>`)
    .join("");
}

function updateCredentialMode() {
  const form = $("#account-form"), mode = $("#credential-mode").value;
  $("#external-option").hidden = mode !== "external";
  $("#password-label").hidden = mode !== "password";
  form.elements.password.required = mode === "password" && !state.editingAccount;
  form.elements.secret_file.required = mode === "external";
  if (mode !== "password") form.elements.password.value = "";
  if (mode !== "external") form.elements.secret_file.value = "";
}

function openAccount(account = null) {
  const form = $("#account-form");
  form.reset();
  state.editingAccount = account?.id || null;
  $("#account-error").textContent = "";
  $("#account-dialog-title").textContent = account ? "编辑邮箱配置" : "添加邮箱";
  $("#account-dialog-eyebrow").textContent = account ? "MANAGE MAILBOX" : "CONNECT A MAILBOX";
  $("#account-edit-note").hidden = !account;
  $("button[type=submit]", form).textContent = account ? "保存修改" : "加密保存邮箱";
  $('#credential-mode option[value="keep"]').hidden = !account;
  $("#credential-mode").value = account ? "keep" : "password";
  $("#account-password-hint").textContent = account
    ? "填写新授权码以替换当前授权；留空则保留当前授权。保存后不会回显。"
    : "保存后不会回显；不要填写邮箱网页登录密码。";
  if (account) {
    form.elements.name.value = account.name;
    form.elements.provider.value = account.provider;
    form.elements.username.value = account.username;
    form.elements.port.value = account.port;
    form.elements.folders.value = account.folders.join("\n");
  }
  updateProvider();
  if (account) form.elements.host.value = account.host;
  updateCredentialMode();
  $("#account-dialog").showModal();
}

$("#credential-mode").addEventListener("change", () => {
  updateCredentialMode();
  if ($("#credential-mode").value === "external") {
    $("#account-form").elements.secret_file.value =
      state.accounts.find((a) => a.id === state.editingAccount)?.secret_file || "";
  }
});

$("#account-dialog").addEventListener("close", () => {
  $("#account-form").reset();
  state.editingAccount = null;
});

async function readMessage(ref) {
  const dialog = $("#message-dialog"),
    content = $("#message-content");
  content.innerHTML = '<div class="loading">正在以只读方式安全获取正文…</div>';
  dialog.showModal();
  try {
    const result = await api(`/api/messages/${encodeURIComponent(ref)}`),
      m = result.message;
    content.innerHTML = `
      <h2 class="message-title">${esc(m.subject || "（无主题）")}</h2>
      <div class="message-meta">
        <div><strong>发件人：</strong>${esc(m.sender)}</div>
        <div><strong>接收时间：</strong>${esc(date(m.sent_at))}</div>
        <div><strong>文件夹：</strong>${esc(m.folder_label || folderLabel(m.folder))} · 原始${m.unread ? "未读" : "已读"}状态保持不变</div>
      </div>
      ${result.notice || result.truncated ? `<div class="info-strip message-dialog-notice">${esc(result.notice || "正文已按大小限制截断")}</div>` : ""}
      <pre class="message-body">${esc(result.text || (result.notice ? "" : "（没有可显示的文本正文）"))}</pre>
      ${result.attachments.length ? `
        <div class="attachment-list">
          <strong>附件信息（共 ${result.attachments.length} 个，仅显示元数据不自动下载）</strong>
          <ul>${result.attachments.map((a) => `<li>${esc(a.name || "未命名附件")} · ${esc(a.type)} · ${number(Math.ceil(a.size / 1024))} KiB</li>`).join("")}</ul>
        </div>` : ""}
    `;
  } catch (e) {
    content.innerHTML = `<p class="error-inline">${esc(e.message)}</p>`;
  }
}

$("#login-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const form = e.currentTarget;
  $("#login-error").textContent = "";
  try {
    await busy($("button[type=submit]", form), "正在登录…", async () => {
      await api("/api/login", {
        method: "POST",
        body: JSON.stringify(Object.fromEntries(new FormData(form))),
      });
      form.reset();
      await checkSession();
    });
  } catch (err) {
    $("#login-error").textContent = err.message;
  }
});

$("#setup-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const form = e.currentTarget,
    data = Object.fromEntries(new FormData(form));
  $("#setup-error").textContent = "";
  if (data.password !== data.confirm) {
    $("#setup-error").textContent = "两次输入的密码不一致";
    return;
  }
  delete data.confirm;
  try {
    await busy($("button[type=submit]", form), "正在保存…", async () => {
      await api("/api/setup", { method: "POST", body: JSON.stringify(data) });
      form.reset();
      await checkSession();
      notify("账号已设置，请使用新用户名和密码登录");
    });
  } catch (err) {
    $("#setup-error").textContent = err.message;
  }
});

function resetCredentials() {
  const form = $("#credentials-form");
  form.reset();
  form.elements.username.value = state.session?.username || "";
  $("#settings-username").textContent = state.session?.username || "";
  $("#settings-expires").textContent = state.session ? date(state.session.expires) : "";
  $("#credentials-error").textContent = "";
}

$("#reset-credentials").addEventListener("click", resetCredentials);
$("#credentials-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const form = e.currentTarget,
    data = Object.fromEntries(new FormData(form)),
    error = $("#credentials-error");
  error.textContent = "";
  if (data.password !== data.confirm) {
    error.textContent = "两次输入的密码不一致";
    return;
  }
  if (data.password && [...data.password].length < 12) {
    error.textContent = "新密码须至少 12 个字符";
    return;
  }
  if (data.username === state.session.username && !data.password) {
    error.textContent = "请修改用户名或填写新密码";
    return;
  }
  delete data.confirm;
  try {
    await busy($("button[type=submit]", form), "正在保存…", async () => {
      await api("/api/settings/credentials", { method: "POST", body: JSON.stringify(data) });
      form.reset();
      state.view = "dashboard";
      state.accounts = [];
      state.offset = 0;
      await checkSession();
      $("#login-form").elements.username.value = data.username;
      $("#login-form").elements.password.focus();
      notify("账号设置已保存，所有登录会话已退出，请重新登录");
    });
  } catch (err) {
    error.textContent = err.message;
  }
});

async function logout() {
  try {
    await api("/api/logout", { method: "POST", body: "{}" });
    hideDialogs();
    state.accounts = [];
    state.offset = 0;
    state.view = "dashboard";
    await checkSession();
  } catch (e) {
    notify(e.message, true);
  }
}

$("#logout").addEventListener("click", logout);
$("#setup-logout").addEventListener("click", logout);

$$(".nav-button").forEach((b) =>
  b.addEventListener("click", () =>
    showView(b.dataset.view).catch((e) => notify(e.message, true)),
  ),
);

["#add-account", "#side-add"].forEach((id) =>
  $(id).addEventListener("click", () => openAccount()),
);

$$("[data-close]").forEach((b) =>
  b.addEventListener("click", () => $(`#${b.dataset.close}`).close()),
);

$("#provider").addEventListener("change", updateProvider);

$("#account-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const form = e.currentTarget,
    data = Object.fromEntries(new FormData(form));
  data.folders = data.folders
    .split(/\r?\n/)
    .map((f) => f.trim())
    .filter(Boolean);
  data.port = Number(data.port);
  const editingID = state.editingAccount;
  $("#account-error").textContent = "";
  try {
    await busy($("button[type=submit]", form), "正在加密保存…", async () => {
      await api(editingID ? `/api/accounts/${encodeURIComponent(editingID)}` : "/api/accounts", {
        method: editingID ? "PUT" : "POST",
        body: JSON.stringify(data),
      });
      form.reset();
      $("#account-dialog").close();
      await refresh();
      await showView("accounts");
      notify(editingID ? "邮箱配置已更新，建议先运行连接诊断，再同步邮件" : "邮箱已保存，建议先运行连接诊断");
    });
  } catch (err) {
    $("#account-error").textContent = err.message;
  }
});

$("#account-list").addEventListener("click", async (e) => {
  const button = e.target.closest("[data-action]");
  if (!button) return;
  const card = button.closest("[data-account]"),
    id = card.dataset.account,
    account = state.accounts.find((a) => a.id === id);
  try {
    if (button.dataset.action === "edit") {
      openAccount(account);
    } else if (button.dataset.action === "delete") {
      if (
        !confirm(
          `移除「${account.name}」的连接和本地索引？原始邮箱中的邮件保持不变。`,
        )
      )
        return;
      await busy(button, "移除中…", async () => {
        await api(`/api/accounts/${id}`, { method: "DELETE" });
        await refresh();
        notify("邮箱连接已移除");
      });
    } else if (button.dataset.action === "diagnose") {
      await busy(button, "诊断中…", async () => {
        const result = await api(`/api/accounts/${id}/diagnose`, {
          method: "POST",
          body: "{}",
        });
        $(".diagnosis", card).textContent =
          `TLS 连接与登录成功。可用文件夹：${result.folders.map((f) => folderLabel(f) === f ? f : `${folderLabel(f)}（${f}）`).join(" / ")}`;
        $(".diagnosis", card).hidden = false;
      });
    } else {
      await busy(button, "同步中…", async () => {
        await api(`/api/accounts/${id}/sync`, {
          method: "POST",
          body: "{}",
        });
        await refresh();
        notify("历史同步已提交，服务器会分批读取全部邮件；进度显示在页面上方");
      });
    }
  } catch (err) {
    notify(err.message, true);
  }
});

$("#sync-all").addEventListener("click", (e) =>
  syncAll(e.currentTarget).catch((err) => notify(err.message, true)),
);

document.addEventListener("click", (e) => {
  const b = e.target.closest("[data-empty-action]");
  if (b) {
    if (b.dataset.emptyAction === "add") openAccount();
    else syncAll($("#sync-all")).catch((err) => notify(err.message, true));
    return;
  }

  const quick = e.target.closest("[data-quick-nav]");
  if (quick) {
    showView(quick.dataset.quickNav).catch((err) => notify(err.message, true));
  }
});

$("#message-list").addEventListener("click", (e) => {
  const b = e.target.closest("[data-ref]");
  if (b) readMessage(b.dataset.ref);
});

$("#search-form").addEventListener("submit", (e) => {
  e.preventDefault();
  state.offset = 0;
  search();
});

$("#account-filter").addEventListener("change", updateFolderFilter);
$("#reset-search").addEventListener("click", () => {
  $("#search-form").reset();
  updateFolderFilter();
  state.offset = 0;
  search();
});

$("#prev-page").addEventListener("click", () => {
  state.offset = Math.max(0, state.offset - state.limit);
  search();
});

$("#next-page").addEventListener("click", () => {
  state.offset += state.limit;
  search();
});

$("#refresh-audit").addEventListener("click", () =>
  loadAudits().catch((e) => notify(e.message, true)),
);

checkSession().catch((e) => {
  $("#loading").textContent = `无法连接服务：${e.message}。请刷新页面重试。`;
});

window.addEventListener("hashchange", () => {
  if (state.session && location.hash.startsWith("#task-run=") && window.openTaskRun) {
    window.openTaskRun(decodeURIComponent(location.hash.slice(10))).catch((e) => notify(e.message, true));
  }
});
