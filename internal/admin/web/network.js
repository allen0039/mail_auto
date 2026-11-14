"use strict";
(() => {
  let draft = null;
  let changed = false;
  const policyValue = (policy) => policy?.mode === "proxy" ? `proxy:${policy.proxy_id}` : policy?.mode || "inherit";
  const parsePolicy = (value) => value.startsWith("proxy:")
    ? { mode: "proxy", proxy_id: value.slice(6) }
    : { mode: value, proxy_id: "" };
  const options = (settings, inherit = true) => [
    ...(inherit ? ['<option value="inherit">跟随全局</option>'] : []),
    '<option value="direct">直连</option>',
    ...(settings?.profiles || []).map((profile) => `<option value="proxy:${esc(profile.id)}">代理：${esc(profile.name)}</option>`),
  ].join("");
  const stages = {
    tcp_tunnel: "TCP / 代理隧道", target_tls: "目标 TLS 证书", proxy_connect: "连接代理",
    proxy_tls: "代理 TLS 证书", proxy_auth: "代理认证", proxy_tunnel: "代理隧道",
    timeout: "连接超时", connection: "网络连接", proxy: "代理设置",
    target_dns: "模型目标与 DNS 检查",
    imap_login_folders: "邮箱登录与文件夹", telegram_get_me: "验证机器人", agent_chat: "模型聊天",
  };
  const errors = {
    proxy_connection_failed: "无法连接代理，请检查地址、监听接口和防火墙",
    proxy_auth_failed: "代理认证失败，请核对用户名和密码",
    proxy_socks5_failed: "SOCKS5 隧道失败，请核对认证、目标解析和代理出口",
    proxy_tls_failed: "代理证书校验失败，请核对代理地址与证书",
    target_tls_failed: "目标 TLS 校验或握手失败，请检查出口和目标证书",
    telegram_credential_missing: "请先在 TG 通知保存 Bot Token",
    provider_destination_blocked: "模型地址解析到非公网目标，连接被拒绝",
    network_timeout: "网络连接超时",
    network_connection_failed: "连接失败，请检查网络及 DNS",
    imap_login_or_folders_failed: "邮箱登录或读取目录失败，请核对授权码和 IMAP 状态",
  };
  const wrap = (fn) => async (event) => {
    event?.preventDefault();
    try { await fn(event); } catch (error) { notify(error.message, true); }
  };

  function renderPolicies(settings, container = document) {
    $$("[data-network-scope]", container).forEach((select) => {
      const scope = select.dataset.networkScope;
      select.innerHTML = options(settings, scope !== "global");
      select.value = policyValue(settings[scope]);
    });
  }

  function capturePolicies() {
    for (const scope of ["global", "telegram", "agent"]) {
      draft[scope] = parsePolicy($(`#network-${scope}`).value);
    }
  }

  function markChanged() {
    changed = true;
    $("#network-save-status").textContent = "有待保存的修改；保存后再诊断。";
  }

  function renderProfiles() {
    $("#network-profiles").innerHTML = draft.profiles.length
      ? draft.profiles.map((profile) => `<article class="network-profile">
          <div><strong>${esc(profile.name)}</strong><p class="muted small">${esc(profile.protocol)}://${esc(profile.host)}:${esc(profile.port)} · ${profile.password_set || profile.password ? "已设置认证" : "无认证"}${profile.trusted_remote_dns ? " · 信任代理解析模型域名" : ""}</p></div>
          <div class="organizer-inline"><button type="button" class="secondary" data-proxy-edit="${esc(profile.id)}">编辑</button><button type="button" class="danger" data-proxy-remove="${esc(profile.id)}">移除</button></div>
        </article>`).join("")
      : '<p class="muted">尚未配置代理。填写下方表单加入配置，然后保存。</p>';
  }

  function resetProfile() {
    $("#network-profile-form").reset();
    $("#proxy-id").value = "";
  }

  function renderTargets() {
    $("#network-check-target").innerHTML = '<option value="global">全局出口（Telegram TLS）</option><option value="telegram">Telegram 机器人</option><option value="agent">内置 Agent 模型</option>'
      + state.accounts.map((account) => `<option value="account:${esc(account.id)}">邮箱：${esc(account.name)}</option>`).join("");
    $("#network-check-route").innerHTML = '<option value="configured">使用目标当前配置</option>'
      + options(draft, false);
  }

  window.resetNetwork = () => {
    draft = null;
    changed = false;
    state.network = null;
    resetProfile();
    $("#network-profiles").replaceChildren();
    $("#network-diagnose-results").replaceChildren();
  };
  window.fillAccountNetwork = (account) => {
    $("#account-network-policy").innerHTML = options(state.network);
    $("#account-network-policy").value = policyValue(account?.proxy || {mode: "inherit"});
  };
  window.accountNetworkPolicy = () => parsePolicy($("#account-network-policy").value);
  window.loadNetworkPolicies = async () => {
    state.network = await api("/api/network");
    for (const pane of ["agent", "notifications"]) renderPolicies(state.network, $(`#${pane}-pane`));
    document.dispatchEvent(new CustomEvent("network-policies-rendered"));
  };
  window.loadNetwork = async () => {
    state.network = await api("/api/network");
    draft = structuredClone(state.network);
    changed = false;
    renderPolicies(draft, $("#network-pane"));
    renderProfiles();
    renderTargets();
    resetProfile();
    $("#network-save-status").textContent = `已读取配置 v${draft.version}。默认全局直连；代理失败不自动直连。`;
  };

  async function save(settings) {
    const result = await api("/api/network", {method: "PUT", body: JSON.stringify(settings)});
    state.network = result;
    await refresh();
    return result;
  }

  function renderResult(result) {
    return `<article class="network-diagnostic">
      <strong>${result.ok ? "连接成功" : "连接失败"} · ${esc(result.route)}</strong>
      <p class="muted small">目标 ${esc(result.target)} · 未自动切换出口</p>
      <ul class="agent-status-list">${result.checks.map((check) => `<li><span class="${check.ok ? "ok" : "no"}">${check.ok ? "✔" : "✕"}</span><span>${esc(stages[check.stage] || check.stage)} · ${check.elapsed_ms} ms${check.ok ? "" : ` · ${esc(errors[check.code] || check.code)}`}</span></li>`).join("")}</ul>
      ${result.folders ? `<p class="muted small">已读取 ${result.folders.length} 个文件夹</p>` : ""}
    </article>`;
  }

  async function diagnose(scope, mode, proxyID, container, append = false, accountID = "") {
    if (!append) container.replaceChildren();
    const pending = document.createElement("p");
    pending.className = "muted";
    pending.textContent = "正在从服务器诊断连接…";
    container.append(pending);
    try {
      const result = await api("/api/network/check", {method: "POST", body: JSON.stringify({scope, mode, proxy_id: proxyID, account_id: accountID})});
      pending.outerHTML = renderResult(result);
    } catch (error) {
      pending.className = "error-inline";
      pending.textContent = error.message;
      throw error;
    }
  }
  window.diagnoseAccountNetwork = async (accountID, container) => {
    container.hidden = false;
    await diagnose("account", "configured", "", container, false, accountID);
  };

  $("#network-refresh").addEventListener("click", wrap(() => window.loadNetwork()));
  $("#network-policy-form").addEventListener("change", markChanged);
  $("#network-policy-form").addEventListener("submit", wrap(async (event) => {
    capturePolicies();
    await busy($("button[type=submit]", event.currentTarget), "正在保存…", async () => {
      draft = await save(draft);
      changed = false;
      renderPolicies(draft, $("#network-pane"));
      renderProfiles();
      renderTargets();
      resetProfile();
      $("#network-save-status").textContent = `已保存 v${draft.version}；Agent 有效出口变化时，请重新检测后启用。`;
      notify("代理与连接策略已保存");
    });
  }));
  $("#proxy-reset").addEventListener("click", resetProfile);
  $("#network-profile-form").addEventListener("submit", wrap(() => {
    capturePolicies();
    const id = $("#proxy-id").value || window.crypto.randomUUID();
    const old = draft.profiles.find((profile) => profile.id === id);
    const clear = $("#proxy-clear-password").checked;
    const profile = {
      id, name: $("#proxy-name").value.trim(), protocol: $("#proxy-protocol").value,
      host: $("#proxy-host").value.trim(), port: Number($("#proxy-port").value),
      username: clear ? "" : $("#proxy-username").value,
      password: clear ? "" : $("#proxy-password").value || old?.password || "",
      password_set: clear ? false : !!old?.password_set,
      clear_password: clear, trusted_remote_dns: $("#proxy-trusted-dns").checked,
    };
    draft.profiles = draft.profiles.filter((item) => item.id !== id);
    draft.profiles.push(profile);
    renderPolicies(draft, $("#network-pane"));
    renderProfiles();
    resetProfile();
    markChanged();
  }));
  $("#network-profiles").addEventListener("click", wrap((event) => {
    const edit = event.target.closest("[data-proxy-edit]");
    const remove = event.target.closest("[data-proxy-remove]");
    if (edit) {
      const profile = draft.profiles.find((item) => item.id === edit.dataset.proxyEdit);
      $("#proxy-id").value = profile.id;
      $("#proxy-name").value = profile.name;
      $("#proxy-protocol").value = profile.protocol;
      $("#proxy-host").value = profile.host;
      $("#proxy-port").value = profile.port;
      $("#proxy-username").value = profile.username;
      $("#proxy-password").value = "";
      $("#proxy-clear-password").checked = !!profile.clear_password;
      $("#proxy-trusted-dns").checked = !!profile.trusted_remote_dns;
      $("#proxy-name").focus();
    }
    if (remove) {
      capturePolicies();
      const id = remove.dataset.proxyRemove;
      const referenced = ["global", "telegram", "agent"].some((scope) => draft[scope].proxy_id === id)
        || state.accounts.some((account) => account.proxy?.proxy_id === id);
      if (referenced) throw new Error("代理仍被引用，请先保存其他连接方式，再移除代理");
      draft.profiles = draft.profiles.filter((item) => item.id !== id);
      renderPolicies(draft, $("#network-pane"));
      renderProfiles();
      markChanged();
    }
  }));
  $$("[data-network-save]").forEach((button) => button.addEventListener("click", wrap(async () => {
    const scope = button.dataset.networkSave;
    const selected = parsePolicy($(`#${scope}-network-policy`).value);
    await busy(button, "正在保存…", async () => {
      const settings = await api("/api/network");
      settings[scope] = selected;
      await save(settings);
      await window.loadNetworkPolicies();
      if (scope === "agent" && window.loadAgent) await window.loadAgent();
      notify(scope === "agent" ? "Agent 连接方式已保存；出口变化后需重新检测并启用" : "TG 连接方式已保存");
    });
  })));
  $$("[data-network-check]").forEach((button) => button.addEventListener("click", wrap(async () => {
    const scope = button.dataset.networkCheck;
    if (scope === "global" && changed) throw new Error("请先保存代理与连接策略，再诊断");
    if (scope !== "global" && $(`#${scope}-network-policy`).value !== policyValue(state.network[scope])) {
      throw new Error("请先保存所选连接方式，再诊断");
    }
    await busy(button, "正在诊断…", () => diagnose(scope, "configured", "", $(`#${scope}-network-result`)));
  })));
  const runDiagnostic = async (compare, button) => {
    if (changed) throw new Error("请先保存代理与连接策略，再诊断");
    const target = $("#network-check-target").value;
    const scope = target.startsWith("account:") ? "account" : target;
    const accountID = scope === "account" ? target.slice(8) : "";
    const route = $("#network-check-route").value;
    if (compare && !route.startsWith("proxy:")) throw new Error("请先在“测试出口”选择一个代理");
    await busy(button, "正在诊断…", async () => {
      const container = $("#network-diagnose-results");
      if (compare) await diagnose(scope, "direct", "", container, false, accountID);
      await diagnose(scope, route.startsWith("proxy:") ? "proxy" : route, route.startsWith("proxy:") ? route.slice(6) : "", container, compare, accountID);
    });
  };
  $("#network-diagnose-form").addEventListener("submit", wrap((event) => runDiagnostic(false, $("button[type=submit]", event.currentTarget))));
  $("#network-compare").addEventListener("click", wrap((event) => runDiagnostic(true, event.currentTarget)));
})();
