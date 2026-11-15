#!/usr/bin/env python3
"""Read-only release check for a deployed Mail Auto version.

Collects release evidence again and compares it with what is expected; it
never deploys, pulls, changes configuration, runs tasks, calls models, writes
mail, verifies or replays operations, or removes backups and images.

  scripts/release-verify/release_verify.py --version 0.8.9 --commit <sha> \
      --ssh <ssh target> --compose-dir <compose dir on server> \
      --backup <pre-upgrade backup dir on server>

Local side: git (remote main and version tag) and the registry (docker buildx
imagetools). Server side: this same file is piped to `python3 -` over SSH and
reads docker inspect, /healthz, /readyz and SQLite (mode=ro, query_only). The
master key is compared on the server; only a boolean leaves it.

stdout: one redacted JSON document. stderr: a short conclusion. Failures are
reported by command name, exit status or error type only; command stderr and
exception text are never passed through.
Exit status:
  0  every required check matched
  1  at least one required check did not match (inconsistency found)
  2  usage error
  3  no mismatch, but at least one required check is unverified (evidence
     missing: no backup, no permission, command failed, no post-deploy run...)
This entry collects evidence repeatably; it does not replace an independent audit.
"""
import argparse
import datetime
import json
import os
import re
import shlex
import sqlite3
import subprocess
import sys

MATCH, MISMATCH, UNVERIFIED = "match", "mismatch", "unverified"
EXIT = {MATCH: 0, MISMATCH: 1, UNVERIFIED: 3}
PLATFORMS = ["linux/amd64", "linux/arm64"]
DATA_MOUNT, KEY_MOUNT = "/var/lib/mail-auto/data", "/var/lib/mail-auto/keys"
# Configuration tables compared row by row with the pre-upgrade backup.
CONFIG_TABLES = [
    ("accounts", "id"), ("agent_provider_settings", "id"), ("agent_provider_connections", "id"),
    ("automation_policies", "account_id"), ("mail_rules", "id"), ("mail_rule_versions", "rule_id,version"),
    ("folder_settings", "account_id"), ("network_settings", "id"), ("proxy_profiles", "id"),
    ("account_network", "account_id"), ("agent_grants", "id"), ("telegram_interaction_settings", "id"),
    ("telegram_bindings", "id"), ("notification_channel", "id"), ("mcp_settings", "id"), ("mcp_tokens", "id"),
]
# Task fields advanced by natural runs; reported apart from user settings.
TASK_RUNTIME_COLUMNS = {"next_run_at", "updated_at"}
TASK_RUNTIME_FIELDS = {"cursor", "last_triggered", "next_run_at", "next_times", "last_state"}
DOC_PREFIXES = ("docs/",)
# Run classes accepted as post-deploy evidence; every other class is unverified,
# except "failed" which is a mismatch.
PASS_CLASSES = {"noop", "succeeded", "observed"}
FINAL_STATES = {"noop": "noop", "completed": "succeeded", "observed": "observed",
                "cancelled": "cancelled", "limited": "limited", "requires_ai": "requires_ai"}
# Marker for the server-side collection piped over SSH; not a public option.
REMOTE_MODE = "--internal-remote-collect"
REMOTE_KEYS = {"compose_dir", "backup", "service"}
SAFE_TEXT = re.compile(r"[A-Za-z0-9_ .,()-]{1,120}")


class SafeError(RuntimeError):
    """An error whose message is built only from fixed text, command names and numbers."""


def label(cmd):
    """Command name for error messages; never arguments, which may hold hosts, paths or URLs."""
    name = os.path.basename(cmd[0])
    if name not in ("git", "docker", "ssh", "curl"):
        name = "command"
    if name == "git" and len(cmd) > 3:
        return "git " + cmd[3]
    if name == "docker" and len(cmd) > 2:
        return "docker " + (" ".join(cmd[1:3]) if cmd[1] in ("buildx", "image") else cmd[1])
    return name


