// Mail Auto 0.8.x regression check for audit items S2-01, S2-02, S2-03,
// S2-04 (page text) and S2-05: which mailbox a response or save belongs to
// (including actions taken while a switch is still loading, 0.8.2),
// edits made while an editor saves, rule condition buttons as edits, and old
// run links. It replaces the page's API with an in-memory fake (two mailboxes
// whose settings share the same version numbers), so it needs no mailbox and
// sends nothing anywhere. Usage: open a throwaway instance, log in, evaluate
// this file. Resolves to {pass, results}.
(async () => {
  const results = [];
  const check = (name, ok, detail = "") => results.push({ name, ok: !!ok, detail: ok ? "" : String(detail) });
  const wait = (ms) => new Promise((r) => setTimeout(r, ms));
  const until = async (cond, tries = 150) => {
    for (let i = 0; i < tries; i++) {
      if (cond()) return true;
      await wait(20);
    }
    return false;
  };
  const realApi = window.api, realConfirm = window.confirm, realNotify = window.notify;
  let answer = false;
  const asked = [], notes = [], sent = [], previews = [];
  window.confirm = (m) => (asked.push(m), answer);
  window.notify = (m, bad) => notes.push(String(m));
  const now = Math.floor(Date.now() / 1000);
  const accounts = ["a1", "a2"].map((id, i) => ({ id, name: i ? "B邮箱" : "A邮箱", username: `tester${i}@163.com`, provider: "netease", host: "imap.163.com", port: 993, folders: [], synced_folders: ["INBOX", "Archive"] }));
  // Same versions on both accounts: ownership must not rely on version conflicts.
  const policy = (id) => ({ account_id: id, version: 4, enabled: true, mode: "execute", source_folders: ["INBOX"], destinations: ["Archive"], actions: ["move"], allow_internal: true, protect_unread: true, protect_flagged: true, protected_senders: [], max_batch: id === "a1" ? 20 : 8, stable_seconds: 180, include_history: true, since: 0, expires: now + 86400 * 20 });
  const rule = (id) => ({ id: "r-" + id, account_id: id, name: "规则-" + id, version: 1, priority: 10, enabled: true, source_folders: ["INBOX"], match: { mode: "all", conditions: [{ field: "subject", operator: "contains", value: "账单" }], groups: [] }, action: "move", also: [], destination: "Archive" });
  const task = (id) => ({ id: "t-" + id, account_id: id, name: "任务-" + id, version: 1, enabled: false, trigger_mode: "manual", engine: "rules", mode: "observe", source_folders: ["INBOX"], max_batch: 8, protect_unread: true, protect_flagged: true, notify: "inherit", schedule: { frequency: "daily", hour: 2, minute: 0, weekday: 0, timezone: "Asia/Shanghai" } });
  const issues = { a1: [], a2: [{ code: "source_not_synced", area: "account", target: "Other", blocking: true }] };
  const gates = {};
  const hold = (key) => {
    let release;
    const p = new Promise((r) => (release = r));
    gates[key] = { p, release: () => { delete gates[key]; release(); } };
    return gates[key].release;
  };
  const owner = (url) => (/(\/|=)a2(\b|$)/.test(url) ? "a2" : "a1");
  // One GET area per endpoint, so areas can be delayed ("get:a2:tasks") or
  // failed (fails.add("a2:tasks")) separately.
  const kindOf = (url) => (url.startsWith("/api/tasks/readiness") ? "readiness" : /^\/api\/automation\/(\w+)/.exec(url)?.[1] || /^\/api\/([\w-]+)/.exec(url)?.[1]);
  const fails = new Set();
  window.api = async (url, options = {}) => {
    const method = options.method || "GET", body = options.body ? JSON.parse(options.body) : null;
    if (url === "/api/rules/preview") {
      // Read-only: recorded apart from writes. Page 1 and later pages can be delayed separately.
      previews.push(body);
      const k = body.cursor ? "preview-more" : "preview";
      if (gates[k]) await gates[k].p;
      return { items: [{ message: { subject: `预览-${body.rule_id}-${body.cursor || "p1"}`, folder: "INBOX" }, status: "matched", action: "keep", also: [], reasons: [] }], total: 2, next_cursor: body.cursor ? "" : "next-" + body.rule_id };
    }
    const hist = /^\/api\/rules\/(r-a[12])\/history$/.exec(url);
    if (hist) {
      if (gates.history) await gates.history.p;
      return [{ ...rule(hist[1].slice(2)), updated_at: now }];
    }
    if (method !== "GET") {
      sent.push({ url, method, body });
      if (gates.save) await gates.save.p;
      if (url === "/api/accounts") return { ...body, id: "a9" };
      if (url.startsWith("/api/accounts/")) return { ...body, id: url.split("/").pop() };
      if (url === "/api/rules") return { ...body, id: "r-new", version: 1 };
      if (url.startsWith("/api/rules/")) return { ...body, version: body.version + 1 };
      if (url === "/api/tasks") return { ...body, id: "t-new", version: 1 };
      if (url.startsWith("/api/tasks/")) return { ...body, version: body.version + 1 };
      if (url.startsWith("/api/automation/policy/")) return { ...body, version: body.version + 1 };
      if (url.startsWith("/api/folder-settings/")) return { ok: true };
      if (url.startsWith("/api/automation/batches/")) return {};
      if (url.startsWith("/api/task-runs/")) return {};
      throw new Error("unhandled " + method + " " + url);
    }
    const id = owner(url);
    const kind = kindOf(url);
    if (!url.startsWith("/api/task-runs/")) for (const k of ["get:" + id, `get:${id}:${kind}`]) if (gates[k]) await gates[k].p;
    if (fails.has(`${id}:${kind}`)) throw new Error("模拟加载失败");
    if (url === "/api/accounts") return accounts;
    if (url === "/api/status") return { version: "0.8.1", accounts: 2, messages: 0, unread: 0, folders: 0 };
    if (url === "/api/network") return { version: 1, global: { mode: "direct" }, agent: { mode: "inherit" }, telegram: { mode: "inherit" }, profiles: [] };
    if (url.startsWith("/api/automation/policy/")) return policy(id);
    if (url.startsWith("/api/automation/capabilities/")) return { folders: [{ name: "INBOX" }, { name: "Archive" }] };
    if (url.startsWith("/api/automation/batches")) return [{ id: "b-" + id, created_at: now - 60, state: "claimed", mode: "observe", run_key: "k-" + id, scanned: 0, policy_version: 4, items: [], decisions: [], operations: [], plan_ids: [] }];
    if (url.startsWith("/api/folder-settings/")) return { account_id: id, archive_folder: "Archive", trash_folder: "", writes_enabled: id === "a1", version: 2 };
    if (url.startsWith("/api/rules?")) return [rule(id)];
    if (url.startsWith("/api/tasks/readiness")) return [{ task_id: "t-" + id, ready: !issues[id].length, issues: issues[id] }];
    if (url.startsWith("/api/tasks?")) return [task(id)];
    if (url.startsWith("/api/task-runs/")) return { id: url.split("/").pop(), account_id: url.endsWith("b") ? "a2" : "a1", task_id: url.endsWith("b") ? "t-a2" : "t-a1" };
    if (/\/api\/tasks\/[^/]+\/runs$/.test(url)) return [];
    throw new Error("unhandled " + method + " " + url);
  };
  const edit = (sel, value) => {
    const el = $(sel);
    if (el.type === "checkbox") el.checked = value;
    else el.value = value;
    el.dispatchEvent(new Event("input", { bubbles: true }));
    el.dispatchEvent(new Event("change", { bubbles: true }));
  };
  const reset = async (account = "a1") => {
    for (const [f] of drafts) markClean(f);
    for (const d of $$("dialog[open]")) d.close();
    await wait(10);
    answer = true;
    state.accounts = accounts;
    if (state.view !== "automation") await showView("automation");
    if (autoScope.account !== account) {
      $("#auto-account").value = account;
      $("#auto-account").dispatchEvent(new Event("change", { bubbles: true }));
      await until(() => autoScope.account === account && $("#tasks-list").innerText.includes("任务-" + account));
    }
    answer = false;
    asked.length = 0;
    notes.length = 0;
  };
  const pageAccount = () => ({
    picker: $("#auto-account").value,
    rules: $("#rules-account").value,
    tasks: $("#tasks-account").value,
    folders: $("#organizer-account").value,
    summary: $("#auto-summary").innerText,
    rulesList: $("#rules-list").innerText,
    tasksList: $("#tasks-list").innerText,
  });
  const allB = (p) => p.picker === "a2" && p.rules === "a2" && p.tasks === "a2" && p.folders === "a2" && p.summary.includes("B邮箱") && p.rulesList.includes("规则-a2") && p.tasksList.includes("任务-a2") && !p.summary.includes("A邮箱");
  const allA = (p) => p.picker === "a1" && p.rules === "a1" && p.tasks === "a1" && p.folders === "a1" && p.summary.includes("A邮箱") && p.rulesList.includes("规则-a1") && p.tasksList.includes("任务-a1");
  const switchTo = (id) => {
    $("#auto-account").value = id;
    $("#auto-account").dispatchEvent(new Event("change", { bubbles: true }));
  };

  try {
    state.accounts = accounts;
    await showView("automation");
    await reset("a1");

    // ---- S2-01: late responses and saves across mailboxes ----
    let release = hold("get:a1");
    $("#refresh-automation").click();
    await wait(40);
    switchTo("a2");
    await until(() => $("#auto-summary").innerText.includes("B邮箱"));
    release();
    await wait(150);
    let p = pageAccount();
    check("S2-01 刷新 A 迟到后切到 B：选择器、摘要、规则、任务、文件夹均为 B", allB(p), JSON.stringify(p).slice(0, 300));
    check("S2-01 迟到的 A 未改写 B 的授权表单（每批 8）与写入开关（关）", $("#policy-max-batch").value === "8" && !$("#organizer-writes").checked, `${$("#policy-max-batch").value}/${$("#organizer-writes").checked}`);
    edit("#policy-max-batch", "7");
    let n = sent.length;
    $("#automation-policy-form").requestSubmit();
    await until(() => sent.length > n);
    let s = sent[n];
    check("S2-01 随后保存授权：写到 B，内容是 B 的（相同版本号）", s && s.url === "/api/automation/policy/a2" && s.body.account_id === "a2" && s.body.max_batch === 7, JSON.stringify(s));
    await wait(50);
    n = sent.length;
    $("#folder-settings-form").requestSubmit();
    await until(() => sent.length > n);
    s = sent[n];
    check("S2-01 随后保存文件夹设置：写到 B，写入开关仍为 B 的“关”", s && s.url === "/api/folder-settings/a2" && s.body.writes_enabled === false, JSON.stringify(s));

    // A → B → A: B's late response must not land on A.
    await reset("a1");
    release = hold("get:a2");
    switchTo("a2");
    await wait(40);
    switchTo("a1");
    await until(() => autoScope.account === "a1");
    await wait(80);
    release();
    await wait(150);
    p = pageAccount();
    check("S2-01 A→B→A：B 的迟到响应不覆盖 A", allA(p) && $("#policy-max-batch").value === "20" && $("#organizer-writes").checked, JSON.stringify(p).slice(0, 300));

    // A form not yet loaded for the shown account cannot be saved.
    await reset("a1");
    release = hold("get:a2");
    switchTo("a2");
    await wait(40);
    n = sent.length;
    notes.length = 0;
    $("#automation-policy-form").requestSubmit();
    $("#folder-settings-form").requestSubmit();
    await wait(80);
    check("S2-01 切换加载中：授权与文件夹表单都拒绝提交（不发送旧账号内容）", sent.length === n && notes.some((m) => m.includes("请等加载完成")), `${sent.length - n} sent; ${notes.join("|")}`);
    release();
    await until(() => $("#policy-max-batch").value === "8");
    edit("#policy-max-batch", "6");
    n = sent.length;
    $("#automation-policy-form").requestSubmit();
    await until(() => sent.length > n);
    check("S2-01 加载完成后可正常保存到 B", sent[n]?.url === "/api/automation/policy/a2" && sent[n]?.body.account_id === "a2", JSON.stringify(sent[n]));
    await wait(50);

    // Switching while a save is in flight.
    await reset("a1");
    edit("#policy-max-batch", "19");
    release = hold("save");
    n = sent.length;
    $("#automation-policy-form").requestSubmit();
    await until(() => sent.length > n);
    answer = true;
    switchTo("a2");
    await until(() => $("#auto-summary").innerText.includes("B邮箱"));
    answer = false;
    release();
    await wait(150);
    p = pageAccount();
    check("S2-01 保存中切换：保存仍写 A 的 URL 与内容", sent[n].url === "/api/automation/policy/a1" && sent[n].body.account_id === "a1" && sent[n].body.max_batch === 19, JSON.stringify(sent[n]));
    check("S2-01 保存中切换：A 的保存响应不改动 B 的页面，并提示已保存到 A", allB(p) && $("#policy-max-batch").value === "8" && notes.some((m) => m.includes("A邮箱") && m.includes("已保存")), notes.join("|"));

    // ---- S2-02: edits made while an editor saves ----
    const editors = [
      { kind: "邮箱", open: () => openAccount(null), prep: () => { edit('#account-form [name="username"]', "new@163.com"); $('#account-form [name="password"]').value = "test-only"; }, form: "#account-form", dialog: "#account-dialog", field: '#account-form [name="name"]', msg: "#account-error", first: "/api/accounts", second: "/api/accounts/a9" },
      { kind: "规则", open: () => $('[data-rule-command="edit"][data-rule-id="r-a1"]').click(), prep: () => {}, form: "#rule-editor", dialog: "#rule-editor-dialog", field: "#rule-name", msg: "#rule-editor-error", first: "/api/rules/r-a1", second: "/api/rules/r-a1", version: 2 },
      { kind: "新规则", open: () => $("#new-rule").click(), prep: () => { $('#rule-sources input[value="INBOX"]').checked = true; edit("#rule-action", "keep"); }, form: "#rule-editor", dialog: "#rule-editor-dialog", field: "#rule-name", msg: "#rule-editor-error", first: "/api/rules", second: "/api/rules/r-new", version: 1 },
      { kind: "任务", open: () => $('[data-task-command="edit"][data-task-id="t-a1"]').click(), prep: () => {}, form: "#task-editor", dialog: "#task-editor-dialog", field: "#task-name", msg: "#task-editor-error", first: "/api/tasks/t-a1", second: "/api/tasks/t-a1", version: 2 },
    ];
    for (const ed of editors) {
      await reset("a1");
      ed.open();
      await until(() => $(ed.dialog).open);
      ed.prep();
      edit(ed.field, "已提交名称");
      release = hold("save");
      n = sent.length;
      $(ed.form).requestSubmit();
      await until(() => sent.length > n);
      edit(ed.field, "保存期间的新名称");
      release();
      await until(() => $(ed.msg).textContent.includes("尚未保存") || !$(ed.dialog).open);
      await wait(30);
      check(`S2-02 ${ed.kind}：保存期间继续编辑，窗口保持打开、草稿保留并提示仍有修改`, $(ed.dialog).open && isDirty($(ed.form)) && $(ed.field).value === "保存期间的新名称" && $(ed.msg).textContent.includes("尚未保存"), `${$(ed.dialog).open}/${isDirty($(ed.form))}/${$(ed.field).value}/${$(ed.msg).textContent}`);
      check(`S2-02 ${ed.kind}：首次提交的是提交时的内容`, sent[n].url === ed.first && sent[n].body.name === "已提交名称", JSON.stringify(sent[n]).slice(0, 200));
      n = sent.length;
      $(ed.form).requestSubmit();
      await until(() => sent.length > n);
      await until(() => !$(ed.dialog).open);
      const s2 = sent[n];
      check(`S2-02 ${ed.kind}：再次保存更新同一记录（${ed.second}${ed.version ? ` v${ed.version}` : ""}），不重复新建`, s2 && s2.url === ed.second && s2.method === "PUT" && (!ed.version || s2.body.version === ed.version) && s2.body.name === "保存期间的新名称", JSON.stringify(s2).slice(0, 200));
      check(`S2-02 ${ed.kind}：无后续编辑的保存正常关闭并清理草稿`, !$(ed.dialog).open && !isDirty($(ed.form)), `${$(ed.dialog).open}/${isDirty($(ed.form))}`);
    }
    // Closed and reopened while the old save is in flight.
    for (const ed of editors.filter((e) => e.kind === "规则" || e.kind === "任务" || e.kind === "邮箱")) {
      await reset("a1");
      ed.open();
      await until(() => $(ed.dialog).open);
      ed.prep();
      edit(ed.field, "旧窗口的名称");
      release = hold("save");
      n = sent.length;
      $(ed.form).requestSubmit();
      await until(() => sent.length > n);
      answer = true;
      closeGuarded($(ed.dialog));
      answer = false;
      await wait(20);
      ed.open();
      await until(() => $(ed.dialog).open);
      ed.prep();
      edit(ed.field, "新窗口的编辑");
      notes.length = 0;
      release();
      await until(() => notes.some((m) => m.includes("已保存")));
      await wait(30);
      check(`S2-02 ${ed.kind}：旧窗口关闭重开后，旧请求的回调不关闭、不清理、不改动新编辑`, $(ed.dialog).open && isDirty($(ed.form)) && $(ed.field).value === "新窗口的编辑" && !$(ed.msg).textContent.includes("尚未保存"), `${$(ed.dialog).open}/${isDirty($(ed.form))}/${$(ed.field).value}/${$(ed.msg).textContent}`);
    }

    // The dialog close event is queued: closing and reopening in the same
    // task must not let the late event wipe the new opening's draft.
    await reset("a1");
    $('[data-rule-command="edit"][data-rule-id="r-a1"]').click();
    await until(() => $("#rule-editor-dialog").open);
    $("#rule-editor-dialog").close();
    $('[data-rule-command="edit"][data-rule-id="r-a1"]').click();
    edit("#rule-name", "立即重开后的编辑");
    await wait(60);
    check("S2-02 关闭后立即重开：迟到的关闭事件不清除新的草稿", $("#rule-editor-dialog").open && isDirty($("#rule-editor")) && $("#rule-name").value === "立即重开后的编辑", `${$("#rule-editor-dialog").open}/${isDirty($("#rule-editor"))}`);

    // ---- S2-03: rule condition buttons are edits ----
    const ruleOpen = async () => {
      await reset("a1");
      $('[data-rule-command="edit"][data-rule-id="r-a1"]').click();
      await until(() => $("#rule-editor-dialog").open);
    };
    await ruleOpen();
    $("#rule-conditions [data-remove-condition]").click();
    check("S2-03 仅点“移除条件”即为未保存修改", isDirty($("#rule-editor")) && dirtyLabels().includes("规则编辑"), dirtyLabels().join(","));
    $("#close-rule-editor").click();
    check("S2-03 关闭按钮询问；取消后窗口与修改保留", asked.length === 1 && $("#rule-editor-dialog").open && $("#rule-conditions").querySelectorAll("[data-condition-field]").length === 0, `${asked.length}/${$("#rule-editor-dialog").open}`);
    const esc = new Event("cancel", { cancelable: true });
    $("#rule-editor-dialog").dispatchEvent(esc);
    check("S2-03 Esc 询问；取消后不关闭", asked.length === 2 && esc.defaultPrevented, `${asked.length}/${esc.defaultPrevented}`);
    const unload = new Event("beforeunload", { cancelable: true });
    window.dispatchEvent(unload);
    check("S2-03 离开页面时提示（beforeunload）", unload.defaultPrevented);
    await ruleOpen();
    $("#rule-conditions [data-add-group]").click();
    const afterAdd = isDirty($("#rule-editor"));
    markClean($("#rule-editor"));
    $("#rule-conditions [data-remove-group]").click();
    check("S2-03 添加 / 移除子条件组均计为修改", afterAdd && isDirty($("#rule-editor")));
    await ruleOpen();
    $("#rule-conditions [data-add-condition]").click();
    check("S2-03 添加条件计为修改", isDirty($("#rule-editor")));
    await ruleOpen();
    check("S2-03 打开与重绘本身不制造未保存修改", !isDirty($("#rule-editor")));
    await ruleOpen();
    release = hold("save");
    n = sent.length;
    $("#rule-editor").requestSubmit();
    await until(() => sent.length > n);
    $("#rule-conditions [data-add-condition]").click();
    release();
    await until(() => $("#rule-editor-error").textContent.includes("尚未保存") || !$("#rule-editor-dialog").open);
    check("S2-03 保存期间点按钮修改：窗口保留且仍为未保存", $("#rule-editor-dialog").open && isDirty($("#rule-editor")) && $("#rule-editor-error").textContent.includes("尚未保存"), `${$("#rule-editor-dialog").open}/${isDirty($("#rule-editor"))}/${$("#rule-editor-error").textContent}`);

    // ---- S2-04: the readiness blocker names the grant folder ----
    await reset("a2");
    const blocker = $('[data-task-card="t-a2"] .task-blockers')?.innerText || "";
    check("S2-04 就绪提示指出授权中不再同步的目录并提供两个入口", blocker.includes("「Other」") && blocker.includes("整理授权") && !!$('[data-task-card="t-a2"] [data-task-fix="account"]') && !!$('[data-task-card="t-a2"] [data-task-fix="policy"]'), blocker);

    // ---- S2-05: old run links switch the whole page ----
    await reset("a1");
    edit("#policy-max-batch", "17");
    answer = false;
    await window.openTaskRun("run-b");
    p = pageAccount();
    check("S2-05 有未保存授权时打开 B 的运行链接并取消：仍是 A，草稿保留", asked.length === 1 && allA(p) && $("#policy-max-batch").value === "17" && isDirty($("#automation-policy-form")), `${asked.length}/${JSON.stringify(p).slice(0, 200)}`);
    answer = true;
    await window.openTaskRun("run-b");
    answer = false;
    p = pageAccount();
    check("S2-05 确认后整页切到 B：选择器、摘要、规则、任务、文件夹设置一致", allB(p) && $("#policy-max-batch").value === "8" && $("#automation-policy-form").dataset.account === "a2" && $("#folder-settings-form").dataset.account === "a2", JSON.stringify(p).slice(0, 300));
    check("S2-05 定位到 B 的任务运行记录", $("#task-runs-title").textContent.includes("任务-a2"), $("#task-runs-title").textContent);
    await reset("a1");
    await showView("dashboard");
    await window.openTaskRun("run-b");
    p = pageAccount();
    check("S2-05 从其他页面打开 B 的链接：进入自动整理且整页为 B", state.view === "automation" && allB(p), JSON.stringify(p).slice(0, 200));
    await reset("a1");
    await window.openTaskRun("run-a");
    check("S2-05 同邮箱链接不询问、不重载其他区域", asked.length === 0 && allA(pageAccount()) && $("#task-runs-title").textContent.includes("任务-a1"));

    // ---- S2-01 (0.8.2): actions while a switch is still loading ----
    // Stale A controls are put back by hand, so the handlers' own checks are
    // tested, not only the page hiding them.
    const stale = {
      ruleToggle: () => { $("#rules-list").insertAdjacentHTML("beforeend", '<button type="button" data-rule-command="toggle" data-rule-id="r-a1">x</button>'); $('#rules-list [data-rule-id="r-a1"]').click(); },
      ruleEdit: () => { $("#rules-list").insertAdjacentHTML("beforeend", '<button type="button" data-rule-command="edit" data-rule-id="r-a1">x</button>'); $('#rules-list [data-rule-command="edit"][data-rule-id="r-a1"]').click(); },
      ruleTask: () => { $("#rules-list").insertAdjacentHTML("beforeend", '<button type="button" data-rule-command="task" data-rule-id="r-a1">x</button>'); $('#rules-list [data-rule-command="task"][data-rule-id="r-a1"]').click(); },
      taskToggle: () => { $("#tasks-list").insertAdjacentHTML("beforeend", '<button type="button" data-task-command="toggle" data-task-id="t-a1">x</button>'); $('#tasks-list [data-task-command="toggle"][data-task-id="t-a1"]').click(); },
      taskRun: () => { $("#tasks-list").insertAdjacentHTML("beforeend", '<button type="button" data-task-command="run" data-task-id="t-a1">x</button>'); $('#tasks-list [data-task-command="run"][data-task-id="t-a1"]').click(); },
      taskEdit: () => { $("#tasks-list").insertAdjacentHTML("beforeend", '<button type="button" data-task-command="edit" data-task-id="t-a1">x</button>'); $('#tasks-list [data-task-command="edit"][data-task-id="t-a1"]').click(); },
      runContinue: () => { $("#task-runs-list").insertAdjacentHTML("beforeend", '<button type="button" data-task-continue="run-x">x</button>'); $('#task-runs-list [data-task-continue]').click(); },
      batchCancel: () => { $("#ai-runs").insertAdjacentHTML("beforeend", '<button type="button" data-cancel-batch="b-a1">x</button>'); $('#ai-runs [data-cancel-batch="b-a1"]').click(); },
      newRule: () => { const b = $("#new-rule"), was = b.disabled; b.disabled = false; b.click(); b.disabled = was; },
      newTask: () => { const b = $("#new-task"), was = b.disabled; b.disabled = false; b.click(); b.disabled = was; },
    };
    const noEditorOpen = () => !$("#rule-editor-dialog").open && !$("#task-editor-dialog").open;
    const tryAll = async (names) => {
      const before = sent.length;
      notes.length = 0;
      for (const n of names) stale[n]();
      await wait(80);
      return { sent: sent.slice(before), editors: noEditorOpen(), told: notes.some((m) => m.includes("正在加载所选邮箱")) };
    };

    // Everything for B delayed.
    await reset("a1");
    release = hold("get:a2");
    switchTo("a2");
    await wait(40);
    p = pageAccount();
    check("S2-01b B 加载中：旧规则与任务列表已移出页面，显示加载提示", !$('#rules-list [data-rule-id="r-a1"]') && !$('#tasks-list [data-task-id="t-a1"]') && $("#rules-list").innerText.includes("正在加载") && $("#tasks-list").innerText.includes("正在加载"), `${p.rulesList.slice(0, 40)} | ${p.tasksList.slice(0, 40)}`);
    check("S2-01b B 加载中：新建规则 / 新建任务按钮不可用，摘要不再显示 A", $("#new-rule").disabled && $("#new-task").disabled && !$("#auto-summary").innerText.includes("A邮箱"), $("#auto-summary").innerText.slice(0, 60));
    let r = await tryAll(["ruleToggle", "taskToggle", "taskRun", "runContinue", "batchCancel"]);
    check("S2-01b B 加载中：旧规则暂停、任务启用、手动运行、继续处理、取消外部批次都不发送请求", r.sent.length === 0 && r.told, JSON.stringify(r.sent).slice(0, 300));
    r = await tryAll(["newRule", "newTask", "ruleEdit", "taskEdit", "ruleTask"]);
    check("S2-01b B 加载中：新建 / 编辑规则与任务、规则创建任务都不打开编辑器、不发送请求", r.sent.length === 0 && r.editors && r.told, JSON.stringify(r).slice(0, 300));
    release();
    await until(() => !$("#new-task").disabled && !$("#new-rule").disabled && $("#auto-summary").innerText.includes("B邮箱"));
    p = pageAccount();
    check("S2-01b B 加载完成：列表为 B，新建按钮恢复可用", allB(p) && !$("#new-rule").disabled && !$("#new-task").disabled, JSON.stringify(p).slice(0, 200));

    // After loading, normal actions use B's ids, account and limits.
    let n2 = sent.length;
    $('[data-rule-command="toggle"][data-rule-id="r-a2"]').click();
    await until(() => sent.length > n2);
    check("S2-01b 加载后暂停规则：PUT /api/rules/r-a2，账号 a2", sent[n2]?.url === "/api/rules/r-a2" && sent[n2]?.body.account_id === "a2", JSON.stringify(sent[n2]));
    await wait(60);
    n2 = sent.length;
    $('[data-task-command="run"][data-task-id="t-a2"]').click();
    await until(() => sent.length > n2);
    check("S2-01b 加载后手动运行：POST /api/tasks/t-a2/run", sent[n2]?.url === "/api/tasks/t-a2/run" && sent[n2]?.body.task_id === "t-a2", JSON.stringify(sent[n2]));
    await wait(60);
    $("#new-task").click();
    await until(() => $("#task-editor-dialog").open);
    check("S2-01b 加载后新建任务：预填 B 的每批上限 8", $("#task-limit").value === "8" && $("#task-limit").max === "8", `${$("#task-limit").value}/${$("#task-limit").max}`);
    edit("#task-name", "B 的任务");
    n2 = sent.length;
    $("#task-editor").requestSubmit();
    await until(() => sent.length > n2);
    check("S2-01b 加载后保存新任务：POST /api/tasks，账号 a2、每批 8", sent[n2]?.url === "/api/tasks" && sent[n2]?.body.account_id === "a2" && sent[n2]?.body.max_batch === 8, JSON.stringify(sent[n2]).slice(0, 200));
    await until(() => !$("#task-editor-dialog").open);
    $("#new-rule").click();
    await until(() => $("#rule-editor-dialog").open);
    edit("#rule-name", "B 的规则");
    $('#rule-sources input[value="INBOX"]').checked = true;
    edit("#rule-action", "keep");
    n2 = sent.length;
    $("#rule-editor").requestSubmit();
    await until(() => sent.length > n2);
    check("S2-01b 加载后新建规则：POST /api/rules，账号 a2", sent[n2]?.url === "/api/rules" && sent[n2]?.body.account_id === "a2", JSON.stringify(sent[n2]).slice(0, 200));
    await until(() => !$("#rule-editor-dialog").open);
    await window.createTaskFromRule("r-a2");
    check("S2-01b 加载后规则创建任务：编辑器属于 B（规则 r-a2，每批上限 8）", $("#task-editor-dialog").open && $("#task-limit").max === "8" && $('#task-rules input[value="r-a2"]')?.checked !== false, `${$("#task-editor-dialog").open}/${$("#task-limit").max}`);
    answer = true;
    closeGuarded($("#task-editor-dialog"));
    answer = false;

    // Areas finish at different times: rules first, tasks later (and the reverse).
    await reset("a1");
    release = hold("get:a2:tasks");
    switchTo("a2");
    await until(() => $("#rules-list").innerText.includes("规则-a2"));
    check("S2-01b 规则先加载完成：规则区为 B 且可新建规则，任务区仍在加载", !$("#new-rule").disabled && $("#new-task").disabled && $("#tasks-list").innerText.includes("正在加载"));
    r = await tryAll(["taskRun", "taskToggle", "newTask"]);
    check("S2-01b 任务未加载：旧任务运行 / 启用 / 新建都不发送、不打开", r.sent.length === 0 && r.editors, JSON.stringify(r.sent).slice(0, 200));
    $('[data-rule-command="task"][data-rule-id="r-a2"]').click();
    await wait(80);
    check("S2-01b 任务未加载：B 的规则“创建任务”不使用 A 的任务授权缓存", !$("#task-editor-dialog").open && notes.some((m) => m.includes("正在加载所选邮箱")));
    release();
    await until(() => !$("#new-task").disabled);
    check("S2-01b 任务随后加载完成：任务区为 B 且恢复可用", $("#tasks-list").innerText.includes("任务-a2") && !$("#new-task").disabled);
    await reset("a1");
    release = hold("get:a2:capabilities");
    switchTo("a2");
    await until(() => $("#tasks-list").innerText.includes("任务-a2"));
    r = await tryAll(["ruleToggle", "newRule", "ruleEdit"]);
    check("S2-01b 能力（规则区）迟到、任务已加载：规则操作仍不发送、不打开", $("#new-rule").disabled && r.sent.length === 0 && r.editors, JSON.stringify(r.sent).slice(0, 200));
    release();
    await until(() => !$("#new-rule").disabled);
    check("S2-01b 能力随后加载完成：规则区恢复可用", $("#rules-list").innerText.includes("规则-a2"));

    // A failed load keeps blocking until a retry succeeds.
    await reset("a1");
    fails.add("a2:tasks");
    switchTo("a2");
    await until(() => $("#tasks-list").innerText.includes("加载失败"));
    r = await tryAll(["taskRun", "taskToggle", "newTask"]);
    check("S2-01b 任务加载失败：提示重试；旧任务运行 / 启用 / 新建仍被阻止", $("#tasks-list").innerText.includes("重试") && $("#new-task").disabled && r.sent.length === 0 && r.editors, JSON.stringify(r.sent).slice(0, 200));
    fails.delete("a2:tasks");
    $("#refresh-automation").click();
    await until(() => !$("#new-task").disabled);
    n2 = sent.length;
    $('[data-task-command="run"][data-task-id="t-a2"]').click();
    await until(() => sent.length > n2);
    check("S2-01b 重试成功后恢复：运行 B 的任务 POST /api/tasks/t-a2/run", sent[n2]?.url === "/api/tasks/t-a2/run", JSON.stringify(sent[n2]));
    await wait(60);

    // A → B → A while both loads are delayed: A's earlier load does not count.
    await reset("a1");
    release = hold("get:a2");
    switchTo("a2");
    await wait(40);
    const releaseA = hold("get:a1");
    switchTo("a1");
    await wait(40);
    r = await tryAll(["taskRun", "ruleToggle", "newTask"]);
    check("S2-01b A→B→A 且 A 重新加载中：A 先前的加载不算数，仍不发送", r.sent.length === 0 && r.editors, JSON.stringify(r.sent).slice(0, 200));
    releaseA();
    await until(() => !$("#new-task").disabled && $("#tasks-list").innerText.includes("任务-a1"));
    release();
    await wait(120);
    n2 = sent.length;
    $('[data-task-command="run"][data-task-id="t-a1"]').click();
    await until(() => sent.length > n2);
    check("S2-01b A→B→A：B 迟到不覆盖 A；A 加载后运行 t-a1", allA(pageAccount()) && sent[n2]?.url === "/api/tasks/t-a1/run", JSON.stringify(sent[n2]));
    await wait(60);

    // ---- S2-01-R2 (0.8.3): rule 试运行 / 历史 responses after a switch ----
    const previewOpen = () => $("#rule-preview-dialog").open, historyOpen = () => $("#rule-history-dialog").open;
    const closeViews = () => { for (const d of ["#rule-preview-dialog", "#rule-history-dialog"]) if ($(d).open) $(d).close(); };
    for (const [label, gate, cmd, open, items] of [["试运行", "preview", "preview", previewOpen, "#rule-preview-items"], ["历史", "history", "history", historyOpen, "#rule-history-items"]]) {
      // Late response after switching to B.
      await reset("a1");
      release = hold(gate);
      $(`[data-rule-command="${cmd}"][data-rule-id="r-a1"]`).click();
      await wait(30);
      switchTo("a2");
      await until(() => $("#tasks-list").innerText.includes("任务-a2"));
      release();
      await wait(120);
      check(`S2-01-R2 A 的${label}迟到、页面已切到 B：弹窗不打开，不显示 A 的结果`, !open() && allB(pageAccount()) && !$(items).innerText.includes("r-a1"), `${open()}/${$(items).innerText.slice(0, 60)}`);
      // A → B → A: the earlier A session is not accepted either.
      await reset("a1");
      release = hold(gate);
      $(`[data-rule-command="${cmd}"][data-rule-id="r-a1"]`).click();
      await wait(30);
      switchTo("a2");
      await until(() => $("#tasks-list").innerText.includes("任务-a2"));
      switchTo("a1");
      await until(() => $("#tasks-list").innerText.includes("任务-a1") && !$("#new-rule").disabled);
      release();
      await wait(120);
      check(`S2-01-R2 A→B→A 后 A 的旧${label}迟到：弹窗不打开`, !open() && allA(pageAccount()), `${open()}`);
      // Normal use on the shown mailbox still works.
      await reset("a2");
      $(`[data-rule-command="${cmd}"][data-rule-id="r-a2"]`).click();
      await until(open);
      check(`S2-01-R2 当前邮箱 B 的${label}正常打开并显示 r-a2`, open() && $(items).innerText.includes("a2") && !$(items).innerText.includes("r-a1"), $(items).innerText.slice(0, 80));
      $(cmd === "preview" ? "#close-rule-preview" : "#close-rule-history").click();
      check(`S2-01-R2 ${label}可正常关闭`, !open());
    }
    // Paging keeps the session's own mailbox, rule and cursor.
    await reset("a2");
    previews.length = 0;
    $('[data-rule-command="preview"][data-rule-id="r-a2"]').click();
    await until(previewOpen);
    $("#rule-preview-more").click();
    await until(() => previews.length > 1 && $("#rule-preview-items").innerText.includes("next-r-a2"));
    check("S2-01-R2 翻页：请求为 a2 / r-a2 / next-r-a2，结果追加在同一弹窗", JSON.stringify(previews[1]) === JSON.stringify({ account_id: "a2", rule_id: "r-a2", cursor: "next-r-a2", limit: 25 }) && $("#rule-preview-items").querySelectorAll(".organizer-item").length === 2, JSON.stringify(previews));
    // Paging after a switch sends nothing (the old session is over).
    await reset("a1");
    $('[data-rule-command="preview"][data-rule-id="r-a1"]').click();
    await until(previewOpen);
    switchTo("a2");
    await until(() => $("#tasks-list").innerText.includes("任务-a2"));
    previews.length = 0;
    $("#rule-preview-more").hidden = false;
    $("#rule-preview-more").click();
    await wait(60);
    check("S2-01-R2 切换后对旧试运行翻页：不发送请求，不混用 B 账号与 A 规则", previews.length === 0 && !previewOpen(), JSON.stringify(previews));
    // Close, reopen: the previous session's late page does not land in the new dialog.
    await reset("a1");
    $('[data-rule-command="preview"][data-rule-id="r-a1"]').click();
    await until(previewOpen);
    release = hold("preview-more");
    $("#rule-preview-more").click();
    await wait(30);
    $("#close-rule-preview").click();
    $('[data-rule-command="preview"][data-rule-id="r-a1"]').click();
    await until(() => previewOpen() && $("#rule-preview-items").querySelectorAll(".organizer-item").length === 1);
    release();
    await wait(120);
    check("S2-01-R2 关闭后重开：上一会话迟到的下一页不追加到新弹窗", previewOpen() && $("#rule-preview-items").querySelectorAll(".organizer-item").length === 1 && $("#rule-preview-summary").textContent.includes("已查看 1 /"), $("#rule-preview-summary").textContent);
    closeViews();
    // A second 试运行 started before the first returns replaces it.
    await reset("a1");
    previews.length = 0;
    release = hold("preview");
    $('[data-rule-command="preview"][data-rule-id="r-a1"]').click();
    await wait(30);
    $('[data-rule-command="preview"][data-rule-id="r-a1"]').click();
    await wait(30);
    release();
    await until(previewOpen);
    await wait(80);
    check("S2-01-R2 重复发起试运行：只显示最新一次的结果（1 条，无重复追加）", previews.length === 2 && previewOpen() && $("#rule-preview-items").querySelectorAll(".organizer-item").length === 1, `${previews.length}/${$("#rule-preview-items").querySelectorAll(".organizer-item").length}`);
    closeViews();

    // A request sent for A stays A's; its callback does not touch B's page.
    await reset("a1");
    release = hold("save");
    n2 = sent.length;
    $('[data-task-command="run"][data-task-id="t-a1"]').click();
    await until(() => sent.length > n2);
    switchTo("a2");
    await until(() => $("#tasks-list").innerText.includes("任务-a2"));
    notes.length = 0;
    release();
    await wait(150);
    check("S2-01b 已提交 A 的运行后切到 B：请求仍为 t-a1，回调不改动 B 的页面并说明属于 A", sent[n2].url === "/api/tasks/t-a1/run" && allB(pageAccount()) && !$("#task-runs-title").textContent.includes("任务-a1") && notes.some((m) => m.includes("A邮箱")), notes.join("|"));
  } catch (e) {
    check("脚本异常", false, e.stack || e);
  } finally {
    for (const [f] of drafts) markClean(f);
    for (const d of $$("dialog[open]")) d.close();
    window.api = realApi;
    window.confirm = realConfirm;
    window.notify = realNotify;
  }
  return { pass: results.every((r) => r.ok), results };
})()
