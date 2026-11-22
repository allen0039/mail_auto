# Fixture tests for release_verify.py: python3 -m unittest discover -s scripts/release-verify
# Every external command (git, docker, curl, ssh) is answered by a fake; the
# server side runs in-process against temporary SQLite files and key files.
import copy
import hashlib
import io
import json
import os
import shlex
import subprocess
import sys
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
        payload = dict({"scanned": 50, "claimed": 0, "completed": 0, "failed": 0, "unknown": 0, "protected": 0,
                        "queued": 0, "running": 0, "finalized": True}, **extra)
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
        self.real = set()  # command names run through rv.run against a fake executable in bindir
        self.bindir = os.path.join(tmp, "bin")
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
        if cmd[0] in self.real:  # through the real runner, to a fake executable
            return rv.run([os.path.join(self.bindir, cmd[0])] + cmd[1:], input=input, timeout=timeout)
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
            assert cmd[-1].startswith("python3 - %s " % rv.REMOTE_MODE)
            assert input is not None and "def collect_remote" in input
            params = json.loads(shlex.split(cmd[-1])[3])
            out = io.StringIO()
            rv.main([rv.REMOTE_MODE, json.dumps(params)], run=self.run, out=out)
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
        self.last_err = err.getvalue()
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
            ([("manual_kept", {"manual_kept": 1})], "unverified", "manual_kept 1"),
        ]
        for runs, status, text in cases:
            with self.subTest(runs=runs):
                self.fx.sql("live", "DELETE FROM task_runs")
                for i, (state, extra) in enumerate(runs):
                    payload = dict({"scanned": 1, "failed": 0, "unknown": 0, "queued": 0, "running": 0, "finalized": True}, **extra)
                    self.fx.sql("live", "INSERT INTO task_runs VALUES(?,?,?,?,?,?,?)", "r%d" % i, "task-0001", "internal", state, json.dumps(payload), STARTED + 10 + i, STARTED + 11)
                _, report = self.fx.execute()
                check = next(c for c in report["checks"] if c["id"] == "tasks.post_deploy_runs")
                self.assertEqual(check["status"], status)
                self.assertIn(text, check["detail"])
                self.assertEqual("succeeded" in text or status == "mismatch", "not evidence of a real write" not in check["detail"])

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


SECRET = "SECRET-7f3a"
LEAKY = ("#!/bin/sh\necho 'fatal: https://user:%s@private.example.net/repo /srv/%s/compose' >&2\n"
         "echo 'Traceback (most recent call last): %s' >&2\nexit 128\n") % (SECRET, SECRET, SECRET)


def add_run(fx, run_id, state, payload, created=STARTED + 10):
    fx.sql("live", "INSERT INTO task_runs VALUES(?,?,?,?,?,?,?)", run_id, "task-0001", "internal", state,
           payload if isinstance(payload, str) else json.dumps(payload), created, created + 1)


def final(**extra):
    return dict({"scanned": 5, "claimed": 0, "completed": 0, "failed": 0, "unknown": 0, "queued": 0, "running": 0, "finalized": True}, **extra)