def run(cmd, input=None, timeout=60):
    """Runs a command and returns stdout; raises SafeError on failure.

    The command's stderr is discarded: it can carry hosts, paths, URLs with
    credentials or other private text, so only the command name and exit status
    are reported."""
    try:
        r = subprocess.run(cmd, input=input, capture_output=True, text=True, timeout=timeout)
    except subprocess.TimeoutExpired:
        raise SafeError("%s timed out" % label(cmd))
    except FileNotFoundError:
        raise SafeError("%s not found" % label(cmd))
    except OSError as e:
        raise SafeError("%s could not start (%s)" % (label(cmd), type(e).__name__))
    if r.returncode != 0:
        raise SafeError("%s exited %d" % (label(cmd), r.returncode))
    return r.stdout


def short(e):
    """Redacted one-line description of an exception: never its free text."""
    if isinstance(e, SafeError):
        return str(e)
    if isinstance(e, KeyError):
        return "missing field"
    if isinstance(e, (ValueError, TypeError, IndexError, StopIteration)):
        return "invalid data (%s)" % type(e).__name__
    if isinstance(e, PermissionError):
        return "permission denied"
    if isinstance(e, FileNotFoundError):
        return "file not found"
    if isinstance(e, sqlite3.Error):
        return "sqlite error (%s)" % type(e).__name__
    return "error (%s)" % type(e).__name__


def scrub(value):
    """Replaces error strings received from the server with a fixed text unless plainly safe."""
    if isinstance(value, dict):
        return {k: (v if SAFE_TEXT.fullmatch(v) else "error (details withheld)") if isinstance(v, str) and k.endswith("error")
                else scrub(v) for k, v in value.items()}
    if isinstance(value, list):
        return [scrub(v) for v in value]
    return value


def iso(ts):
    return datetime.datetime.fromtimestamp(ts, datetime.timezone.utc).isoformat() if ts else None


# ---------------------------------------------------------------- server side

def connect(path):
    if not os.path.isfile(path):
        raise FileNotFoundError("database not found")
    conn = sqlite3.connect("file:%s?mode=ro" % path, uri=True)
    conn.row_factory = sqlite3.Row
    conn.execute("PRAGMA query_only=ON")
    conn.execute("BEGIN")
    return conn


def count(payload, key):
    v = payload.get(key, 0)
    return v if isinstance(v, int) and not isinstance(v, bool) and v >= 0 else None


def classify_run(state, payload):
    """Class of one run from its state and saved summary.

    Only a finalized run with no write in flight and a known final state can
    count as evidence; anything else (still running, cancelled but not settled,
    missing or malformed summary, unknown state) is reported as such."""
    if not isinstance(payload, dict):
        return "missing_evidence"
    if state in ("queued", "running", "awaiting_ai"):
        return "running"
    counts = {k: count(payload, k) for k in ("failed", "unknown", "queued", "running")}
    if state in ("failed", "partial", "interrupted") or counts["failed"]:
        return "failed"
    if state == "needs_verification" or counts["unknown"]:
        return "needs_verification"
    if None in counts.values() or not isinstance(payload.get("finalized"), bool):
        return "missing_evidence"
    if not payload["finalized"] or counts["queued"] or counts["running"]:
        return "not_finalized"
    return FINAL_STATES.get(state, "unknown_state")


def compare_config(cur, old):
    out = {}
    for table, order in CONFIG_TABLES:
        try:
            cols = [r["name"] for r in cur.execute("PRAGMA table_info(%s)" % table)]
            if not cols:
                out[table] = {"error": "table missing"}
                continue
            q = "SELECT * FROM %s ORDER BY %s" % (table, order)
            a, b = [tuple(r) for r in old.execute(q)], [tuple(r) for r in cur.execute(q)]
            changed = [c for i, c in enumerate(cols) if len(a) != len(b) or any(x[i] != y[i] for x, y in zip(a, b))]
            out[table] = {"same": a == b, "before_rows": len(a), "after_rows": len(b), "changed_columns": changed}
        except sqlite3.Error as e:
            out[table] = {"error": short(e)}
    return out


