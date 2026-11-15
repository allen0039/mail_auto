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
  network: null,
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

// Unsaved-edit guard. A form opts in with guardForm(form, "名称"): user input
// marks it dirty and bumps a generation counter. Loaders skip re-rendering a
// dirty form; a save settles the draft only if nothing was typed while the
// request was in flight. Drafts live only in page memory.
const drafts = new Map();
const extraDrafts = [];
function guardForm(form, label) {
  if (!form || drafts.has(form)) return;
  form.dataset.guard = label;
  // session counts openings of the dialog holding the form (see showModal below).
  drafts.set(form, { gen: 0, dirty: false, session: 0 });
  const touch = (e) => {
    if (e.target?.closest?.("[data-guard-ignore]")) return;
    touchDraft(form);
  };
  form.addEventListener("input", touch);
  form.addEventListener("change", touch);
}
// A real edit that does not come from typing (e.g. a button that adds or
// removes a rule condition held in memory) counts like input.
function touchDraft(form) {
  const d = drafts.get(form);
  if (!d) return;
  d.gen++;
  d.dirty = true;
}
// Editor saves: capture a ticket at submit; after the response, editorState
// says whether the editor still shows exactly what was sent ("same"), was
// edited during the save ("edited"), or was closed (and maybe reopened for
// something else) in the meantime ("closed").
function editorTicket(form) {
  const d = drafts.get(form);
  return { form, gen: d.gen, session: d.session };
}
function editorState(t) {
  const d = drafts.get(t.form), dialog = t.form.closest("dialog");
  // The close event is queued, so an already closed dialog counts too.
  if (d.session !== t.session || (dialog && !dialog.open)) return "closed";
  return d.gen === t.gen ? "same" : "edited";
}
const STILL_UNSAVED = "已保存提交时的内容；保存期间的新修改尚未保存，请检查后再次保存。";