class RunEvidenceTest(unittest.TestCase):
    """RV-01: only finalized runs in a known final state count; the rest is unverified."""

    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory()
        self.fx = Fixture(self._tmp.name)
        self.fx.sql("live", "DELETE FROM task_runs")

    def tearDown(self):
        self._tmp.cleanup()

    def check(self):
        code, report = self.fx.execute()
        return code, next(c for c in report["checks"] if c["id"] == "tasks.post_deploy_runs"), report

    def test_audit_repro_cancelled_not_finalized_with_running_operation(self):
        add_run(self.fx, "r1", "cancelled", final(finalized=False, running=1))
        self.fx.sql("live", "INSERT INTO operations VALUES('op-new','running',?,?)", STARTED + 20, STARTED + 20)
        code, check, report = self.check()
        self.assertEqual((code, report["result"], check["status"]), (3, "unverified", "unverified"))
        self.assertIn("not_finalized 1", check["detail"])
        self.assertIn("still queued or running", check["detail"])
        self.assertEqual(report["evidence"]["server"]["runs_since_start"][0]["finalized"], False)

    def test_unsettled_unknown_or_unproven_states_are_unverified(self):
        cases = [
            ("future_unknown_state", final(), "unknown_state 1"),
            ("completed", final(finalized=False, claimed=2, completed=2), "not_finalized 1"),
            ("completed", final(queued=1), "not_finalized 1"),
            ("completed", {k: v for k, v in final().items() if k != "finalized"}, "missing_evidence 1"),
            ("noop", final(finalized="yes"), "missing_evidence 1"),
            ("noop", final(running="0"), "missing_evidence 1"),
            ("noop", "{not json", "missing_evidence 1"),
            ("noop", "[]", "missing_evidence 1"),
            ("cancelled", final(), "cancelled 1"),
            ("limited", final(), "limited 1"),
            ("requires_ai", final(), "requires_ai 1"),
            ("awaiting_ai", final(finalized=False), "running 1"),
            ("needs_verification", final(unknown=1), "needs_verification 1"),
        ]
        for state, payload, text in cases:
            with self.subTest(state=state, payload=payload):
                self.fx.sql("live", "DELETE FROM task_runs")
                add_run(self.fx, "r1", state, payload)
                code, check, _ = self.check()
                self.assertEqual((code, check["status"]), (3, "unverified"))
                self.assertIn(text, check["detail"])

    def test_settled_states_keep_their_meaning(self):
        cases = [
            ("noop", final(), 0, "match", "noop 1", True),
            ("completed", final(claimed=2, completed=2), 0, "match", "succeeded 1", False),
            ("observed", final(claimed=3), 0, "match", "observed 1", True),
            ("failed", final(failed=1), 1, "mismatch", "failed 1", False),
            ("partial", final(completed=1, failed=1), 1, "mismatch", "failed 1", False),
            ("interrupted", final(finalized=False), 1, "mismatch", "failed 1", False),
        ]
        for state, payload, code_want, status, text, no_write_note in cases:
            with self.subTest(state=state):
                self.fx.sql("live", "DELETE FROM task_runs")
                add_run(self.fx, "r1", state, payload)
                code, check, _ = self.check()
                self.assertEqual((code, check["status"]), (code_want, status))
                self.assertIn(text, check["detail"])
                self.assertEqual("not evidence of a real write" in check["detail"], no_write_note)

    def test_finished_operation_since_start_does_not_block(self):
        add_run(self.fx, "r1", "completed", final(claimed=1, completed=1))
        self.fx.sql("live", "INSERT INTO operations VALUES('op-new','completed',?,?)", STARTED + 20, STARTED + 21)
        code, check, report = self.check()
        self.assertEqual((code, check["status"]), (0, "match"))
        self.assertEqual(report["evidence"]["server"]["operations_since_start"], {"completed": 1})

    def test_one_unsettled_run_among_good_ones(self):
        add_run(self.fx, "r1", "noop", final())
        add_run(self.fx, "r2", "cancelled", final(finalized=False), created=STARTED + 20)
        code, check, _ = self.check()
        self.assertEqual((code, check["status"]), (3, "unverified"))
        self.assertIn("noop 1, not_finalized 1", check["detail"])