def task_rows(conn):
    tasks = {}
    for r in conn.execute("SELECT * FROM automation_tasks ORDER BY id"):
        row = dict(r)
        runtime = {k: row.pop(k) for k in TASK_RUNTIME_COLUMNS if k in row}
        payload = json.loads(row.pop("payload"))
        task = payload.get("task", {})
        runtime.update({"task." + k: task.pop(k) for k in TASK_RUNTIME_FIELDS if k in task})
        tasks[row["id"]] = (dict(row, payload=payload), runtime)
    return tasks


def compare_tasks(cur, old):
    a, b = task_rows(old), task_rows(cur)
    out = {"same": set(a) == set(b), "added": len(set(b) - set(a)), "removed": len(set(a) - set(b)), "tasks": []}
    for tid in sorted(set(a) & set(b)):
        same = a[tid][0] == b[tid][0]
        out["same"] = out["same"] and same
        out["tasks"].append({
            "task": tid[:8], "user_fields_same": same,
            "runtime_fields_changed": sorted(k for k in set(a[tid][1]) | set(b[tid][1]) if a[tid][1].get(k) != b[tid][1].get(k)),
        })
    return out


def collect_remote(params, run=run):
    """Server-side collection. Every section records an error instead of raising."""
    out = {}
    cid = None
    try:
        ids = run(["docker", "ps", "-a", "--filter", "label=com.docker.compose.project.working_dir=" + params["compose_dir"],
                   "--filter", "label=com.docker.compose.service=" + params["service"], "--format", "{{.ID}}"]).split()
        if len(ids) != 1:
            raise SafeError("expected one service container for the compose directory, found %d" % len(ids))
        cid = ids[0]
        c = json.loads(run(["docker", "inspect", cid]))[0]
        img = json.loads(run(["docker", "image", "inspect", c["Image"]]))[0]
        started = c["State"]["StartedAt"]
        since = int(datetime.datetime.strptime(started[:19], "%Y-%m-%dT%H:%M:%S").replace(tzinfo=datetime.timezone.utc).timestamp())
        out["container"] = {
            "image_ref": c["Config"]["Image"], "image_id": c["Image"], "status": c["State"]["Status"],
            "health": c["State"].get("Health", {}).get("Status"), "restarts": c["RestartCount"], "started_at": iso(since),
            "since": since, "platform": "%s/%s" % (img.get("Os"), img.get("Architecture")), "repo_digests": img.get("RepoDigests", []),
        }
        mounts = {m["Destination"]: m["Source"] for m in c["Mounts"]}
        db_path = os.path.join(mounts[DATA_MOUNT], "mail-auto.db")
        key_path = os.path.join(mounts[KEY_MOUNT], "master.key")
        port = next(iter((c["NetworkSettings"]["Ports"] or {}).values()))[0]
    except Exception as e:
        out["container"] = {"error": short(e)}
        return out
    host = "127.0.0.1" if port["HostIp"] in ("", "0.0.0.0", "::") else port["HostIp"]
    for name in ("healthz", "readyz"):
        try:
            out[name] = json.loads(run(["curl", "-fsS", "--max-time", "5", "http://%s:%s/%s" % (host, port["HostPort"], name)]))
        except Exception as e:
            out[name] = {"error": short(e)}
    cur = old = None
    try:
        cur = connect(db_path)
        out["schema"] = cur.execute("PRAGMA user_version").fetchone()[0]
        r = cur.execute("SELECT count(*) AS folders, coalesce(sum(synced_at>=?),0) AS synced, max(synced_at) AS newest FROM mailboxes", (since,)).fetchone()
        out["sync"] = {"folders": r["folders"], "synced_since_start": r["synced"], "newest_synced_at": iso(r["newest"])}
        runs = []
        for r in cur.execute("SELECT id,task_id,source,state,created_at,payload FROM task_runs WHERE created_at>=? ORDER BY created_at,id", (since,)):
            try:
                p = json.loads(r["payload"])
            except ValueError:
                p = None
            item = {"run": r["id"][:8], "task": r["task_id"][:8], "source": r["source"], "state": r["state"],
                    "class": classify_run(r["state"], p), "created_at": iso(r["created_at"])}
            if isinstance(p, dict):
                item["finalized"] = p.get("finalized")
                item.update({k: count(p, k) for k in ("scanned", "claimed", "completed", "failed", "unknown", "protected", "queued", "running")})
            runs.append(item)
        out["runs"] = runs
        out["open_operations"] = {r[0]: r[1] for r in cur.execute(
            "SELECT state,count(*) FROM operations WHERE state IN ('queued','running','needs_verification') GROUP BY state")}
        out["operations_since_start"] = {r[0]: r[1] for r in cur.execute(
            "SELECT state,count(*) FROM operations WHERE created_at>=? GROUP BY state", (since,))}
    except Exception as e:
        out["database_error"] = short(e)
    backup = params["backup"]
    out["backup"] = {"name": os.path.basename(backup.rstrip("/")), "present": os.path.isdir(backup)}
    if cur is not None:
        try:
            old = connect(os.path.join(backup, "mail-auto.db"))
            out["backup"]["schema"] = old.execute("PRAGMA user_version").fetchone()[0]
            out["config"] = compare_config(cur, old)
            out["tasks"] = compare_tasks(cur, old)
        except Exception as e:
            out["backup"]["error"] = short(e)
    try:
        with open(key_path, "rb") as f1, open(os.path.join(backup, "keys", "master.key"), "rb") as f2:
            out["master_key_same"] = f1.read() == f2.read()
    except Exception as e:
        out["master_key_error"] = short(e)
    for conn in (cur, old):
        if conn is not None:
            conn.rollback()
            conn.close()
    return out


