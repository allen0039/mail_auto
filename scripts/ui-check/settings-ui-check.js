// Mail Auto 0.8.x settings UI check: 自动整理 hub, task readiness and
// results, rule → task shortcut, unsaved-draft protection, late/failed saves
// and the Telegram page. It replaces the page's API with an in-memory fake,
// so it needs no mailbox, model or bot and sends nothing anywhere.
// Usage: open a throwaway instance, log in, evaluate this file. Resolves to
// {pass, results}.
(async () => {
  const results = [];
  const check = (name, ok, detail = "") => results.push({ name, ok: !!ok, detail });
  const wait = (ms) => new Promise((r) => setTimeout(r, ms));
  const until = async (cond) => {
    for (let i = 0; i < 100; i++) {
      if (cond()) return true;
      await wait(30);
    }
    return false;
  };
  const realApi = window.api, realConfirm = window.confirm, realAccounts = state.accounts;
  let confirmAnswer = false;
  const asked = [];
  window.confirm = (m) => {
    asked.push(m);
    return confirmAnswer;
  };
  const now = Math.floor(Date.now() / 1000);
  const acc = { id: "a1", name: "测试邮箱", username: "tester@163.com", provider: "netease", host: "imap.163.com", port: 993, folders: [], synced_folders: ["INBOX", "Archive", "Bills"] };
  const acc2 = { ...acc, id: "a2", name: "第二个邮箱", username: "other@qq.com" };
  let policy = { account_id: "a1", version: 4, enabled: true, mode: "execute", source_folders: ["INBOX"], destinations: ["Archive"], actions: ["move"], allow_internal: true, protect_unread: true, protect_flagged: true, protected_senders: ["boss@example.com"], max_batch: 20, stable_seconds: 180, include_history: false, since: now - 86400 * 30, expires: now + 86400 * 20 };
  let folders = { account_id: "a1", archive_folder: "Archive", trash_folder: "", writes_enabled: true, version: 2 };
  let rules = [
    { id: "r1", account_id: "a1", name: "高优先级", version: 3, priority: 5, enabled: true, source_folders: ["INBOX"], match: { mode: "all", conditions: [], groups: [] }, action: "keep", destination: "" },
    { id: "r2", account_id: "a1", name: "账单归档", version: 2, priority: 10, enabled: true, source_folders: ["INBOX"], match: { mode: "all", conditions: [], groups: [] }, action: "move", destination: "Archive" },
    { id: "r3", account_id: "a1", name: "停用规则", version: 1, priority: 1, enabled: false, source_folders: ["INBOX"], match: { mode: "all", conditions: [], groups: [] }, action: "move", destination: "Archive" },
  ];
  const task = (id, name, extra = {}) => ({ id, account_id: "a1", name, version: 1, enabled: true, trigger_mode: "internal", engine: "rules", mode: "execute", source_folders: ["INBOX"], max_batch: 20, since: 0, protect_unread: true, protect_flagged: true, notify: "inherit", schedule: { frequency: "daily", hour: 2, minute: 0, weekday: 0, timezone: "Asia/Shanghai" }, next_run_at: now + 3600, ...extra });
  let tasks = [task("t1", "没有命中"), task("t2", "保护跳过"), task("t3", "失败"), task("t4", "待核验"), task("t5", "已整理"), task("t6", "未知状态"), task("t7", "从未运行", { enabled: false })];
  const run = (id, extra) => ({ id, task_id: "", state: "noop", source: "internal", mode: "execute", finalized: true, created_at: now - 60, scanned: 120, claimed: 0, completed: 0, failed: 0, unknown: 0, kept: 0, protected: 0, needs_ai: 0, queued: 0, running: 0, ...extra });
  const lastRuns = {
    t1: run("run1", {}),
    t2: run("run2", { protected: 3 }),
    t3: run("run3", { state: "failed", error_code: "permission_denied" }),
    t4: run("run4", { state: "needs_verification", claimed: 2, unknown: 2 }),
    t5: run("run5", { state: "completed", claimed: 4, completed: 4, protected: 1 }),
    t6: run("run6", { state: "limited", claimed: 1 }),
  };
  let readiness = {
    t1: { ready: true, issues: [] },
    t2: { ready: false, issues: [{ code: "internal_not_allowed", area: "policy", blocking: true }] },
    t3: { ready: false, issues: [{ code: "archive_folder_missing", area: "folders", target: "r2", blocking: true }] },
    t4: { ready: false, issues: [{ code: "something_new", area: "task", blocking: true }] },
    t5: { ready: true, issues: [] },
    t6: { ready: true, issues: [] },
    t7: { ready: true, issues: [{ code: "task_paused", area: "task", blocking: false }] },
  };
  let telegram = { version: 3, enabled: true, configured: true, chat_id: "12345", policy: "failures", recovery: false };
  let interaction = { version: 2, enabled: false, state: "off", credential_ready: true, binding: null };
  const gates = {};
  const failNext = {};
  const sent = [];
  window.api = async (url, opts = {}) => {
    const body = opts.body ? JSON.parse(opts.body) : {};
    const method = opts.method || "GET";
    const key = `${method} ${url.split("?")[0]}`;
    if (method !== "GET") sent.push({ key, body });
    if (gates[key]) await gates[key];
    if (failNext[key]) {
      delete failNext[key];
      throw new Error("保存失败：网络中断，请重试（network_error）");
    }
    if (url.startsWith("/api/rules?")) return url.includes("a1") ? rules : [];
    if (url.startsWith("/api/automation/policy/")) {
      if (method === "PUT") {
        policy = { ...body, version: policy.version + 1 };
        return policy;
      }
      return url.endsWith("a1") ? policy : { ...policy, account_id: "a2", enabled: false, source_folders: [] };
    }
    if (url.startsWith("/api/automation/capabilities/")) return { folders: acc.synced_folders.map((name) => ({ name })) };
    if (url.startsWith("/api/automation/batches")) return [];
    if (url.startsWith("/api/tasks/readiness")) return url.includes("a1") ? tasks.map((t) => ({ task_id: t.id, ...readiness[t.id], last_run: lastRuns[t.id] ? { ...lastRuns[t.id], task_id: t.id } : undefined })) : [];
    if (url.startsWith("/api/tasks?")) return url.includes("a1") ? tasks : [];
    if (url === "/api/tasks" && method === "POST") {
      const t = { ...body, id: "t-new", version: 1, enabled: false, mode: "observe" };
      tasks = [...tasks, t];
      readiness[t.id] = { ready: true, issues: [] };
      return t;
    }
    if (/^\/api\/tasks\/[^/]+\/runs$/.test(url)) {
      const id = url.split("/")[3];
      return lastRuns[id] ? [{ ...lastRuns[id], task_id: id }] : [];
    }
    if (url.startsWith("/api/folder-settings/")) {
      if (method === "PUT") {
        folders = { ...folders, ...body, version: folders.version + 1 };
        return folders;
      }
      return url.endsWith("a1") ? folders : { ...folders, account_id: "a2", writes_enabled: false };
    }
    if (url === "/api/notifications/telegram") {
      if (method === "PUT") {
        telegram = { ...telegram, enabled: body.enabled, chat_id: body.chat_id, policy: body.policy, recovery: body.recovery, version: telegram.version + 1 };
        return telegram;
      }
      return telegram;
    }
    if (url === "/api/notifications/deliveries") return [];
    if (url === "/api/telegram/interaction") {
      if (method === "PUT") {
        interaction = { ...interaction, enabled: body.enabled, state: body.enabled ? "online" : "off", version: interaction.version + 1 };
        return interaction;
      }
      return interaction;
    }
    if (url === "/api/network") return { version: 1, global: { mode: "direct", proxy_id: "" }, telegram: { mode: "inherit", proxy_id: "" }, agent: { mode: "inherit", proxy_id: "" }, profiles: [] };
    throw new Error("unexpected " + key);
  };
  const edit = (sel, value) => {
    const el = $(sel);
    if (el.type === "checkbox") el.checked = value;
    else el.value = value;
    el.dispatchEvent(new Event("input", { bubbles: true }));
    el.dispatchEvent(new Event("change", { bubbles: true }));
  };
  const card = (id) => $(`[data-task-card="${id}"]`);
  try {
    state.accounts = [acc, acc2];
    await showView("automation");
    const summary = $("#auto-summary").innerText;
    check("hub shows one account picker driving hidden selects", $("#auto-account").value === "a1" && $("#rules-account").hidden && $("#tasks-account").hidden && $("#rules-account").value === "a1" && $("#tasks-account").value === "a1" && $("#organizer-account").value === "a1");
    check("summary shows account, mode, batch, protection, history, expiry", ["测试邮箱", "授权范围内执行", "最多 20 封", "保留未读", "保留星标", "1 个保护联系人", "之后的新邮件", "授权到期", "原邮箱写入", "已允许"].every((s) => summary.includes(s)), summary);
    check("settings collapsed by default, rules and tasks visible", !$("#auto-settings").open && $("#auto-rules").offsetParent && $("#auto-tasks").offsetParent && $$("#rules-list .rule-record").length === 3);
    check("old routes land on the hub", (await showView("rules"), state.view === "automation") && (await showView("tasks"), state.view === "automation"));

    // Results: only server counts, unknown stays unknown.
    const result = (id) => $(".task-last-result", card(id))?.innerText || "";
    check("result: no match", result("t1").includes("没有可处理的邮件") && result("t1").includes("扫描 120 封"), result("t1"));
    check("result: protection skip", result("t2").includes("保护跳过") && result("t2").includes("3 封"), result("t2"));
    check("result: failure with code", result("t3").includes("执行失败") && result("t3").includes("permission_denied"), result("t3"));
    check("result: needs verification is not failure", result("t4").includes("写入待核验") && !result("t4").includes("执行失败"), result("t4"));
    check("result: completed", result("t5").includes("已整理") && result("t5").includes("4 封"), result("t5"));
    check("result: unbreakable state is unknown", result("t6").includes("服务器没有提供可细分的原因"), result("t6"));
    check("result: never ran", result("t7").includes("尚未运行"), result("t7"));
    check("result links to runs and verification", !!$('[data-task-command="history"]', card("t4")) && !!$('[data-quick-nav="organizer"]', card("t4")) && !$('[data-quick-nav="organizer"]', card("t1")));

    // Readiness from the server, with fixes.
    check("ready task says so", card("t1").innerText.includes("运行条件已满足"));
    check("blocker named in Chinese with code", card("t2").innerText.includes("整理授权未允许内置定时") && card("t2").innerText.includes("internal_not_allowed"));
    check("rule-specific blocker names the rule", card("t3").innerText.includes("「账单归档」需要归档文件夹"), card("t3").innerText);
    check("unknown code shown as unknown", card("t4").innerText.includes("未知原因") && card("t4").innerText.includes("something_new"));
    check("non-blocking note does not block", card("t7").innerText.includes("运行条件已满足") && card("t7").innerText.includes("任务已暂停"));
    $('[data-task-fix="policy"]', card("t2")).click();
    check("fix button opens the grant settings", $("#auto-settings").open);
    $("#auto-settings").open = false;
    $('[data-task-fix="folders"]', card("t3")).click();
    check("folder fix opens the same settings", $("#auto-settings").open);

    // Rule → task shortcut.
    $('[data-rule-command="task"][data-rule-id="r2"]').click();
    await until(() => $("#task-editor-dialog").open);
    const ruleChecks = $$("#task-rules input:checked").map((x) => x.value).sort().join();
    check("shortcut opens editor prefilled with account, sources and rules", $("#task-editor-dialog").open && $("#task-name").value.startsWith("账单归档") && $$("#task-sources input:checked").map((x) => x.value).join() === "INBOX" && !$("#task-rules-all").checked && ruleChecks === "r1,r2", ruleChecks);
    check("shortcut binds higher-priority rules and says why", $("#task-editor-error").textContent.includes("优先级更高"));
    check("new task stays a paused observation draft", $("#task-mode").value === "observe" && $("#task-trigger").value === "manual" && Number($("#task-limit").value) === 20);
    edit("#task-name", "改过的名称");
    asked.length = 0;
    confirmAnswer = false;
    $("#close-task-editor").click();
    check("closing an edited task editor asks and keeps it", asked.length === 1 && $("#task-editor-dialog").open && $("#task-name").value === "改过的名称");
    $("#task-editor").requestSubmit();
    await until(() => !$("#task-editor-dialog").open);
    const posted = sent.find((x) => x.key === "POST /api/tasks");
    check("saved task carries the binding", !!posted && posted.body.account_id === "a1" && posted.body.rule_ids.join() === "r1,r2" && posted.body.rule_versions.r2 === 2 && posted.body.name === "改过的名称", JSON.stringify(posted?.body));
    $('[data-rule-command="task"][data-rule-id="r3"]').click();
    await wait(50);
    check("disabled rule cannot seed a task", !$("#task-editor-dialog").open);

    // Unsaved policy draft vs refresh, navigation and account switch.
    $("#auto-settings").open = true;
    edit("#policy-max-batch", "7");
    await window.loadAutomation();
    check("refresh keeps unsaved grant edit", $("#policy-max-batch").value === "7" && $("#policy-status").textContent.includes("未保存"));
    asked.length = 0;
    confirmAnswer = false;
    $('.nav-button[data-view="agent"]').click();
    await wait(50);
    check("leaving with a draft asks and stays", asked.length === 1 && state.view === "automation" && $("#policy-max-batch").value === "7");
    edit("#auto-account", "a2");
    $("#auto-account").value = "a2";
    $("#auto-account").dispatchEvent(new Event("change", { bubbles: true }));
    await wait(80);
    check("account switch with a draft asks and stays", $("#auto-account").value === "a1" && $("#policy-max-batch").value === "7");

    // Failed save keeps the draft.
    failNext["PUT /api/automation/policy/a1"] = true;
    $("#automation-policy-form").requestSubmit();
    await wait(120);
    check("failed save keeps draft and dirty state", $("#policy-max-batch").value === "7" && isDirty($("#automation-policy-form")));

    // Late response must not wipe an edit made meanwhile.
    let release;
    gates["PUT /api/automation/policy/a1"] = new Promise((r) => (release = r));
    $("#automation-policy-form").requestSubmit();
    await wait(50);
    edit("#policy-stable", "9");
    release();
    delete gates["PUT /api/automation/policy/a1"];
    await until(() => policy.max_batch === 7);
    await wait(100);
    check("late success keeps the newer edit", policy.max_batch === 7 && $("#policy-stable").value === "9" && isDirty($("#automation-policy-form")));
    $("#automation-policy-form").requestSubmit();
    await until(() => policy.stable_seconds === 540);
    await wait(100);
    check("second save settles the draft", !isDirty($("#automation-policy-form")) && $("#auto-summary").innerText.includes("最多 7 封"), $("#auto-summary").innerText);

    // Folder settings in the same place.
    edit("#organizer-writes", false);
    $("#folder-settings-form").requestSubmit();
    await until(() => folders.writes_enabled === false);
    await wait(150);
    check("folder switch saved from the hub and summary warns", $("#auto-summary").innerText.includes("未允许") && $("#auto-settings-note").textContent.includes("原邮箱写入未允许"), $("#auto-settings-note").textContent);

    // Discarding on account switch.
    edit("#policy-max-batch", "3");
    confirmAnswer = true;
    $("#auto-account").value = "a2";
    $("#auto-account").dispatchEvent(new Event("change", { bubbles: true }));
    await until(() => $("#rules-account").value === "a2");
    await wait(80);
    check("confirmed switch discards draft and loads the other account", $("#tasks-account").value === "a2" && !isDirty($("#automation-policy-form")) && $("#auto-summary").innerText.includes("第二个邮箱"));
    $("#auto-account").value = "a1";
    $("#auto-account").dispatchEvent(new Event("change", { bubbles: true }));
    await until(() => $("#rules-account").value === "a1");

    // Telegram page.
    confirmAnswer = false;
    await showView("notifications");
    const overview = $("#tg-overview").innerText;
    check("telegram overview shows bot, notifications and interaction", overview.includes("Bot Token 已保存") && overview.includes("任务通知已开启") && overview.includes("通知发往 12345") && overview.includes("私聊交互未开启"), overview);
    check("interaction card lives on the telegram page", !!$("#notifications-pane #agent-telegram-toggle") && !$("#agent-pane #agent-telegram-toggle"));
    check("long help is folded", $$("#notifications-pane details.help-details").length >= 2 && $$("#notifications-pane details.help-details").every((d) => !d.open));
    edit("#telegram-chat", "999");
    await window.loadNotifications();
    check("refresh keeps unsaved telegram edit", $("#telegram-chat").value === "999" && $("#telegram-status").textContent.includes("未保存"));
    $("#agent-telegram-toggle").click();
    await until(() => interaction.enabled);
    await wait(80);
    check("interaction toggle is independent of notifications", interaction.enabled && telegram.chat_id === "12345" && telegram.enabled && $("#telegram-chat").value === "999" && $("#tg-overview").innerText.includes("私聊交互：在线"));
    gates["PUT /api/notifications/telegram"] = new Promise((r) => (release = r));
    $("#telegram-form").requestSubmit();
    await wait(50);
    edit("#telegram-policy", "all");
    release();
    delete gates["PUT /api/notifications/telegram"];
    await until(() => telegram.chat_id === "999");
    await wait(100);
    check("telegram late save keeps newer edit", $("#telegram-policy").value === "all" && isDirty($("#telegram-form")));
    check("token field never prefilled", $("#telegram-token").value === "" && $("#telegram-token").type === "password");
    const sentToken = sent.filter((x) => x.key === "PUT /api/notifications/telegram").every((x) => x.body.bot_token === "");
    check("blank token is not sent as a value", sentToken);
    markClean($("#telegram-form"));

    // No persistent storage of drafts or secrets.
    let stored = "";
    try {
      stored = JSON.stringify({ ...localStorage }) + JSON.stringify({ ...sessionStorage });
    } catch (_) {}
    check("nothing written to browser storage", !/12345|999|tester|policy|token/i.test(stored), stored);

    // Hidden output-length control stays hidden (0.7.2).
    check("output-length control still absent", !$("#model-max-param") && !document.body.innerText.includes("输出长度参数"));

    // Layout at the current width.
    for (const pane of ["automation", "notifications"]) {
      await showView(pane);
      const el = $(`#${pane}-pane`);
      check(`${pane} fits the width`, el.scrollWidth <= el.clientWidth + 1 && document.documentElement.scrollWidth <= innerWidth, `${el.scrollWidth}/${el.clientWidth} doc ${document.documentElement.scrollWidth}/${innerWidth}`);
    }
  } catch (e) {
    check("exception", false, String((e && e.stack) || e));
  } finally {
    window.api = realApi;
    window.confirm = realConfirm;
    state.accounts = realAccounts;
    for (const f of ["#automation-policy-form", "#folder-settings-form", "#telegram-form", "#task-editor", "#rule-editor"]) markClean($(f));
  }
  return { pass: results.every((r) => r.ok), total: results.length, failed: results.filter((r) => !r.ok), width: innerWidth };
})();
