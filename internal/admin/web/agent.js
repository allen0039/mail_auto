"use strict";
(() => {
  let provider = null, grant = null, interaction = null, accounts = [], folders = {};
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
      line(caps.chat === "ok", "聊天可用", caps.chat && caps.chat !== "ok" ? caps.chat : caps.chat_at ? `检测于 ${time(caps.chat_at)}` : "未检测"),
      line(caps.tool_loop === "ok", "工具闭环可用", caps.tool_loop && caps.tool_loop !== "ok" ? caps.tool_loop : caps.tool_loop_at ? `检测于 ${time(caps.tool_loop_at)}` : "未检测"),
      line(!!provider?.enabled, "Agent 已启用"),
      line(!!interaction?.credential_ready, "TG 凭据有效（来自 TG 通知设置）"),
      line(interaction?.state === "online", "轮询在线", stateLabels[interaction?.state] || interaction?.state || ""),
      line(!!interaction?.binding, "已绑定私聊", interaction?.binding ? `用户 ${interaction.binding.user_id}` : ""),
      line(write, "拥有写入相关授权", grantOK ? grant.actions.map((a) => actionLabels[a] || a).join("、") : "未授权或已过期"),
      line(false, "持续 AI 语义分类", "首版未支持"),
    ].join("");
  }

  function renderProvider() {
    $("#agent-base-url").value = provider.base_url || "";
    $("#agent-model").value = provider.model || "";
    $("#agent-timeout").value = provider.timeout_seconds || 60;
    $("#agent-max-param").value = provider.max_tokens_param || "";
    $("#agent-enabled").checked = !!provider.enabled;
    $("#agent-clear-key").checked = false;
    $("#agent-key").value = "";
    $("#agent-key-note").textContent = provider.key_configured ? "已保存密钥（不回显）；留空表示保留。" : "未保存密钥。";
    $("#agent-endpoint").textContent = `请求地址：${provider.endpoint || "—"}`;
    const caps = provider.capabilities || {};
    $("#agent-provider-status").textContent = provider.enabled
      ? "Agent 已启用。"
      : caps.tool_loop === "ok"
        ? "工具闭环检测已通过，可勾选“启用 Agent”后保存。"
        : caps.chat === "ok"
          ? "可聊天，未验证工具调用：不能启用写操作 Agent。"
          : "尚未检测。";
  }

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
    const [p, g, i, accs] = await Promise.all([api("/api/agent/provider"), api("/api/agent/grant"), api("/api/telegram/interaction"), api("/api/accounts")]);
    provider = p;
    grant = g.grant;
    interaction = i;
    accounts = accs;
    folders = {};
    await Promise.all(accounts.map(async (a) => (folders[a.id] = await api(`/api/accounts/${encodeURIComponent(a.id)}/folders`).catch(() => []))));
    renderProvider();
    renderGrant();
    renderInteraction();
    renderCapabilities();
    await loadRecords();
  };

  $("#agent-provider-form").addEventListener("submit", wrap(async (e) => {
    await busy($('button[type="submit"]', e.currentTarget), "保存中…", async () => {
      provider = await request("/api/agent/provider", "PUT", {
        version: provider.version,
        enabled: $("#agent-enabled").checked,
        base_url: $("#agent-base-url").value.trim(),
        model: $("#agent-model").value.trim(),
        timeout_seconds: Number($("#agent-timeout").value) || 60,
        max_tokens_param: $("#agent-max-param").value,
        api_key: $("#agent-key").value.trim(),
        clear_key: $("#agent-clear-key").checked,
      });
      renderProvider();
      renderCapabilities();
      notify("模型设置已保存");
    });
  }));

  for (const [selector, kind, label] of [["#agent-check-chat", "chat", "聊天测试"], ["#agent-check-tool", "tool_loop", "工具闭环测试"]]) {
    $(selector).addEventListener("click", wrap(async () => {
      await busy($(selector), "检测中…", async () => {
        const out = await request("/api/agent/provider/check", "POST", { kind });
        provider = out.provider;
        renderProvider();
        renderCapabilities();
        $("#agent-provider-status").textContent = out.ok ? `${label}通过。` : `${label}未通过：${out.result}`;
      });
    }));
  }

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