class RedactionTest(unittest.TestCase):
    """RV-02: failures never pass command stderr or exception text through."""

    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory()
        self.fx = Fixture(self._tmp.name)
        os.makedirs(self.fx.bindir)
        for name in ("ssh", "git", "docker", "curl"):
            path = os.path.join(self.fx.bindir, name)
            with open(path, "w") as f:
                f.write(LEAKY)
            os.chmod(path, 0o755)

    def tearDown(self):
        self._tmp.cleanup()

    def assertClean(self, text):
        for bad in (SECRET, "private.example", "Traceback", "/srv/"):
            self.assertNotIn(bad, text)

    def test_real_command_failures_are_redacted(self):
        cases = [
            ("ssh", "ssh exited 128", {"server.image", "master_key"}),
            ("git", "git rev-parse exited 128", {"git.tag", "git.main"}),
            ("docker", "docker buildx imagetools exited 128", {"image.platforms"}),
            ("curl", "curl exited 128", {"server.version"}),
        ]
        for name, message, some_unverified in cases:
            with self.subTest(command=name):
                self.fx.real = {name}
                code, report = self.fx.execute()
                self.assertEqual(code, 3)
                self.assertLessEqual(some_unverified, set(report["unverified"]))
                self.assertIn(message, self.fx.last_output)
                self.assertClean(self.fx.last_output)

    def test_remote_docker_failure_is_redacted(self):
        # docker on the server side fails: the container section reports only the command name.
        self.fx.real = {"docker"}
        self.fx.fail = set()
        code, report = self.fx.execute()
        self.assertEqual(code, 3)
        self.assertEqual(report["evidence"]["server"]["container"], {"error": "docker ps exited 128"})
        self.assertClean(self.fx.last_output)

    def test_timeout_missing_command_and_exception_text(self):
        slow = os.path.join(self.fx.bindir, "slow")
        with open(slow, "w") as f:
            f.write("#!/bin/sh\nsleep 5\n")
        os.chmod(slow, 0o755)
        with self.assertRaises(rv.SafeError) as e:
            rv.run([slow, "--token=" + SECRET], timeout=0.2)
        self.assertEqual(str(e.exception), "command timed out")
        with self.assertRaises(rv.SafeError) as e:
            rv.run([os.path.join(self.fx.bindir, "nope-" + SECRET)])
        self.assertNotIn(SECRET, str(e.exception))
        for exc in (ValueError(SECRET), KeyError(SECRET), sqlite3.OperationalError("no such table: " + SECRET),
                    PermissionError(13, "denied", "/srv/" + SECRET), OSError(SECRET), RuntimeError(SECRET)):
            self.assertNotIn(SECRET, rv.short(exc))

    def test_permission_denied_on_key(self):
        key = os.path.join(self.fx.keys, "master.key")
        os.chmod(key, 0)
        try:
            if os.access(key, os.R_OK):
                self.skipTest("running as root")
            code, report = self.fx.execute()
        finally:
            os.chmod(key, 0o600)
        self.assertEqual((code, report["unverified"]), (3, ["master_key"]))
        self.assertIn("permission denied", self.fx.last_output)
        self.assertNotIn(self.fx.keys, self.fx.last_output)

    def test_remote_crash_and_odd_remote_errors(self):
        out, err = io.StringIO(), io.StringIO()
        real = rv.collect_remote
        rv.collect_remote = lambda params, run: (_ for _ in ()).throw(RuntimeError(SECRET))
        try:
            code = rv.main([rv.REMOTE_MODE, json.dumps({"compose_dir": "/x", "backup": "/y", "service": "app"})], out=out, err=err)
        finally:
            rv.collect_remote = real
        self.assertEqual(code, 0)
        self.assertEqual(json.loads(out.getvalue()), {"error": "remote collection failed: error (RuntimeError)"})
        scrubbed = rv.scrub({"error": "boom " + SECRET + " /srv/x", "container": {"error": "docker ps exited 1"},
                             "config": {"t": {"error": "sqlite error (OperationalError)"}}, "master_key_error": "permission denied"})
        self.assertEqual(scrubbed, {"error": "error (details withheld)", "container": {"error": "docker ps exited 1"},
                                    "config": {"t": {"error": "sqlite error (OperationalError)"}}, "master_key_error": "permission denied"})


class ArgumentTest(unittest.TestCase):
    """RV-03: public --remote never collides with the internal collection mode."""

    SCRIPT = os.path.join(os.path.dirname(os.path.abspath(__file__)), "release_verify.py")

    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory()
        self.fx = Fixture(self._tmp.name)

    def tearDown(self):
        self._tmp.cleanup()

    def test_remote_option_in_any_position(self):
        base = self.fx.args()
        for argv in (["--remote", "origin"] + base, base + ["--remote", "origin"], ["--remote=origin"] + base,
                     base[:4] + ["--remote", "upstream"] + base[4:]):
            with self.subTest(argv=argv[:2]):
                out, err = io.StringIO(), io.StringIO()
                self.assertEqual(rv.main(argv, run=self.fx.run, out=out, err=err), 0, err.getvalue())
                self.assertEqual(json.loads(out.getvalue())["result"], "match")
        ls_remote = [c for c in self.fx.calls if c[0] == "git" and c[3] == "ls-remote"]
        self.assertEqual(ls_remote[-1][4], "upstream")

    def cli(self, *argv):
        return subprocess.run([sys.executable, self.SCRIPT, *argv], capture_output=True, text=True, timeout=30)

    def test_usage_errors_exit_2_without_traceback(self):
        base = self.fx.args()
        cases = [
            ["--remote"],
            base + ["--remote"],
            ["--remote", "origin", "--version", "0.8.9"],
            [x if x != "0.8.9" else "0.8" for x in base],
            [x if x != COMMIT[:7] else "not-a-sha" for x in base],
            base + ["--internal-remote-collect", "{}"],
            base + ["--rem", "origin"],
            [rv.REMOTE_MODE],
            [rv.REMOTE_MODE, "{not json"],
            [rv.REMOTE_MODE, json.dumps({"compose_dir": "/x", "backup": "/y"})],
            [rv.REMOTE_MODE, json.dumps({"compose_dir": "/x", "backup": "/y", "service": "app", "extra": 1})],
            [rv.REMOTE_MODE, json.dumps({"compose_dir": "/x", "backup": 3, "service": "app"})],
            [rv.REMOTE_MODE, "{}", "extra"],
        ]
        for argv in cases:
            with self.subTest(argv=argv):
                r = self.cli(*argv)
                self.assertEqual(r.returncode, 2, r.stderr)
                self.assertEqual(r.stdout, "")
                self.assertNotIn("Traceback", r.stderr)
                self.assertTrue("usage:" in r.stderr or "invalid internal collection parameters" in r.stderr, r.stderr)


