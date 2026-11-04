"use strict";
(() => {
  const selected = new Map();
  const folders = new Map();
  let currentPlan = null, pendingPlans = [], oauthID = "", mcpVersion = 0;
  const actionNames = {read: "读取", folder_manage: "在原邮箱创建文件夹", archive: "归档", move: "分类到文件夹", trash: "移入回收站", restore: "恢复", rule_manage: "创建和调整自定义规则", task_manage: "配置和启停整理任务", automation: "定时任务批次（受自动整理授权约束）"};
  const statusNames = {queued: "排队", running: "执行中", completed: "完成", partial: "部分完成", failed: "失败", needs_verification: "待核验", cancelled: "已停止", skipped: "已跳过"};
  const run = fn => async e => {
    if (e) e.preventDefault();
    try { await fn(e); } catch (err) { notify(err.message, true); }
  };
  function folderNames(id) {
    return folders.get(id) || (state.accounts.find(a => a.id === id)?.synced_folders || []).map(name => ({name, label: folderLabel(name)}));
  }
  function options(items, empty = "未配置") {
    return `<option value="">${esc(empty)}</option>` + items.map(f => `<option value="${esc(f.name)}">${esc(f.label || folderLabel(f.name))}</option>`).join("");
  }
  function updateSelection() {
    $("#selection-count").textContent = `已选 ${selected.size} 封`;
    const accounts = [...new Set(selected.values())];
    const destination = $("#bulk-destination"), previous = destination.value;
    destination.hidden = $("#bulk-action").value !== "move";
    destination.innerHTML = options(accounts.length === 1 ? folderNames(accounts[0]) : [], accounts.length > 1 ? "请分邮箱选择邮件" : "选择原邮箱目的文件夹");
    destination.value = previous;
    $("#selection-note").textContent = accounts.length > 1 ? "分类移动请分邮箱选择；归档和回收站使用各邮箱分别保存的配置。" : "分类会移动原邮箱中的邮件，邮箱 App 和手机端同步后可见。目的文件夹为空时，请到「整理与 MCP」刷新或创建。";
  }
  window.clearMailSelection = () => { selected.clear(); window.restoreMailSelection(); };
  window.restoreMailSelection = () => {
    $$("[data-select-ref]").forEach(el => { el.checked = selected.has(el.dataset.selectRef); });
    updateSelection();
  };
  $("#bulk-action").addEventListener("change", updateSelection);
  $("#message-list").addEventListener("change", e => {
    const el = e.target.closest("[data-select-ref]");
    if (!el) return;
    if (el.checked) {
      if (selected.size >= 500) { el.checked = false; notify("单次最多选择 500 封", true); return; }
      const message = state.pageMessages.find(m => m.ref === el.dataset.selectRef);
      if (message) selected.set(message.ref, message.account_id);
    } else selected.delete(el.dataset.selectRef);
    updateSelection();
  });
  $("#select-page").addEventListener("click", () => {
    for (const m of state.pageMessages || []) if (selected.size < 500) selected.set(m.ref, m.account_id);
    window.restoreMailSelection();
  });
  $("#clear-selection").addEventListener("click", window.clearMailSelection);
  function showPlan(p) {
    currentPlan = p;
    $("#plan-summary").textContent = `${actionNames[p.action] || p.action} · ${p.items.length} 封 · 有效期至 ${date(p.expires)}`;
    $("#plan-items").innerHTML = p.items.map(x => `<div class="organizer-item"><strong>${esc(x.message.subject || "（无主题）")}</strong><p>${esc(folderLabel(x.message.folder))}${x.destination ? ` → ${esc(folderLabel(x.destination))}` : ""}${x.skip ? ` · 跳过：${esc(x.skip)}` : ""}</p></div>`).join("");
    $("#execute-plan").disabled = false;
    if (!$("#plan-dialog").open) $("#plan-dialog").showModal();
  }
  $("#close-plan").addEventListener("click", () => { $("#plan-dialog").close(); currentPlan = null; pendingPlans = []; });
  $("#preview-selection").addEventListener("click", run(async () => {
    if (!selected.size) throw new Error("请先选择邮件");
    const action = $("#bulk-action").value, destination = action === "move" ? $("#bulk-destination").value : "";
    if (action === "move" && !destination) throw new Error("请选择同一邮箱的邮件和原邮箱目的文件夹");
    pendingPlans = [];
    showPlan(await api("/api/plans", {method: "POST", body: JSON.stringify({refs: [...selected.keys()], action, destination})}));
  }));
  $("#execute-plan").addEventListener("click", run(async () => {
    if (!currentPlan) return;
    await busy($("#execute-plan"), "提交中…", async () => {
      const out = await api(`/api/plans/${encodeURIComponent(currentPlan.id)}/execute`, {method: "POST", body: JSON.stringify({idempotency_key: currentPlan.id})});
      notify(`原邮箱整理任务已提交：${out.id}`);
      $("#plan-dialog").close(); currentPlan = null;
      if (pendingPlans.length) showPlan(pendingPlans.shift());
      else { selected.clear(); window.restoreMailSelection(); await showView("organizer"); }
    });
  }));
  function renderFolders(id, archive, trash) {
    const items = folderNames(id);
    for (const selector of ["#organizer-archive", "#organizer-trash"]) $(selector).innerHTML = options(items);
    $("#organizer-archive").value = archive || "";
    $("#organizer-trash").value = trash || "";
    $("#provider-folders").innerHTML = items.length ? items.map(f => `<span class="badge">${esc(f.label || folderLabel(f.name))}</span>`).join("") : '<p class="muted">尚未发现文件夹，请刷新原邮箱文件夹。</p>';
    updateSelection();
  }
  async function loadFolderSettings() {
    const id = $("#organizer-account").value;
    if (!id) { renderFolders("", "", ""); return; }
    const settings = await api(`/api/folder-settings/${encodeURIComponent(id)}`);
    renderFolders(id, settings.archive_folder, settings.trash_folder);
    $("#organizer-writes").checked = settings.writes_enabled;
    $("#capability-result").textContent = "刷新可读取原邮箱最新文件夹并检查整理能力；不会移动邮件。";
  }
  $("#organizer-account").addEventListener("change", run(loadFolderSettings));
  $("#probe-folders").addEventListener("click", run(async e => {
    const id = $("#organizer-account").value;
    if (!id) throw new Error("请先添加邮箱");
    await busy(e.currentTarget, "读取中…", async () => {
      const out = await api(`/api/folder-settings/${encodeURIComponent(id)}/probe`, {method: "POST", body: "{}"});
      folders.set(id, out.folders);
      const archive = $("#organizer-archive").value || out.folders.find(f => f.role === "archive")?.name;
      const trash = $("#organizer-trash").value || out.folders.find(f => f.role === "trash")?.name;
      renderFolders(id, archive, trash);
      const mode = {uid_move: "支持原生移动", uidplus_copy: "支持兼容移动（UIDPLUS）", unsupported: "该邮箱暂不支持安全移动，整理开关需保持关闭"}[out.move_mode];
      $("#capability-result").textContent = `${mode}；发现 ${out.folders.length} 个原邮箱文件夹。`;
    });
  }));
  $("#folder-settings-form").addEventListener("submit", run(async e => {
    const id = $("#organizer-account").value;
    if (!id) throw new Error("请先选择邮箱");
    await busy($('button[type="submit"], button.primary', e.currentTarget), "保存中…", async () => {
      await api(`/api/folder-settings/${encodeURIComponent(id)}`, {method: "PUT", body: JSON.stringify({archive_folder: $("#organizer-archive").value, trash_folder: $("#organizer-trash").value, writes_enabled: $("#organizer-writes").checked})});
      notify("原邮箱整理设置已保存");
    });
  }));
  $("#folder-create").addEventListener("submit", run(async e => {
    const id = $("#organizer-account").value, form = e.currentTarget, name = form.elements.name.value.trim();
    if (!id) throw new Error("请先选择邮箱");
    await busy($("button", form), "创建中…", async () => {
      const out = await api("/api/remote-folders", {method: "POST", body: JSON.stringify({account_id: id, name})});
      const items = folderNames(id).filter(f => f.name !== out.name);
      folders.set(id, [...items, out]);
      renderFolders(id, $("#organizer-archive").value, $("#organizer-trash").value);
      form.reset(); notify("已在原邮箱创建文件夹，邮箱 App 同步后可见");
    });
  }));
  async function loadOperations() {
    const operations = await api("/api/operations");
    $("#operations-list").innerHTML = operations.length ? operations.map(o => `<details class="organizer-item"><summary>${esc(statusNames[o.state] || o.state)} · ${o.items.length} 封 · ${esc(date(o.created_at))}</summary><p class="ref">${esc(o.id)}</p><div class="organizer-inline"><button type="button" class="secondary" data-operation="${esc(o.id)}" data-op-action="cancel">停止剩余项</button><button type="button" class="secondary" data-operation="${esc(o.id)}" data-op-action="reconcile">核验未知结果</button><button type="button" class="secondary" data-operation="${esc(o.id)}" data-op-action="undo">预览可撤销项</button></div>${o.items.map(x => `<p>${esc(x.ref.slice(0, 12))}… · ${esc(statusNames[x.state] || x.state)}${x.error_code ? ` · ${esc(x.error_code)}` : ""}${x.destination.folder ? ` → ${esc(folderLabel(x.destination.folder))}` : ""}</p>`).join("")}</details>`).join("") : '<p class="muted">还没有整理任务。</p>';
  }
  $("#refresh-operations").addEventListener("click", run(loadOperations));
  $("#operations-list").addEventListener("click", run(async e => {
    const b = e.target.closest("[data-operation]"); if (!b) return;
    const out = await api(`/api/operations/${encodeURIComponent(b.dataset.operation)}/${b.dataset.opAction}`, {method: "POST", body: "{}"});
    if (b.dataset.opAction === "undo") {
      if (!out.length) throw new Error("当前没有可确认的撤销项，或当前位置已变化");
      pendingPlans = out.slice(1); showPlan(out[0]);
    } else { await loadOperations(); notify(b.dataset.opAction === "cancel" ? "已请求停止剩余项" : "核验完成；无法确认的项会继续保留"); }
  }));
  async function loadTokens() {
    const tokens = await api("/api/mcp/tokens");
    $("#mcp-tokens").innerHTML = tokens.length ? tokens.map(t => `<div class="organizer-item organizer-inline"><strong>${esc(t.name)}</strong><span>${t.revoked ? "已撤销" : `到期 ${esc(date(t.expires))}`}</span>${!t.revoked ? `<button class="secondary" data-revoke-token="${esc(t.id)}">撤销授权</button>` : ""}</div>`).join("") : '<p class="muted">尚无客户端授权。</p>';
  }
  $("#mcp-tokens").addEventListener("click", run(async e => {
    const b = e.target.closest("[data-revoke-token]"); if (!b) return;
    await api(`/api/mcp/tokens/${encodeURIComponent(b.dataset.revokeToken)}`, {method: "DELETE"});
    await loadTokens(); notify("授权已撤销");
  }));
  function renderMCP(out) {
    mcpVersion = out.version;
    $("#mcp-public-url").value = out.public_url;
    $("#mcp-enabled").checked = out.enabled;
    $("#mcp-endpoint").value = out.endpoint;
    $("#mcp-codex-command").value = out.codex_command;
    $("#mcp-codex-toml").value = out.codex_toml;
    $("#mcp-connection-info").hidden = !out.endpoint;
    $("#mcp-setup-note").textContent = out.enabled ? "MCP 已启用。完成反向代理后检测连接，再在客户端连接并登录授权。" : "MCP 已关闭。域名可暂时留空，原邮箱整理仍可使用。";
  }
  $("#mcp-settings-form").addEventListener("submit", run(async e => {
    await busy($("button.primary", e.currentTarget), "保存中…", async () => {
      renderMCP(await api("/api/mcp/settings", {method: "PUT", body: JSON.stringify({enabled: $("#mcp-enabled").checked, public_url: $("#mcp-public-url").value.trim(), version: mcpVersion})}));
      $("#mcp-checks").textContent = ""; await loadTokens(); notify("MCP 配置已保存并立即生效");
    });
  }));
  $("#check-mcp").addEventListener("click", run(async e => {
    await busy(e.currentTarget, "检测中…", async () => {
      const out = await api("/api/mcp/settings/check", {method: "POST", body: "{}"});
      $("#mcp-checks").innerHTML = '<p class="muted small">以下检测使用已保存配置。</p>' + out.checks.map(c => `<p>${c.ok ? "✓" : "·"} ${esc(c.name)}：${esc(c.detail)}</p>`).join("");
    });
  }));
  $$('[data-copy]').forEach(b => b.addEventListener("click", run(async () => {
    const input = document.getElementById(b.dataset.copy);
    if (navigator.clipboard && window.isSecureContext) { await navigator.clipboard.writeText(input.value); notify("已复制"); }
    else { input.focus(); input.select(); notify("已选中，请使用复制快捷键或长按复制"); }
  })));
  window.loadOrganizer = async () => {
    const previous = $("#organizer-account").value;
    $("#organizer-account").innerHTML = state.accounts.map(a => `<option value="${esc(a.id)}">${esc(a.name)}</option>`).join("");
    if (state.accounts.some(a => a.id === previous)) $("#organizer-account").value = previous;
    await Promise.all([loadFolderSettings(), loadOperations(), loadTokens(), api("/api/mcp/settings").then(renderMCP)]);
  };
  window.checkOAuthConsent = async () => {
    const id = new URLSearchParams(location.search).get("oauth_request"); if (!id || oauthID === id) return;
    const request = await api(`/api/oauth/requests/${encodeURIComponent(id)}`); oauthID = id;
    $("#oauth-client").textContent = `客户端：${request.client_name || request.client_id}；回调地址：${request.redirect_uri}`;
    $("#oauth-accounts").innerHTML = '<legend>邮箱范围（默认均未选择）</legend>' + state.accounts.map(a => `<label class="checkbox-label"><input name="account" type="checkbox" value="${esc(a.id)}" />${esc(a.name)} · ${esc(a.username)}</label>`).join("");
    $("#oauth-actions").innerHTML = '<legend>允许动作</legend><p>读取邮件</p>' + (request.scope.split(" ").includes("mail.write") ? Object.entries(actionNames).filter(([k]) => k !== "read").map(([k, v]) => `<label class="checkbox-label"><input name="action" type="checkbox" value="${k}" />${v}</label>`).join("") : "<p>此客户端只申请了读取权限。</p>");
    $("#oauth-dialog").showModal();
  };
  async function consent(deny) {
    const form = $("#oauth-consent"), accounts = $$('input[name="account"]:checked', form).map(x => x.value);
    if (!deny && !accounts.length) throw new Error("至少选择一个邮箱");
    const out = await api(`/api/oauth/requests/${encodeURIComponent(oauthID)}`, {method: "POST", body: JSON.stringify({accounts, actions: $$('input[name="action"]:checked', form).map(x => x.value), deny})});
    location.assign(out.redirect);
  }
  $("#oauth-consent").addEventListener("submit", run(() => consent(false)));
  $("#oauth-deny").addEventListener("click", run(() => consent(true)));
})();
