#!/usr/bin/env python3
"""Run local candidate gates and emit counts without copying raw logs into docs."""

import collections
import datetime
import json
import os
import pathlib
import re
import subprocess
import sys
import time
import tarfile

ROOT = pathlib.Path(__file__).resolve().parents[1]
EVIDENCE = ROOT / ".planning/gmail-outlook-implementation/validation"
SUMMARY = ROOT / "docs/gmail-outlook-test-results.json"
COMMANDS = [
    ("race", ["go", "test", "-race", "-json", "./..."]),
    ("vet", ["go", "vet", "./..."]),
    ("format", ["gofmt", "-l", "."]),
    ("source-format", ["gofmt", "-l", "cmd", "internal", "scripts"]),
    ("staticcheck", ["go", "run", "honnef.co/go/tools/cmd/staticcheck@v0.8.2", "./..."]),
    ("govulncheck", ["go", "run", "golang.org/x/vuln/cmd/govulncheck@v1.9.0", "./..."]),
    ("release-fixtures", ["python3", "-m", "unittest", "discover", "-s", "scripts/release-verify", "-v"]),
    ("compose", ["docker", "compose", "config", "--quiet"]),
    ("build", ["make", "build"]),
    ("ui", ["scripts/ui-check/run.sh"]),
    ("ui-gate", ["node", "--test", "--test-reporter=tap", "scripts/ui-check/gate-test.cjs"]),
    ("multiarch", ["docker", "buildx", "build", "--platform", "linux/amd64,linux/arm64", "--output", "type=oci,dest=.planning/gmail-outlook-implementation/candidate-image.tar", "."]),
    ("diff", ["git", "diff", "--check"]),
]


def counts(name, text):
    if name == "race":
        events = {}
        for line in text.splitlines():
            try:
                item = json.loads(line)
            except ValueError:
                continue
            if item.get("Test") and item.get("Action") in ("pass", "fail", "skip"):
                events[(item["Package"], item["Test"])] = item["Action"]
        leaves = {key: value for key, value in events.items()
                  if not any(other[0] == key[0] and other[1].startswith(key[1] + "/") for other in events)}
        tally = collections.Counter(leaves.values())
        return {"pass": tally["pass"], "fail": tally["fail"], "skip": tally["skip"], "count_basis": "leaf Go tests", "all_test_events": len(events)}
    if name == "ui":
        try:
            result = json.loads(text)
        except ValueError:
            return None
        entries = result.get("out", [])
        passed = sum(item.get("total", 0) - len(item.get("failed", [])) for item in entries)
        failed = sum(len(item.get("failed", [])) for item in entries)
        return {"pass": passed, "fail": failed, "skip": 0, "viewports": entries, "page_errors": len(result.get("pageErrors", []))}
    if name == "ui-gate":
        result = {}
        for label, field in (("pass", "pass"), ("fail", "fail"), ("skipped", "skip")):
            match = re.search(r"^# " + label + r" (\d+)$", text, re.MULTILINE)
            if match:
                result[field] = int(match.group(1))
        return result or None
    if name == "release-fixtures":
        match = re.search(r"Ran (\d+) tests?", text)
        return {"pass": int(match.group(1)), "fail": 0, "skip": 0} if match and re.search(r"^OK$", text, re.MULTILINE) else None
    if name in ("format", "source-format"):
        return {"unformatted_files": len(text.splitlines())}
    return None


def main():
    EVIDENCE.mkdir(parents=True, exist_ok=True)
    environment = dict(os.environ)
    environment.setdefault("PUPPETEER", "puppeteer-core")
    chrome = "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
    if pathlib.Path(chrome).exists():
        environment.setdefault("CHROME_PATH", chrome)
    report = {"status": "local_self_test_only", "real_accounts": "pending", "timezone": "Asia/Shanghai", "commands": [], "versions": {}}
    versions = {
        "go": ["go", "version"], "node": ["node", "--version"], "python": ["python3", "--version"],
        "docker": ["docker", "--version"], "buildx": ["docker", "buildx", "version"],
        "compose": ["docker", "compose", "version"],
        "staticcheck": ["go", "run", "honnef.co/go/tools/cmd/staticcheck@v0.8.2", "-version"],
        "govulncheck": ["go", "run", "golang.org/x/vuln/cmd/govulncheck@v1.9.0", "-version"],
    }
    if environment.get("CHROME_PATH"):
        versions["chrome"] = [environment["CHROME_PATH"], "--version"]
    for name, command in versions.items():
        result = subprocess.run(command, cwd=ROOT, env=environment, text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
        report["versions"][name] = {"exit_code": result.returncode, "output": result.stdout.strip()[:1000]}
    selected = set(sys.argv[1:])
    if SUMMARY.exists():
        previous = json.loads(SUMMARY.read_text())
        report["prior_attempts"] = previous.get("prior_attempts", []) + [item for item in previous.get("commands", []) if not selected or item["name"] in selected]
        if selected:
            report["commands"] = [item for item in previous.get("commands", []) if item["name"] not in selected]
    for name, command in COMMANDS:
        if selected and name not in selected:
            continue
        started = datetime.datetime.now(datetime.timezone(datetime.timedelta(hours=8))).isoformat()
        print("running " + name, flush=True)
        log = EVIDENCE / (name + "-" + datetime.datetime.now().strftime("%Y%m%d-%H%M%S") + ".log")
        if name == "multiarch" and environment.get("MAIL_AUTO_TEST_BUILDER"):
            command = command[:3] + ["--builder", environment["MAIL_AUTO_TEST_BUILDER"]] + command[3:]
        before = time.monotonic()
        with log.open("w") as output:
            result = subprocess.run(command, cwd=ROOT, env=environment, stdout=output, stderr=subprocess.STDOUT)
        text = log.read_text(errors="replace")
        item = {"name": name, "command": command, "started_at": started, "elapsed_seconds": round(time.monotonic() - before, 3), "exit_code": result.returncode, "counts": counts(name, text), "raw_log": str(log.relative_to(ROOT)), "evidence": "raw local logs excluded from Git; this summary contains no provider responses or credentials"}
        report["commands"].append(item)
        SUMMARY.write_text(json.dumps(report, ensure_ascii=False, indent=2) + "\n")
        print(json.dumps({key: item[key] for key in ("name", "exit_code", "counts")}, ensure_ascii=False), flush=True)
        if name == "multiarch" and result.returncode == 0:
            with tarfile.open(ROOT / ".planning/gmail-outlook-implementation/candidate-image.tar") as archive:
                index = json.load(archive.extractfile("index.json"))
            (ROOT / "docs/gmail-outlook-image-build.json").write_text(json.dumps(index, indent=2) + "\n")
    return int(any(item["exit_code"] != 0 or (item["name"] in ("format", "source-format") and item["counts"]["unformatted_files"] != 0) for item in report["commands"]))


if __name__ == "__main__":
    sys.exit(main())
