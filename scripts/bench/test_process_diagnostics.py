"""A real self-killed child must leave bounded, explicitly scoped diagnostics."""
import contextlib
import io
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
import time
import unittest
from unittest.mock import patch

import run as bench


@unittest.skipUnless(os.name == "posix" and hasattr(os, "wait4"), "POSIX child signal status")
class ProcessDiagnosticsTests(unittest.TestCase):
    def test_self_sigkill_context_and_known_harness_send(self):
        messages = io.StringIO()
        with tempfile.TemporaryDirectory() as root, contextlib.redirect_stderr(messages):
            program = "import os,signal,time; time.sleep(.15); os.kill(os.getpid(),signal.SIGKILL)"
            with self.assertRaises(RuntimeError):
                bench.timed_command(Path(sys.executable), ["-c", program], dict(os.environ), root)
            records = [json.loads(line) for line in messages.getvalue().splitlines()]
            deaths = [row for row in records if row["event"] == "bench.child_sigkill"]
            self.assertEqual(len(deaths), 1, messages.getvalue())
            death = deaths[0]
            self.assertEqual(death["matching_owner_signals"], [])
            self.assertEqual(death["attribution"], "unexplained")
            self.assertIn(death["last_observed_rss"]["status"], ("observed", "unknown", "unsupported"))
            if death["last_observed_rss"]["status"] == "observed":
                self.assertGreater(death["last_observed_rss"]["bytes"], 0)
            else:
                self.assertIsNone(death["last_observed_rss"]["bytes"])
            self.assertEqual(death["memory_events"]["attribution"], "cgroup_aggregate_context_only")
            self.assertEqual(len(death["journal"]), 2)
            for entry in death["journal"]:
                self.assertIn(entry["status"], ("available", "no_matching_events", "unavailable", "no_access", "unsupported", "timeout"))
                self.assertEqual(entry["attribution"], "context_only")
                self.assertLessEqual(len(entry.get("events", [])), 16)
                for event in entry.get("events", []):
                    self.assertLessEqual(len(event.get("MESSAGE", "")), 512)

            from process_diagnostics import ChildObservation
            child = subprocess.Popen([sys.executable, "-c", "import time; print('ready',flush=True); time.sleep(30)"],
                                     stdout=subprocess.PIPE, text=True, start_new_session=True)
            observation = ChildObservation(child.pid, "regression-owned-child")
            actual_killpg = os.killpg

            def deliver(pid, sig):
                before = [json.loads(line) for line in messages.getvalue().splitlines()]
                sends = [row for row in before if row["event"] == "bench.signal_send" and row["target_pid"] == child.pid]
                self.assertEqual(sends[-1]["signal"], signal.SIGKILL)
                self.assertEqual(sends[-1]["reason"], "regression-owned-cleanup")
                self.assertEqual(sends[-1]["ownership"]["kind"], "popen_process_group")
                self.assertIn("status", sends[-1]["incarnation"])
                actual_killpg(pid, sig)

            try:
                self.assertEqual(child.stdout.readline().strip(), "ready")
                with patch.object(os, "killpg", deliver):
                    observation.send_group(signal.SIGKILL, "regression-owned-cleanup")
                self.assertEqual(child.wait(timeout=5), -signal.SIGKILL)
                observation.finish(child.returncode)
                records = [json.loads(line) for line in messages.getvalue().splitlines()]
                known = [row for row in records if row["event"] == "bench.child_sigkill" and row["target_pid"] == child.pid][0]
                self.assertEqual(known["attribution"], "owner_signal_recorded")
                self.assertEqual(known["matching_owner_signals"], ["regression-owned-cleanup"])
            finally:
                if child.poll() is None:
                    observation.send_group(signal.SIGKILL, "regression-owned-rescue")
                    child.wait(timeout=5)
                observation.close()
                child.stdout.close()

            from process_diagnostics import journal_context
            wording = "Killed /user.slice/sample.scope due to memory pressure for /user.slice being 80.00% > 60.00%"
            journal = Path(root) / "journalctl"
            journal.write_text(f"#!{sys.executable}\nimport json,re,sys\nmessage={wording!r}\n"
                               "if '--grep' in sys.argv and not re.search(sys.argv[sys.argv.index('--grep')+1],message):\n"
                               " raise SystemExit(1)\n"
                               "print(json.dumps({'MESSAGE':message,'_SYSTEMD_UNIT':'systemd-oomd.service'}))\n")
            journal.chmod(0o700)
            with patch.dict(os.environ, {"PATH": root}):
                context = journal_context("systemd-oomd", time.time() - 1, time.time())
            self.assertEqual(context["status"], "available")
            self.assertEqual(context["events"][0]["MESSAGE"], wording)
            self.assertEqual(context["attribution"], "context_only")
            messages.write(json.dumps({"event": "regression.oomd_known_positive", **context}) + "\n")
        print(messages.getvalue(), file=sys.stderr, end="")


if __name__ == "__main__":
    unittest.main()
