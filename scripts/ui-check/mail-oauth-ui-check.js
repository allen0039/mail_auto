(async () => {
  const results = [];
  const check = (name, ok) => results.push({name, ok: !!ok});
  const wait = () => new Promise((resolve) => setTimeout(resolve, 30));
  const until = async (condition) => { for (let attempt = 0; attempt < 100; attempt++) { if (condition()) return; await wait(); } throw new Error("OAuth UI timed out"); };
  const originalApi = window.api, originalConfirm = window.confirm, originalAccounts = state.accounts;
  let allowDiscard = true, pendingLoad = null, pendingSave = null, pendingStart = null, saved = null, startRequests = 0;
  const apps = [{provider: "google", configured: false, client_id: "", origin: ""}, {provider: "microsoft", configured: true, client_id: "ms-client", origin: "https://mail.test", authority: "consumers"}];
  window.confirm = () => allowDiscard;
  window.api = async (url, options = {}) => {
    if (url === "/api/settings/mail-oauth") { const snapshot = structuredClone(apps); if (pendingLoad) await pendingLoad; return snapshot; }
    if (url.startsWith("/api/settings/mail-oauth/") && options.method === "PUT") { saved = JSON.parse(options.body); if (pendingSave) await pendingSave; return {configured: true}; }
    if (url.endsWith("/oauth-start")) { startRequests++; if (pendingStart) await pendingStart; return {url: "https://accounts.google.com/o/oauth2/v2/auth"}; }
    return originalApi(url, options);
  };
  try {
    await showView("accounts");
    openAccount();
    const accountForm = $("#account-form");
    accountForm.elements.username.value = "fixture@gmail.com";
    accountForm.elements.username.dispatchEvent(new Event("input", {bubbles: true}));
    check("Gmail preset uses official host", $("#provider").value === "google" && $("#host").value === "imap.gmail.com");
    check("Gmail uses OAuth without password", !$("#account-oauth-note").hidden && $("#password-label").hidden && !accountForm.elements.password.required);
    check("Gmail restriction is explicit", $("#account-oauth-note").textContent.includes("回收站") && $("#account-oauth-note").textContent.includes("拒绝"));
    check("Gmail unsafe undo is explicitly refused", $("#account-oauth-note").textContent.includes("撤销") && $("#account-oauth-note").textContent.includes("操作前标签证据"));
    for (const domain of ["outlook.com", "hotmail.com", "live.com"]) {
      accountForm.elements.username.value = `fixture@${domain}`;
      accountForm.elements.username.dispatchEvent(new Event("input", {bubbles: true}));
      check(`${domain} preset uses IMAPS`, $("#provider").value === "microsoft" && $("#host").value === "outlook.office365.com" && accountForm.elements.port.value === "993");
    }
    $("#provider").value = "qq";
    $("#provider").dispatchEvent(new Event("change", {bubbles: true}));
    check("legacy password form restored", !$("#password-label").hidden && accountForm.elements.password.required);
    accountForm.elements.username.value = "fixture@custom.test";
    accountForm.elements.username.dispatchEvent(new Event("input", {bubbles: true}));
    $("#provider").value = "google";
    $("#provider").dispatchEvent(new Event("change", {bubbles: true}));
    check("custom domain manual provider stays fixed", $("#host").value === "imap.gmail.com");
    const draftAddress = accountForm.elements.username.value;
    $("#account-oauth-config").click();
    await until(() => $("#mail-oauth-error").textContent.includes("尚未配置"));
    check("unconfigured app has guidance", $("#mail-oauth-error").textContent.includes("服务商控制台"));
    check("account draft survives app settings", accountForm.elements.username.value === draftAddress && $("#account-dialog").open);
    const form = $("#mail-oauth-form");
    form.elements.origin.value = "https://mail.test";
    form.elements.origin.dispatchEvent(new Event("input", {bubbles: true}));
    check("exact callback displayed", $("#mail-oauth-callback").textContent.includes("https://mail.test/api/mail-oauth/google/callback"));
    check("Testing restriction visible", form.textContent.includes("七天") && form.textContent.includes("Graph"));
    form.elements.client_id.value = "test-client";
    form.elements.client_secret.value = "synthetic-app-secret";
    form.dispatchEvent(new Event("input", {bubbles: true}));
    allowDiscard = false;
    $("#mail-oauth-provider").value = "microsoft";
    $("#mail-oauth-provider").dispatchEvent(new Event("change", {bubbles: true}));
    check("cancelled provider switch keeps draft binding", $("#mail-oauth-provider").value === "google" && form.elements.client_id.value === "test-client" && isDirty(form));
    allowDiscard = true;
    let releaseSave;
    pendingSave = new Promise((resolve) => { releaseSave = resolve; });
    form.requestSubmit();
    await until(() => saved !== null);
    check("app submission has correct fields", saved.client_id === "test-client" && saved.client_secret === "synthetic-app-secret" && saved.secret_expires === 0);
    form.elements.client_id.value = "newer-draft";
    form.dispatchEvent(new Event("input", {bubbles: true}));
    releaseSave(); pendingSave = null;
    await until(() => !$("button[type=submit]", form).disabled);
    check("late save preserves newer draft", form.elements.client_id.value === "newer-draft" && isDirty(form));
    markClean(form);
    $("#mail-oauth-dialog").close();
    await wait();
    check("close clears secret", form.elements.client_secret.value === "");
    let releaseLoad;
    pendingLoad = new Promise((resolve) => { releaseLoad = resolve; });
    openMailOAuth("google");
    $("#mail-oauth-provider").value = "microsoft";
    $("#mail-oauth-provider").dispatchEvent(new Event("change", {bubbles: true}));
    $("#mail-oauth-provider").value = "google";
    $("#mail-oauth-provider").dispatchEvent(new Event("change", {bubbles: true}));
    form.elements.client_id.value = "A-B-A-draft";
    form.dispatchEvent(new Event("input", {bubbles: true}));
    releaseLoad(); pendingLoad = null;
    await wait(); await wait();
    check("A B A late response preserves draft", form.elements.client_id.value === "A-B-A-draft");
    markClean(form); $("#mail-oauth-dialog").close();
    openMailOAuth("microsoft");
    await until(() => form.elements.client_id.value === "ms-client");
    check("reopen loads selected app", $("#mail-oauth-provider").value === "microsoft");
    check("configured app never echoes secret", form.elements.client_secret.value === "");
    check("Microsoft precise callback", $("#mail-oauth-callback").textContent.includes("/microsoft/callback"));
    check("dialog fits viewport", $("#mail-oauth-dialog").getBoundingClientRect().width <= window.innerWidth && document.documentElement.scrollWidth <= window.innerWidth);
    const base = {id: "oauth-fixture", name: "OAuth fixture", username: "fixture@example.test", provider: "google", authentication: "oauth2", host: "imap.gmail.com", port: 993, folders: [], synced_folders: [], oauth: {status: "pending"}};
    state.accounts = [base]; renderAccounts();
    check("account card exposes reauthorization", !!$('[data-action="oauth"]'));
    for (const [status, text] of [["pending", "待授权"], ["reauthorize", "需要重新授权"], ["network_error", "网络失败"]]) check(`account status ${status}`, getAccountStatus({...base, oauth: {status}}, null, false).label === text);
    for (const error_code of ["oauth_scope_missing", "oauth_imap_denied"]) check(`permission status ${error_code}`, getAccountStatus({...base, oauth: {status: "reauthorize", error_code}}, null, false).label === "权限受限");
    check("denied callback has actionable Chinese error", mailOAuthMessage("oauth_denied").includes("重新"));
    check("account mismatch is explicit", mailOAuthMessage("oauth_identity_mismatch").includes("原账号"));
    markClean(form); $("#mail-oauth-dialog").close();
    const beforeAuthorization = location.href;
    let releaseStart;
    pendingStart = new Promise((resolve) => { releaseStart = resolve; });
    const authorizing = startMailOAuth(base.id);
    await until(() => startRequests === 1);
    openAccount(); markClean(accountForm); $("#account-dialog").close();
    openAccount(); markClean(accountForm); $("#account-dialog").close();
    releaseStart(); pendingStart = null;
    await authorizing;
    check("late authorization cannot redirect after close reopen", location.href === beforeAuthorization);
    pendingStart = new Promise((resolve) => { releaseStart = resolve; });
    const navigating = startMailOAuth(base.id);
    await until(() => startRequests === 2);
    await showView("system"); await showView("accounts");
    releaseStart(); pendingStart = null;
    await navigating;
    check("late authorization cannot redirect after A B A navigation", location.href === beforeAuthorization);
  } finally {
    window.api = originalApi; window.confirm = originalConfirm; state.accounts = originalAccounts;
    for (const selector of ["#mail-oauth-dialog", "#account-dialog"]) { const dialog = $(selector); for (const form of dialog.querySelectorAll("form")) markClean(form); if (dialog.open) dialog.close(); }
  }
  return {pass: results.every((result) => result.ok), results};
})()
