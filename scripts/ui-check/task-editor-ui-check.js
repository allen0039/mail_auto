// Mail Auto 0.8.6–0.8.7 task editor and task error regression check (lightweight
// plan, suggestions 2 and 3): default fields, the “更多设置” fold and its
// summary, open-and-save of existing tasks without losing fields, checks on
// folded fields, save / enable / run failure messages, and stale responses
// after edits, reopening or a mailbox switch. 0.8.7: the summary states what
// the grant still enforces (LE-01) and the write note follows a task created
// while edits continued (LE-02). It replaces the page's API with
// an in-memory fake, so it needs no mailbox and sends nothing anywhere.
// Usage: run through scripts/ui-check/run.sh, or open a throwaway instance,
// log in and evaluate this file. Resolves to {pass, results}.
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
  let gets = 0;
  window.confirm = (m) => (asked.push(m), answer);
  window.notify = (m) => notes.push(String(m));
  const now = Math.floor(Date.now() / 1000);
  // Midnight Asia/Shanghai of a past day, as the editor stores a 历史起点.
  const sinceDay = "2026-10-01", since = Math.floor(new Date(`${sinceDay}T00:00:00+08:00`).getTime() / 1000);
  const accounts = ["a1", "a2"].map((id, i) => ({ id, name: i ? "B邮箱" : "A邮箱", username: `tester${i}@163.com`, provider: "netease", host: "imap.163.com", port: 993, folders: [], synced_folders: ["INBOX", "Archive"] }));
  let grantOver = {}; // per-case grant changes (LE-01)
  const policy = (id) => ({ ...grantBase(id), ...grantOver });
  const grantBase = (id) => ({ account_id: id, version: 4, enabled: true, mode: "execute", source_folders: ["INBOX", "Archive"], destinations: ["Archive"], actions: ["move"], allow_internal: true, protect_unread: true, protect_flagged: true, protected_senders: [], max_batch: 20, stable_seconds: 180, include_history: true, since: 0, expires: now + 86400 * 20 });
  const rules = (id) => [
    { id: "r1-" + id, account_id: id, name: "账单归档", version: 3, priority: 10, enabled: true, source_folders: ["INBOX"], match: { mode: "all", conditions: [], groups: [] }, action: "move", also: [], destination: "Archive" },
    { id: "r2-" + id, account_id: id, name: "通知归档", version: 5, priority: 20, enabled: true, source_folders: ["INBOX"], match: { mode: "all", conditions: [], groups: [] }, action: "move", also: [], destination: "Archive" },
  ];
  const base = { account_id: "a1", enabled: false, trigger_mode: "manual", engine: "rules", mode: "observe", source_folders: ["INBOX"], max_batch: 20, since: 0, protect_unread: true, protect_flagged: true, notify: "inherit", expected_seconds: 0, rule_ids: [], rule_versions: {}, schedule: { frequency: "daily", hour: 2, minute: 0, weekday: 0, every_minutes: 0, cron: "", timezone: "Asia/Shanghai" } };
  // Fixed rules, Cron in a non-default time zone, bound rule versions and every advanced setting customised.
  const custom = { ...base, id: "t-custom", name: "自定义固定规则", version: 6, enabled: true, trigger_mode: "internal", mode: "execute", source_folders: ["INBOX", "Archive"], max_batch: 7, since, protect_unread: false, notify: "failures", rule_ids: ["r1-a1", "r2-a1"], rule_versions: { "r1-a1": 2, "r2-a1": 5 }, schedule: { frequency: "cron", hour: 0, minute: 0, weekday: 0, every_minutes: 0, cron: "15 */3 * * 1-5", timezone: "America/New_York" } };
  const ai = { ...base, id: "t-ai", name: "外部 AI 分类", version: 3, enabled: true, trigger_mode: "external", engine: "ai", expected_seconds: 7200 };
  const plain = { ...base, id: "t-plain", name: "普通定时", version: 2, enabled: true, trigger_mode: "internal", mode: "execute", schedule: { ...base.schedule, hour: 8, minute: 10 } };
  const obs = { ...base, id: "t-obs", name: "观察任务", version: 1 };
  // Looser than a stricter grant: no own protection, an earlier history start, 12 per run.
  const loose = { ...base, id: "t-loose", name: "宽松任务", version: 4, max_batch: 12, since, protect_unread: false, protect_flagged: false, notify: "failures" };
  const taskB = { ...base, id: "t-b", account_id: "a2", name: "任务-B", version: 1 };
  const runOf = (task_id, x) => ({ id: "run-" + task_id, task_id, created_at: now - 300, updated_at: now - 200, finalized: true, scanned: 40, claimed: 0, completed: 0, kept: 0, protected: 0, failed: 0, unknown: 0, needs_ai: 0, queued: 0, running: 0, mode: "execute", ...x });
  const lastRuns = {
    "t-custom": runOf("t-custom", { state: "failed", failed: 2, claimed: 2, error_code: "imap_timeout" }),
    "t-ai": runOf("t-ai", { state: "needs_verification", unknown: 1, claimed: 1 }),
    "t-plain": runOf("t-plain", { state: "noop" }),
    "t-obs": runOf("t-obs", { state: "observed", mode: "observe", claimed: 3 }),
  };
  const tasksOf = (id) => structuredClone(id === "a1" ? [custom, ai, plain, obs, loose] : [taskB]);
  const owner = (url) => (/(\/|=)a2(\b|$)|t-b\b/.test(url) ? "a2" : "a1");
  // Writes answer from this queue (default: success); a gate holds one.
  const answers = [];
  const gates = {};
  const hold = (key) => {
    let release;
    const p = new Promise((r) => (release = r));
    gates[key] = { p, release: () => { delete gates[key]; release(); } };
    return gates[key].release;
  };
  const failure = (status, code, extra = {}, message = "服务器拒绝") => () => {
    throw Object.assign(new Error(message), { status, code, area: "", target: "", ...extra });
  };
  window.api = async (url, options = {}) => {
    const method = options.method || "GET", body = options.body ? JSON.parse(options.body) : null;
    if (method !== "GET") {
      sent.push({ url, method, body });
      const answer = answers.shift();
      if (gates.save) await gates.save.p;
      if (answer) return answer(body);
      if (url === "/api/tasks") return { ...body, id: "t-new", version: 1, enabled: false, mode: "observe" };
      if (/\/api\/tasks\/[^/]+$/.test(url)) return { ...body, version: body.version + 1 };
      if (url.endsWith("/enabled")) return { version: body.version + 1, enabled: body.enabled };
      if (url.endsWith("/run")) return { id: "run-new", state: "queued" };
      throw new Error("unhandled " + method + " " + url);
    }
    gets++;
    const id = owner(url);
    if (url === "/api/accounts") return accounts;
    if (url === "/api/status") return { version: "0.8.6", accounts: 2, messages: 0, unread: 0, folders: 0 };
    if (url === "/api/network") return { version: 1, global: { mode: "direct" }, agent: { mode: "inherit" }, telegram: { mode: "inherit" }, profiles: [] };
    if (url.startsWith("/api/automation/policy/")) return policy(id);
    if (url.startsWith("/api/automation/capabilities/")) return { folders: [{ name: "INBOX" }, { name: "Archive" }] };
    if (url.startsWith("/api/automation/batches")) return [];
    if (url.startsWith("/api/folder-settings/")) return { account_id: id, archive_folder: "Archive", trash_folder: "", writes_enabled: true, version: 2 };
    if (url.startsWith("/api/rules?")) return rules(id);
    if (url.startsWith("/api/tasks/readiness")) return tasksOf(id).map((t) => ({ task_id: t.id, ready: true, issues: [], last_run: lastRuns[t.id] }));
    if (url.startsWith("/api/tasks?")) return tasksOf(id);
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
  const switchTo = async (id) => {
    $("#auto-account").value = id;
    $("#auto-account").dispatchEvent(new Event("change", { bubbles: true }));
    await until(() => autoScope.account === id && $("#tasks-list").innerText.includes(id === "a1" ? "普通定时" : "任务-B"));
  };
  const reset = async (account = "a1") => {
    for (const [f] of drafts) markClean(f);
    for (const d of $$("dialog[open]")) d.close();
    await wait(10);
    answer = true;
    answers.length = 0;
    state.accounts = accounts;
    if (state.view !== "automation") await showView("automation");
    if (autoScope.account !== account) await switchTo(account);
    $("#refresh-tasks").click();
    await until(() => $("#tasks-list").innerText.includes(account === "a1" ? "普通定时" : "任务-B"));
    await wait(30);
    answer = false;
    asked.length = 0;
    notes.length = 0;
    sent.length = 0;
  };
  const openTask = async (id) => {
    $(`[data-task-command="edit"][data-task-id="${id}"]`).click();
    await until(() => $("#task-editor-dialog").open);
  };
  const openNew = async () => {
    $("#new-task").click();
    await until(() => $("#task-editor-dialog").open);
  };
  const submit = async () => {
    const n = sent.length;
    $("#task-editor").requestSubmit();
    await until(() => sent.length > n, 50);
    await wait(60);
    return sent[n];
  };
  const summary = () => $("#task-summary").innerText;
  const fold = () => $("#task-more").open;
  const err = () => $("#task-editor-error").textContent;
  const card = (id) => $(`[data-task-card="${id}"]`);
  const same = (a, b) => JSON.stringify(a) === JSON.stringify(b);
  const kept = ["trigger_mode", "engine", "mode", "enabled", "source_folders", "max_batch", "since", "protect_unread", "protect_flagged", "notify", "expected_seconds", "rule_ids", "rule_versions"];
  const diff = (sentBody, orig) => kept.filter((k) => !same(sentBody[k], orig[k])).concat(["frequency", "hour", "minute", "weekday", "cron", "timezone"].filter((k) => !same(sentBody.schedule[k], orig.schedule[k])).map((k) => "schedule." + k));

  try {
    state.accounts = accounts;
    await showView("automation");
    await reset("a1");

    // ---- 1. New task: defaults and the short form ----
    await openNew();
    check("新任务：名称为空、仅手动、只观察、更多设置折叠", $("#task-name").value === "" && $("#task-trigger").value === "manual" && $("#task-mode").value === "observe" && !fold(), [$("#task-trigger").value, $("#task-mode").value, fold()]);
    check("新任务：默认可见名称、源目录、规则范围、触发、模式", ["#task-name", "#task-sources", "#task-rules-all", "#task-trigger", "#task-mode"].every((s) => $(s).offsetParent !== null));
    check("新任务：引擎、每批、历史、通知、保护在更多设置中", ["#task-engine", "#task-limit", "#task-since", "#task-notify", "#task-protect-unread", "#task-protect-flagged"].every((s) => $(s).closest("#task-more")));
    check("新任务：日程与外部监测隐藏（仅手动）", $("#task-schedule-fields").hidden && $("#task-expected-label").hidden);
    check("新任务摘要：固定规则 · 每轮 20 · 保护未读和星标 · 历史跟随授权 · 通知继承，无自定义", ["固定规则", "每轮最多 20 封", "保护未读和星标", "历史范围跟随整理授权", "通知：继承默认"].every((x) => summary().includes(x)) && !summary().includes("自定义") && $("#task-more-note").textContent === "", summary());
    check("新任务：说明保存为暂停的只观察任务、不写入原邮箱", $("#task-mode-note").textContent.includes("暂停的只观察任务") && $("#task-mode-note").textContent.includes("不会写入原邮箱"));
    check("新任务：保存按钮仍为“保存为暂停观察任务”", $("#save-task").textContent === "保存为暂停观察任务");
    edit("#task-trigger", "internal");
    check("内置定时才显示日程", !$("#task-schedule-fields").hidden && $("#task-expected-label").hidden);
    edit("#task-trigger", "external");
    check("外部 MCP 才显示外部调用监测", $("#task-schedule-fields").hidden && !$("#task-expected-label").hidden);
    edit("#task-trigger", "manual");
    edit("#task-name", "新的观察任务");
    let req = await submit();
    const b = req?.body || {};
    check("新任务提交：POST 默认值不变（观察、暂停、固定规则、每批 20、保护、无历史起点、继承通知、全部规则）", req?.method === "POST" && b.mode === "observe" && !b.enabled && b.engine === "rules" && b.trigger_mode === "manual" && b.max_batch === 20 && b.since === 0 && b.protect_unread && b.protect_flagged && b.notify === "inherit" && same(b.rule_ids, []) && same(b.source_folders, ["INBOX"]) && b.schedule.timezone === "Asia/Shanghai", JSON.stringify(b));
    check("新任务保存后关闭编辑器", !$("#task-editor-dialog").open);

    // ---- 2. Existing tasks: open and save without changes keeps every field ----
    await reset("a1");
    await openTask("t-custom");
    check("自定义任务：更多设置默认展开并标明 4 项自定义", fold() && $("#task-more-note").textContent.includes("4 项自定义"), $("#task-more-note").textContent);
    check("自定义任务摘要：每批 7、历史起点、通知仅失败标为自定义；未读仍由整理授权保护", ["每轮最多 7 封（自定义）", "保护未读和星标（自定义；任务未额外保护，整理授权仍要求保护）", `只处理 ${sinceDay} 起收到的邮件（自定义）`, "通知：仅失败（自定义）"].every((x) => summary().includes(x)) && !summary().includes("不保护未读"), summary());
    check("自定义任务：真实执行说明会写入原邮箱", $("#task-mode-note").textContent.includes("会移动或修改原邮箱") && $("#task-mode-note").classList.contains("task-mode-write"));
    check("自定义任务：Cron 与非默认时区显示", !$("#task-schedule-fields").hidden && $("#task-frequency-select").value === "cron" && $("#task-cron").value === "15 */3 * * 1-5" && $("#task-timezone").value === "America/New_York");
    $("#task-more").open = false; // folded fields are still submitted as shown
    req = await submit();
    check("自定义任务折叠后原样保存：所有字段（含规则版本、Cron、时区、高级参数）不变", req?.method === "PUT" && req.url === "/api/tasks/t-custom" && diff(req.body, custom).length === 0, diff(req?.body || {}, custom).join(","));
    check("折叠不是未保存修改：保存时未询问", asked.length === 0);

    await reset("a1");
    await openTask("t-ai");
    check("外部 AI 任务：更多设置默认展开，摘要标明外部 AI 判断", fold() && summary().includes("外部 AI 判断（自定义）"), summary());
    check("外部 AI 任务：外部调用监测可见并保留 7200 秒", !$("#task-expected-label").hidden && $("#task-expected").value === "7200");
    req = await submit();
    check("外部 AI 任务原样保存：引擎、触发、监测间隔等不变", req?.method === "PUT" && diff(req.body, ai).length === 0, diff(req?.body || {}, ai).join(","));

    await reset("a1");
    await openTask("t-plain");
    check("默认设置的已有任务：更多设置折叠、摘要无自定义", !fold() && !summary().includes("自定义"), summary());
    req = await submit();
    check("默认设置的已有任务原样保存：日程 08:10 Asia/Shanghai 等不变", req?.method === "PUT" && diff(req.body, plain).length === 0, diff(req?.body || {}, plain).join(","));

    await reset("a1");
    await openTask("t-plain");
    edit("#task-limit", "12");
    check("修改折叠区字段后摘要立即标明自定义", summary().includes("每轮最多 12 封（自定义）") && $("#task-more-note").textContent.includes("1 项自定义"), summary());
    $("#task-more").open = false;
    req = await submit();
    check("折叠后修改的字段照常提交", req?.body?.max_batch === 12 && diff({ ...req.body, max_batch: 20 }, plain).length === 0, JSON.stringify(req?.body));

    // ---- 3. Checks on folded fields open the fold and point at the field ----
    await reset("a1");
    await openNew();
    edit("#task-name", "校验");
    edit("#task-limit", "0");
    $("#task-more").open = false;
    let n = sent.length;
    $("#task-editor").requestSubmit();
    await wait(80);
    check("每批数量无效（折叠中）：展开更多设置，不发送请求", fold() && sent.length === n, [fold(), sent.length - n]);
    edit("#task-limit", "99");
    $("#task-more").open = false;
    $("#task-editor").requestSubmit();
    await wait(80);
    check("每批数量超过授权上限（折叠中）：展开，不发送请求", fold() && sent.length === n);
    edit("#task-limit", "20");
    const tomorrow = new Date(Date.now() + 2 * 86400000).toLocaleDateString("sv-SE", { timeZone: "Asia/Shanghai" });
    edit("#task-since", tomorrow);
    $("#task-more").open = false;
    $("#task-editor").requestSubmit();
    await wait(80);
    check("历史起点晚于今天（折叠中）：展开，不发送请求", fold() && sent.length === n);
    // The page's own check also covers a form submitted without browser validation.
    $("#task-editor").noValidate = true;
    $("#task-more").open = false;
    $("#task-editor").requestSubmit();
    await wait(80);
    check("历史起点（页面检查）：展开、聚焦字段并说明原因", fold() && document.activeElement === $("#task-since") && err().includes("不能晚于今天") && sent.length === n, err());
    edit("#task-since", "");
    edit("#task-limit", "0");
    $("#task-more").open = false;
    $("#task-editor").requestSubmit();
    await wait(80);
    check("每批数量（页面检查）：展开、聚焦字段并说明 1–20", fold() && document.activeElement === $("#task-limit") && err().includes("1–20") && sent.length === n, err());
    $("#task-editor").noValidate = false;

    // ---- 4. Save failures: reason, next step, draft kept, no retry ----
    await reset("a1");
    await openNew();
    edit("#task-name", "保存失败草稿");
    edit("#task-trigger", "internal");
    answers.push(failure(403, "internal_not_allowed", { area: "policy" }));
    req = await submit();
    check("保存失败（internal_not_allowed）：说明整理授权未允许内置定时及去向", err().includes("整理授权未允许内置定时") && err().includes("整理设置 → 整理授权") && err().includes("当前编辑内容已保留"), err());
    check("保存失败：编辑器保持打开、草稿与未保存标记保留、没有自动重试", $("#task-editor-dialog").open && $("#task-name").value === "保存失败草稿" && $("#task-trigger").value === "internal" && isDirty($("#task-editor")) && sent.length === 1);

    answers.push(failure(403, "permission_denied"));
    await submit();
    check("保存失败（笼统 permission_denied）：不推断具体缺口", err().includes("permission_denied") && err().includes("没有给出具体原因") && !err().includes("内置定时") && !err().includes("源目录"), err());

    answers.push(failure(400, "invalid_input"));
    $("#task-more").open = false;
    await submit();
    check("保存失败（invalid_input）：列出检查项并展开更多设置", err().includes("invalid_input") && err().includes("每轮最多处理 1–20 封") && fold(), err());

    answers.push(failure(422, "provider_exploded", {}, "sk-SECRET 原始响应 Subject: 机密"));
    await submit();
    check("保存失败（未知代码）：简洁兜底并保留脱敏代码，不显示服务器原文", err().includes("原因未能识别（代码：provider_exploded）") && !err().includes("SECRET") && !err().includes("机密"), err());

    answers.push(failure(500, ""));
    await submit();
    check("保存失败（无代码）：显示 HTTP 状态兜底", err().includes("原因未能识别（HTTP 500）"), err());

    answers.push(failure(422, "bad code!"));
    await submit();
    check("保存失败（非法代码）：不回显非法代码", err().includes("原因未能识别（HTTP 422）") && !err().includes("bad code"), err());

    await reset("a1");
    await openTask("t-custom");
    edit("#task-name", "冲突草稿");
    const g = gets;
    answers.push(failure(409, "task_version_conflict", { area: "task" }));
    req = await submit();
    await until(() => gets > g);
    check("版本冲突：说明已在其他位置更新，保留草稿，刷新列表，不自动覆盖或重试", err().includes("已在其他位置更新") && $("#task-name").value === "冲突草稿" && $("#task-editor-dialog").open && gets > g && sent.length === 1 && isDirty($("#task-editor")), [err(), sent.length, gets - g]);

    await reset("a1");
    await openTask("t-custom");
    answers.push(failure(409, "rules_changed", { area: "rules" }));
    await submit();
    check("保存失败（rules_changed）：说明规则已变化及可行的下一步", err().includes("绑定的规则在此期间已被修改或停用") && err().includes("跟随账号全部启用规则"), err());

    // ---- 5. Enable / run failures on the card ----
    await reset("a1");
    answers.push(failure(403, "policy_disabled", { area: "policy" }));
    $('[data-task-command="toggle"][data-task-id="t-obs"]').click();
    await until(() => card("t-obs")?.querySelector(".task-action-error"));
    let box = card("t-obs")?.querySelector(".task-action-error");
    check("启用失败（policy_disabled）：卡片显示原因与“打开整理授权”入口", box?.textContent.includes("启用未完成") && box.textContent.includes("整理授权未开启") && box.querySelector('[data-task-fix="policy"]'), box?.textContent);
    check("启用失败：只发送一次请求，不自动重试", sent.length === 1);

    answers.push(failure(409, "task_version_conflict", { area: "task" }));
    let g2 = gets;
    $('[data-task-command="toggle"][data-task-id="t-plain"]').click();
    await until(() => card("t-plain")?.querySelector(".task-action-error") && gets > g2);
    box = card("t-plain")?.querySelector(".task-action-error");
    check("暂停冲突：说明已在其他位置更新并刷新列表", box?.textContent.includes("这次没有暂停") && box.textContent.includes("已在其他位置更新") && gets > g2, box?.textContent);

    answers.push(failure(409, "rules_changed", { area: "rules" }));
    $('[data-task-command="run"][data-task-id="t-custom"]').click();
    await until(() => card("t-custom")?.querySelector(".task-action-error"));
    box = card("t-custom")?.querySelector(".task-action-error");
    check("运行失败（rules_changed）：复用任务卡说明与“编辑任务”入口", box?.textContent.includes("运行未完成") && box.textContent.includes("绑定的规则已修改") && box.querySelector('[data-task-fix="rules"]'), box?.textContent);

    answers.push(failure(422, "", {}, "IMAP 原文 password=xyz"));
    $('[data-task-command="run"][data-task-id="t-plain"]').click();
    await until(() => card("t-plain")?.querySelector(".task-action-error")?.textContent.includes("HTTP"));
    box = card("t-plain")?.querySelector(".task-action-error");
    check("运行失败（未知）：兜底说明，不显示服务器原文", box?.textContent.includes("运行未完成，原因未能识别（HTTP 422）") && !box.textContent.includes("password"), box?.textContent);

    $('[data-task-command="run"][data-task-id="t-plain"]').click();
    await until(() => !card("t-plain")?.querySelector(".task-action-error"));
    check("之后运行成功：清除该任务的错误", !card("t-plain")?.querySelector(".task-action-error") && notes.some((m) => m.includes("本轮已提交")));
    check("其他任务的错误保留", !!card("t-custom")?.querySelector(".task-action-error"));

    check("运行结果区分：执行失败 / 写入待核验 / 没有可处理的邮件 / 观察完成", card("t-custom").innerText.includes("执行失败") && card("t-ai").innerText.includes("写入待核验") && card("t-plain").innerText.includes("没有可处理的邮件") && card("t-obs").innerText.includes("观察完成"));

    // ---- 6. Stale responses and draft protection ----
    await reset("a1");
    await openTask("t-plain");
    let release = hold("save");
    answers.push(failure(403, "execute_not_allowed", { area: "policy" }));
    $("#task-editor").requestSubmit();
    await until(() => sent.length === 1);
    edit("#task-name", "保存中继续编辑");
    release();
    await wait(120);
    check("保存中继续编辑后失败：新编辑保留，错误显示在同一编辑器", $("#task-name").value === "保存中继续编辑" && err().includes("整理授权为“只观察”") && isDirty($("#task-editor")), err());

    await reset("a1");
    await openTask("t-custom");
    release = hold("save");
    answers.push(failure(409, "task_version_conflict", { area: "task" }));
    $("#task-editor").requestSubmit();
    await until(() => sent.length === 1);
    answer = true;
    $("#close-task-editor").click();
    await wait(20);
    await openTask("t-plain");
    answer = false;
    release();
    await wait(150);
    check("旧保存失败迟到：不改动重新打开的另一任务编辑器，改为提示", $("#task-name").value === "普通定时" && err() === "" && notes.some((m) => m.includes("「自定义固定规则」没有保存")), [$("#task-name").value, err(), notes.join("|")]);

    await reset("a1");
    release = hold("save");
    answers.push(failure(403, "policy_disabled", { area: "policy" }));
    $('[data-task-command="toggle"][data-task-id="t-obs"]').click();
    await until(() => sent.length === 1);
    await switchTo("a2");
    notes.length = 0;
    release();
    await wait(150);
    check("A 的启用失败在切到 B 后返回：B 页面无错误，提示属于 A", !$("#tasks-list .task-action-error") && $("#tasks-list").innerText.includes("任务-B") && notes.some((m) => m.includes("A邮箱") && m.includes("页面已切换")), notes.join("|"));
    await switchTo("a1");
    check("切回 A：旧错误不再显示", !$("#tasks-list .task-action-error"));

    await reset("a1");
    await openTask("t-plain");
    $("#task-more").open = true;
    $("#task-more").open = false;
    $("#close-task-editor").click();
    await wait(20);
    check("只展开 / 折叠不算修改：关闭时不询问", asked.length === 0 && !$("#task-editor-dialog").open);
    await openTask("t-plain");
    $("#task-more").open = true;
    edit("#task-notify", "all");
    $("#task-more").open = false;
    $("#close-task-editor").click();
    await wait(20);
    check("折叠区内的修改受未保存保护：关闭时询问并保留", asked.length === 1 && $("#task-editor-dialog").open && $("#task-notify").value === "all");

    // ---- 7. LE-01: the summary shows what the grant still enforces ----
    const grantDay = "2026-11-01", grantSince = Math.floor(new Date(`${grantDay}T00:00:00+08:00`).getTime() / 1000);
    grantOver = { since: grantSince, protect_unread: true, protect_flagged: true };
    await reset("a1");
    await openTask("t-loose");
    check("LE-01 授权强制保护、任务未额外保护：摘要显示仍保护未读和星标", summary().includes("保护未读和星标（自定义；任务未额外保护，整理授权仍要求保护）") && !summary().includes("不保护"), summary());
    check("LE-01 授权历史起点更晚：摘要显示授权起点并注明任务设置", summary().includes(`只处理 ${grantDay} 起收到的邮件（自定义；任务设为 ${sinceDay}，整理授权起点更晚）`), summary());
    check("LE-01 未收紧的每批数量照常显示", summary().includes("每轮最多 12 封（自定义）"), summary());
    req = await submit();
    check("LE-01 摘要不改字段：原样保存保护、历史起点、每批、通知", req?.method === "PUT" && diff(req.body, loose).length === 0, diff(req?.body || {}, loose).join(","));
    grantOver = { max_batch: 10 };
    await reset("a1");
    await openTask("t-loose");
    check("LE-01 授权数量上限收紧：摘要显示有效上限并注明任务设置", summary().includes("每轮最多 10 封（自定义；任务设为 12，受整理授权上限 10 限制）"), summary());
    check("LE-01 收紧上限时任务字段不被改写", $("#task-limit").value === "12");
    grantOver = { protect_unread: false, protect_flagged: false, since: grantSince };
    await reset("a1");
    await openTask("t-loose");
    check("LE-01 授权也不保护时：如实显示不保护（无授权说明）", summary().includes("不保护未读和星标（自定义）") && !summary().includes("仍要求保护"), summary());
    await reset("a1");
    await openTask("t-plain");
    check("LE-01 任务未设历史起点：显示授权起点", summary().includes(`历史范围跟随整理授权（${grantDay} 起）`) && !summary().includes("自定义"), summary());
    grantOver = {};

    // ---- 8. LE-02: a task created while edits continue is then edited as an existing task ----
    for (const mode of ["execute", "observe"]) {
      await reset("a1");
      await openNew();
      edit("#task-name", "创建中-" + mode);
      edit("#task-mode", mode);
      release = hold("save");
      $("#task-editor").requestSubmit();
      await until(() => sent.length === 1);
      edit("#task-name", "创建后较新名称-" + mode);
      if (mode === "observe") edit("#task-notify", "all");
      release();
      await until(() => $("#save-task").textContent === "保存修改");
      await wait(60);
      const note = $("#task-mode-note");
      check(`LE-02 创建成功后保留较新草稿（${mode}）：编辑器、名称、模式、未保存标记`, $("#task-editor-dialog").open && $("#task-editor-title").textContent === "编辑整理任务" && $("#task-name").value === "创建后较新名称-" + mode && $("#task-mode").value === mode && isDirty($("#task-editor")));
      check(`LE-02 写入说明跟随当前模式（${mode}）`, !note.textContent.includes("新任务保存为") && (mode === "execute" ? note.textContent.includes("真实执行：运行时会移动或修改原邮箱") && note.classList.contains("task-mode-write") : note.textContent.includes("只观察：运行时只记录建议") && !note.classList.contains("task-mode-write")), note.textContent);
      if (mode === "observe") check("LE-02 摘要包含保存期间修改的通知", summary().includes("通知：每次结束（自定义）"), summary());
      req = await submit();
      check(`LE-02 下次保存（${mode}）：PUT 已创建任务与新版本，不重复创建，带较新字段`, req?.method === "PUT" && req.url === "/api/tasks/t-new" && req.body.version === 1 && req.body.mode === mode && req.body.name === "创建后较新名称-" + mode && sent.filter((x) => x.method === "POST" && x.url === "/api/tasks").length === 1, JSON.stringify(req && { url: req.url, method: req.method, version: req.body.version, mode: req.body.mode }));
    }

    // ---- 9. Layout at this width (custom task, fold open) ----
    await reset("a1");
    await openTask("t-custom");
    const dialog = $("#task-editor-dialog");
    check(`编辑器在 ${innerWidth}px 下无横向溢出`, dialog.scrollWidth <= dialog.clientWidth + 1 && $("#task-summary").offsetParent !== null, [dialog.scrollWidth, dialog.clientWidth]);
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
