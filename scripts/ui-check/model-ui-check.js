// Mail Auto 0.7.0 model-connection UI check. It replaces the page's API with
// an in-memory fake, so it needs no model key and sends nothing anywhere.
// Usage: open a throwaway instance (e.g. 127.0.0.1:18081), log in, paste this
// file into the console or evaluate it through automation. Resolves to
// {pass, results}.
(async () => {
  const results = [];
  const check = (name, ok, detail = "") => results.push({name, ok: !!ok, detail});
  const wait = (ms) => new Promise((r) => setTimeout(r, ms));
  const until = async (cond) => {
    for (let i = 0; i < 100; i++) {
      if (cond()) return true;
      await wait(30);
    }
    return false;
  };
  const realApi = window.api;
  const realConfirm = window.confirm;
  window.confirm = () => true;
  const calls = [];
  let provider = {version: 3, enabled: true, base_url: "https://a.example/v1", model: "m-a", timeout_seconds: 60, max_tokens_param: "", key_configured: true, connection_id: "c1", capabilities: {current: true, tool_loop: "ok", tool_loop_at: 1}};
  let conns = [{id: "c1", version: 1, name: "旧配置", base_url: "https://a.example/v1", endpoint: "https://a.example/v1/chat/completions", model: "m-a", timeout_seconds: 60, max_tokens_param: "", key_configured: true}];
  let nextActivate = null;
  let modelsGate = null;
  const view = () => ({connections: conns.map((c) => ({...c, active: c.id === provider.connection_id, pending: false})), provider});
  window.api = async (url, opts = {}) => {
    const body = opts.body ? JSON.parse(opts.body) : {};
    calls.push(`${opts.method || "GET"} ${url}`);
    if (url === "/api/agent/connections" && !opts.method) return view();
    if (url === "/api/agent/connections/models") {
      if (modelsGate) await modelsGate;
      if (body.base_url.includes("nolist")) throw new Error("该服务未提供可用的模型列表：请直接手动填写模型名称（provider_models_unsupported）");
      return {models: body.base_url.includes("b.example") ? ["b-1", "b-2"] : ["a-late"]};
    }
    if (url === "/api/agent/connections/activate") {
      if (body.provider_version !== provider.version) throw new Error("配置已被修改，请刷新后重试");
      if (nextActivate === "fail") {
        nextActivate = null;
        return {ok: false, result: "provider_auth_failed", message: "API Key 无效或无权访问（provider_auth_failed）", provider};
      }
      let c = conns.find((x) => x.id === body.id);
      if (!c) {
        c = {id: "c" + (conns.length + 1), version: 1, key_configured: true};
        conns.push(c);
      } else c.version++;
      Object.assign(c, {name: body.name || new URL(body.base_url).hostname, base_url: body.base_url, model: body.model, timeout_seconds: body.timeout_seconds, max_tokens_param: body.max_tokens_param});
      provider = {...provider, version: provider.version + 1, enabled: true, base_url: c.base_url, model: c.model, connection_id: c.id, capabilities: {current: true, tool_loop: "ok", tool_loop_at: 2}};
      return {ok: true, result: "ok", provider, connection: {...c, active: true}};
    }
    if (url === "/api/agent/connections" && opts.method === "POST") {
      const c = {id: "c" + (conns.length + 1), version: 1, key_configured: !!body.api_key, name: body.name || "backup", base_url: body.base_url, model: body.model, timeout_seconds: 60, max_tokens_param: ""};
      conns.push(c);
      return {...c, active: false};
    }
    if (url === "/api/agent/connections/delete") {
      conns = conns.filter((c) => c.id !== body.id);
      return {deleted: true};
    }
    if (url === "/api/agent/grant") return {grant: null, actions: []};
    if (url === "/api/telegram/interaction") return {version: 1, enabled: false, state: "off"};
    if (url === "/api/accounts") return [];
    if (url.startsWith("/api/agent/conversations")) return {turns: [], proposals: [], run_links: [], queued: 0};
    if (url === "/api/network") return realApi(url, opts);
    throw new Error("unexpected " + url);
  };
  try {
    await showView("agent");
    await window.loadAgent();
    check("current shown as active", $("#model-current").textContent.includes("当前生效：旧配置") && $("#model-current").classList.contains("is-active"));
    check("active connection selected with its model", $("#model-connection").value === "c1" && $("#model-model").value === "m-a");
    check("save-only hidden for connection in use", $("#model-save").hidden);
    check("delete hidden for connection in use, preset hidden for saved", $("#model-delete").hidden && $("#model-preset-field").hidden);
    check("advanced closed, no summary by default", !$("#model-advanced").open && $("#model-advanced-summary").textContent === "");

    // New connection via preset.
    $("#model-connection").value = "";
    $("#model-connection").dispatchEvent(new Event("change", {bubbles: true}));
    check("new connection shows preset", !$("#model-preset-field").hidden && $("#model-base-url").value === "");
    $("#model-preset").value = "deepseek";
    $("#model-preset").dispatchEvent(new Event("change", {bubbles: true}));
    check("preset fills address and name", $("#model-base-url").value === "https://api.deepseek.com/v1" && $("#model-name").value === "DeepSeek");
    check("endpoint preview", $("#model-endpoint").textContent.includes("/v1/chat/completions"));

    // Late model list for an old address is discarded.
    $("#model-base-url").value = "https://a.example/v1";
    $("#model-base-url").dispatchEvent(new Event("input", {bubbles: true}));
    $("#model-key").value = "k";
    let release;
    modelsGate = new Promise((r) => (release = r));
    $("#model-fetch").click();
    await wait(30);
    $("#model-base-url").value = "https://b.example/v1";
    $("#model-base-url").dispatchEvent(new Event("input", {bubbles: true}));
    release();
    modelsGate = null;
    await wait(60);
    check("late list from old address discarded", $("#model-options").children.length === 0);
    $("#model-fetch").click();
    await until(() => $("#model-options").children.length === 2);
    check("list fetched for current address", [...$("#model-options").options].map((o) => o.value).join() === "b-1,b-2" && $("#model-list-note").textContent.includes("2 个"));

    // Unsupported list keeps manual entry possible.
    $("#model-base-url").value = "https://nolist.example/v1";
    $("#model-base-url").dispatchEvent(new Event("input", {bubbles: true}));
    $("#model-fetch").click();
    await until(() => $("#model-list-note").textContent.includes("手动"));
    check("list failure explains manual entry", $("#model-list-note").textContent.includes("仍可直接填写"));

    // Failed check keeps current config and the draft.
    $("#model-base-url").value = "https://b.example/v1";
    $("#model-model").value = "b-1";
    $("#model-model").dispatchEvent(new Event("input", {bubbles: true}));
    nextActivate = "fail";
    $("#model-form").requestSubmit();
    await until(() => $("#model-status").textContent.includes("未通过"));
    check("failure message in Chinese, current unchanged", $("#model-status").textContent.includes("当前生效配置未改变") && $("#model-status").textContent.includes("API Key") && provider.connection_id === "c1");
    check("draft kept after failure", $("#model-model").value === "b-1" && $("#model-base-url").value === "https://b.example/v1");

    // Success switches and marks the new one in use.
    $("#model-form").requestSubmit();
    await until(() => $("#model-status").textContent.includes("已切换"));
    check("switch applied", provider.connection_id === "c2" && $("#model-connection").value === "c2" && $("#model-current").textContent.includes("DeepSeek"));
    check("old connection listed as backup", [...$("#model-connection").options].some((o) => o.value === "c1" && !o.text.includes("使用中")));

    // Switch back: choose c1, keeps its model, no key needed.
    $("#model-connection").value = "c1";
    $("#model-connection").dispatchEvent(new Event("change", {bubbles: true}));
    check("switch target keeps last model, key optional", $("#model-model").value === "m-a" && $("#model-key-note").textContent.includes("留空继续使用") && !$("#model-save").hidden);
    $("#model-base-url").value = "https://other.example/v1";
    $("#model-base-url").dispatchEvent(new Event("input", {bubbles: true}));
    check("host change asks for key", $("#model-key-note").textContent.includes("须重新填写密钥"));
    $("#model-cancel").click();
    check("cancel restores saved values", $("#model-base-url").value === "https://a.example/v1" && $("#model-cancel").hidden);

    // Advanced non-default summary.
    $("#model-timeout").value = "90";
    $("#model-timeout").dispatchEvent(new Event("input", {bubbles: true}));
    check("non-default advanced summary", $("#model-advanced-summary").textContent.includes("超时 90 秒"));

    // Refresh keeps an unsaved draft.
    await window.loadAgent();
    check("refresh keeps unsaved draft", $("#model-timeout").value === "90" && $("#model-connection").value === "c1");
    $("#model-cancel").click();
    $("#model-form").requestSubmit();
    await until(() => provider.connection_id === "c1");
    check("switch back applied", provider.connection_id === "c1" && provider.version === 5);

    // Narrow screen: no horizontal overflow in the card.
    const card = $("#model-form").closest("article");
    check("card fits its width", card.scrollWidth <= card.clientWidth + 1, `${card.scrollWidth}/${card.clientWidth}`);
  } catch (e) {
    check("exception", false, String(e && e.stack || e));
  } finally {
    window.api = realApi;
    window.confirm = realConfirm;
  }
  return {pass: results.every((r) => r.ok), results, calls: calls.length};
})()
