// Runs UI check scripts against a logged-in local instance in headless Chrome.
// Usage: node runchecks.cjs <puppeteer path> <base url> <creds json> <script>...
const [, , pptr, base, credsFile, ...scripts] = process.argv;
const fs = require("fs");
const puppeteer = require(pptr);
const creds = require(credsFile);
(async () => {
  const browser = await puppeteer.launch({ headless: true, executablePath: "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" });
  const page = await browser.newPage();
  const errors = [];
  page.on("pageerror", (e) => errors.push(String(e)));
  page.on("dialog", (d) => d.dismiss());
  await page.goto(base, { waitUntil: "networkidle0" });
  await page.evaluate(async (c) => api("/api/login", { method: "POST", body: JSON.stringify({ username: c.nu, password: c.np }) }), creds);
  const out = [];
  for (const file of scripts) {
    const code = fs.readFileSync(file, "utf8");
    for (const [w, h] of [[1280, 900], [375, 812], [390, 844]]) {
      await page.setViewport({ width: w, height: h });
      await page.goto(base, { waitUntil: "networkidle0" });
      const r = await page.evaluate(code);
      out.push({ script: file.split("/").pop(), width: w, pass: r.pass, total: r.results ? r.results.length : r.total, failed: (r.failed || r.results.filter((x) => !x.ok)).map((x) => x.name + (x.detail ? `: ${x.detail}` : "")) });
    }
  }
  console.log(JSON.stringify({ out, pageErrors: errors }, null, 1));
  await browser.close();
})();