class EvidenceCompletenessTest(unittest.TestCase):
    """RV-04: absent counts and failed or absent operation queries are missing evidence, never 0 or empty."""

    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory()
        self.fx = Fixture(self._tmp.name)
        self.fx.sql("live", "DELETE FROM task_runs")

    def tearDown(self):
        self._tmp.cleanup()

    def task_check(self):
        code, report = self.fx.execute()
        return code, next(c for c in report["checks"] if c["id"] == "tasks.post_deploy_runs"), report

    def put(self, state, payload):
        self.fx.sql("live", "DELETE FROM task_runs")
        add_run(self.fx, "r1", state, payload)

    def test_each_required_field_missing_or_malformed(self):
        for state in ("noop", "completed", "observed"):
            for key in rv.REQUIRED_COUNTS + ("finalized",):
                bad_values = [None] + ([-1, "0", 0.0, True, None] if key != "finalized" else ["true", 1, None])
                for bad in bad_values:
                    with self.subTest(state=state, key=key, value=bad):
                        payload = final(claimed=1, completed=1) if state == "completed" else final()
                        if bad is None:
                            payload.pop(key)
                        else:
                            payload[key] = bad
                        self.put(state, payload)
                        code, check, report = self.task_check()
                        self.assertEqual((code, report["result"], check["status"]), (3, "unverified", "unverified"))
                        self.assertIn("missing_evidence 1", check["detail"])

    def test_audit_repro_only_finalized(self):
        self.put("noop", {"finalized": True})
        code, check, _ = self.task_check()
        self.assertEqual((code, check["status"]), (3, "unverified"))
        self.assertIn("missing_evidence 1", check["detail"])

    def test_valid_zero_counts_pass(self):
        for state, payload, text in [("noop", final(), "noop 1"), ("observed", final(claimed=2), "observed 1"),
                                     ("completed", final(claimed=1, completed=1), "succeeded 1")]:
            with self.subTest(state=state):
                self.put(state, payload)
                code, check, report = self.task_check()
                self.assertEqual((code, check["status"]), (0, "match"))
                self.assertIn(text, check["detail"])
                self.assertEqual(report["evidence"]["server"]["runs_since_start"][0]["queued"], 0)

    def test_definite_states_keep_meaning_without_counts(self):
        for state, code_want, status, text in [("failed", 1, "mismatch", "failed 1"), ("partial", 1, "mismatch", "failed 1"),
                                               ("needs_verification", 3, "unverified", "needs_verification 1"),
                                               ("running", 3, "unverified", "running 1")]:
            with self.subTest(state=state):
                self.put(state, {"finalized": False})
                code, check, _ = self.task_check()
                self.assertEqual((code, check["status"]), (code_want, status))
                self.assertIn(text, check["detail"])
        for key, value, cls in [("failed", 2, "failed 1"), ("unknown", 1, "needs_verification 1")]:
            with self.subTest(count=key):
                self.put("completed", final(**{key: value}))
                self.assertIn(cls, self.task_check()[1]["detail"])

    def test_audit_repro_running_operation_and_since_start_query_failure(self):
        add_run(self.fx, "r1", "noop", final())
        self.fx.sql("live", "INSERT INTO operations VALUES('op-new','running',?,?)", STARTED + 20, STARTED + 20)
        self.fx.sql("live", "ALTER TABLE operations RENAME TO operations_raw")
        self.fx.sql("live", "CREATE VIEW operations AS SELECT id,state FROM operations_raw")
        code, check, report = self.task_check()
        server = report["evidence"]["server"]
        self.assertEqual(server["open_operations"], {"needs_verification": 1, "running": 1})
        self.assertIsNone(server["operations_since_start"])
        self.assertEqual(server["operations_since_start_error"], "sqlite error (OperationalError)")
        self.assertEqual((code, report["result"], check["status"]), (3, "unverified", "unverified"))
        self.assertIn("operations_since_start not collected: sqlite error (OperationalError)", check["detail"])
        self.assertNotIn("created_at", self.fx.last_output.split('"checks"')[1].split('"evidence"')[0])

    def test_both_operation_queries_fail(self):
        add_run(self.fx, "r1", "noop", final())
        self.fx.sql("live", "DROP TABLE operations")
        code, check, report = self.task_check()
        self.assertEqual((code, check["status"]), (3, "unverified"))
        self.assertIn("open_operations not collected", check["detail"])
        self.assertIn("operations_since_start not collected", check["detail"])
        self.assertNotIn("no such table", self.fx.last_output)

    def test_runs_or_sync_query_failure(self):
        self.fx.sql("live", "ALTER TABLE task_runs RENAME TO task_runs_raw")
        self.fx.sql("live", "CREATE VIEW task_runs AS SELECT id,task_id,source,state,created_at FROM task_runs_raw")
        self.fx.sql("live", "ALTER TABLE mailboxes RENAME TO mailboxes_raw")
        code, report = self.fx.execute()
        checks = statuses(report)
        self.assertEqual((code, checks["tasks.post_deploy_runs"], checks["sync.folders"]), (3, "unverified", "unverified"))
        detail = {c["id"]: c["detail"] for c in report["checks"]}
        self.assertEqual(detail["tasks.post_deploy_runs"], "runs not collected: sqlite error (OperationalError)")
        self.assertEqual(detail["sync.folders"], "sqlite error (OperationalError)")

    def test_absent_operation_results_from_server(self):
        add_run(self.fx, "r1", "noop", final())
        code, report = self.fx.execute()
        self.assertEqual(code, 0)
        args = rv.parse_args(self.fx.args())
        srv = rv.collect_server(args, self.fx.run)
        g, reg = rv.collect_git(args, self.fx.run), rv.collect_registry(args, self.fx.run)
        for key in ("open_operations", "operations_since_start"):
            for value in ("drop", None, [], "x"):
                with self.subTest(key=key, value=value):
                    bad = copy.deepcopy(srv)
                    if value == "drop":
                        bad.pop(key)
                    else:
                        bad[key] = value
                    report = rv.build_report(args, g, reg, bad)
                    check = next(c for c in report["checks"] if c["id"] == "tasks.post_deploy_runs")
                    self.assertEqual((report["exit_code"], check["status"]), (3, "unverified"))
                    self.assertIn(key + " not collected: no data", check["detail"])

    def test_normal_empty_operations_and_mismatch_priority(self):
        add_run(self.fx, "r1", "noop", final())
        code, check, report = self.task_check()
        self.assertEqual((code, check["status"]), (0, "match"))
        self.assertEqual(report["evidence"]["server"]["operations_since_start"], {})
        # A failed run stays a mismatch even when operation evidence is missing.
        self.put("failed", final(failed=1))
        self.fx.sql("live", "DROP TABLE operations")
        code, check, report = self.task_check()
        self.assertEqual((code, report["result"], check["status"]), (1, "mismatch", "mismatch"))
        # Another check's mismatch wins over this unverified item.
        self.put("noop", {"finalized": True})
        self.fx.health_version = "0.8.8"
        code, check, report = self.task_check()
        self.assertEqual((code, check["status"]), (1, "unverified"))
        self.assertIn("server.version", report["mismatched"])