// 自动整理 page ownership. account is the mailbox the page shows; gen bumps
// on every (re)load and epoch on every account switch. A loader applies its
// response only while its ticket's gen is current; a save started for one
// account touches the page only while the same switch epoch is shown.
const autoScope = { account: "", gen: 0, epoch: 0 };
const autoTicket = () => ({ ...autoScope });
const autoCurrent = (t) => t.gen === autoScope.gen && t.account === autoScope.account;
const autoSameAccount = (t) => t.epoch === autoScope.epoch && t.account === autoScope.account;
const accountName = (id) => state.accounts.find((a) => a.id === id)?.name || id;
// A form rendered for an account carries data-account; a save refuses a form
// that is not (yet) rendered for the account the page shows.
function autoFormAccount(form) {
  const id = form.dataset.account || "";
  if (!id || id !== autoScope.account) throw new Error("页面正在加载所选邮箱的设置，请等加载完成后再保存");
  return id;
}
const isDirty = (form) => !!drafts.get(form)?.dirty;
const draftGen = (form) => drafts.get(form)?.gen || 0;
function markClean(form) {
  const d = drafts.get(form);
  if (d) d.dirty = false;
}
// Call with the generation captured at submit; true when the draft is now
// clean (the saved values are what the form shows).
function settleDraft(form, gen) {
  if (draftGen(form) !== gen) return false;
  markClean(form);
  return true;
}
function dirtyLabels(scope) {
  const out = [...drafts].filter(([f, d]) => d.dirty && (!scope || scope.contains(f))).map(([f]) => f.dataset.guard);
  for (const x of extraDrafts) if ((!scope || scope.contains(x.root)) && x.dirty()) out.push(x.label);
  return out;
}
// Asks before discarding unsaved edits inside scope (default: the whole page).
function confirmDiscard(scope) {
  const labels = dirtyLabels(scope);
  if (!labels.length) return true;
  if (!confirm(`有未保存的修改：${labels.join("、")}。放弃这些修改？`)) return false;
  for (const [f] of drafts) if (!scope || scope.contains(f)) markClean(f);
  for (const x of extraDrafts) if ((!scope || scope.contains(x.root)) && x.discard) x.discard();
  return true;
}
// Closes a dialog unless the user keeps its unsaved edits.
function closeGuarded(dialog) {
  if (confirmDiscard(dialog)) dialog.close();
}
window.addEventListener("beforeunload", (e) => {
  if (dirtyLabels().length) {
    e.preventDefault();
    e.returnValue = "";
  }
});
document.addEventListener("cancel", (e) => {
  // Escape on a dialog with unsaved edits asks first.
  if (e.target instanceof HTMLDialogElement && dirtyLabels(e.target).length && !confirmDiscard(e.target)) e.preventDefault();
}, true);
document.addEventListener("close", (e) => {
  // A closed dialog has been saved or discarded. The event is queued, so it
  // may arrive after the dialog was opened again: then it is stale.
  if (e.target instanceof HTMLDialogElement && !e.target.open) {
    for (const [f, d] of drafts) if (e.target.contains(f)) d.dirty = false;
  }
}, true);
// Each opening of a dialog starts a new, clean editing session, counted at
// the moment it opens; a save started in an earlier opening sees the change.
{
  const showModal = HTMLDialogElement.prototype.showModal;
  HTMLDialogElement.prototype.showModal = function () {
    for (const [f, d] of drafts) {
      if (!this.contains(f)) continue;
      d.dirty = false;
      d.session++;
    }
    return showModal.call(this);
  };
}
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
    if (window.resetNetwork) window.resetNetwork();
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
  const [accounts, status, network] = await Promise.all([
    api("/api/accounts"),
    api("/api/status"),
    api("/api/network"),
  ]);
  const accountsChanged = JSON.stringify(state.accounts) !== JSON.stringify(accounts);
  const indexChanged = accountsChanged || state.status.index_revision !== status.index_revision || state.status.messages !== status.messages || state.status.unread !== status.unread;
  state.accounts = accounts;
  state.status = status;
  state.network = network;

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
  automation: ["AUTOMATION", "自动整理", "规则、任务和它们需要的整理设置集中在这里；任务不能运行时直接显示原因。"],
  organizer: ["VERIFICATION & MCP", "核验记录与 MCP", "查看原邮箱写入的核验记录，配置外部 MCP 连接。"],
  agent: ["AGENT & MODEL", "Agent 与模型", "配置内置管理 Agent 的模型连接与授权；默认关闭。"],
  notifications: ["TELEGRAM", "Telegram", "Bot 凭据、任务通知与私聊交互集中在这里，通知与交互分别开关。"],
  audit: ["ACTIVITY LOG", "操作记录", "查看登录、同步和配置变更的足迹。"],
  network: ["OUTBOUND CONNECTIONS", "网络与代理", "管理全局、邮箱、TG 与 Agent 的出口，查看真实连接诊断。"],
  system: [
    "SYSTEM SETTINGS",
    "系统设置",
    "管理登录账号与密码，查看版本信息和备份恢复方法。",
  ],
};

// Old page names stay valid (links, saved hashes, other scripts) and open
// the section of the page that now holds them.
const viewAliases = { rules: ["automation", "#auto-rules"], tasks: ["automation", "#auto-tasks"] };
const paneOf = (view) => viewAliases[view]?.[0] || view;