# ----------------------------------------------------------------- local side

def collect_git(args, run=run):
    git = ["git", "-C", args.repo]
    out = {}
    try:
        commit = run(git + ["rev-parse", "--verify", args.commit + "^{commit}"]).strip()
        out["expected_commit"] = commit
        refs = {}
        for line in run(git + ["ls-remote", args.remote, "refs/heads/main", "refs/tags/v" + args.version, "refs/tags/v%s^{}" % args.version]).splitlines():
            sha, ref = line.split("\t")
            refs[ref] = sha
        out["main"] = refs.get("refs/heads/main")
        out["tag"] = refs.get("refs/tags/v%s^{}" % args.version) or refs.get("refs/tags/v" + args.version)
    except Exception as e:
        out["error"] = short(e)
        return out
    if out["main"] and out["main"] != commit:
        try:
            run(git + ["cat-file", "-e", out["main"] + "^{commit}"])
        except SafeError:
            out["main_error"] = "remote main %s is not in the local checkout, run git fetch first" % out["main"][:12]
            return out
        try:
            if run(git + ["merge-base", commit, out["main"]]).strip() == commit:
                out["after_release"] = run(git + ["diff", "--name-only", commit, out["main"]]).split()
            else:
                out["after_release"] = None
        except Exception as e:
            out["main_error"] = short(e)
    return out


def collect_registry(args, run=run):
    out = {}
    try:
        for tag in (args.version, "latest"):
            m = json.loads(run(["docker", "buildx", "imagetools", "inspect", "%s:%s" % (args.image, tag), "--format", "{{json .Manifest}}"]))
            plats = {"%s/%s" % (x["platform"]["os"], x["platform"]["architecture"]): x["digest"]
                     for x in m.get("manifests", []) if x.get("platform", {}).get("os") not in (None, "unknown")}
            out[tag] = {"digest": m["digest"], "platforms": plats}
        out["configs"] = {}
        for plat, digest in sorted(out[args.version]["platforms"].items()):
            raw = json.loads(run(["docker", "buildx", "imagetools", "inspect", "--raw", "%s@%s" % (args.image, digest)]))
            out["configs"][plat] = raw["config"]["digest"]
    except Exception as e:
        out["error"] = short(e)
    return out


def collect_server(args, run=run):
    params = {"compose_dir": args.compose_dir, "backup": args.backup, "service": args.service}
    with open(os.path.abspath(__file__), encoding="utf-8") as f:
        source = f.read()
    try:
        cmd = "python3 - %s %s" % (REMOTE_MODE, shlex.quote(json.dumps(params)))
        out = json.loads(run(["ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=15", args.ssh, cmd], input=source, timeout=180))
        if not isinstance(out, dict):
            raise ValueError("not an object")
        return scrub(out)
    except Exception as e:
        return {"error": short(e)}