class FailurePrecedenceTest(unittest.TestCase):
    """RV-05: a definite failure is a mismatch even when other evidence is pending or missing."""

    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory()
        self.fx = Fixture(self._tmp.name)
        self.fx.sql("live", "DELETE FROM task_runs")

    def tearDown(self):
        self._tmp.cleanup()

    def report_for(self, runs):
        self.fx.sql("live", "DELETE FROM task_runs")
        for i, (state, payload) in enumerate(runs):
            add_run(self.fx, "r%d" % i, state, payload, created=STARTED + 10 + i)
        code, report = self.fx.execute()
        check = next(c for c in report["checks"] if c["id"] == "tasks.post_deploy_runs")
        line = next(l for l in self.fx.last_err.splitlines() if "tasks.post_deploy_runs" in l)
        return code, report, check, line

    def assertMismatch(self, runs, text="failed"):
        code, report, check, line = self.report_for(runs)
        self.assertEqual((code, report["exit_code"], report["result"]), (1, 1, "mismatch"))
        self.assertEqual(report["mismatched"], ["tasks.post_deploy_runs"])
        self.assertEqual(check["status"], "mismatch")
        self.assertIn(text, check["detail"])
        self.assertTrue(line.split()[0] == "mismatch", line)
        self.assertIn("release-verify 0.8.9: mismatch (exit 1)", self.fx.last_err)
        return report

    def assertUnverified(self, runs, text):
        code, report, check, line = self.report_for(runs)
        self.assertEqual((code, report["exit_code"], report["result"]), (3, 3, "unverified"))
        self.assertEqual((report["mismatched"], report["unverified"]), ([], ["tasks.post_deploy_runs"]))
        self.assertIn(text, check["detail"])
        self.assertTrue(line.split()[0] == "unverified", line)
        self.assertIn("release-verify 0.8.9: unverified (exit 3)", self.fx.last_err)

    def test_audit_repro_needs_verification_with_failed_count(self):
        report = self.assertMismatch([("needs_verification", final(failed=2, unknown=1))], "failed 1")
        run = report["evidence"]["server"]["runs_since_start"][0]
        self.assertEqual((run["class"], run["failed"], run["unknown"]), ("failed", 2, 1))

    def test_valid_failed_count_wins_over_missing_or_malformed_fields(self):
        for state in ("cancelled", "completed", "noop", "needs_verification", "running", "future_state"):
            for broken in ({"finalized": None}, {"unknown": None}, {"queued": None}, {"running": None},
                           {"unknown": "1"}, {"queued": -1}, {"finalized": "yes"}, {"running": 1, "finalized": False}):
                with self.subTest(state=state, broken=broken):
                    payload = final(failed=2)
                    for k, v in broken.items():
                        if v is None:
                            payload.pop(k)
                        else:
                            payload[k] = v
                    self.assertMismatch([(state, payload)])
        self.assertMismatch([("cancelled", {"failed": 2})])

    def test_failure_states_win_without_any_counts(self):
        for state in ("failed", "partial", "interrupted"):
            for payload in ({"finalized": False}, "{not json", final(failed="x")):
                with self.subTest(state=state, payload=payload):
                    self.assertMismatch([(state, payload)])

    def test_failure_among_pending_runs_and_missing_operations(self):
        self.fx.sql("live", "DROP TABLE operations")
        self.assertMismatch([("needs_verification", final(unknown=1)), ("noop", {"finalized": True}),
                             ("completed", final(failed=1, claimed=2, completed=1))])

    def test_invalid_failed_value_is_neither_failure_nor_zero(self):
        for bad in ("2", -1, 2.0, True, None, [2]):
            with self.subTest(failed=bad):
                payload = final()
                if bad is None:
                    payload.pop("failed")
                else:
                    payload["failed"] = bad
                self.assertUnverified([("noop", payload)], "missing_evidence 1")

    def test_no_definite_failure_stays_unverified(self):
        cases = [
            ([("needs_verification", final(unknown=1))], "needs_verification 1"),
            ([("needs_verification", {"finalized": True})], "needs_verification 1"),
            ([("noop", final(unknown=1))], "needs_verification 1"),
            ([("noop", {"finalized": True})], "missing_evidence 1"),
            ([("completed", final(finalized=False))], "not_finalized 1"),
            ([("future_state", final())], "unknown_state 1"),
            ([("running", final(finalized=False, running=1))], "running 1"),
            ([("noop", final(failed=0, unknown=0))], "noop 1"),  # with an operations failure below
        ]
        for runs, text in cases[:-1]:
            with self.subTest(runs=runs):
                self.assertUnverified(runs, text)
        self.fx.sql("live", "ALTER TABLE operations RENAME TO operations_raw")
        self.fx.sql("live", "CREATE VIEW operations AS SELECT id,state FROM operations_raw")
        self.assertUnverified(cases[-1][0], "operations_since_start not collected: sqlite error (OperationalError)")

    def test_normal_states_and_zero_values_still_pass(self):
        for state, payload, text in [("noop", final(), "noop 1"), ("observed", final(claimed=3), "observed 1"),
                                     ("completed", final(claimed=2, completed=2), "succeeded 1")]:
            with self.subTest(state=state):
                code, report, check, line = self.report_for([(state, payload)])
                self.assertEqual((code, report["result"], check["status"]), (0, "match", "match"))
                self.assertIn(text, check["detail"])
                self.assertIn("release-verify 0.8.9: match (exit 0)", self.fx.last_err)
                run = report["evidence"]["server"]["runs_since_start"][0]
                self.assertEqual([run[k] for k in rv.REQUIRED_COUNTS], [0, 0, 0, 0])

    def test_rv04_original_repros_unchanged(self):
        self.assertUnverified([("noop", {"finalized": True})], "missing_evidence 1")
        add_run(self.fx, "r9", "noop", final())
        self.fx.sql("live", "INSERT INTO operations VALUES('op-new','running',?,?)", STARTED + 20, STARTED + 20)
        self.fx.sql("live", "ALTER TABLE operations RENAME TO operations_raw")
        self.fx.sql("live", "CREATE VIEW operations AS SELECT id,state FROM operations_raw")
        code, report = self.fx.execute()
        self.assertEqual((code, report["result"]), (3, "unverified"))
        self.assertEqual(report["evidence"]["server"]["open_operations"], {"needs_verification": 1, "running": 1})
        self.assertIsNone(report["evidence"]["server"]["operations_since_start"])


