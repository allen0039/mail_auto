// Mail Auto 0.8.1 regression check for audit items S2-01, S2-02, S2-03,
// S2-04 (page text) and S2-05: which mailbox a response or save belongs to,
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
  const asked = [], notes = [], sent = [];
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
  window.api = async (url, options = {}) => {
    const method = options.method || "GET", body = options.body ? JSON.parse(options.body) : null;
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
      throw new Error("unhandled " + method + " " + url);
    }
    const id = owner(url);
    if (gates["get:" + id] && !url.startsWith("/api/task-runs/")) await gates["get:" + id].p;
    if (url === "/api/accounts") return accounts;
    if (url === "/api/status") return { version: "0.8.1", accounts: 2, messages: 0, unread: 0, folders: 0 };
    if (url === "/api/network") return { version: 1, global: { mode: "direct" }, agent: { mode: "inherit" }, telegram: { mode: "inherit" }, profiles: [] };
    if (url.startsWith("/api/automation/policy/")) return policy(id);
    if (url.startsWith("/api/automation/capabilities/")) return { folders: [{ name: "INBOX" }, { name: "Archive" }] };
    if (url.startsWith("/api/automation/batches")) return [];
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
