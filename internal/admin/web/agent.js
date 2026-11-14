"use strict";
(() => {
  let provider = null, connections = [], grant = null, interaction = null, accounts = [], folders = {};
  const request = (url, method, body) => api(url, { method, body: JSON.stringify(body) });
  const wrap = (fn) => async (e) => {
    if (shouldPreventDefault(e)) e.preventDefault();
    try {
      await fn(e);
    } catch (err) {
      notify(err.message, true);
    }
  };
  const time = (at) => (at ? new Date(at * 1000).toLocaleString("zh-CN", { timeZone: "Asia/Shanghai" }) : "—");
  const stateLabels = {
    off: "未开启", starting: "启动中", online: "在线接收", webhook_conflict: "冲突：Bot 已设置 webhook（不会自动删除）",
    consumer_conflict: "冲突：另一个程序正在接收该 Bot 更新", token_invalid: "Bot Token 无效", credential_missing: "未保存 Bot Token", error: "接收异常",
  };
  const actionLabels = { read: "查询", rule_manage: "规则管理", task_manage: "任务管理", automation: "手动运行", task_run_manage: "运行管理" };

  function line(ok, text, extra = "") {
    return `<li><span class="${ok ? "ok" : "no"}">${ok ? "✔" : "○"}</span><span>${esc(text)}</span>${extra ? `<span class="muted small">${esc(extra)}</span>` : ""}</li>`;
  }

  function renderCapabilities() {
    const caps = provider?.capabilities || {};
    const configured = !!(provider?.key_configured && provider?.model && provider?.base_url);
    const grantOK = !!(grant && grant.enabled && !grant.stale_admin_version && grant.expires * 1000 > Date.now());
    const write = grantOK && grant.actions.some((a) => a !== "read");
    $("#agent-capabilities").innerHTML = [
      line(configured, "模型配置完成", configured ? provider.model : ""),
      line(caps.tool_loop === "ok", "模型检测通过（含工具调用）", caps.tool_loop && caps.tool_loop !== "ok" ? caps.tool_loop : caps.tool_loop_at ? `检测于 ${time(caps.tool_loop_at)}` : "未检测"),
      line(!!provider?.enabled, "Agent 已启用"),
      line(!!interaction?.credential_ready, "TG 凭据有效（来自 TG 通知设置）"),
      line(interaction?.state === "online", "轮询在线", stateLabels[interaction?.state] || interaction?.state || ""),
      line(!!interaction?.binding, "已绑定私聊", interaction?.binding ? `用户 ${interaction.binding.user_id}` : ""),
      line(write, "拥有写入相关授权", grantOK ? grant.actions.map((a) => actionLabels[a] || a).join("、") : "未授权或已过期"),
      line(false, "持续 AI 语义分类", "首版未支持"),
    ].join("");
  }

  // ---- 模型连接 ----
  // The form is an in-memory draft: nothing is applied until “检测并使用”
  // passes, and a refresh never overwrites unsaved edits. Model lists live
  // only on this page and belong to one connection + address + key; any
  // switch or edit bumps listToken so a late response is discarded.
  // `base` is what the draft was loaded from (connection and provider
  // versions). A refresh that keeps a draft never moves it, so a draft made
  // before a change elsewhere still conflicts. While a check or save is in
  // flight the form is locked, so the response cannot overwrite newer edits.
  const presets = [
    ["", "自定义兼容接口", ""],
    ["openai", "OpenAI", "https://api.openai.com/v1"],
    ["deepseek", "DeepSeek", "https://api.deepseek.com/v1"],
    ["openrouter", "OpenRouter", "https://openrouter.ai/api/v1"],
    ["dashscope", "阿里云百炼", "https://dashscope.aliyuncs.com/compatible-mode/v1"],
    ["siliconflow", "硅基流动", "https://api.siliconflow.cn/v1"],
  ];
  let selected = "", dirty = false, listToken = 0, locked = false, draftGen = 0;
  let base = { id: "", version: 0, providerVersion: 0 };
  const current = () => connections.find((c) => c.id === selected) || null;
  const endpointOf = (base) => {
    try {
      const u = new URL(base.trim());
      return u.protocol === "https:" ? `${u.origin}${u.pathname.replace(/\/+$/, "")}/chat/completions` : "仅支持 https 地址";
    } catch {
      return "—";
    }
  };
  const providerCodes = {
    tool_call_unsupported: "模型不支持工具调用", provider_auth_failed: "密钥无效", provider_network_error: "网络错误",
    provider_timeout: "超时", provider_request_rejected: "请求被拒绝", provider_destination_blocked: "地址不允许",
  };

  function resetModelList(note = "") {
    listToken++;
    $("#model-options").innerHTML = "";
    $("#model-list-note").textContent = note;
  }

  function renderCurrent() {
    const p = provider;
    const conn = connections.find((c) => c.id === p.connection_id);
    const caps = p.capabilities || {};
    let text;
    if (p.enabled && caps.tool_loop === "ok") {
      text = `当前生效：${conn ? conn.name : "未保存的配置"} · ${p.model}（检测于 ${time(caps.tool_loop_at)}）`;
    } else if (p.base_url) {
      text = `Agent 未启用。上次配置：${conn ? conn.name : p.base_url} · ${p.model || "未选模型"}${caps.tool_loop && caps.tool_loop !== "ok" ? `；检测未通过（${caps.tool_loop}）` : ""}`;
    } else {
      text = "尚未配置模型。选择服务商、填写密钥并选择模型后点击“检测并使用”。";
    }
    $("#model-current").textContent = text;
    $("#model-current").classList.toggle("is-active", !!(p.enabled && caps.tool_loop === "ok"));
    $("#model-disable").hidden = !p.enabled;
  }

  function renderConnectionOptions() {
    $("#model-connection").innerHTML = connections.map((c) => {
      const tag = c.active ? (provider.enabled ? "（使用中）" : "（上次使用）") : "";
      return `<option value="${esc(c.id)}">${esc(c.name)}${tag} · ${esc(c.model || "未选模型")}</option>`;
    }).join("") + '<option value="">＋ 添加新连接</option>';
    $("#model-connection").value = selected;
  }

  function advancedSummary() {
    const t = Number($("#model-timeout").value) || 60, m = $("#model-max-param").value;
    const parts = [];
    if (t !== 60) parts.push(`超时 ${t} 秒`);
    if (m) parts.push(m);
    const net = $("#agent-network-policy");
    if (net && net.value && net.selectedIndex > 0) parts.push(`连接方式：${net.options[net.selectedIndex].text}`);
    $("#model-advanced-summary").textContent = parts.length ? `（当前非默认：${parts.join("，")}）` : "";
  }

  function fillForm() {
    const c = current();
    const isNew = !c;
    $("#model-preset-field").hidden = !isNew;
    $("#model-preset").value = "";
    $("#model-name").value = c?.name || "";
    $("#model-base-url").value = c?.base_url || "";
    $("#model-model").value = c?.model || "";
    $("#model-key").value = "";
    $("#model-timeout").value = c?.timeout_seconds || 60;
    $("#model-max-param").value = c?.max_tokens_param || "";
    $("#model-endpoint").textContent = `请求地址：${c?.endpoint || endpointOf($("#model-base-url").value)}`;
    keyNote();
    $("#model-delete").hidden = isNew || !!(c.active && provider.enabled);
    // For the connection in use, saving without checking would look like a
    // change that is not in effect; the only primary action is 检测并使用.
    $("#model-save").hidden = !!(c && c.active && provider.enabled);
    $("#model-cancel").hidden = true;
    $("#model-status").textContent = c?.pending ? "该连接有已保存但尚未生效的修改：点击“检测并使用”后生效。" : "";
    base = { id: c?.id || "", version: c?.version || 0, providerVersion: provider.version };
    dirty = false;
    resetModelList();
    advancedSummary();
  }

  function keyNote() {
    const c = current();
    let note = "首次使用须填写密钥。";
    if (c?.key_configured) {
      const same = (() => {
        try {
          return new URL($("#model-base-url").value.trim()).origin === new URL(c.base_url).origin;
        } catch {
          return false;
        }
      })();
      note = same ? "已保存密钥（不回显）；留空继续使用。" : "API 地址的域名已更改，须重新填写密钥。";
    }
    $("#model-key-note").textContent = note;
    $("#model-key").placeholder = c?.key_configured ? "留空表示保留原密钥" : "填写密钥";
  }

  function markDirty() {
    dirty = true;
    draftGen++;
    $("#model-cancel").hidden = false;
    $("#model-status").textContent = "有未应用的修改。";
  }

  function draftBody() {
    return {
      id: base.id,
      version: base.version,
      name: $("#model-name").value.trim(),
      base_url: $("#model-base-url").value.trim(),
      model: $("#model-model").value.trim(),
      api_key: $("#model-key").value.trim(),
      timeout_seconds: Number($("#model-timeout").value) || 60,
      max_tokens_param: $("#model-max-param").value,
    };
  }

  async function loadConnections(keepDraft) {
    const out = await api("/api/agent/connections");
    connections = out.connections;
    provider = out.provider;
    if (keepDraft && (dirty || locked) && (selected === "" || connections.some((c) => c.id === selected))) {
      renderConnectionOptions();
      renderCurrent();
      const c = current();
      if (dirty && ((c && c.version !== base.version) || provider.version !== base.providerVersion)) {
        $("#model-status").textContent = "有未应用的修改；此连接或当前配置已在其他页面或操作中更新，提交会提示冲突。可点击“取消修改”载入最新配置。";
      }
      return;
    }
    if (!connections.some((c) => c.id === selected)) {
      selected = (connections.find((c) => c.active) || connections[0] || { id: "" }).id;
    }
    renderConnectionOptions();
    renderCurrent();
    fillForm();
  }

  function lock(on) {
    locked = on;
    $("#model-fields").disabled = on;
  }

  // Runs a submit with the form locked. A version conflict keeps the draft
  // and its base; the list is refreshed so the newer state is visible.
  // A success only reloads the form when the draft is still the one that
  // was submitted; otherwise the newer draft stays, rebased on the versions
  // this submission produced (they came from this draft, not another page).
  async function settle(gen, conn, providerVersion, applied) {
    selected = conn?.id || selected;
    if (draftGen !== gen && dirty) {
      base = { id: conn?.id || base.id, version: conn?.version || base.version, providerVersion };
      await loadConnections(true);
      $("#model-status").textContent = `${applied}；之后的修改尚未应用。`;
      return;
    }
    dirty = false;
    await loadConnections();
    $("#model-status").textContent = `${applied}。`;
  }

  async function submitLocked(button, label, task) {
    lock(true);
    try {
      await busy(button, label, () => task(draftGen));
    } catch (err) {
      if (/已被修改/.test(err.message)) {
        await loadConnections(true).catch(() => {});
        $("#model-status").textContent = "未提交：此连接或当前配置已在其他页面或操作中被修改。你的草稿已保留；点击“取消修改”载入最新配置后再提交。";
      }
      throw err;
    } finally {
      lock(false);
    }
  }

  $("#model-preset").innerHTML = presets.map(([id, label]) => `<option value="${id}">${esc(label)}</option>`).join("");
  $("#model-preset").addEventListener("change", () => {
    const preset = presets.find(([id]) => id === $("#model-preset").value);
    if (preset && preset[2]) {
      $("#model-base-url").value = preset[2];
      if (!$("#model-name").value.trim() || presets.some(([, label]) => label === $("#model-name").value.trim())) $("#model-name").value = preset[1];
    }
    $("#model-base-url").dispatchEvent(new Event("input", { bubbles: true }));
  });

  $("#model-connection").addEventListener("change", () => {
    if (dirty && !confirm("放弃当前未应用的修改？")) {
      $("#model-connection").value = selected;
      return;
    }
    selected = $("#model-connection").value;
    fillForm();
  });

  $("#model-form").addEventListener("input", (e) => {
    if (e.target.id === "model-preset" || e.target.id === "model-connection" || e.target.id === "agent-network-policy") return;
    if (e.target.id === "model-base-url" || e.target.id === "model-key") {
      resetModelList("地址或密钥已更改，请重新获取模型列表。");
      $("#model-endpoint").textContent = `请求地址：${endpointOf($("#model-base-url").value)}`;
      keyNote();
    }
    if (e.target.id === "model-timeout" || e.target.id === "model-max-param") advancedSummary();
    markDirty();
  });
  $("#model-form").addEventListener("change", (e) => {
    if (e.target.id === "model-max-param") {
      advancedSummary();
      markDirty();
    }
    if (e.target.id === "agent-network-policy") advancedSummary();
  });

  $("#model-fetch").addEventListener("click", wrap(async () => {
    const body = draftBody();
    if (!body.base_url) throw new Error("请先填写 API 地址");
    const token = ++listToken;
    $("#model-list-note").textContent = "正在获取模型列表…";
    await busy($("#model-fetch"), "获取中…", async () => {
      try {
        const out = await request("/api/agent/connections/models", "POST", { id: body.id, base_url: body.base_url, api_key: body.api_key });
        if (token !== listToken) return; // switched or edited meanwhile
        $("#model-options").innerHTML = out.models.map((m) => `<option value="${esc(m)}"></option>`).join("");
        $("#model-list-note").textContent = out.models.length
          ? `已获取 ${out.models.length} 个模型：在“模型”框中输入关键字搜索，或直接填写。`
          : "模型列表为空：请直接填写模型名称。";
        if (out.models.length) $("#model-model").focus();
      } catch (err) {
        if (token === listToken) $("#model-list-note").textContent = `${err.message}。仍可直接填写模型名称。`;
      }
    });
  }));

  $("#model-form").addEventListener("submit", wrap(async (e) => {
    const body = draftBody();
    if (!body.base_url || !body.model) throw new Error("请填写 API 地址并选择或输入模型");
    await submitLocked($("#model-activate"), "检测中…", async (gen) => {
      $("#model-status").textContent = "正在检测（约 5–30 秒），完成前暂不能修改…";
      const out = await request("/api/agent/connections/activate", "POST", { ...body, provider_version: base.providerVersion });
      if (!out.ok) {
        provider = out.provider || provider;
        $("#model-status").textContent = `检测未通过，当前生效配置未改变：${out.message || out.result}`;
        notify("检测未通过，未切换", true);
        renderCurrent();
        renderCapabilities();
        return;
      }
      await settle(gen, out.connection, out.provider.version, out.unchanged ? "该配置已在使用中，无需重新检测" : `检测通过，已切换并启用（模型 ${body.model}）`);
      renderCapabilities();
      notify(out.unchanged ? "配置未变化" : "已检测并使用新的模型连接");
    });
  }));

  $("#model-save").addEventListener("click", wrap(async () => {
    const body = draftBody();
    if (!body.base_url) throw new Error("请填写 API 地址");
    await submitLocked($("#model-save"), "保存中…", async (gen) => {
      const saved = await request("/api/agent/connections", "POST", body);
      await settle(gen, saved, base.providerVersion, "已保存为备用连接，当前生效配置未改变");
      notify("连接已保存");
    });
  }));

  $("#model-cancel").addEventListener("click", () => fillForm());
  // Network policies load after the model card; recompute the summary so a
  // non-default Agent route is visible while the section is collapsed.
  document.addEventListener("network-policies-rendered", advancedSummary);

  $("#model-delete").addEventListener("click", wrap(async () => {
    const c = current();
    if (!c || !confirm(`删除连接“${c.name}”？其保存的密钥会一并删除。`)) return;
    await request("/api/agent/connections/delete", "POST", { id: c.id, version: c.version });
    selected = "";
    dirty = false;
    await loadConnections();
    notify("连接已删除");
  }));

  $("#model-disable").addEventListener("click", wrap(async () => {
    if (!confirm("停用 Agent？排队和进行中的 Agent 工作会取消，待确认方案失效；已启用的固定任务不受影响。")) return;
    const p = provider;
    await request("/api/agent/provider", "PUT", { version: p.version, enabled: false, base_url: p.base_url, model: p.model, timeout_seconds: p.timeout_seconds, max_tokens_param: p.max_tokens_param });
    await loadConnections(true);
    renderCapabilities();
    notify("Agent 已停用");
  }));

  $("#model-check-chat").addEventListener("click", wrap(async () => {
    await busy($("#model-check-chat"), "检测中…", async () => {
      const out = await request("/api/agent/provider/check", "POST", { kind: "chat" });
      provider = out.provider;
      renderCapabilities();
      $("#model-status").textContent = out.ok ? "聊天测试通过（当前生效配置）。" : `聊天测试未通过：${providerCodes[out.result] || out.result}`;
    });
  }));

  function renderGrant() {
    const current = grant || { accounts: [], folders: {}, actions: ["read"], enabled: true };
    $("#agent-grant-accounts").innerHTML = accounts.length
      ? accounts.map((a) => {
          const on = current.accounts.includes(a.id);
          const scoped = current.folders?.[a.id] || [];
          const allScope = on && (current.all_folders ? current.all_folders.includes(a.id) : scoped.length === 0);
          const list = folders[a.id] || [];
          return `<fieldset data-account="${esc(a.id)}"><legend><label class="checkbox-label"><input type="checkbox" data-role="account" ${on ? "checked" : ""} />${esc(a.name)} · ${esc(a.username)}</label></legend>
            <label class="checkbox-label"><input type="checkbox" data-role="all" ${allScope || !on ? "checked" : ""} />全部文件夹</label>
            <div class="agent-folders">${list.map((f) => `<label class="checkbox-label"><input type="checkbox" data-role="folder" value="${esc(f.name)}" ${scoped.includes(f.name) ? "checked" : ""} />${esc(f.name)}</label>`).join("")}</div></fieldset>`;
        }).join("")
      : '<p class="muted">请先添加邮箱。</p>';
    $$(".agent-actions input").forEach((x) => (x.checked = x.value === "read" || current.actions.includes(x.value)));
    $("#agent-grant-enabled").checked = current.enabled !== false;
    $("#agent-grant-status").textContent = grant
      ? `版本 ${grant.version}，有效期至 ${time(grant.expires)}${grant.stale_admin_version ? "。管理员凭据已变更，授权已失效：请重新保存。" : ""}`
      : "尚未授权。";
  }

  function renderInteraction() {
    const i = interaction;
    $("#agent-telegram-state").innerHTML = `<p>状态：<strong>${esc(stateLabels[i.state] || i.state)}</strong>${i.error_code ? `（${esc(i.error_code)}）` : ""}</p>
      <p class="muted small">Bot：${i.bot_username ? "@" + esc(i.bot_username) : "—"} · 最近接收：${time(i.last_poll_at)} · 接收起点：${time(i.activation_time)}</p>
      <p class="muted small">${i.binding ? `已绑定：Telegram 用户 ${esc(i.binding.user_id)}（私聊 ${esc(i.binding.chat_id)}），绑定于 ${time(i.binding.created_at)}` : "未绑定。生成绑定码后，在 Telegram 私聊 Bot 发送 /bind 绑定码。"}</p>`;
    $("#agent-telegram-toggle").textContent = i.enabled ? "关闭交互" : "开启交互";
  }

  async function loadRecords() {
    const out = await api("/api/agent/conversations");
    const states = { queued: "排队", running: "处理中", completed: "完成", failed: "失败", cancelled: "已停止", interrupted: "中断" };
    const turns = out.turns.map((t) => `<details class="agent-record" data-turn="${esc(t.id)}"><summary>${time(t.created_at)} · 第 ${t.seq} 轮 · ${esc(states[t.state] || t.state)}${t.error_code ? ` · ${esc(t.error_code)}` : ""} · 模型请求 ${t.model_requests} · 工具 ${t.tool_calls}</summary><div class="agent-turn-detail muted small">展开加载…</div>${t.state === "running" || t.state === "queued" ? `<button type="button" class="secondary" data-stop="${esc(t.id)}">停止</button>` : ""}</details>`).join("");
    const props = out.proposals.map((p) => `<div class="agent-record small">方案 ${esc(p.id.slice(0, 8))} · ${esc(p.kind)} · ${esc(p.state)}${p.error_code ? ` · ${esc(p.error_code)}` : ""} · ${time(p.created_at)}</div>`).join("");
    const links = out.run_links.map((l) => `<div class="agent-record small">运行 <a href="#task-run=${encodeURIComponent(l.task_run_id)}">${esc(l.task_run_id.slice(0, 8))}</a> · 当前 ${esc(l.run_state || "—")} · 推送 ${esc(l.last_state || "—")}</div>`).join("");
    $("#agent-records").innerHTML = `<p class="muted small">排队轮次：${out.queued}</p><h3>轮次</h3>${turns || '<p class="muted">暂无记录。</p>'}<h3>确认方案</h3>${props || '<p class="muted">暂无。</p>'}<h3>关联任务运行</h3>${links || '<p class="muted">暂无。</p>'}`;
  }

  window.loadAgent = async () => {
    const [, g, i, accs] = await Promise.all([loadConnections(true), api("/api/agent/grant"), api("/api/telegram/interaction"), api("/api/accounts")]);
    grant = g.grant;
    interaction = i;
    accounts = accs;
    folders = {};
    await Promise.all(accounts.map(async (a) => (folders[a.id] = await api(`/api/accounts/${encodeURIComponent(a.id)}/folders`).catch(() => []))));
    renderGrant();
    renderInteraction();
    renderCapabilities();
    await loadRecords();
  };

  $("#agent-grant-form").addEventListener("change", (e) => {
    const box = e.target.closest("fieldset[data-account]");
    if (!box) return;
    if (e.target.dataset.role === "folder" && e.target.checked) $('[data-role="all"]', box).checked = false;
    if (e.target.dataset.role === "all" && e.target.checked) $$('[data-role="folder"]', box).forEach((x) => (x.checked = false));
  });

  // Folder scope is explicit per account: "全部文件夹" goes to all_folders;
  // otherwise at least one folder must be ticked. An empty selection is never
  // sent (the stored empty list means "all folders").
  function grantBody() {
    const chosen = $$("fieldset[data-account]").filter((f) => $('[data-role="account"]', f).checked);
    if (!chosen.length) return { error: "请至少选择一个账号。" };
    const all = [], folders = {};
    for (const f of chosen) {
      const id = f.dataset.account;
      if ($('[data-role="all"]', f).checked) {
        all.push(id);
        folders[id] = [];
        continue;
      }
      const picked = $$('[data-role="folder"]:checked', f).map((x) => x.value);
      if (!picked.length) return { error: "未勾选“全部文件夹”的账号必须至少选择一个具体文件夹。" };
      folders[id] = picked;
    }
    return {
      body: {
        version: grant?.version || 0,
        enabled: $("#agent-grant-enabled").checked,
        days: Number($("#agent-grant-days").value) || 30,
        accounts: chosen.map((f) => f.dataset.account),
        all_folders: all,
        folders,
        actions: $$(".agent-actions input").filter((x) => x.checked).map((x) => x.value),
      },
    };
  }

  $("#agent-grant-form").addEventListener("submit", wrap(async (e) => {
    const { body, error } = grantBody();
    if (error) {
      $("#agent-grant-status").textContent = error;
      notify(error, true);
      return;
    }
    await busy($('button[type="submit"]', e.currentTarget), "保存中…", async () => {
      grant = await request("/api/agent/grant", "PUT", body);
      renderGrant();
      renderCapabilities();
      notify("授权已保存；待确认的方案和旧对话已失效");
    });
  }));

  $("#agent-grant-revoke").addEventListener("click", wrap(async () => {
    if (!grant || !confirm("撤销 Agent 授权？绑定会一并解除，排队和待确认的 Agent 工作会取消。已启用的固定任务继续按原授权运行。")) return;
    await request("/api/agent/grant/revoke", "POST", {});
    notify("已撤销授权并解除绑定");
    await window.loadAgent();
  }));

  $("#agent-telegram-toggle").addEventListener("click", wrap(async () => {
    await busy($("#agent-telegram-toggle"), "处理中…", async () => {
      interaction = await request("/api/telegram/interaction", "PUT", { version: interaction.version, enabled: !interaction.enabled });
      renderInteraction();
      renderCapabilities();
      notify(interaction.enabled ? "交互已开启：此前积压的消息不会触发管理动作" : "交互已关闭：未开始的 Agent 工作已取消");
    });
  }));

  $("#agent-bind-code").addEventListener("click", wrap(async () => {
    const out = await request("/api/telegram/bind-code", "POST", {});
    $("#agent-bind-output").innerHTML = `5 分钟内在 Telegram 私聊 Bot 发送（只显示这一次）：<br /><span class="agent-code">${esc(out.command)}</span>`;
  }));

  $("#agent-unbind").addEventListener("click", wrap(async () => {
    if (!confirm("解除 Telegram 绑定？未开始的 Agent 工作和待确认卡片会失效。")) return;
    await request("/api/telegram/unbind", "POST", {});
    $("#agent-bind-output").textContent = "";
    notify("已解除绑定");
    await window.loadAgent();
  }));

  $("#agent-refresh").addEventListener("click", wrap(loadRecords));

  $("#agent-records").addEventListener("click", wrap(async (e) => {
    const stop = e.target.closest("[data-stop]");
    if (stop) {
      const out = await request(`/api/agent/turns/${encodeURIComponent(stop.dataset.stop)}/stop`, "POST", {});
      notify(out.result);
      await loadRecords();
    }
  }));

  $("#agent-records").addEventListener("toggle", (e) => {
    const box = e.target;
    if (!box.open || !box.dataset.turn || box.dataset.loaded) return;
    box.dataset.loaded = "1";
    api(`/api/agent/turns/${encodeURIComponent(box.dataset.turn)}`).then((d) => {
      const calls = d.tool_calls.map((c) => `<li>${esc(c.name)} · ${esc(c.state)}${c.result_code && c.result_code !== "ok" ? ` · ${esc(c.result_code)}` : ""}${c.target_id ? ` · 对象 ${esc(c.target_id)} v${c.target_version}` : ""}</li>`).join("");
      const steps = d.proposal_steps.map((s) => `<li>方案步骤 ${s.ordinal + 1} ${esc(s.kind)} · ${esc(s.state)}${s.target_id ? ` · ${esc(s.target_id)} v${s.target_version}` : ""}${s.error_code ? ` · ${esc(s.error_code)}` : ""}</li>`).join("");
      $(".agent-turn-detail", box).innerHTML = `<ul>${calls || "<li>无工具调用</li>"}${steps}</ul>`;
    }).catch((err) => ($(".agent-turn-detail", box).textContent = err.message));
  }, true);
})();
