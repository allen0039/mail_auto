# Fixture tests for release_verify.py: python3 -m unittest discover -s scripts/release-verify
# Every external command (git, docker, curl, ssh) is answered by a fake; the
# server side runs in-process against temporary SQLite files and key files.
import copy
import hashlib
import io
import json
import os
import shlex
import sqlite3
import tempfile
import unittest

import release_verify as rv

COMMIT, DOCS_COMMIT, CODE_COMMIT = "a" * 40, "b" * 40, "c" * 40
INDEX, LATEST_OTHER = "sha256:" + "1" * 64, "sha256:" + "9" * 64
AMD, ARM = "sha256:" + "2" * 64, "sha256:" + "3" * 64
AMD_CFG, ARM_CFG = "sha256:" + "4" * 64, "sha256:" + "5" * 64
STARTED = 1_000_000
KEY = b"fixture-master-key-0123456789abcdef"


def make_db(path, runs=(), task_cursor="c1"):
    conn = sqlite3.connect(path)
    conn.execute("PRAGMA user_version=13")
    for table, order in rv.CONFIG_TABLES:
        cols = order.split(",")
        conn.execute("CREATE TABLE %s(%s, payload TEXT)" % (table, ",".join(cols)))
        conn.execute("INSERT INTO %s VALUES(%s)" % (table, ",".join("?" * (len(cols) + 1))), [*("k%d" % i for i in range(len(cols))), '{"x":1}'])
    conn.execute("CREATE TABLE automation_tasks(id TEXT PRIMARY KEY,account_id TEXT,version INTEGER,service_owner TEXT,payload TEXT,enabled INTEGER,trigger_mode TEXT,next_run_at INTEGER,updated_at INTEGER)")
    task = {"task": {"id": "task-0001", "max_batch": 20, "protect_unread": True, "protect_flagged": True, "notify": "inherit",
                     "cursor": task_cursor, "last_triggered": STARTED - 100, "next_run_at": STARTED + 3600}}
    conn.execute("INSERT INTO automation_tasks VALUES('task-0001','acc',3,'owner-secret',?,1,'internal',?,?)", (json.dumps(task), STARTED + 3600, STARTED - 100))
    conn.execute("CREATE TABLE task_runs(id TEXT,task_id TEXT,source TEXT,state TEXT,payload TEXT,created_at INTEGER,updated_at INTEGER)")
    for i, (state, extra) in enumerate(runs):
        payload = dict({"scanned": 50, "claimed": 0, "completed": 0, "failed": 0, "unknown": 0, "protected": 0}, **extra)
        conn.execute("INSERT INTO task_runs VALUES(?,?,?,?,?,?,?)", ("run-%04d" % i, "task-0001", "internal", state, json.dumps(payload), STARTED + 60 + i, STARTED + 61 + i))
    conn.execute("CREATE TABLE mailboxes(account_id TEXT,name TEXT,uidvalidity INTEGER,synced_at INTEGER)")
    for i in range(3):
        conn.execute("INSERT INTO mailboxes VALUES('acc',?,1,?)", ("Folder%d" % i, STARTED + 30))
    conn.execute("CREATE TABLE operations(id TEXT,state TEXT,created_at INTEGER,updated_at INTEGER)")
    conn.execute("INSERT INTO operations VALUES('op-old','needs_verification',?,?)", (STARTED - 86400, STARTED - 86400))
    conn.commit()
    conn.close()


