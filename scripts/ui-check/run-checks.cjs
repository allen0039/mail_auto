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
function problems(r) {
  if (!r || typeof r !== "object" || typeof r.pass !== "boolean") return ["invalid result: missing boolean pass"];
  const list = Array.isArray(r.results) ? r.results : null;
  const total = list ? list.length : r.total;
  if (!Number.isInteger(total) || total <= 0) return ["invalid result: no checks reported"];
  const failed = Array.isArray(r.failed) ? r.failed : list ? list.filter((x) => !x || !x.ok) : null;
  if (!failed) return ["invalid result: no failure list"];
  const out = failed.map((x) => (x && x.name ? x.name + (x.detail ? `: ${x.detail}` : "") : "unnamed check failed"));
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

main().then(
  (pass) => process.exit(pass ? 0 : 1),
  (e) => {
    console.error(`UI check aborted: ${e && e.message ? e.message.split("\n")[0] : e}`);
    process.exit(1);
  },
);
