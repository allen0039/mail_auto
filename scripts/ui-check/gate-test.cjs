// Regression for the UI check gate itself: node --test scripts/ui-check/gate-test.cjs
// Result validation cases always run. With PUPPETEER set (see run.sh), each case
// also runs end to end through run.sh on a throwaway instance and asserts the
// exit code and that no temporary directory or instance process is left behind.
const test = require("node:test");
const assert = require("node:assert");
const fs = require("fs");
const os = require("os");
const path = require("path");
const { spawnSync } = require("child_process");
const { problems } = require("./run-checks.cjs");

// [name, script source, should pass]
const cases = [
  ["valid results shape", '({pass: true, results: [{name: "a", ok: true}, {name: "b", ok: true}]})', true],
  ["valid total/failed shape", "({pass: true, total: 2, failed: []})", true],
  ["valid results with matching failed list", '({pass: true, results: [{name: "a", ok: true}], failed: []})', true],
  ["LG-01: empty failed list masks ok:false", '({pass: true, results: [{name: "masked", ok: false}], failed: []})', false],
  ["LG-01: string ok", '({pass: true, results: [{name: "string false", ok: "false"}]})', false],
  ["numeric ok", '({pass: true, results: [{name: "one", ok: 1}]})', false],
  ["missing ok", '({pass: true, results: [{name: "none"}]})', false],
  ["non-object entry", "({pass: true, results: [true]})", false],
  ["failed list names a failure the results do not", '({pass: true, results: [{name: "a", ok: true}], failed: [{name: "a", ok: false}]})', false],
  ["invalid failed entry", '({pass: true, total: 1, failed: ["x"]})', false],
  ["total disagrees with results", '({pass: true, total: 3, results: [{name: "a", ok: true}]})', false],
  ["plain assertion failure", '({pass: false, results: [{name: "f", ok: false}]})', false],
  ["pass:true with failure", '({pass: true, results: [{name: "f", ok: false}]})', false],
  ["pass:false without failures", '({pass: false, results: [{name: "a", ok: true}]})', false],
  ["failure via total/failed", '({pass: false, total: 2, failed: [{name: "f", ok: false}]})', false],
  ["missing pass", "({total: 1, failed: []})", false],
  ["zero checks", "({pass: true, results: []})", false],
  ["no check list", "({pass: true})", false],
];

for (const [name, src, ok] of cases) {
  test(`validate: ${name}`, () => {
    const found = problems(eval(src));
    assert.strictEqual(found.length === 0, ok, JSON.stringify(found));
  });
}

const pptr = process.env.PUPPETEER;
const runSh = path.join(__dirname, "run.sh");
const tmp = process.env.TMPDIR || "/tmp";
const leftovers = () => ({
  dirs: fs.readdirSync(tmp).filter((f) => f.startsWith("mail-auto-ui-check.")).length,
  procs: spawnSync("pgrep", ["-f", "mail-auto-ui-check\\."]).stdout.toString().trim().split("\n").filter(Boolean).length,
});
const e2e = [
  ...cases.filter(([name]) => /LG-01|valid results shape|valid total|plain assertion|missing pass|zero checks/.test(name)),
  ["page error", '(async () => { setTimeout(() => { throw new Error("injected page error"); }, 0); await new Promise((r) => setTimeout(r, 100)); return {pass: true, results: [{name: "a", ok: true}]}; })()', false],
  ["script exception", 'Promise.reject(new Error("injected exception"))', false],
];

for (const [name, src, ok] of e2e) {
  test(`run.sh: ${name} exits ${ok ? "0" : "non-zero"}`, { skip: !pptr && "set PUPPETEER to run end to end" }, () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), "gate-fixture-"));
    try {
      const file = path.join(dir, "case.js");
      fs.writeFileSync(file, src);
      const before = leftovers();
      const r = spawnSync(runSh, [file], { encoding: "utf8", timeout: 300000 });
      assert.strictEqual(r.status === 0, ok, `exit ${r.status}\n${r.stdout}\n${r.stderr}`);
      assert.deepStrictEqual(leftovers(), before);
    } finally {
      fs.rmSync(dir, { recursive: true, force: true });
    }
  });
}