# ----------------------------------------------------------------- evaluation

def evaluate(args, g, reg, srv):
    checks = []

    def add(cid, status, detail):
        checks.append({"id": cid, "status": status, "detail": detail})

    # Release commit, tag and main.
    if "error" in g or not g.get("expected_commit"):
        add("git.tag", UNVERIFIED, g.get("error", "commit not resolved"))
        add("git.main", UNVERIFIED, g.get("error", "commit not resolved"))
    else:
        c = g["expected_commit"]
        add("git.tag", MATCH if g["tag"] == c else MISMATCH,
            "v%s -> %s, expected %s" % (args.version, (g["tag"] or "missing")[:12], c[:12]))
        if not g["main"] or g.get("main_error"):
            add("git.main", UNVERIFIED, g.get("main_error") or "remote main not found")
        elif g["main"] == c:
            add("git.main", MATCH, "remote main is the release commit")
        elif g.get("after_release") is None:
            add("git.main", MISMATCH, "remote main %s does not contain the release commit" % g["main"][:12])
        else:
            code = [f for f in g["after_release"] if not (f.startswith(DOC_PREFIXES) or f.endswith(".md"))]
            add("git.main", MISMATCH if code else MATCH,
                ("remote main %s has non-documentation changes after the release: %s" % (g["main"][:12], ", ".join(code[:10]))) if code
                else "remote main %s adds only documentation after the release: %s" % (g["main"][:12], ", ".join(g["after_release"][:10])))

    # Registry: version and latest, platforms.
    if "error" in reg:
        add("image.latest_is_version", UNVERIFIED, reg["error"])
        add("image.platforms", UNVERIFIED, reg["error"])
    else:
        v, l = reg[args.version], reg["latest"]
        add("image.latest_is_version", MATCH if v["digest"] == l["digest"] else MISMATCH,
            "%s %s, latest %s" % (args.version, v["digest"], l["digest"]))
        missing = [p for p in PLATFORMS if p not in v["platforms"]]
        add("image.platforms", MISMATCH if missing else MATCH, ("missing " + ", ".join(missing)) if missing else ", ".join(PLATFORMS))

    # Server: container, image, version, health, restarts.
    ctr = srv.get("container", {})
    if "error" in srv or "error" in ctr:
        reason = srv.get("error") or ctr.get("error")
        for cid in ("server.image", "server.version", "server.health", "server.restarts"):
            add(cid, UNVERIFIED, reason)
    else:
        if "error" in reg:
            add("server.image", UNVERIFIED, "registry evidence missing")
        else:
            want = reg["configs"].get(ctr["platform"])
            index_ref = "%s@%s" % (args.image, reg[args.version]["digest"])
            add("server.image", UNVERIFIED if not want else (MATCH if ctr["image_id"] == want else MISMATCH),
                "%s image %s, release config %s, repo digest %s" % (
                    ctr["platform"], ctr["image_id"], want or "none for platform",
                    "matches" if index_ref in ctr["repo_digests"] else "differs"))
        h, r = srv.get("healthz", {}), srv.get("readyz", {})
        if "error" in h or "error" in r:
            add("server.version", UNVERIFIED, h.get("error") or r.get("error"))
        else:
            ok = h.get("version") == args.version and r.get("version") == args.version and r.get("status") == "ready"
            add("server.version", MATCH if ok else MISMATCH,
                "healthz %s %s, readyz %s %s schema %s" % (h.get("status"), h.get("version"), r.get("status"), r.get("version"), r.get("schema")))
        add("server.health", MATCH if ctr["status"] == "running" and ctr["health"] == "healthy" else MISMATCH,
            "%s, %s" % (ctr["status"], ctr["health"]))
        add("server.restarts", MATCH if ctr["restarts"] == 0 else MISMATCH, "%d restarts since %s" % (ctr["restarts"], ctr["started_at"]))

    # Sync and natural task runs since the container started.
    sync, runs = srv.get("sync"), srv.get("runs")
    db_reason = srv.get("database_error") or srv.get("error") or ctr.get("error") or "no data"
    if sync is None:
        add("sync.folders", UNVERIFIED, db_reason)
    else:
        done = sync["folders"] > 0 and sync["synced_since_start"] == sync["folders"]
        add("sync.folders", MATCH if done else UNVERIFIED,
            "%d/%d folders synced since start, newest %s" % (sync["synced_since_start"], sync["folders"], sync["newest_synced_at"]))
    if runs is None:
        add("tasks.post_deploy_runs", UNVERIFIED, db_reason)
    else:
        classes = {}
        for x in runs:
            classes[x["class"]] = classes.get(x["class"], 0) + 1
        summary = ", ".join("%s %d" % kv for kv in sorted(classes.items())) or "no runs since start"
        in_flight = sum(n for k, n in (srv.get("operations_since_start") or {}).items() if k in ("queued", "running"))
        if in_flight:
            summary += "; %d operation(s) since start still queued or running" % in_flight
        if classes.get("failed"):
            status = MISMATCH
        elif not runs or in_flight or set(classes) - PASS_CLASSES:
            status = UNVERIFIED
        else:
            status = MATCH
        if runs and status != MISMATCH and not classes.get("succeeded"):
            summary += " (no succeeded run: not evidence of a real write)"
        add("tasks.post_deploy_runs", status, summary)

    # Pre-upgrade backup comparisons.
    bk = srv.get("backup", {})
    reason = bk.get("error") or ("backup missing" if bk and not bk.get("present") else db_reason)
    cfg = srv.get("config")
    if cfg is None:
        add("config.tables", UNVERIFIED, reason)
    else:
        bad = [t for t, x in cfg.items() if "error" in x]
        diff = [t for t, x in cfg.items() if "error" not in x and not x["same"]]
        schema = srv.get("schema") == bk.get("schema")
        add("config.tables", MISMATCH if diff or not schema else (UNVERIFIED if bad else MATCH),
            "%d/%d tables same, schema %s -> %s%s%s" % (
                len(cfg) - len(diff) - len(bad), len(cfg), bk.get("schema"), srv.get("schema"),
                ("; changed: " + ", ".join("%s(%s)" % (t, ",".join(cfg[t]["changed_columns"])) for t in diff)) if diff else "",
                ("; unverified: " + ", ".join("%s(%s)" % (t, cfg[t]["error"]) for t in bad)) if bad else ""))
    tasks = srv.get("tasks")
    if tasks is None:
        add("tasks.user_fields", UNVERIFIED, reason)
    else:
        n = sum(1 for t in tasks["tasks"] if t["user_fields_same"])
        add("tasks.user_fields", MATCH if tasks["same"] else MISMATCH,
            "%d/%d tasks unchanged, added %d, removed %d" % (n, len(tasks["tasks"]), tasks["added"], tasks["removed"]))
    if "master_key_same" in srv:
        add("master_key", MATCH if srv["master_key_same"] else MISMATCH, "live key equals backup key: %s" % srv["master_key_same"])
    else:
        add("master_key", UNVERIFIED, srv.get("master_key_error") or reason)
    return checks