class Fixture:
    """A server (live volumes + backup) and the registry/git answers for a matching release."""

    def __init__(self, tmp):
        self.tmp = tmp
        self.data, self.keys, self.backup = (os.path.join(tmp, d) for d in ("data", "keys", "backup"))
        for d in (self.data, self.keys, os.path.join(self.backup, "keys")):
            os.makedirs(d)
        make_db(os.path.join(self.backup, "mail-auto.db"))
        make_db(os.path.join(self.data, "mail-auto.db"), runs=[("noop", {})], task_cursor="c2")
        for path in (os.path.join(self.keys, "master.key"), os.path.join(self.backup, "keys", "master.key")):
            with open(path, "wb") as f:
                f.write(KEY)
        self.main = COMMIT
        self.latest = INDEX
        self.image_id = ARM_CFG
        self.health_version = "0.8.9"
        self.restarts = 0
        self.fail = set()  # command names that fail
        self.calls = []

    def args(self, **kw):
        a = ["--version", "0.8.9", "--commit", COMMIT[:7], "--ssh", "ssh-target-host", "--compose-dir", "/srv/mail_auto", "--backup", self.backup]
        for k, v in kw.items():
            a += ["--" + k.replace("_", "-"), v]
        return a

    def manifest(self, digest):
        return {"digest": digest, "manifests": [
            {"digest": AMD, "platform": {"os": "linux", "architecture": "amd64"}},
            {"digest": ARM, "platform": {"os": "linux", "architecture": "arm64"}},
            {"digest": "sha256:" + "6" * 64, "platform": {"os": "unknown", "architecture": "unknown"}}]}

    def run(self, cmd, input=None, timeout=60):
        self.calls.append(cmd)
        key = cmd[0] if cmd[0] != "docker" else "docker " + cmd[1]
        if key in self.fail or cmd[0] in self.fail:
            raise RuntimeError("%s exited 1: fixture failure" % cmd[0])
        if cmd[0] == "git":
            sub = cmd[3]
            if sub == "rev-parse":
                given = cmd[5].split("^")[0]
                return (given if len(given) == 40 else COMMIT) + "\n"
            if sub == "ls-remote":
                return "%s\trefs/heads/main\n%s\trefs/tags/v0.8.9\n%s\trefs/tags/v0.8.9^{}\n" % (self.main, "d" * 40, COMMIT)
            if sub == "cat-file":
                return ""
            if sub == "merge-base":
                return COMMIT + "\n"
            if sub == "diff":
                return "docs/acceptance-0.8.9.md\n" if self.main == DOCS_COMMIT else "docs/x.md\ninternal/app.go\n"
        if cmd[:3] == ["docker", "buildx", "imagetools"]:
            ref = cmd[-1] if "--raw" in cmd else cmd[4]
            if "--raw" in cmd:
                return json.dumps({"config": {"digest": AMD_CFG if ref.endswith(AMD) else ARM_CFG}})
            return json.dumps(self.manifest(self.latest if ref.endswith(":latest") else INDEX))
        if cmd[0] == "ssh":
            assert cmd[-1].startswith("python3 - --remote ")
            assert input is not None and "def collect_remote" in input
            params = json.loads(shlex.split(cmd[-1])[3])
            out = io.StringIO()
            rv.main(["--remote", json.dumps(params)], run=self.run, out=out)
            return out.getvalue()
        if cmd[:2] == ["docker", "ps"]:
            return "abc123\n"
        if cmd[:2] == ["docker", "inspect"]:
            return json.dumps([{
                "Image": self.image_id, "RestartCount": self.restarts,
                "Config": {"Image": "allen0039/mail-auto:latest"},
                "State": {"Status": "running", "StartedAt": "1970-01-12T13:46:40.123456789Z", "Health": {"Status": "healthy"}},
                "Mounts": [{"Destination": rv.DATA_MOUNT, "Source": self.data}, {"Destination": rv.KEY_MOUNT, "Source": self.keys}],
                "NetworkSettings": {"Ports": {"8080/tcp": [{"HostIp": "127.0.0.1", "HostPort": "61609"}]}}}])
        if cmd[:3] == ["docker", "image", "inspect"]:
            return json.dumps([{"Os": "linux", "Architecture": "arm64", "RepoDigests": ["allen0039/mail-auto@" + INDEX]}])
        if cmd[0] == "curl":
            if cmd[-1].endswith("/healthz"):
                return json.dumps({"status": "ok", "version": self.health_version})
            return json.dumps({"status": "ready", "version": self.health_version, "schema": 13})
        raise AssertionError("unexpected command %r" % cmd)

    def execute(self, **kw):
        out, err = io.StringIO(), io.StringIO()
        code = rv.main(self.args(**kw), run=self.run, out=out, err=err)
        report = json.loads(out.getvalue())
        self.last_output = out.getvalue() + err.getvalue()
        return code, report

    def files_digest(self):
        h = {}
        for root, _, files in os.walk(self.tmp):
            for name in sorted(files):
                p = os.path.join(root, name)
                with open(p, "rb") as f:
                    h[p] = (hashlib.sha256(f.read()).hexdigest(), os.stat(p).st_mtime_ns)
        return h

    def sql(self, which, query, *params):
        conn = sqlite3.connect(os.path.join(self.backup if which == "backup" else self.data, "mail-auto.db"))
        conn.execute(query, params)
        conn.commit()
        conn.close()


def statuses(report):
    return {c["id"]: c["status"] for c in report["checks"]}


