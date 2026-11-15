"use strict";
// 自动整理: one account picker drives the existing rules, tasks and folder
// settings scripts (their own selects stay in the page, hidden), plus a
// read-only summary of the settings every task inherits.
(() => {
  const run = (fn) => async (e) => {
    if (shouldPreventDefault(e)) e.preventDefault();
    try {
      await fn(e);
    } catch (err) {
      notify(err.message, true);
    }
  };
  const picker = () => $("#auto-account");
  let shown = "", wanted = "";
  const editors = ["#rule-editor-dialog", "#task-editor-dialog"];
  // Dialogs that show one mailbox's records; closed when the page switches.
  const recordDialogs = [...editors, "#rule-preview-dialog", "#rule-history-dialog", "#task-detail-dialog"];

  function syncSelects(id) {
    const options = state.accounts.map((a) => `<option value="${esc(a.id)}">${esc(a.name)}</option>`).join("");
    for (const sel of ["#rules-account", "#tasks-account", "#organizer-account"]) {
      $(sel).innerHTML = options;
      $(sel).value = id;
    }
  }

  function summary(account, p, fs) {
    if (!account) {
      $("#auto-summary").innerHTML = '<p class="muted">请先在“邮箱连接”中添加邮箱。</p>';
      $("#auto-settings-note").textContent = "";
      return;
    }
    const now = Date.now() / 1000;
    const expired = p.expires <= now;
    const history = p.include_history
      ? p.since ? `自 ${date(p.since).split(" ")[0]} 起的历史邮件` : "全部历史邮件"
      : `${date(p.since).split(" ")[0]} 之后的新邮件`;
    const items = [
      ["邮箱", `${account.name} · ${account.username}`],
      ["整理授权", p.enabled ? "已开启" : "未开启", !p.enabled],
      ["执行模式", p.mode === "execute" ? "授权范围内执行" : "只观察（不移动邮件）"],
      ["每批数量", `最多 ${p.max_batch} 封`],
      ["保护", `${p.protect_unread ? "保留未读" : "不保留未读"} · ${p.protect_flagged ? "保留星标" : "不保留星标"}${(p.protected_senders || []).length ? ` · ${p.protected_senders.length} 个保护联系人` : ""}`],
      ["历史范围", history],
      ["授权到期", `${date(p.expires)}${expired ? "（已到期）" : ""}`, expired],
      ["原邮箱写入", fs?.writes_enabled ? "已允许" : "未允许", p.mode === "execute" && !fs?.writes_enabled],
      ["内置定时", p.allow_internal ? "已允许" : "未允许"],
    ];
    $("#auto-summary").innerHTML = `<dl class="auto-summary-list">${items
      .map(([k, v, warn]) => `<div class="${warn ? "is-warning" : ""}"><dt>${esc(k)}</dt><dd>${esc(v)}</dd></div>`)
      .join("")}</dl>`;
    const todo = [!p.enabled && "整理授权未开启", expired && "授权已到期", p.mode === "execute" && !fs?.writes_enabled && "原邮箱写入未允许"].filter(Boolean);
    $("#auto-settings-note").textContent = todo.length ? `· 需要处理：${todo.join("、")}` : "· 写入开关、文件夹映射与整理授权";
  }

  async function refreshSummary() {
    const t = autoTicket();
    const account = state.accounts.find((a) => a.id === t.account);
    if (!account) return summary(null);
    let p, fs;
    try {
      [p, fs] = await Promise.all([api(`/api/automation/policy/${encodeURIComponent(t.account)}`), window.loadFolderSettings()]);
    } catch (err) {
      if (autoCurrent(t)) $("#auto-summary").innerHTML = AUTO_FAILED;
      throw err;
    }
    if (autoCurrent(t) && fs) summary(account, p, fs);
  }

  // Every load of the page goes through here. A new ticket makes responses
  // of earlier loads stale; switching accounts also marks the editable forms
  // as not yet loaded, so they cannot be saved before the new account's
  // values are in them.
  async function show(id) {
    const switching = id !== autoScope.account;
    autoScope.account = id;
    autoScope.gen++;
    if (switching) {
      autoScope.epoch++;
      for (const f of ["#automation-policy-form", "#folder-settings-form"]) delete $(f).dataset.account;
      for (const d of recordDialogs) if ($(d).open) $(d).close();
      // The previous mailbox's records leave the page; each area shows its
      // own again (and re-enables its 新建 button) once loaded for this one.
      for (const l of ["#auto-summary", "#rules-list", "#tasks-list", "#task-runs-list", "#ai-runs"]) $(l).innerHTML = AUTO_LOADING;
      $("#new-rule").disabled = $("#new-task").disabled = true;
    }
    shown = id;
    picker().value = id;
    syncSelects(id);
    await Promise.all([window.loadRules(), window.loadTasks(), refreshSummary()]);
  }

  // Unsaved edits on the page or in its editors are dropped only after the
  // user agrees.
  function confirmLeave() {
    const scope = { contains: (el) => $("#automation-pane").contains(el) || editors.some((d) => $(d).contains(el)) };
    return confirmDiscard(scope);
  }

  window.loadAutomation = async () => {
    const previous = wanted || picker().value || shown;
    wanted = "";
    picker().innerHTML = state.accounts.map((a) => `<option value="${esc(a.id)}">${esc(a.name)} · ${esc(a.username)}</option>`).join("");
    if (state.accounts.some((a) => a.id === previous)) picker().value = previous;
    await show(picker().value);
  };

  // The one way to put the page on another mailbox from outside the picker
  // (old run links). Returns false when the user keeps unsaved edits.
  window.selectAutomationAccount = async (id) => {
    if (!state.accounts.some((a) => a.id === id)) throw new Error("该运行记录所属的邮箱已不存在");
    if (state.view !== "automation") {
      const pane = $(`#${paneOf(state.view)}-pane`);
      if (pane && !confirmDiscard(pane)) return false;
      if (!confirmLeave()) return false;
      wanted = id;
      await showView("automation");
      return true;
    }
    if (id === autoScope.account) return true;
    if (!confirmLeave()) return false;
    await show(id);
    return true;
  };

  // Called after a rule, task, grant or folder setting is saved: the summary
  // and every task's readiness depend on all of them.
  window.automationChanged = async () => {
    if (state.view !== "automation") return;
    await Promise.all([refreshSummary(), window.loadTasks()]);
  };

  window.openAutomationSettings = (area) => {
    $("#auto-settings").open = true;
    $(area === "folders" ? "#auto-folders" : "#auto-policy").scrollIntoView({ block: "start", behavior: "smooth" });
  };

  picker().addEventListener(
    "change",
    run(async () => {
      if (!confirmLeave()) {
        picker().value = shown;
        return;
      }
      await show(picker().value);
    }),
  );
  $("#refresh-automation").addEventListener("click", run(() => show(picker().value)));
})();
