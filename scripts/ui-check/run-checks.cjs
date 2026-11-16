// Runs UI check scripts against a throwaway local instance in headless Chrome.
// Usage: node run-checks.cjs <puppeteer module> <base url> <creds json> <script>...
// Normally started by run.sh, which creates and removes the instance.
// creds json: {nu, np} logs in directly; with {u, p} as well it first completes
// the first-time setup from the temporary bootstrap account u/p to nu/np.
// Browser: CHROME_PATH (or PUPPETEER_EXECUTABLE_PATH) if set, otherwise the
// installed Chrome found by puppeteer (channel "chrome").
// Exit 0 only when every script returns a valid result with all checks passed
// and the page raised no errors; any failure, page error or exception exits 1.
const [, , pptr, base, credsFile, ...scripts] = process.argv;
const fs = require("fs");
const widths = [[1280, 900], [375, 812], [390, 844]];

// Returns failure messages for one script result; empty means it passed.
// Accepted shapes: {pass, results:[{name, ok}]} and {pass, total, failed:[...]}.
// When both results and failed are present, every piece of evidence is checked:
// an empty failed list never hides an ok:false entry in results.
function problems(r) {
  if (!r || typeof r !== "object" || typeof r.pass !== "boolean") return ["invalid result: missing boolean pass"];
  const hasResults = r.results !== undefined;
  const hasFailed = r.failed !== undefined;
  if (hasResults && !Array.isArray(r.results)) return ["invalid result: results is not a list"];
  if (hasFailed && !Array.isArray(r.failed)) return ["invalid result: failed is not a list"];
  if (!hasResults && !hasFailed) return ["invalid result: no check list"];
  const label = (x) => x.name + (x.detail ? `: ${x.detail}` : "");
  const out = [];
  let total = r.total;
  if (hasResults) {
    r.results.forEach((x, i) => {
      if (!x || typeof x !== "object" || typeof x.ok !== "boolean") out.push(`invalid check #${i + 1}: ok must be a boolean`);
      else if (!x.ok) out.push(label(x));
    });
    if (total !== undefined && total !== r.results.length) out.push("invalid result: total does not match results");
    total = r.results.length;
  }
  if (!Number.isInteger(total) || total <= 0) return ["invalid result: no checks reported"];
  if (hasFailed) {
    if (r.failed.length > total) out.push("invalid result: more failures than checks");
    r.failed.forEach((x, i) => {
      if (!x || typeof x !== "object" || typeof x.name !== "string" || (x.ok !== undefined && x.ok !== false)) out.push(`invalid failure entry #${i + 1}`);
      else if (!hasResults) out.push(label(x));
    });
    if (hasResults && r.failed.length !== r.results.filter((x) => x && x.ok === false).length) out.push("invalid result: failed list does not match results");
  }
  if (!r.pass && !out.length) out.push("pass is false");
  if (r.pass && out.length) out.unshift("pass is true but checks failed");
  return out;
}

async function main() {
  if (!pptr || !base || !credsFile || !scripts.length) throw new Error("usage: node run-checks.cjs <puppeteer module> <base url> <creds json> <script>...");
  const puppeteer = require(pptr);
  const creds = JSON.parse(fs.readFileSync(credsFile, "utf8"));
  const executablePath = process.env.CHROME_PATH || process.env.PUPPETEER_EXECUTABLE_PATH;
  const out = [];
  const pageErrors = [];
  let browser;
  try {
    browser = await puppeteer.launch({ headless: true, ...(executablePath ? { executablePath } : { channel: "chrome" }) });
    const page = await browser.newPage();
    page.on("pageerror", (e) => pageErrors.push(String(e)));
    page.on("dialog", (d) => d.dismiss());
    await page.goto(base, { waitUntil: "networkidle0" });
    if (creds.u) {
      await page.evaluate(async (c) => {
        const s = await api("/api/login", { method: "POST", body: JSON.stringify({ username: c.u, password: c.p }) });
        state.csrf = s.csrf;
        await api("/api/setup", { method: "POST", body: JSON.stringify({ username: c.nu, password: c.np }) });
      }, creds);
      await page.goto(base, { waitUntil: "networkidle0" });
    }
    await page.evaluate(async (c) => api("/api/login", { method: "POST", body: JSON.stringify({ username: c.nu, password: c.np }) }), creds);
    for (const file of scripts) {
      const code = fs.readFileSync(file, "utf8");
      for (const [w, h] of widths) {
        await page.setViewport({ width: w, height: h });
        await page.goto(base, { waitUntil: "networkidle0" });
        const r = await page.evaluate(code);
        if (file.endsWith("/mail-oauth-ui-check.js") && Array.isArray(r?.results)) {
          const startupAccount = {id: "startup-oauth-fixture", name: "Startup QA", username: "fixture@example.test", provider: "google", authentication: "oauth2", host: "imap.gmail.com", port: 993, folders: [], synced_folders: [], oauth: {status: "pending", generation: 1}};
          const intercept = (request) => {
            if (new URL(request.url()).pathname === "/api/accounts") request.respond({status: 200, contentType: "application/json", body: JSON.stringify([startupAccount])});
            else request.continue();
          };
          await page.setRequestInterception(true);
          page.on("request", intercept);
          try {
            await page.goto(`${base}/?mail_oauth=oauth_scope_missing&account=${startupAccount.id}&code=synthetic-code&state=synthetic-state`, {waitUntil: "networkidle0"});
            await page.waitForSelector(".mail-oauth-result");
            r.results.push(...await page.evaluate(() => [
              {name: "real startup captures before account load and cleans URL", ok: location.search === "" && document.querySelector(".mail-oauth-result").textContent.includes("明确同意")},
              {name: "real startup notice fits viewport", ok: document.documentElement.scrollWidth <= innerWidth},
            ]));
            await page.reload({waitUntil: "networkidle0"});
            await page.waitForSelector(".mail-oauth-result");
            r.results.push({name: "real page reload retains sanitized callback notice", ok: await page.$eval(".mail-oauth-result", (notice, accountID) => notice.textContent.includes("明确同意 IMAP 邮件访问") && notice.closest("[data-account]").dataset.account === accountID && location.search === "", startupAccount.id)});
            await page.click(".mail-oauth-result [data-action=oauth-dismiss]");
            await page.reload({waitUntil: "networkidle0"});
            r.results.push({name: "real reload cannot resurrect dismissed callback", ok: await page.$(".mail-oauth-result") === null});
          } finally {
            page.off("request", intercept);
            await page.setRequestInterception(false);
          }
          r.pass = r.results.every((result) => result.ok);
        }
        const failed = problems(r);
        out.push({ script: file.split("/").pop(), width: w, pass: failed.length === 0, total: r && (Array.isArray(r.results) ? r.results.length : r.total), failed });
      }
    }
  } finally {
    if (browser) await browser.close().catch(() => browser.process()?.kill("SIGKILL"));
  }
  const pass = out.length === scripts.length * widths.length && out.every((x) => x.pass) && pageErrors.length === 0;
  console.log(JSON.stringify({ pass, out, pageErrors }, null, 1));
  return pass;
}

module.exports = { problems };
if (require.main === module) {
  main().then(
    (pass) => process.exit(pass ? 0 : 1),
    (e) => {
      console.error(`UI check aborted: ${e && e.message ? e.message.split("\n")[0] : e}`);
      process.exit(1);
    },
  );
}
