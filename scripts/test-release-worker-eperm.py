#!/usr/bin/env python3
"""Pure/mock controls for the opt-in untraced Linux EPERM fixture unit."""
import ast
import fcntl
import hashlib
import json
import os
from pathlib import Path
import tempfile
import time
import unittest
from unittest import mock

from fixtures import release_worker_eperm as eperm, release_worker_snapshot as snapshot, release_workers as workers
from fixtures import release_worker_groups


LOG = 'worker: start /bin/sh: child-start path="/bin/sh" errno=1 stage=unavailable: fork/exec /bin/sh: operation not permitted\n'
CLEAN_COMMAND = {"cleanup": {"reaped": True, "stdoutClosed": True}}


class RetryControls(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="mesh-eperm-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.binary = self.root / "selected-original"
        self.binary.write_bytes(b"original frozen image, never executed")
        self.digest = hashlib.sha256(self.binary.read_bytes()).hexdigest()
        self.path = self.root / "cohort.json"
        self.ledger = self.root / "recurrences.json"
        self.row = {"session": "W010", "pid": 0, "shellPid": 0, "digest": self.digest, "protocol": 1, "version": "v0.1.226"}
        self.record = {"schema": 1, "workers": [], "observations": []}
        self.arguments = [str(self.binary), "session-worker", "--id", "W010", "--", "/bin/sh"]
        self.environment = {"fixture": "same-environment"}
        self.item = {"version": "v0.1.226", "artifact": {"binarySha256": self.digest}}
        self.calls, self.deadlines = [], []
        self.errors = [RuntimeError("original startup EPERM"), None]
        self.failure_log = LOG
        self.addCleanup(mock.patch.stopall)
        mock.patch.dict(os.environ, MESH_WORKER_EPERM_LEDGER=str(self.ledger)).start()
        mock.patch.object(workers, "spawn_selected", side_effect=self.spawn).start()
        mock.patch.object(workers, "wait_startup", side_effect=self.wait).start()
        self.diagnostic = mock.patch.object(snapshot, "capture", return_value={"cleanupConfirmed": True,
            "kernelJournal": {"unavailable": "permission denied"}}).start()

    def spawn(self, arguments, environment, directory, row):
        if self.calls:
            retained = json.loads(self.path.read_bytes())["workers"][0]["startupAttempts"][0]
            self.assertEqual(retained["exception"]["message"], "original startup EPERM")
            self.assertTrue(Path(retained["retainedLog"]["path"]).exists())
        self.calls.append((arguments, environment))
        (directory / "worker.log").write_text(self.failure_log)
        process = mock.Mock(pid=100 + len(self.calls))
        process.poll.return_value = 1
        row["pid"] = process.pid
        row.pop("startupReconciled", None)
        return process

    def wait(self, directory, process, item, record, row, path, deadline):
        self.deadlines.append(deadline)
        error = self.errors.pop(0)
        if error is None:
            (directory / "meta.json").write_text('{"pid":999}')
            return
        row["startupFailure"] = {"pid": process.pid, "workerLogTail": self.failure_log,
                                 "workerAtObservation": {"unavailable": "fast exited"},
                                 "mappedImage": {"unavailable": "fast exited"}, "failingCall": "unavailable"}
        if isinstance(error, ValueError):
            row["startupFailure"]["workerLogTail"] = "unrelated startup error"
        workers.save(path, record)
        raise error

    def launch(self):
        with eperm.ledger() as ledger:
            workers.launch_selected(self.binary, self.arguments, self.environment, self.root,
                                    self.item, self.record, self.row, self.path, ledger)

    def test_successful_retry_keeps_original_failure_and_same_image_arguments_environment_deadline(self):
        self.launch()
        self.assertEqual(len(self.calls), 2)
        self.assertIs(self.calls[0][0], self.calls[1][0])
        self.assertIs(self.calls[0][1], self.calls[1][1])
        self.assertEqual(self.deadlines[0], self.deadlines[1])
        self.assertEqual(self.row["session"], "W010")
        self.assertNotIn("startupFailure", self.row)
        self.assertEqual([row["outcome"] for row in self.row["startupAttempts"]], ["EPERM", "started"])
        retained = json.loads(self.ledger.read_bytes())
        self.assertEqual(len(retained["events"]), 1)
        self.assertEqual(retained["events"][0]["failure"]["pid"], 101)
        self.assertEqual(retained["events"][0]["retry"]["pid"], 102)
        self.assertEqual(self.record["epermRecurrenceCount"], 1)
        self.assertEqual(Path(retained["events"][0]["failure"]["retainedLog"]["path"]).read_text(), LOG)
        self.assertIn("unavailable", retained["events"][0]["failure"]["startupFailure"]["workerAtObservation"])

    def test_second_recurrence_stops_before_third_spawn_and_future_cohorts_share_cap(self):
        self.errors = [RuntimeError("original startup EPERM"), RuntimeError("second startup EPERM")]
        with self.assertRaisesRegex(RuntimeError, "cap reached"):
            self.launch()
        self.assertEqual(len(self.calls), 2)
        self.assertEqual(len(json.loads(self.ledger.read_bytes())["events"]), 2)
        self.assertEqual(self.record["epermRecurrenceCount"], 2)
        with mock.patch.object(workers, "start_selected") as launch:
            with self.assertRaisesRegex(RuntimeError, "cap reached"):
                workers.start_one(None, None, None, None, None, None, None)
        launch.assert_not_called()

    def test_second_cohort_event_stops_without_retry_and_does_not_reset_ledger(self):
        self.launch()
        self.calls.clear()
        self.errors = [RuntimeError("original startup EPERM")]
        self.row = dict(self.row, pid=0, shellPid=0)
        self.row.pop("startupAttempts")
        self.record = {"schema": 1, "workers": [], "observations": []}
        with self.assertRaisesRegex(RuntimeError, "cap reached"):
            self.launch()
        self.assertEqual(len(self.calls), 1)
        self.assertEqual(len(json.loads(self.ledger.read_bytes())["events"]), 2)

    def test_non_eperm_exception_is_unchanged_without_snapshot_or_retry(self):
        self.failure_log = LOG.replace("errno=1", "errno=13")
        original = RuntimeError("different startup error")
        self.errors = [original]
        with self.assertRaises(RuntimeError) as raised:
            self.launch()
        self.assertIs(raised.exception, original)
        self.assertEqual(len(self.calls), 1)
        self.diagnostic.assert_not_called()
        self.assertFalse(self.ledger.exists())

    def test_disabled_unit_preserves_failure_without_retry(self):
        with mock.patch.dict(os.environ, MESH_WORKER_EPERM_LEDGER=""):
            with self.assertRaisesRegex(RuntimeError, "original startup EPERM"):
                self.launch()
        self.assertEqual(len(self.calls), 1)
        self.diagnostic.assert_not_called()

    def test_unknown_snapshot_cleanup_preserves_failure_and_refuses_retry(self):
        self.diagnostic.side_effect = PermissionError(13, "kernel context denied")
        original = self.errors[0]
        with self.assertRaises(RuntimeError) as caught:
            self.launch()
        self.assertIs(caught.exception, original)
        self.assertEqual(len(self.calls), 1)
        failure = json.loads(self.ledger.read_bytes())["events"][0]["failure"]
        self.assertEqual(failure["snapshot"], {"unavailable": "PermissionError", "errno": 13, "cleanupConfirmed": False})
        self.assertIn("unconfirmed", caught.exception.__notes__[0])

    def test_ledger_contention_refuses_before_any_worker_spawn(self):
        with mock.patch("fcntl.flock", side_effect=BlockingIOError()) as lock, mock.patch.object(workers, "start_selected") as launch:
            with self.assertRaisesRegex(RuntimeError, "ledger is busy"):
                workers.start_one(None, None, {}, 0, "host", {}, self.path)
        lock.assert_called_once_with(mock.ANY, fcntl.LOCK_EX | fcntl.LOCK_NB)
        launch.assert_not_called()

    def test_unreaped_snapshot_reader_refuses_retry_and_retains_its_pid(self):
        self.diagnostic.return_value = {"cleanupConfirmed": False, "kernelJournal": {
            "output": "partial denial", "cleanup": {"pid": 2468, "reaped": False, "stdoutClosed": True}}}
        original = self.errors[0]
        with self.assertRaises(RuntimeError) as caught:
            self.launch()
        self.assertIs(caught.exception, original)
        self.assertEqual(len(self.calls), 1)
        failure = json.loads(self.ledger.read_bytes())["events"][0]["failure"]
        self.assertEqual(failure["snapshot"]["kernelJournal"]["cleanup"]["pid"], 2468)

    def test_changed_original_image_refuses_retry_after_retaining_failure(self):
        self.diagnostic.side_effect = self.change_image
        with self.assertRaisesRegex(RuntimeError, "image changed"):
            self.launch()
        self.assertEqual(len(self.calls), 1)
        self.assertEqual(len(json.loads(self.ledger.read_bytes())["events"]), 1)

    def change_image(self, *_):
        self.binary.write_bytes(b"changed")
        return {"cleanupConfirmed": True}

    def test_deadline_exhaustion_after_capture_refuses_retry(self):
        self.diagnostic.side_effect = self.exhaust_deadline
        with self.assertRaisesRegex(RuntimeError, "deadline exhausted"):
            self.launch()
        self.assertEqual(len(self.calls), 1)

    def exhaust_deadline(self, *_):
        mock.patch.object(workers.time, "monotonic", return_value=1e20).start()
        return {"unavailable": "deadline exhausted control", "cleanupConfirmed": True}

    def test_non_eperm_retry_error_remains_original_and_visible(self):
        original = ValueError("retry different error")
        self.errors[1] = original
        with self.assertRaises(ValueError) as raised:
            self.launch()
        self.assertIs(raised.exception, original)
        self.assertEqual(len(self.calls), 2)
        self.assertEqual(json.loads(self.ledger.read_bytes())["events"][0]["retry"]["outcome"], "failed")

    def test_retry_observation_keeps_schema_one_and_exact_group_coverage(self):
        self.launch()
        self.row["shellPid"] = 999
        source = {"digest": "f" * 64, "protocol": 1}
        original = {"session": "ORIGINAL", "pid": 500, "shellPid": 501, "protocol": 1,
                    "digest": source["digest"], "stateRun": "original-state"}
        identities = [{key: self.row[key] for key in ("session", "pid", "shellPid", "digest", "protocol", "version")}]
        self.record["observations"] = [{"label": "checkpoint", "workers": 1, "processes": 2,
                                        "identities": identities, "original": original}]
        group = {"members": [dict(self.item, protocol=1)]}
        self.assertEqual(release_worker_groups.validate_observations(self.record, group, ["checkpoint"], source), original)
        self.assertEqual(self.record["schema"], 1)
        self.assertEqual(self.record["workers"][0]["startupAttempts"][0]["outcome"], "EPERM")

    def test_failure_to_retain_original_log_never_spawns_retry(self):
        with mock.patch.object(eperm, "retain_log", side_effect=PermissionError(13, "retain denied")):
            with self.assertRaises(PermissionError):
                self.launch()
        self.assertEqual(len(self.calls), 1)
        self.assertEqual(len(json.loads(self.ledger.read_bytes())["events"]), 1)


class SnapshotControls(unittest.TestCase):
    def test_kernel_commands_have_window_caps_and_partial_permissions(self):
        with mock.patch.object(snapshot, "command", return_value=CLEAN_COMMAND | {"returncode": 1, "output": "permission denied"}) as command, mock.patch.object(snapshot, "read_file", side_effect=PermissionError(13, "denied")), mock.patch.object(snapshot, "mounts", side_effect=FileNotFoundError(2, "missing")), mock.patch.dict(os.environ, {}, clear=True):
            result = snapshot.capture({}, "/fixture cwd", time.monotonic() + 6)
        self.assertEqual(command.call_count, 2)
        self.assertIn("--until", command.call_args_list[0].args[0])
        self.assertEqual(result["windowUnixSeconds"]["until"] - result["windowUnixSeconds"]["since"], 60)
        self.assertEqual(result["sysctls"]["kernel/apparmor_restrict_unprivileged_userns"]["unavailable"], "PermissionError")
        self.assertIn("unavailable", result["shellChild"])
        self.assertTrue(result["cleanupConfirmed"])

    def test_command_stops_at_exact_output_byte_cap(self):
        process = mock.Mock()
        process.poll.return_value = None
        with mock.patch.object(snapshot.select, "select", return_value=([process.stdout], [], [])), mock.patch.object(snapshot.os, "read", side_effect=lambda _, size: b"x" * size):
            result = snapshot.read_command(process, ["journalctl"], time.monotonic() + 1)
        self.assertEqual(result["bytes"], snapshot.OUTPUT_LIMIT)
        self.assertEqual(len(result["output"]), snapshot.OUTPUT_LIMIT)
        self.assertTrue(result["byteLimitReached"])

    def test_command_always_kills_and_reaps_partial_reader(self):
        process = mock.Mock()
        process.poll.return_value = None
        with mock.patch.object(snapshot.subprocess, "Popen", return_value=process), mock.patch.object(snapshot, "read_command", return_value={"incomplete": True}):
            self.assertTrue(snapshot.command(["journalctl"], time.monotonic() + 1)["incomplete"])
        process.kill.assert_called_once()
        process.wait.assert_called_once()
        process.stdout.close.assert_called_once()

    def test_no_command_spawn_after_deadline(self):
        with mock.patch.object(snapshot.subprocess, "Popen") as spawn:
            self.assertIn("unavailable", snapshot.command(["journalctl"], time.monotonic() - 1))
        spawn.assert_not_called()

    def test_timed_out_reader_closes_pipe_and_keeps_partial_output_within_deadline(self):
        process = mock.Mock(pid=2468, returncode=None)
        process.poll.return_value = None
        process.wait.side_effect = snapshot.subprocess.TimeoutExpired(["journalctl"], 0.01)
        with mock.patch.object(snapshot.subprocess, "Popen", return_value=process), \
                mock.patch.object(snapshot, "read_command", return_value={"output": "partial denial", "bytes": 14}), \
                mock.patch.object(snapshot.time, "monotonic", return_value=0):
            result = snapshot.command(["journalctl"], 0.01)
        self.assertEqual(result["output"], "partial denial")
        self.assertLessEqual(process.wait.call_args.kwargs["timeout"], 0.01)
        process.stdout.close.assert_called_once()
        self.assertFalse(result["cleanup"]["reaped"])
        self.assertEqual(result["cleanup"]["pid"], 2468)

    def test_failed_fallback_retains_prior_snapshot_and_stops_command_expansion(self):
        journal = CLEAN_COMMAND | {"returncode": 1, "output": "permission denied"}
        fallback = {"output": "partial audit", "cleanup": {"reaped": False, "stdoutClosed": True, "pid": 2468}}
        with mock.patch.object(snapshot, "command", side_effect=[journal, fallback]) as command, \
                mock.patch.object(snapshot, "read_file", return_value="1"), \
                mock.patch.object(snapshot, "mounts", return_value=[{"options": "nosuid"}]), \
                mock.patch.dict(os.environ, MESH_WORKER_EPERM_SUDO="1", GITHUB_ACTIONS="true"):
            result = snapshot.capture({}, "/fixture", time.monotonic() + 1)
        self.assertEqual(command.call_count, 2)
        self.assertEqual(result["kernelJournal"], journal)
        self.assertEqual(result["kernelDmesg"], fallback)
        self.assertEqual(result["sysctls"]["kernel/apparmor_restrict_unprivileged_userns"], "1")
        self.assertEqual(result["mounts"], [{"options": "nosuid"}])
        self.assertFalse(result["cleanupConfirmed"])

    def test_sysctl_file_and_retained_log_byte_bounds(self):
        with tempfile.TemporaryDirectory(prefix="eperm-bytes-") as directory:
            source, retained = Path(directory) / "source", Path(directory) / "retained"
            source.write_bytes(b"x" * 100000)
            with self.assertRaisesRegex(ValueError, "byte bound"):
                snapshot.read_file(source, 256)
            result = eperm.retain_log(source, retained)
            self.assertEqual(retained.stat().st_size, 65536)
            self.assertTrue(result["byteLimitReached"])

    def test_gone_worker_is_reaped_without_a_pid_lookup_or_signal(self):
        process = mock.Mock()
        process.poll.return_value = 1
        with mock.patch.object(eperm.evidence, "Startup") as lookup, mock.patch.object(eperm.os, "kill") as kill:
            self.assertEqual(eperm.reconcile(process, {}), {"worker": "reaped", "shell": "no published child PID"})
        lookup.assert_not_called()
        kill.assert_not_called()

    def test_live_worker_with_changed_birth_is_not_signalled(self):
        process = mock.Mock(pid=42)
        process.poll.return_value = None
        observed = mock.MagicMock()
        observed.__enter__.return_value.before = {"stat": {"startTicks": 200}}
        with mock.patch.object(eperm.evidence, "Startup", return_value=observed), mock.patch.object(eperm.os, "kill") as kill:
            with self.assertRaisesRegex(RuntimeError, "birth identity"):
                eperm.reconcile(process, {"workerAtObservation": {"stat": {"startTicks": 100}}})
        kill.assert_not_called()

    def test_exact_failure_match_excludes_other_shells_and_errno_prefixes(self):
        for log in (LOG.replace("/bin/sh", "/bin/bash"), LOG.replace("errno=1 ", "errno=13 "), "fork/exec /bin/sh: operation not permitted"):
            self.assertFalse(eperm.eligible({"shellPid": 0, "startupFailure": {"workerLogTail": log}}))

    def test_namespace_comparison_keeps_unknown_and_changed_identities_distinct(self):
        failure = {"launcherAtLaunch": {"namespaces": {"user": "user:1", "mnt": "mnt:1"}},
                   "workerAtObservation": {"namespaces": {"user": "user:1", "mnt": "mnt:2"}},
                   "workerAtFailure": {"unavailable": "exited"}}
        result = snapshot.namespace_comparison(failure)
        self.assertTrue(result["workerAtObservation"]["user"]["same"])
        self.assertFalse(result["workerAtObservation"]["mnt"]["same"])
        self.assertIsNone(result["workerAtFailure"]["mnt"]["same"])

    def test_mounts_select_actual_shell_and_escaped_working_directory_options(self):
        lines = ["1 0 1:1 / / rw - ext4 /dev/root rw",
                 r"2 1 1:2 / /fixture\040cwd rw,noexec,nosuid - ext4 /dev/work rw",
                 "3 1 1:3 / /usr ro - ext4 /dev/usr ro"]
        with mock.patch.object(snapshot.os.path, "realpath", side_effect=lambda value: "/usr/bin/dash" if value == "/bin/sh" else value):
            shell = snapshot.mount_for("/bin/sh", lines)
            cwd = snapshot.mount_for("/fixture cwd", lines)
        self.assertEqual(shell["actualPath"], "/usr/bin/dash")
        self.assertEqual(shell["options"], "ro")
        self.assertEqual(cwd["options"], "rw,noexec,nosuid")

    def test_sudo_is_read_only_explicit_and_hosted_only(self):
        with mock.patch.object(snapshot, "command", return_value=CLEAN_COMMAND | {"returncode": 1, "bytes": 0}) as command, mock.patch.object(snapshot, "read_file", return_value="0"), mock.patch.object(snapshot, "mounts", return_value=[]), mock.patch.dict(os.environ, {"MESH_WORKER_EPERM_SUDO": "1", "GITHUB_ACTIONS": "true"}):
            snapshot.capture({}, "/fixture", time.monotonic() + 6)
        self.assertEqual(command.call_args.args[0][:4], ["sudo", "-n", "journalctl", "-k"])
        with mock.patch.object(snapshot, "command", return_value=CLEAN_COMMAND | {"returncode": 0, "bytes": 10}) as command, mock.patch.object(snapshot, "read_file", return_value="0"), mock.patch.object(snapshot, "mounts", return_value=[]), mock.patch.dict(os.environ, {"MESH_WORKER_EPERM_SUDO": "1", "GITHUB_ACTIONS": "false"}):
            snapshot.capture({}, "/fixture", time.monotonic() + 6)
        self.assertEqual(command.call_count, 1)
        with mock.patch.object(snapshot, "command", return_value=CLEAN_COMMAND | {"returncode": 0, "bytes": 10}) as command, mock.patch.object(snapshot, "read_file", return_value="0"), mock.patch.object(snapshot, "mounts", return_value=[]), mock.patch.dict(os.environ, {"MESH_WORKER_EPERM_SUDO": "1", "GITHUB_ACTIONS": "true"}):
            snapshot.capture({}, "/fixture", time.monotonic() + 6)
        self.assertEqual(command.call_count, 1)

    def test_new_blocks_stay_within_three_levels(self):
        for path in (Path(eperm.__file__), Path(snapshot.__file__), Path(workers.__file__), Path(__file__)):
            self.assertLessEqual(depth(ast.parse(path.read_text())), 3, str(path))


def depth(node, nesting=0):
    if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)):
        nesting = 1
    elif isinstance(node, (ast.If, ast.For, ast.While, ast.With, ast.Try)):
        nesting += 1
    return max((depth(child, nesting) for child in ast.iter_child_nodes(node)), default=nesting)


if __name__ == "__main__":
    unittest.main()
