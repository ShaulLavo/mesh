#!/usr/bin/env python3
"""Exercise the scope integration's real decision block without starting a session."""

import json
import os
from pathlib import Path
import shlex
import subprocess
import sys
import tempfile
import unittest

from session_scope import unit_state


HELPERS = Path(__file__).resolve().parent
UNIT = "mesh-session-ORACLE.scope"


def unit_reply(load="loaded", active="active", unit=UNIT):
    record = [unit, unit, load, active, "running", "", "/unit/oracle", 0, "", "/"]
    return {"type": "a(ssssssouso)", "data": [[record]]}


class ScopeOracleTest(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="m-scope-oracle-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.tools = self.root / "bin"
        self.tools.mkdir()
        for name in ("home", "runtime", "config", "state"):
            (self.root / name).mkdir()
        self.calls = self.root / "calls"
        self.steps = self.root / "steps"
        self.killed = self.root / "killed"
        self.tool("systemctl", "#!/bin/sh\n"
                  "echo 'Failed to connect to user scope bus via local transport: No such file or directory' >&2\n"
                  "exit 1\n")
        self.tool("busctl", f"#!{sys.executable}\n" +
                  "import json, os, pathlib, sys\n"
                  "if sys.argv[1:] == ['--user', 'status']: sys.exit(1)\n"
                  "root = pathlib.Path(os.environ['ORACLE_ROOT'])\n"
                  "with (root / 'calls').open('a') as out: out.write(json.dumps(sys.argv[1:]) + '\\n')\n"
                  "steps = json.loads((root / 'steps').read_text())\n"
                  "step = steps.pop(0) if len(steps) > 1 else steps[0]\n"
                  "(root / 'steps').write_text(json.dumps(steps))\n"
                  "if isinstance(step, dict): print(json.dumps(step)); sys.exit(0)\n"
                  "print('Failed to connect to bus: No such file or directory', file=sys.stderr)\n"
                  "sys.exit(step)\n")

    def tool(self, name, content):
        path = self.tools / name
        path.write_text(content)
        path.chmod(0o700)

    def observe(self, steps):
        self.steps.write_text(json.dumps(steps))
        entry = (HELPERS.parent / "session_scope.sh").read_text()
        cgroup_check = next(line for line in entry.splitlines() if line.startswith("grep -q "))
        start = entry.index(cgroup_check) + len(cgroup_check)
        block = entry[start:entry.index('SID=""', start)]
        command = ("set -uo pipefail\nfail() { echo \"FAIL: $*\" >&2; exit 1; }\n"
                   f"mesh() {{ printf killed >> {shlex.quote(str(self.killed))}; }}\n"
                   "sleep() { :; }\n" + block + "\necho ACCEPTED\n")
        environment = {"PATH": str(self.tools) + os.pathsep + os.defpath, "SID": "ORACLE", "MESH": "mesh",
                       "HOME": str(self.root / "home"), "XDG_RUNTIME_DIR": str(self.root / "runtime"),
                       "MESH_CONFIG_DIR": str(self.root / "config"), "MESH_STATE_DIR": str(self.root / "state"),
                       "DBUS_SESSION_BUS_ADDRESS": "unix:path=/fixture-user-bus", "ORACLE_ROOT": str(self.root),
                       "scope_observer": str(HELPERS / "session_scope.py"), "PYTHONDONTWRITEBYTECODE": "1"}
        return subprocess.run(["bash", "-c", command], env=environment, capture_output=True, text=True, timeout=15)

    def assert_rejected(self, steps, killed):
        result = self.observe(steps)
        self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertNotIn("ACCEPTED", result.stdout)
        self.assertEqual(self.killed.exists(), killed, result.stdout + result.stderr)
        return result

    def test_active_then_inactive_or_not_found_passes(self):
        for load, active in (("loaded", "inactive"), ("not-found", "inactive")):
            with self.subTest(load=load):
                result = self.observe([unit_reply(), unit_reply(load, active)])
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertIn("ACCEPTED", result.stdout)
                self.assertTrue(self.killed.exists())
                calls = [json.loads(line) for line in self.calls.read_text().splitlines()]
                expected = ["--address=unix:path=/fixture-user-bus", "--json=short", "call",
                            "org.freedesktop.systemd1", "/org/freedesktop/systemd1",
                            "org.freedesktop.systemd1.Manager", "ListUnitsByNames", "as", "1", UNIT]
                self.assertTrue(all(call == expected for call in calls), calls)

    def test_unavailable_queries_cannot_pass(self):
        result = self.observe([1])
        self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertNotIn("ACCEPTED", result.stdout)
        self.assertFalse(self.killed.exists(), "kill ran before a positive active observation")

    def test_query_failure_after_kill_cannot_pass(self):
        result = self.assert_rejected([unit_reply(), 1], killed=True)
        self.assertIn("could not observe scope after kill", result.stderr)

    def test_final_query_failure_cannot_pass(self):
        result = self.assert_rejected([unit_reply(), unit_reply("loaded", "inactive"), 1], killed=True)
        self.assertIn("could not confirm scope exit", result.stderr)

    def test_inactive_before_kill_cannot_pass(self):
        self.assert_rejected([unit_reply("loaded", "inactive")], killed=False)

    def test_missing_before_kill_cannot_pass(self):
        self.assert_rejected([unit_reply("not-found", "inactive")], killed=False)

    def test_malformed_successful_query_cannot_pass(self):
        self.assert_rejected([unit_reply(), {"type": "unexpected", "data": []}], killed=True)

    def test_deactivating_is_polled_until_inactive(self):
        result = self.observe([unit_reply(), unit_reply("loaded", "deactivating"), unit_reply("loaded", "inactive")])
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(len(self.calls.read_text().splitlines()), 4)

    def test_active_scope_retains_original_poll_limit(self):
        result = self.assert_rejected([unit_reply()], killed=True)
        self.assertIn("scope outlived the session", result.stderr)
        self.assertEqual(len(self.calls.read_text().splitlines()), 102)

    def test_unavailable_manager_is_explicit_before_launch(self):
        entry = (HELPERS.parent / "session_scope.sh").read_text()
        start = entry.index('if [ "$(uname -s)"')
        block = entry[start:entry.index('if [ -z "${MESH:-}"', start)]
        result = subprocess.run(["bash", "-c", block], capture_output=True, text=True,
                                env={"PATH": str(self.tools) + os.pathsep + os.defpath}, timeout=5)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("PASS: no systemd user manager; sessions keep their launcher's cgroup", result.stdout)
        self.assertFalse(self.killed.exists())


class UnitReplyTest(unittest.TestCase):
    def test_unexpected_replies_are_query_errors(self):
        replies = [None, {}, {"type": "unexpected"}, {"type": "a(ssssssouso)", "data": []},
                   {"type": "a(ssssssouso)", "data": [[]]},
                   {"type": "a(ssssssouso)", "data": [[[]]]}, unit_reply(unit="different.scope"),
                   unit_reply("not-found", "active"), unit_reply("error", "inactive"),
                   unit_reply("loaded", "failed"), unit_reply(active=[]), unit_reply(load=None)]
        for reply in replies:
            with self.subTest(reply=reply), self.assertRaises(ValueError):
                unit_state(reply, UNIT)


if __name__ == "__main__":
    unittest.main()