class ReleaseVerifyTest(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory()
        self.fx = Fixture(self._tmp.name)

    def tearDown(self):
        self._tmp.cleanup()

    def test_normal_match_is_read_only_and_repeatable(self):
        before = self.fx.files_digest()
        code, first = self.fx.execute()
        self.assertEqual(code, 0, self.fx.last_output)
        self.assertEqual(first["result"], "match")
        self.assertEqual(set(statuses(first).values()), {"match"})
        self.assertEqual(len(first["checks"]), 13)
        code2, second = self.fx.execute()
        self.assertEqual(code2, 0)
        first.pop("checked_at"), second.pop("checked_at")
        self.assertEqual(first, second)
        self.assertEqual(self.fx.files_digest(), before, "databases or keys changed")
        # Only read commands were issued.
        for cmd in self.fx.calls:
            self.assertIn(" ".join(cmd[:3]) if cmd[0] == "docker" else cmd[0],
                          {"git", "ssh", "curl", "docker buildx imagetools", "docker ps -a", "docker inspect abc123", "docker image inspect"})
            if cmd[0] == "git":
                self.assertIn(cmd[3], {"rev-parse", "ls-remote", "cat-file", "merge-base", "diff"})

    def test_output_is_redacted(self):
        _, report = self.fx.execute()
        text = self.fx.last_output
        for secret in (KEY.decode(), hashlib.sha256(KEY).hexdigest(), "owner-secret", "Folder0", self.fx.backup, "ssh-target-host", "/srv/mail_auto", '{"x":1}'):
            self.assertNotIn(secret, text)
        self.assertEqual(report["evidence"]["server"]["backup"]["name"], "backup")

    def test_version_and_digest_mismatch(self):
        self.fx.latest = LATEST_OTHER
        self.fx.health_version = "0.8.8"
        self.fx.image_id = "sha256:" + "7" * 64
        code, report = self.fx.execute()
        self.assertEqual(code, 1)
        self.assertEqual(report["result"], "mismatch")
        self.assertEqual(set(report["mismatched"]), {"image.latest_is_version", "server.version", "server.image"})

    def test_tag_on_other_commit_and_restart(self):
        self.fx.restarts = 2
        code, report = self.fx.execute(commit="e" * 40)
        self.assertEqual(code, 1)
        self.assertEqual(set(report["mismatched"]), {"git.tag", "git.main", "server.restarts"})

    def test_config_mismatch(self):
        self.fx.sql("live", "UPDATE mail_rules SET payload='{\"x\":2}'")
        self.fx.sql("live", "UPDATE automation_tasks SET payload=json_set(payload,'$.task.max_batch',50)")
        code, report = self.fx.execute()
        self.assertEqual(code, 1)
        self.assertEqual(set(report["mismatched"]), {"config.tables", "tasks.user_fields"})
        self.assertEqual(report["evidence"]["server"]["config_tables"]["mail_rules"]["changed_columns"], ["payload"])
        self.assertNotIn('{"x":2}', self.fx.last_output)

    def test_master_key_mismatch(self):
        with open(os.path.join(self.fx.keys, "master.key"), "wb") as f:
            f.write(b"another key")
        code, report = self.fx.execute()
        self.assertEqual((code, report["mismatched"]), (1, ["master_key"]))

    def test_missing_backup_is_unverified_not_pass(self):
        code, report = self.fx.execute(backup=os.path.join(self.fx.tmp, "no-such-backup"))
        self.assertEqual(code, 3)
        self.assertEqual(report["result"], "unverified")
        self.assertEqual(set(report["unverified"]), {"config.tables", "tasks.user_fields", "master_key"})
        self.assertFalse(report["evidence"]["server"]["backup"]["present"])

    def test_unreadable_database_is_unverified(self):
        os.rename(os.path.join(self.fx.data, "mail-auto.db"), os.path.join(self.fx.data, "moved.db"))
        code, report = self.fx.execute()
        self.assertEqual(code, 3)
        self.assertEqual(set(report["unverified"]), {"sync.folders", "tasks.post_deploy_runs", "config.tables", "tasks.user_fields"})

    def test_command_failures_are_unverified(self):
        for failing, expected in [
            ("ssh", {"server.image", "server.version", "server.health", "server.restarts", "sync.folders", "tasks.post_deploy_runs",
                     "config.tables", "tasks.user_fields", "master_key"}),
            ("docker buildx", {"image.latest_is_version", "image.platforms", "server.image"}),
            ("git", {"git.tag", "git.main"}),
            ("curl", {"server.version"}),
            ("docker inspect", {"server.image", "server.version", "server.health", "server.restarts", "sync.folders",
                                "tasks.post_deploy_runs", "config.tables", "tasks.user_fields", "master_key"}),
        ]:
            with self.subTest(failing=failing):
                self.fx.fail = {failing}
                code, report = self.fx.execute()
                self.assertEqual(code, 3, report["checks"])
                self.assertEqual(set(report["unverified"]), expected)
                self.assertEqual(report["mismatched"], [])

    def test_no_post_deploy_run_is_reported_immediately(self):
        self.fx.sql("live", "DELETE FROM task_runs")
        code, report = self.fx.execute()
        self.assertEqual(code, 3)
        self.assertEqual(report["unverified"], ["tasks.post_deploy_runs"])
        self.assertIn("no runs since start", next(c["detail"] for c in report["checks"] if c["id"] == "tasks.post_deploy_runs"))

    def test_run_classes(self):
        cases = [
            ([("noop", {})], "match", "noop 1"),
            ([("completed", {"claimed": 2, "completed": 2})], "match", "succeeded 1"),
            ([("noop", {}), ("failed", {"failed": 1})], "mismatch", "failed 1"),
            ([("needs_verification", {"unknown": 1})], "unverified", "needs_verification 1"),
            ([("running", {})], "unverified", "running 1"),
        ]
        for runs, status, text in cases:
            with self.subTest(runs=runs):
                self.fx.sql("live", "DELETE FROM task_runs")
                for i, (state, extra) in enumerate(runs):
                    payload = dict({"scanned": 1, "failed": 0, "unknown": 0}, **extra)
                    self.fx.sql("live", "INSERT INTO task_runs VALUES(?,?,?,?,?,?,?)", "r%d" % i, "task-0001", "internal", state, json.dumps(payload), STARTED + 10 + i, STARTED + 11)
                _, report = self.fx.execute()
                check = next(c for c in report["checks"] if c["id"] == "tasks.post_deploy_runs")
                self.assertEqual(check["status"], status)
                self.assertIn(text, check["detail"])
                self.assertEqual("succeeded" in text, "not evidence of a real write" not in check["detail"])

    def test_natural_run_fields_are_listed_apart_from_user_fields(self):
        self.fx.sql("live", "UPDATE automation_tasks SET next_run_at=next_run_at+3600, updated_at=updated_at+3600, "
                            "payload=json_set(payload,'$.task.last_triggered',?,'$.task.next_run_at',?)", STARTED + 60, STARTED + 7200)
        code, report = self.fx.execute()
        self.assertEqual(code, 0, report["checks"])
        self.assertEqual(statuses(report)["tasks.user_fields"], "match")
        self.assertEqual(report["evidence"]["server"]["task_runtime_fields"],
                         [{"task": "task-000", "changed": ["next_run_at", "task.cursor", "task.last_triggered", "task.next_run_at", "updated_at"]}])

    def test_folders_not_yet_synced_are_unverified(self):
        self.fx.sql("live", "UPDATE mailboxes SET synced_at=? WHERE name='Folder1'", STARTED - 10)
        code, report = self.fx.execute()
        self.assertEqual((code, report["unverified"]), (3, ["sync.folders"]))

    def test_main_after_release(self):
        self.fx.main = DOCS_COMMIT
        code, report = self.fx.execute()
        self.assertEqual(code, 0)
        self.assertIn("only documentation", next(c["detail"] for c in report["checks"] if c["id"] == "git.main"))
        self.fx.main = CODE_COMMIT
        code, report = self.fx.execute()
        self.assertEqual((code, report["mismatched"]), (1, ["git.main"]))

    def test_mismatch_wins_over_unverified(self):
        self.fx.health_version = "0.8.8"
        code, report = self.fx.execute(backup=os.path.join(self.fx.tmp, "none"))
        self.assertEqual((code, report["result"]), (1, "mismatch"))
        self.assertTrue(report["unverified"])

    def test_usage_error_exit_2(self):
        with self.assertRaises(SystemExit) as e, open(os.devnull, "w") as devnull:
            import contextlib
            with contextlib.redirect_stderr(devnull):
                rv.main(["--version", "0.8.9"], run=self.fx.run)
        self.assertEqual(e.exception.code, 2)


if __name__ == "__main__":
    unittest.main()
