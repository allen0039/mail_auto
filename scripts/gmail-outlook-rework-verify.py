#!/usr/bin/env python3
"""Run rework gates while preserving the original candidate and acceptance evidence."""

import importlib.util
import os

SPEC = importlib.util.spec_from_file_location("candidate_verify", os.path.join(os.path.dirname(__file__), "gmail-outlook-verify.py"))
VERIFY = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(VERIFY)
VERIFY.EVIDENCE = VERIFY.ROOT / ".planning/gmail-outlook-rework/validation"
VERIFY.SUMMARY = VERIFY.ROOT / "docs/gmail-outlook-rework-test-results.json"
VERIFY.COMMANDS = [(name, command) for name, command in VERIFY.COMMANDS if name != "multiarch"]
VERIFY.COMMANDS[0] = ("race", ["go", "test", "-race", "-count=1", "-json", "./..."])
VERIFY.COMMANDS.append(("oci", ["docker", "buildx", "build", "--builder", os.environ.get("MAIL_AUTO_TEST_BUILDER", "mail-auto-rework-validation"), "--platform", "linux/amd64,linux/arm64", "--output", "type=oci,dest=.planning/gmail-outlook-rework/candidate-image.tar", "."]))

if __name__ == "__main__":
    raise SystemExit(VERIFY.main())
