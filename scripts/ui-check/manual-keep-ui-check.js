// Development-only fake API: no real mailbox or live credentials.
(async () => {
  const results = [], calls = [];
  const check = (name, ok, detail = "") => results.push({name, ok: !!ok, detail: ok ? "" : String(detail)});
  const wait = ms => new Promise(r => setTimeout(r, ms));
  const until = async fn => { for (let i = 0; i < 200; i++) { if (fn()) return true; await wait(10); } return false; };
  const realApi = window.api, realNotify = window.notify;
  const now = Math.floor(Date.now()/1000);
  let failPreview = false, failConfirm = false, resolved = false, delayed = false, release = null;
  const evidence = {operation_id: "legacy", source: {ref: "source", folder: "INBOX", uidvalidity: 1, uid: 42}, target: {ref: "target", folder: "验证码", uidvalidity: 7, uid: 99}, message_id: "<pair@fixture>", sha256: "a".repeat(64), bytes: 12540, source_flags: ["\\Seen"], target_flags: ["\\Seen"], checked_at: now};
  const operation = () => ({id: "legacy", state: resolved ? "manual_kept" : "needs_verification", created_at: now-10000, updated_at: now, manual_resolution: resolved ? {...evidence, created_at: now, outcome: "keep_both"} : null, items: [{ref:"source",position:0,state: resolved ? "manual_kept" : "needs_verification",error_code:"remote_state_unknown",destination:{folder:"验证码"},recovery_detail: resolved ? "已人工核验两份一致，保留两份" : "缺少可信目标UID"}]});
  window.notify = () => {};
  window.api = async (url, opts = {}) => {
    calls.push({url, method: opts.method || "GET", body: opts.body ? JSON.parse(opts.body) : null});
    if (url.endsWith("/manual-keep-preview")) {
      if (delayed) await new Promise(r => {release=r;});
      if (failPreview) throw new Error("state_conflict");
      return structuredClone(evidence);
    }
    if (url.endsWith("/manual-keep")) {
      if (failConfirm) throw new Error("state_conflict");
      resolved=true;return {...evidence,outcome:"keep_both",created_at:now};
    }
    if (url === "/api/operations") return [operation()];
    if (url === "/api/network") return {version:0,global:{mode:"direct"},telegram:{mode:"inherit"},agent:{mode:"inherit"},profiles:[]};
    if (url === "/api/mcp/settings") return {enabled:false,public_url:"",version:1};
    if (url.includes("/tokens")) return [];
    return {};
  };
  try {
    document.querySelector("#auth-view").hidden=true;
    document.querySelector("#workspace").hidden=false;
    state.session={};await showView("organizer");
    const list=document.querySelector("#operations-list"), dialog=document.querySelector("#manual-keep-dialog"), confirm=document.querySelector("#manual-keep-confirm"), error=document.querySelector("#manual-keep-error");
    const open = async () => { list.querySelector("summary").click();list.querySelector('[data-op-action="manual-keep-preview"]').click();await until(() => !confirm.disabled || error.textContent); };
    await open();
    check("preview shows complete raw digest and locations",dialog.open && dialog.textContent.includes(evidence.sha256) && dialog.textContent.includes("12540") && dialog.textContent.includes("验证码"));
    check("retention protection includes expiry cleanup",dialog.textContent.includes("到期清理") && dialog.textContent.includes("规则修改"));
    check("preview has no settlement or move",!calls.some(c => /\/(manual-keep|execute|reconcile|undo|cancel)$/.test(c.url)));
    check("dialog fits narrow screen",dialog.getBoundingClientRect().width<=innerWidth && document.documentElement.scrollWidth<=innerWidth);
    document.querySelector("#manual-keep-close").click();await wait(30);
    check("closing preview does not settle",!dialog.open && !resolved);
    failPreview=true;await open();
    check("ambiguous preview disables confirmation",dialog.open && confirm.disabled && error.textContent.includes("核验未通过"));
    failPreview=false;document.querySelector("#manual-keep-refresh").click();await until(() => !confirm.disabled);
    failConfirm=true;confirm.click();await until(() => error.textContent.includes("未结案"));
    check("changed evidence during confirm leaves pending",confirm.disabled && !resolved && error.textContent.includes("重新只读核验"));
    failConfirm=false;document.querySelector("#manual-keep-refresh").click();await until(() => !confirm.disabled);
    confirm.click();await until(() => !dialog.open && list.textContent.includes("人工核验保留"));
    const post=calls.filter(c => c.url.endsWith("/manual-keep")).at(-1);
    check("confirmed request includes expected raw evidence",post?.body?.confirm===true && post?.body?.evidence?.sha256===evidence.sha256 && post?.body?.evidence?.target?.uid===99);
    check("settlement is distinct from automatically completed",resolved && list.textContent.includes("人工核验保留") && list.textContent.includes("待核验 0 项") && list.textContent.includes("人工核验保留 1 项"));
    check("settled record hides move recovery controls",!list.querySelector('[data-op-action="reconcile"]') && !list.querySelector('[data-op-action="undo"]') && !list.querySelector('[data-op-action="manual-keep-preview"]'));
    check("original unknown error retained",list.textContent.includes("remote_state_unknown"));
    // An outstanding preview response must never revive a closed dialog.
    resolved=false;await showView("organizer");delayed=true;
    list.querySelector("summary").click();list.querySelector('[data-op-action="manual-keep-preview"]').click();await until(() => release);
    document.querySelector("#manual-keep-close").click();release();await wait(60);
    check("late preview cannot revive closed modal",!dialog.open && confirm.disabled);
    check("no dangerous endpoint called",!calls.some(c => /\/(execute|reconcile|undo|cancel)$/.test(c.url)));
  } finally {
    document.querySelector("#manual-keep-dialog").close();window.api=realApi;window.notify=realNotify;
  }
  return {pass:results.every(x=>x.ok),results};
})();