def build_report(args, g, reg, srv, now=None):
    checks = evaluate(args, g, reg, srv)
    statuses = {c["status"] for c in checks}
    result = MISMATCH if MISMATCH in statuses else (UNVERIFIED if UNVERIFIED in statuses else MATCH)
    ctr = srv.get("container", {})
    report = {
        "checked_at": now or datetime.datetime.now(datetime.timezone.utc).replace(microsecond=0).isoformat(),
        "expected": {"version": args.version, "commit": g.get("expected_commit") or args.commit, "image": args.image},
        "result": result, "exit_code": EXIT[result],
        "mismatched": [c["id"] for c in checks if c["status"] == MISMATCH],
        "unverified": [c["id"] for c in checks if c["status"] == UNVERIFIED],
        "checks": checks,
        "evidence": {
            "git": {k: g.get(k) for k in ("main", "tag", "after_release", "main_error", "error") if k in g},
            "registry": reg,
            "server": {
                "container": {k: v for k, v in ctr.items() if k != "since"},
                "healthz": srv.get("healthz"), "readyz": srv.get("readyz"), "schema": srv.get("schema"),
                "sync": srv.get("sync"), "runs_since_start": srv.get("runs"),
                "open_operations": srv.get("open_operations"), "operations_since_start": srv.get("operations_since_start"),
                "backup": srv.get("backup"), "config_tables": srv.get("config"),
                "task_runtime_fields": [{"task": t["task"], "changed": t["runtime_fields_changed"]} for t in (srv.get("tasks") or {}).get("tasks", [])],
                **({"error": srv["error"]} if "error" in srv else {}),
            },
        },
        "note": "Read-only evidence collection; it does not replace an independent audit. noop runs do not prove a real write.",
    }
    return report