async function showView(view) {
  const section = viewAliases[view]?.[1];
  view = paneOf(view);
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
  if (view === "automation" && window.loadAutomation) await window.loadAutomation();
  if (view === "notifications" && window.loadNotifications) await window.loadNotifications();
  if (view === "agent" && window.loadAgent) await window.loadAgent();
  if (view === "network" && window.loadNetwork) await window.loadNetwork();
  if ((view === "agent" || view === "notifications") && window.loadNetworkPolicies) await window.loadNetworkPolicies();
  if (section) $(section)?.scrollIntoView({ block: "start" });
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
                <div><dt>网络出口</dt><dd>${esc(a.network_label || "直连")}${a.proxy?.mode === "inherit" ? " · 跟随全局" : ""}</dd></div>
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

// Same allowlist as the server (mail.Providers); domains only pick a value
// from it and never add hosts.
const providerHosts = {
  qq: ["imap.qq.com"],
  netease: ["imap.163.com", "imap.126.com", "imap.yeah.net"],
  icloud: ["imap.mail.me.com"],
};
const domainServers = {
  "qq.com": ["qq", "imap.qq.com"],
  "foxmail.com": ["qq", "imap.qq.com"],
  "163.com": ["netease", "imap.163.com"],
  "126.com": ["netease", "imap.126.com"],
  "yeah.net": ["netease", "imap.yeah.net"],
  "icloud.com": ["icloud", "imap.mail.me.com"],
  "me.com": ["icloud", "imap.mail.me.com"],
  "mac.com": ["icloud", "imap.mail.me.com"],
};
// For a new mailbox the name and server follow the address until the user
// edits them; editing an existing mailbox never changes saved values.
const accountAuto = { name: false, server: false };
const domainOf = (address) => address.trim().toLowerCase().split("@")[1] || "";

function updateProvider() {
  $("#host").innerHTML = (providerHosts[$("#provider").value] || [])
    .map((h) => `<option>${h}</option>`)
    .join("");
}

function inferServer() {
  const form = $("#account-form"), address = form.elements.username.value;
  const domain = domainOf(address), match = domainServers[domain];
  const note = $("#account-provider-note");
  if (accountAuto.server && match) {
    form.elements.provider.value = match[0];
    updateProvider();
    form.elements.host.value = match[1];
    form.elements.port.value = 993;
  }
  if (accountAuto.name && address.includes("@")) {
    const local = address.trim().split("@")[0];
    const label = match ? { "qq.com": "QQ", "foxmail.com": "Foxmail", "163.com": "163", "126.com": "126", "yeah.net": "yeah", "icloud.com": "iCloud", "me.com": "iCloud", "mac.com": "iCloud" }[domain] : domain;
    form.elements.name.value = `${label} · ${local}`.slice(0, 80);
  }
  if (!domain) note.textContent = "支持 QQ / Foxmail、网易 163 / 126 / yeah.net、iCloud。";
  else if (match) note.textContent = `已识别：${providerNames[match[0]]} · ${form.elements.host.value}:${form.elements.port.value}`;
  else if (accountAuto.server)
    note.textContent = "无法根据域名识别服务商。目前支持 QQ / Foxmail、网易 163 / 126 / yeah.net、iCloud；若是这些服务的自定义域名，请在“高级设置”中选择服务商与主机。";
  else note.textContent = `使用高级设置中的服务器：${form.elements.host.value}:${form.elements.port.value}`;
  accountSummary();
}

// Lists what differs from the default for the address, so a collapsed
// section never hides a custom value.
function accountSummary() {
  const form = $("#account-form"), parts = [];
  const match = domainServers[domainOf(form.elements.username.value)];
  const defaultHost = match ? match[1] : providerHosts[form.elements.provider.value]?.[0];
  if (!match || form.elements.host.value !== defaultHost) parts.push(`${providerNames[form.elements.provider.value]} · ${form.elements.host.value}`);
  if (Number(form.elements.port.value) !== 993) parts.push(`端口 ${form.elements.port.value}`);
  const net = $("#account-network-policy");
  if (net.selectedIndex > 0) parts.push(`连接方式：${net.options[net.selectedIndex].text}`);
  const folders = form.elements.folders.value.split(/\r?\n/).filter((f) => f.trim()).length;
  if (folders) parts.push(`指定 ${folders} 个文件夹`);
  if ($("#credential-mode").value === "external") parts.push("外部 secret");
  $("#account-advanced-summary").textContent = parts.length ? `（非默认：${parts.join("，")}）` : "";
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
  accountAuto.name = !account;
  accountAuto.server = !account;
  $("#account-advanced").open = false;
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
  if (window.fillAccountNetwork) window.fillAccountNetwork(account);
  if (account) {
    $("#account-provider-note").textContent = `当前服务器：${account.host}:${account.port}`;
    accountSummary();
  } else inferServer();
  $("#account-dialog").showModal();
}

$("#account-form").addEventListener("input", (e) => {
  const name = e.target.name || e.target.id;
  if (name === "name") accountAuto.name = false;
  if (["provider", "host", "port"].includes(name)) accountAuto.server = false;
  if (name === "username") inferServer();
  else accountSummary();
});
$("#account-form").addEventListener("change", accountSummary);
guardForm($("#account-form"), "邮箱设置");

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

// Leaving a page with unsaved edits asks first; staying keeps the drafts.
function navigate(view) {
  const pane = $(`#${paneOf(state.view)}-pane`);
  if (paneOf(view) !== paneOf(state.view) && pane && !confirmDiscard(pane)) return;
  showView(view).catch((e) => notify(e.message, true));
}
$$(".nav-button").forEach((b) => b.addEventListener("click", () => navigate(b.dataset.view)));

["#add-account", "#side-add"].forEach((id) =>
  $(id).addEventListener("click", () => openAccount()),
);

$$("[data-close]").forEach((b) =>
  b.addEventListener("click", () => closeGuarded($(`#${b.dataset.close}`))),
);

$("#provider").addEventListener("change", () => {
  accountAuto.server = false;
  updateProvider();
  accountSummary();
});

$("#account-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const form = e.currentTarget,
    data = Object.fromEntries(new FormData(form));
  data.folders = data.folders
    .split(/\r?\n/)
    .map((f) => f.trim())
    .filter(Boolean);
  data.port = Number(data.port);
  if (window.accountNetworkPolicy) data.proxy = window.accountNetworkPolicy();
  const editingID = state.editingAccount;
  $("#account-error").textContent = "";
  if (!editingID && accountAuto.server && !domainServers[domainOf(data.username)]) {
    $("#account-error").textContent = "无法根据域名识别服务商：请在“高级设置”中选择服务商与主机后再保存。";
    $("#account-advanced").open = true;
    return;
  }
  const ticket = editorTicket(form);
  try {
    await busy($("button[type=submit]", form), "正在加密保存…", async () => {
      const saved = await api(editingID ? `/api/accounts/${encodeURIComponent(editingID)}` : "/api/accounts", {
        method: editingID ? "PUT" : "POST",
        body: JSON.stringify(data),
      });
      const after = editorState(ticket);
      if (after !== "same") {
        // Closed meanwhile (maybe reopened for another mailbox): leave the
        // dialog alone. Edited meanwhile: keep the dialog and the newer
        // edits; the next save updates the saved mailbox instead of adding
        // a second one.
        if (after === "edited" && saved?.id) {
          state.editingAccount = saved.id;
          $("#account-dialog-title").textContent = "编辑邮箱配置";
          $("#account-error").textContent = STILL_UNSAVED;
        }
        notify(after === "edited" ? STILL_UNSAVED : "邮箱配置已保存");
        await refresh();
        if (state.view === "accounts") await showView("accounts");
        return;
      }
      markClean(form);
      form.reset();
      $("#account-dialog").close();
      await refresh();
      await showView("accounts");
      // Show the result where the mailbox lives: its card carries the
      // connection and background-sync state and the diagnosis button.
      const card = $(`[data-account="${CSS.escape(saved?.id || editingID || "")}"]`);
      if (card) {
        card.scrollIntoView({ block: "center" });
        card.classList.add("just-saved");
        setTimeout(() => card.classList.remove("just-saved"), 4000);
      }
      notify(editingID ? "邮箱配置已更新；连接与同步状态见该邮箱卡片" : "邮箱已保存；后台自动同步会开始读取，状态见该邮箱卡片，可点“连接诊断”检查");
    });
  } catch (err) {
    // The draft stays in the dialog; the error says what to do next.
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
        if (window.diagnoseAccountNetwork) {
          await window.diagnoseAccountNetwork(id, $(".diagnosis", card));
          return;
        }
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
    navigate(quick.dataset.quickNav);
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
