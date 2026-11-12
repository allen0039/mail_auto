// Mail Auto admin UI regression check (R1 expand/collapse, polling, button
// focus and single requests; R2 run counters; R3 preview paging; operation
// verification window; R01 preview error recovery; R02 filter retry cursor;
// R03 account badge updates; R04 timezone validation; R06 bulk selection highlight).
// It replaces the page's API with fixed fixture data, so it needs no login,
// no mailbox and sends nothing to the server.
//
// Usage: start a throwaway instance (e.g. `mail-auto --data-dir /tmp/x/data
// --key-dir /tmp/x/keys --listen 127.0.0.1:18081 serve`), open
// http://127.0.0.1:18081 and paste this file into the browser console, or run
// it through any automation that can evaluate a script and await its result.
// It resolves to {pass, results}. Keyboard toggling with real Enter/Space key
// presses cannot be synthesized from a script; see docs/acceptance-0.4.18.md.
(async () => {
  const results = [];
  const check = (name, ok, detail = "") => results.push({name, ok: !!ok, detail});
  const requiredElement = (selector) => {
    const element = document.querySelector(selector);
    if (!element) throw new Error(`UI check requires element ${selector}`);
    return element;
  };
  const wait = (ms) => new Promise((r) => setTimeout(r, ms));
  const calls = [];
  const now = Math.floor(Date.now() / 1000);
  let activeState = "running";
  let previewFail = false;
  const task = {id: "t1", account_id: "a1", name: "fixture task", version: 7, enabled: true, trigger_mode: "internal", engine: "rules", mode: "execute", source_folders: ["INBOX"], max_batch: 20, schedule: {frequency: "hourly", minute: 0, timezone: "Asia/Shanghai"}, notify: "inherit"};
  const runs = () => [
    {id: "run-done", task_id: "t1", state: "needs_verification", source: "internal", created_at: now - 600, updated_at: now - 500, scanned: 48, claimed: 20, completed: 18, kept: 0, protected: 0, failed: 0, queued: 0, running: 0, unknown: 2, flag_warnings: 1, mode: "execute", has_more: true, error_code: "remote_state_unknown", task_version: 7, policy_version: 7, notification_state: "not_required", finalized: true},
    {id: "run-active", task_id: "t1", state: activeState, source: "manual", created_at: now - 30, updated_at: now, progress_at: now - 5, scanned: 10, claimed: 5, completed: 1, kept: 0, protected: 0, failed: 0, queued: activeState === "running" ? 3 : 0, running: activeState === "running" ? 1 : 0, unknown: 0, mode: "execute", task_version: 7, policy_version: 7, notification_state: "not_required", finalized: activeState !== "running"},
  ];
  const ops = [{id: "op-1", plan_id: "p", state: "needs_verification", created_at: now - 2 * 86400, updated_at: now - 3600, reconcile_mode: "slow", next_check_at: now + 5 * 3600, items: [{position: 0, ref: "refrefrefref0001", state: "needs_verification", error_code: "remote_state_unknown", destination: {folder: "Archive", uidvalidity: 7, uid: 101}, detail: "移除已复制的源邮件：服务器拒绝 UID EXPUNGE", recovery_code: "recovery_fingerprint_mismatch", recovery_detail: "源邮件的元数据或已读/星标状态已变化", checked_at: now - 3600}, {position: 1, ref: "refrefrefref0002", state: "completed", error_code: "flags_not_applied", destination: {folder: "Archive", uidvalidity: 7, uid: 102}, recovery_code: "flags_not_applied", recovery_detail: "移动已核验完成，但附加的标记修改（加星标）未执行", checked_at: now - 60}]}];
  const page = (start, n, matchAt) => Array.from({length: n}, (_, i) => ({message: {subject: `m${start + i}`, folder: "INBOX"}, action: "move", destination: "Archive", status: start + i === matchAt ? "matched" : "unmatched", reasons: ["fixture"]}));
  window.api = async (url, opts = {}) => {
    calls.push(`${opts.method || "GET"} ${url}${opts.body ? " " + opts.body : ""}`);
    if (url.includes("/preview")) {
      if (previewFail) throw new Error("503 Service Unavailable: IMAP 服务暂时不可用");
      const body = JSON.parse(opts.body || "{}");
      if (body.only_matched) return {items: [page(26, 1, 26)[0]], total: 600, scanned: 500, counts: {matched: 1, unmatched: 499}, max_batch: 20, next_cursor: "c-500"};
      return body.cursor ? {items: page(26, 25, 26), total: 600, scanned: 25, counts: {matched: 1, unmatched: 24}, max_batch: 20, next_cursor: "c-50"} : {items: page(1, 25, -1), total: 600, scanned: 25, counts: {unmatched: 25}, max_batch: 20, next_cursor: "c-25"};
    }
    if (url.includes("/runs")) return runs();
    if (url.startsWith("/api/tasks")) return [task];
    if (url.startsWith("/api/automation/policy")) return {account_id: "a1", version: 7, enabled: true, allow_internal: true, mode: "execute", source_folders: ["INBOX"], destinations: ["Archive"], actions: ["move"], max_batch: 20};
    if (url.startsWith("/api/rules")) return [];
    if (url.startsWith("/api/operations")) return ops;
    if (url.startsWith("/api/accounts")) return state.accounts || [];
    if (url.startsWith("/api/status")) return state.status || {};
    return {};
  };
  state.accounts = [{id: "a1", name: "fixture", username: "fixture@example.com", provider: "netease", host: "imap.163.com", port: 993, folders: ["INBOX"], synced_folders: ["INBOX"], credential_source: "encrypted"}];
  state.session = {};
  // Show the workspace as a signed-in session would; hidden elements cannot
  // take keyboard focus, which would invalidate the focus checks.
  document.querySelector("#auth-view").hidden = true;
  document.querySelector("#workspace").hidden = false;
  await showView("tasks").catch(() => {});
  state.view = "tasks";
  await loadTasks();
  const list = document.querySelector("#task-runs-list");
  const rec = (key) => list.querySelector(`details[data-key="${key}"]`);
  const posts = () => calls.filter((c) => c.startsWith("POST"));

  check("active run opens by default, finished run collapsed", rec("run-active")?.open && !rec("run-done")?.open);
  rec("run-done").querySelector("summary").click();
  await wait(50);
  check("summary click expands a finished run", rec("run-done").open);
  rec("run-active").querySelector("summary").click();
  await wait(3600); // one polling refresh (3 s)
  check("polling keeps expanded record open", rec("run-done").open);
  check("polling keeps a user-collapsed active record closed", !rec("run-active").open);
  check("R2 counters shown separately", /排队 3/.test(rec("run-active").textContent.replace(/\s+/g, " ")) && /执行中 1/.test(rec("run-active").textContent.replace(/\s+/g, " ")) && /待核验 0/.test(rec("run-active").textContent.replace(/\s+/g, " ")));
  check("run summary shows follow-up flag warnings", /1 封已移动完成，但附加的标记修改未应用/.test(rec("run-done").textContent));
  check("R2 phase and progress shown", /写入原邮箱中/.test(rec("run-active").textContent) && /最近进度/.test(rec("run-active").textContent));

  rec("run-active").open = true;
  await wait(50);
  rec("run-active").querySelector("[data-task-cancel]").focus();
  await wait(3600);
  const focused = document.activeElement;
  check("polling restores focus to the same action button", focused?.dataset?.taskCancel === "run-active", focused?.outerHTML?.slice(0, 80));
  activeState = "completed"; // the cancel button disappears on the next refresh
  await wait(3600);
  check("focus falls back to the record summary when its button is gone", document.activeElement?.tagName === "SUMMARY" && document.activeElement.closest("details")?.dataset.key === "run-active");
  // Polling correctly stops once no run is active; refresh explicitly.
  activeState = "running";
  document.querySelector("#refresh-task-runs").click();
  await wait(300);

  calls.length = 0;
  rec("run-active").querySelector("[data-task-cancel]").click();
  await wait(300);
  check("cancel sends exactly one request", posts().filter((c) => c.includes("/cancel")).length === 1, posts().join(" | "));
  calls.length = 0;
  rec("run-done").open = true;
  rec("run-done").querySelector("[data-task-continue]").click();
  await wait(400);
  check("continue sends exactly one request", posts().filter((c) => c.includes("/run ")).length === 1, posts().join(" | "));

  // R01: Cold preview failure recovery
  calls.length = 0;
  previewFail = true;
  document.querySelector("#task-detail-content").innerHTML = ""; // simulate cold / config view state
  document.querySelector('#tasks-list [data-task-command="preview"]')?.click();
  await wait(300);
  const previewDialog = document.querySelector("#task-detail-dialog");
  const previewFooter = document.querySelector("#preview-footer");
  check("R01: cold preview failure opens dialog without DOM crash", previewDialog?.open && !!previewFooter);
  check("R01: preview footer displays 503 error and retry button", previewFooter?.textContent.includes("503 Service Unavailable") && !!document.querySelector("#preview-retry"));

  // R01: Retry succeeds from cold failure
  previewFail = false;
  document.querySelector("#preview-retry")?.click();
  await wait(400);
  const summary = () => document.querySelector("#preview-summary")?.textContent.replace(/\s+/g, " ") || "";
  check("R01: retry after cold failure loads preview successfully", /源目录共 600 封/.test(summary()) && /本次扫描 25 封/.test(summary()) && /每轮最多处理 20 封/.test(document.querySelector("#task-detail-content").textContent.replace(/\s+/g, " ")), summary());

  // R3 normal preview paging
  document.querySelector("#preview-more")?.click();
  await wait(400);
  check("continue preview appends the next page", document.querySelectorAll("#preview-items .organizer-item").length === 50 && /命中 1 封/.test(summary()), summary());
  check("continue preview sends the backend cursor", calls.some((c) => c.includes('"cursor":"c-25"')));

  // R02: Filter switch (only_matched) failure does not corrupt cursor or mix results
  calls.length = 0;
  previewFail = true;
  const onlyBox = document.querySelector("#preview-only-matched");
  onlyBox.checked = true;
  onlyBox.dispatchEvent(new Event("change", {bubbles: true}));
  await wait(400);
  check("R02: only-matched failure shows error and retry button", !!document.querySelector("#preview-retry") && /503 Service Unavailable/.test(document.querySelector("#preview-footer")?.textContent || ""));

  // R02: Replaying retry sends empty cursor and only_matched:true, replaces items cleanly
  previewFail = false;
  calls.length = 0;
  document.querySelector("#preview-retry")?.click();
  await wait(400);
  const retryCall = calls.find((c) => c.includes("/preview"));
  check("R02: filter retry sends only_matched:true without stale cursor", retryCall && retryCall.includes('"only_matched":true') && !retryCall.includes('"cursor":"c-25"'));
  check("R02: filter retry replaces list with matched items only", document.querySelectorAll("#preview-items .organizer-item").length === 1 && /本次扫描 500 封/.test(summary()));
  previewDialog.close();

  // R03: Account status badge in-place updates without stale TLS connection badge
  state.view = "accounts";
  await showView("accounts").catch(() => {});
  state.status = { automatic: { enabled: true, interval_seconds: 30, accounts: [{ account_id: "a1", state: "watching", mode: "idle", checked_at: now }] } };
  await refresh();
  const accCard = document.querySelector('[data-account="a1"]');
  const accBadge = () => accCard?.querySelector(".account-status-badge")?.textContent || "";
  check("R03: watching state shows TLS connected badge", /正在监控 · TLS 已连接/.test(accBadge()));

  state.status = { automatic: { enabled: true, interval_seconds: 30, accounts: [{ account_id: "a1", state: "retrying", error: "连接中断", checked_at: now }] } };
  await refresh();
  check("R03: retrying state updates badge to reconnecting in place", /连接中断 · 重连中/.test(accBadge()));

  state.status = { automatic: { enabled: false, interval_seconds: 30, accounts: [] } };
  await refresh();
  check("R03: automatic disabled updates badge and clears auto status subtitle", !/正在监控|重连中/.test(accBadge()) && accCard?.querySelector(".automatic-account")?.textContent === "");

  // R04: Timezone requiredness toggles dynamically with trigger mode
  state.view = "tasks";
  await showView("tasks").catch(() => {});
  document.querySelector('#tasks-list [data-task-command="edit"]')?.click();
  await wait(300);
  const triggerSel = requiredElement("#task-trigger");
  const tzInput = requiredElement("#task-timezone");
  const taskForm = requiredElement("#task-editor");
  triggerSel.value = "internal";
  triggerSel.dispatchEvent(new Event("change"));
  tzInput.value = "";
  check("R04: timezone is required in internal mode", tzInput.required === true && tzInput.checkValidity() === false);
  triggerSel.value = "manual";
  triggerSel.dispatchEvent(new Event("change"));
  check("R04: switching to manual removes required from hidden timezone", tzInput.required === false && tzInput.checkValidity() === true);
  check("R04: form checkValidity succeeds with empty timezone when manual", taskForm.checkValidity() === true);
  document.querySelector("#task-editor-dialog")?.close();

  // R06: Bulk toolbar has-selection class toggling
  state.view = "organizer";
  await showView("organizer").catch(() => {});
  const bulkToolbar = document.querySelector("#bulk-toolbar");
  window.clearMailSelection();
  check("R06: bulk toolbar initially has no has-selection class", bulkToolbar && !bulkToolbar.classList.contains("has-selection"));
  state.pageMessages = [{ ref: "ref-test-1", account_id: "a1", subject: "test" }];
  document.querySelector("#select-page")?.click();
  check("R06: bulk toolbar has has-selection class when messages selected", bulkToolbar.classList.contains("has-selection"));
  window.clearMailSelection();
  check("R06: bulk toolbar removes has-selection class when selection cleared", !bulkToolbar.classList.contains("has-selection"));

  // Operations list (整理与 MCP)
  document.querySelector("#refresh-operations").click();
  await wait(300);
  const op = () => document.querySelector('#operations-list details[data-key="op-1"]');
  op().querySelector("summary").click();
  await wait(50);
  check("operation record expands", op().open);
  check("verification window and reasons shown", /已超自动核验窗口/.test(op().textContent) && /目标 UID 101/.test(op().textContent) && /最近核验/.test(op().textContent));
  check("completed item with flags_not_applied shows a warning", [...op().querySelectorAll(".task-warning")].some((p) => /附加标记未应用或未确认：移动已核验完成/.test(p.textContent) && /加星标/.test(p.textContent)));
  ops[0].items[1].recovery_detail = "移动已完成，但附加的标记修改（加星标）未确认完成：标记可能未应用，也可能已在服务器生效但未记录。";
  document.querySelector("#refresh-operations").click();
  await wait(300);
  const uncertainWarning = op().querySelectorAll(".operation-item")[1].querySelector(".task-warning").textContent;
  check("unknown flag outcome is not presented as definitely unapplied", /附加标记未应用或未确认：/.test(uncertainWarning) && /未确认完成/.test(uncertainWarning) && /也可能已在服务器生效/.test(uncertainWarning) && !/附加标记未应用：/.test(uncertainWarning), uncertainWarning);
  calls.length = 0;
  const verify = op().querySelector('[data-op-action="reconcile"]');
  verify.focus();
  verify.click();
  await wait(400);
  check("verify sends exactly one request", posts().length === 1 && posts()[0].includes("/reconcile"), posts().join(" | "));
  check("record stays open and focus returns to the verify button", op().open && document.activeElement?.dataset?.opAction === "reconcile");

  const pass = results.every((r) => r.ok);
  return {pass, failed: results.filter((r) => !r.ok), results};
})();