def conclusion(report):
    lines = ["release-verify %s: %s (exit %d)" % (report["expected"]["version"], report["result"], report["exit_code"])]
    for c in report["checks"]:
        lines.append("  %-11s %-24s %s" % (c["status"], c["id"], c["detail"]))
    return "\n".join(lines)


def pattern(regex, what):
    def check(value):
        if not re.fullmatch(regex, value):
            raise argparse.ArgumentTypeError("invalid %s" % what)
        return value
    return check


def parse_args(argv):
    p = argparse.ArgumentParser(description=__doc__.split("\n\n")[0], allow_abbrev=False)
    p.add_argument("--version", required=True, type=pattern(r"[0-9]+\.[0-9]+\.[0-9]+", "version (expected X.Y.Z)"), help="expected version, e.g. 0.8.9")
    p.add_argument("--commit", required=True, type=pattern(r"[0-9a-fA-F]{4,40}", "commit (expected 4-40 hex digits)"), help="expected release commit")
    p.add_argument("--ssh", required=True, help="SSH target of the server (host alias or user@host)")
    p.add_argument("--compose-dir", required=True, help="Compose project directory on the server")
    p.add_argument("--backup", required=True, help="pre-upgrade backup directory on the server (mail-auto.db, keys/master.key)")
    p.add_argument("--image", default="allen0039/mail-auto", help="image repository (default: %(default)s)")
    p.add_argument("--service", default="app", help="Compose service name (default: %(default)s)")
    p.add_argument("--remote", default="origin", help="git remote holding main and tags (default: %(default)s)")
    p.add_argument("--repo", default=".", help="local git checkout (default: current directory)")
    return p.parse_args(argv)


def remote_main(argv, run, out, err):
    """Server side: argv is [REMOTE_MODE, JSON with exactly REMOTE_KEYS as strings]."""
    try:
        params = json.loads(argv[1]) if len(argv) == 2 else None
    except ValueError:
        params = None
    if not isinstance(params, dict) or set(params) != REMOTE_KEYS or not all(isinstance(v, str) and v for v in params.values()):
        err.write("release-verify: invalid internal collection parameters\n")
        return 2
    try:
        result = collect_remote(params, run=run)
    except Exception as e:
        result = {"error": "remote collection failed: " + short(e)}
    out.write(json.dumps(result, ensure_ascii=False) + "\n")
    return 0


def main(argv=None, run=run, out=sys.stdout, err=sys.stderr):
    argv = sys.argv[1:] if argv is None else argv
    if argv[:1] == [REMOTE_MODE]:
        return remote_main(argv, run, out, err)
    args = parse_args(argv)
    try:
        report = build_report(args, collect_git(args, run), collect_registry(args, run), collect_server(args, run))
    except Exception as e:
        err.write("release-verify: internal error (%s); nothing verified\n" % type(e).__name__)
        return EXIT[UNVERIFIED]
    out.write(json.dumps(report, ensure_ascii=False, indent=1) + "\n")
    err.write(conclusion(report) + "\n")
    return report["exit_code"]


if __name__ == "__main__":
    sys.exit(main())