if __name__ == "__main__":
    unittest.main()


class ManualKeepSchemaTest(unittest.TestCase):
    def test_only_documented_migration_is_accepted(self):
        for version, before, after, expected in [
            ("0.9.2", 13, 14, True), ("0.9.2", 14, 14, True),
            ("0.9.2", 13, 13, False), ("0.9.2", 12, 14, False),
            ("0.9.2", 14, 15, False), ("0.9.2", None, 14, False),
            ("0.9.1", 13, 13, True), ("0.9.1", 13, 14, False),
            ("0.10.0", 14, 15, True), ("0.10.0", 15, 15, True),
            ("0.10.0", 13, 15, False), ("0.10.0", 14, 14, False),
            ("0.10.0", 15, 16, False), ("0.10.0", None, 15, False),
            ("0.10.1", 14, 15, True), ("0.10.1", 15, 15, True),
            ("0.10.1", 13, 15, False), ("0.10.1", 14, 14, False),
            ("0.10.1", 15, 16, False), ("0.10.1", None, 15, False),
            ("0.10.2", 14, 15, True), ("0.10.2", 15, 15, True),
            ("0.10.2", 13, 15, False), ("0.10.2", 14, 14, False),
            ("0.10.2", 15, 16, False), ("0.10.2", None, 15, False),
            ("0.10.3", 14, 15, True), ("0.10.3", 15, 15, True),
            ("0.10.3", 13, 15, False), ("0.10.3", 14, 14, False),
            ("0.10.3", 15, 16, False), ("0.10.3", None, 15, False),
            ("0.10.4", 14, 15, True), ("0.10.4", 15, 15, True),
            ("0.10.4", 13, 15, False), ("0.10.4", 15, 16, False),
            ("0.10.5", 15, 16, True), ("0.10.5", 16, 16, True),
            ("0.10.5", 14, 16, False), ("0.10.5", 16, 17, False),
            ("0.10.6", 14, 17, True), ("0.10.6", 15, 17, True),
            ("0.10.6", 16, 17, True), ("0.10.6", 17, 17, True),
            ("0.10.6", 13, 17, False), ("0.10.6", 16, 16, False),
        ]:
            with self.subTest(version=version, before=before, after=after):
                self.assertEqual(rv.schema_matches(version, before, after), expected)
