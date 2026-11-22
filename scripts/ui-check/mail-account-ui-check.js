(async () => {
  const results = [];
  const check = (name, ok) => results.push({name, ok: Boolean(ok)});
  const originalAccounts = state.accounts;
  const form = $("#account-form");
  const dialog = $("#account-dialog");
  const close = () => {
    markClean(form);
    if (dialog.open) dialog.close();
  };
  try {
    openAccount();
    check("Only supported providers are selectable", [...form.elements.provider.options].map((option) => option.value).join() === "qq,netease,icloud,google");
    check("Only password and external secret controls are present", [...$("#credential-mode").options].map((option) => option.value).join() === "keep,password,external");
    for (const domain of ["outlook.com", "hotmail.com", "live.com"]) {
      form.elements.username.value = `fixture@${domain}`;
      form.elements.username.dispatchEvent(new Event("input", {bubbles: true}));
      check(`${domain} has no automatic provider`, !domainServers[domain] && $("#account-provider-note").textContent.includes("无法根据域名识别"));
    }
    form.elements.username.value = "fixture@gmail.com";
    form.elements.username.dispatchEvent(new Event("input", {bubbles: true}));
    check("Gmail uses official TLS address", form.elements.provider.value === "google" && form.elements.host.value === "imap.gmail.com" && form.elements.port.value === "993" && form.elements.port.readOnly);
    check("Gmail requires an application password", form.elements.password.required && !$("#password-label").hidden && $("#external-option").hidden);
    check("Gmail explains two-step verification and links to app passwords", $("#account-gmail-note").textContent.includes("需开启两步验证") && !!$('#account-gmail-note a[href="https://myaccount.google.com/apppasswords"]'));
    close();

    const gmail = {id: "gmail", name: "Gmail", username: "fixture@gmail.com", provider: "google", host: "imap.gmail.com", port: 993, folders: [], synced_folders: [], credential_source: "encrypted"};
    const retired = {id: "retired", name: "Historical mailbox", provider: "microsoft", username: "fixture@outlook.com", host: "outlook.office365.com", port: 993, folders: [], synced_folders: ["INBOX"], credential_source: "unsupported"};
    state.accounts = [gmail, retired];
    openAccount(gmail);
    check("Existing app password is never prefilled and need not be replaced", form.elements.password.value === "" && !form.elements.password.required);
    close();
    const missing = {...gmail, credential_source: "missing"};
    state.accounts = [missing, retired];
    openAccount(missing);
    check("Migrated Gmail must be given a new password", form.elements.password.required && form.elements.password.value === "");
    close();
    await showView("accounts");
    renderAccounts();
    const retiredCard = $('[data-account="retired"]', $("#account-list"));
    check("Unsupported historical account cannot be reconnected", retiredCard?.textContent.includes("已停止支持") && !retiredCard.querySelector('[data-action="edit"], [data-action="sync"], [data-action="diagnose"]'));
    check("Unconfigured Gmail is not displayed as verified", $('[data-account="gmail"]', $("#account-list"))?.textContent.includes("待填写密码"));
    check("Mailbox controls fit viewport", document.documentElement.scrollWidth <= innerWidth);
  } finally {
    close();
    state.accounts = originalAccounts;
    renderAccounts();
  }
  return {pass: results.every((result) => result.ok), results};
})();
