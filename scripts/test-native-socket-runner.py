#!/usr/bin/env python3
"""Cheap partition, protocol, process-cleanup and actual Go test-binary controls."""
import asyncio
from collections import Counter
import contextlib
import io
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import AsyncMock, call, patch

import native_socket_runner as runner


class PartitionTest(unittest.TestCase):
    def test_every_current_and_future_entry_is_selected_once(self):
        names = [f"TestFuture{index}" for index in range(103)] + ["ExampleSocket", "FuzzSocket"]
        groups = runner.partition(names, 4)
        self.assertEqual(Counter(name for group in groups for name in group), Counter(names))
        self.assertEqual(groups, runner.partition(list(reversed(names)), 4))
        self.assertLessEqual(max(map(len, groups)) - min(map(len, groups)), 1)
        self.assertEqual(runner.partition([], 4), [[]])
        with self.assertRaisesRegex(ValueError, "duplicate"):
            runner.partition(["TestOne", "TestOne"], 4)

    def test_inventory_includes_examples_and_fuzz_seeds_but_not_benchmarks(self):
        self.assertEqual(runner.inventory(b"TestOne\nExampleOne\nFuzzOne\nBenchmarkOne\n"),
                         ["TestOne", "ExampleOne", "FuzzOne"])
        with self.assertRaisesRegex(ValueError, "unexpected"):
            runner.inventory(b"unrecognized output\n")

    def test_partitions_own_default_state_and_reuse_go_caches(self):
        with tempfile.TemporaryDirectory(prefix="native-env-") as directory:
            root = Path(directory)
            original = {"HOME": "/outside", "MESH_CONFIG_DIR": "/outside/config", "GOCACHE": "/cache"}
            first = runner.isolated_environment(original, root / "one")
            second = runner.isolated_environment(original, root / "two")
        self.assertNotEqual(first["MESH_STATE_DIR"], second["MESH_STATE_DIR"])
        self.assertNotEqual(first["MESH_CONFIG_DIR"], original["MESH_CONFIG_DIR"])
        self.assertEqual(first["GOCACHE"], original["GOCACHE"])
        self.assertEqual(original["HOME"], "/outside")

    def test_coverage_rejects_missing_duplicate_unselected_and_failed_cases(self):
        events = [{"Test": "TestOne", "Action": "run"}, {"Test": "TestOne", "Action": "pass"}]
        runner.check_coverage(["TestOne"], events)
        runner.check_coverage(["TestOne"], [events[0], {"Test": "TestOne", "Action": "skip"}])
        bad = (events[:1], events + events, events + [{"Test": "TestOther", "Action": "run"}],
               [events[0], {"Test": "TestOne", "Action": "fail"}])
        for entries in bad:
            with self.subTest(entries=entries), self.assertRaises(ValueError):
                runner.check_coverage(["TestOne"], entries)

    def test_workflow_keeps_native_matrix_bounds_artifacts_and_blocking_runner(self):
        workflow = (Path(__file__).resolve().parents[1] / ".github/workflows/ci.yml").read_text()
        job = workflow.split("  socket-portability:\n", 1)[1].split("\n  auth-cutover:", 1)[0]
        for contract in ("timeout-minutes: 10", "runner: [ubuntu-24.04, macos-15]",
                         'python3 scripts/native_socket_runner.py --output "$RUNNER_TEMP/native-socket-results.jsonl"',
                         "python3 scripts/test-native-socket-runner.py", "native-socket-results.schedule.json"):
            self.assertIn(contract, job)
        self.assertNotIn("continue-on-error", job)
        self.assertEqual(runner.ALARM, "350s")
        self.assertEqual(runner.PACKAGES, ("daemon", "cli", "worker", "updatebootstrap", "paths", "testenv"))


class ProcessTest(unittest.IsolatedAsyncioTestCase):
    async def test_term_denial_accepts_only_a_disappeared_group(self):
        process = AsyncMock(pid=12345)
        denied = PermissionError(1, "Operation not permitted")
        with patch.object(runner.os, "killpg", side_effect=[denied, ProcessLookupError()]) as killpg, \
                patch.object(runner.asyncio, "sleep", AsyncMock()) as sleep:
            await runner.stop_group(process)
        self.assertEqual(killpg.call_args_list, [call(process.pid, signal.SIGTERM), call(process.pid, 0)])
        sleep.assert_awaited_once_with(0.1)
        process.wait.assert_awaited_once()

    async def test_term_denial_keeps_existing_and_inaccessible_groups_blocking(self):
        for probe in (None, PermissionError(1, "Group remains inaccessible")):
            await self.check_term_denial(probe)

    async def check_term_denial(self, probe):
        process = AsyncMock(pid=12345)
        denied = PermissionError(1, "Operation not permitted")
        with patch.object(runner.os, "killpg", side_effect=[denied, probe]) as killpg, \
                patch.object(runner.asyncio, "sleep", AsyncMock()) as sleep, \
                self.assertRaises(PermissionError):
            await runner.stop_group(process)
        self.assertEqual(killpg.call_args_list, [call(process.pid, signal.SIGTERM), call(process.pid, 0)])
        sleep.assert_awaited_once_with(0.1)
        process.wait.assert_not_awaited()

    async def test_protocol_failure_propagates_after_denied_group_disappears(self):
        with patch.object(runner.os, "killpg", side_effect=[PermissionError(), ProcessLookupError()]), \
                self.assertRaises(json.JSONDecodeError):
            await runner.command((sys.executable, "-c", "print('bad JSON')"), Path.cwd(), os.environ,
                                 json.loads)

    async def test_kill_denial_remains_blocking(self):
        process = AsyncMock(pid=12345)
        denied = PermissionError(1, "Operation not permitted")
        with patch.object(runner.os, "killpg", side_effect=[None, denied]), \
                patch.object(runner.asyncio, "sleep", AsyncMock()), self.assertRaises(PermissionError) as caught:
            await runner.stop_group(process)
        self.assertIs(caught.exception, denied)
        process.wait.assert_not_awaited()

    async def test_setup_and_path_control_failures_remove_scratch(self):
        with tempfile.TemporaryDirectory(prefix="native-cleanup-") as directory:
            await self.check_failure_cleanup(Path(directory))

    async def check_failure_cleanup(self, root):
        (root / "scripts").mkdir()
        (root / "scripts/test-helper-recovery-paths.py").write_text("raise SystemExit(6)\n")
        for error in (RuntimeError("compile failed"), None):
            await self.check_setup_failure(root, error)

    async def check_setup_failure(self, root, error):
        output = root / "results.jsonl"
        with patch.object(runner, "prepare", AsyncMock(return_value=[], side_effect=error)):
            with patch.object(runner, "run_shards", AsyncMock()), self.assertRaises(RuntimeError):
                await runner.run(root, output, root, root)
        schedule = json.loads(output.with_suffix(".schedule.json").read_text())
        self.assertEqual(schedule["status"], "fail")
        self.assertTrue(schedule["scratch"]["removed"])
        self.assertFalse(Path(schedule["scratch"]["short"]).exists())
        self.assertFalse(Path(schedule["scratch"]["long"]).exists())

    async def test_nonzero_command_and_invalid_protocol_propagate(self):
        with self.assertRaises(FileNotFoundError):
            await runner.command(("/nonexistent/native-runner-command",), Path.cwd(), os.environ)
        with self.assertRaisesRegex(RuntimeError, "exited 7"):
            await runner.command((sys.executable, "-c", "raise SystemExit(7)"), Path.cwd(), os.environ)
        with self.assertRaises(json.JSONDecodeError):
            await runner.command((sys.executable, "-c", "print('bad JSON')"), Path.cwd(), os.environ,
                                 json.loads)

    async def test_command_and_protocol_failure_stop_surviving_child(self):
        with tempfile.TemporaryDirectory(prefix="native-failure-") as directory:
            await self.check_child_failure(Path(directory), "raise SystemExit(7)", None, RuntimeError)
            await self.check_child_failure(Path(directory), "print('bad JSON',flush=True); time.sleep(60)",
                                           json.loads, json.JSONDecodeError)

    async def check_child_failure(self, root, ending, consume, error):
        pidfile = root / "pids"
        source = self.fixture_source(pidfile, ending)
        task = asyncio.create_task(runner.command((sys.executable, "-c", source), root, os.environ, consume))
        try:
            with self.assertRaises(error):
                await task
            self.assert_fixture_stopped(pidfile)
        finally:
            await self.cleanup_fixture(task, pidfile)
            pidfile.unlink(missing_ok=True)

    async def test_cancellation_stops_process_group_before_returning(self):
        with tempfile.TemporaryDirectory(prefix="native-cancel-") as directory:
            await self.check_cancel(Path(directory))

    async def test_cancellation_during_creation_handoff_stops_process_group(self):
        with tempfile.TemporaryDirectory(prefix="native-handoff-") as directory:
            await self.check_handoff(Path(directory))

    async def test_repeated_cancellation_during_creation_handoff(self):
        with tempfile.TemporaryDirectory(prefix="native-repeat-") as directory:
            await self.check_handoff(Path(directory), repeat=True)

    async def test_repeated_cancellation_during_group_cleanup(self):
        stopping = asyncio.Event()
        killpg = os.killpg

        def signal_group(pid, signum):
            killpg(pid, signum)
            if signum == signal.SIGTERM:
                stopping.set()

        with tempfile.TemporaryDirectory(prefix="native-stopping-") as directory:
            with patch.object(runner.os, "killpg", signal_group):
                await self.check_cancel(Path(directory), stopping=stopping)

    async def check_handoff(self, root, repeat=False):
        release = asyncio.Event()
        cleaning = asyncio.Event()
        create = asyncio.create_subprocess_exec
        stop_created_group = runner.stop_created_group
        processes = []

        async def delayed_create(*args, **kwargs):
            process = await create(*args, **kwargs)
            processes.append(process)
            await release.wait()
            return process

        async def stop_creation(creation):
            cleaning.set()
            await stop_created_group(creation)

        try:
            with patch.object(runner.asyncio, "create_subprocess_exec", delayed_create), \
                    patch.object(runner, "stop_created_group", stop_creation):
                await self.check_cancel(root, release, cleaning if repeat else None)
        finally:
            for process in processes:
                await runner.stop_group(process)

    def fixture_source(self, pidfile, ending):
        pending = str(pidfile) + ".pending"
        return ("import os,signal,subprocess,sys,time; "
                "signal.signal(signal.SIGTERM, signal.SIG_IGN); "
                "child=subprocess.Popen([sys.executable,'-c','import time; time.sleep(60)'],"
                "stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL); "
                f"open({pending!r},'w').write(f'{{os.getpid()}} {{child.pid}}'); "
                f"os.rename({pending!r},{str(pidfile)!r}); " + ending)

    async def check_cancel(self, root, release=None, cleaning=None, stopping=None):
        pidfile = root / "pids"
        source = self.fixture_source(pidfile, "time.sleep(60)")
        task = asyncio.create_task(runner.command((sys.executable, "-c", source), root, os.environ))
        try:
            await self.await_file(pidfile)
            parent, child = map(int, pidfile.read_text().split())
            self.assertEqual(os.getpgid(parent), parent)
            self.assertEqual(os.getpgid(child), parent)
            task.cancel()
            await self.cancel_again(task, cleaning or stopping)
            if release is not None:
                release.set()
            await self.check_cancelled(task, pidfile)
        finally:
            if release is not None:
                release.set()
            await self.cleanup_fixture(task, pidfile)

    async def cancel_again(self, task, event):
        if event is None:
            return
        await asyncio.wait_for(event.wait(), timeout=5)
        self.assertFalse(task.done())
        task.cancel()

    async def check_cancelled(self, task, pidfile):
        with self.assertRaises(asyncio.CancelledError):
            await task
        self.assert_fixture_stopped(pidfile)

    def assert_fixture_stopped(self, pidfile):
        parent, child = map(int, pidfile.read_text().split())
        self.assert_stopped(parent)
        self.assert_stopped(child)

    async def cleanup_fixture(self, task, pidfile):
        if not task.done():
            task.cancel()
        await asyncio.gather(task, return_exceptions=True)
        if not pidfile.exists():
            return
        parent, child = map(int, pidfile.read_text().split())
        try:
            self.assertEqual(os.getpgid(parent), parent)
            os.killpg(parent, signal.SIGKILL)
        except ProcessLookupError:
            # A reaped leader can leave the fixture child in its original group.
            self.kill_fixture_child(parent, child)

    def kill_fixture_child(self, parent, child):
        try:
            self.assertEqual(os.getpgid(child), parent)
            os.killpg(parent, signal.SIGKILL)
        except ProcessLookupError:
            pass

    async def await_file(self, path):
        await asyncio.wait_for(self.poll_file(path), timeout=5)

    async def poll_file(self, path):
        while not path.exists():
            await asyncio.sleep(0.01)

    def assert_stopped(self, pid):
        # An orphan zombie may await the OS reaper, but cannot execute or retain open files.
        result = subprocess.run(("ps", "-o", "stat=", "-p", str(pid)), capture_output=True, text=True, check=False)
        self.assertTrue(not result.stdout.strip() or result.stdout.strip().startswith("Z"), result.stdout)

    async def test_execution_is_bounded_and_keeps_other_failures_blocking(self):
        active = 0
        peak = 0

        async def fake_execute(shard, semaphore, env, output):
            nonlocal active, peak
            async with semaphore:
                active += 1
                peak = max(active, peak)
                await asyncio.sleep(0.001)
                active -= 1
                shard["status"] = "fail" if shard["id"] == "3" else "pass"

        shards = [{"id": str(index)} for index in range(12)]
        with patch.object(runner, "execute", fake_execute), self.assertRaisesRegex(RuntimeError, "3"):
            await runner.run_shards(shards, {}, io.StringIO())
        self.assertEqual(peak, runner.CONCURRENCY)
        self.assertEqual(sum(shard["status"] == "pass" for shard in shards), 11)


GO_FIXTURE = '''package cli
import (
    "fmt"
    "os"
    "testing"
    "time"
)
type Socket struct{}
func TestMain(m *testing.M) {
    if os.Getenv("NATIVE_FIXTURE_EXIT") != "" { os.Exit(9) }
    if os.Getenv("NATIVE_FIXTURE_OMIT") != "" { os.Exit(0) }
    code := m.Run()
    if os.Getenv("NATIVE_FIXTURE_HIDE_FAILURE") != "" { os.Exit(0) }
    os.Exit(code)
}
func TestOne(t *testing.T) {
    cwd, _ := os.Getwd()
    if cwd != os.Getenv("NATIVE_FIXTURE_CWD") { t.Fatal("wrong cwd", cwd) }
    if os.Getenv("NATIVE_FIXTURE_FAIL") != "" { t.Fatal("controlled failure") }
    if os.Getenv("NATIVE_FIXTURE_HANG") != "" { time.Sleep(time.Second) }
    t.Run("subcase", func(t *testing.T) { t.Log("subcase ran") })
}
func TestTwo(t *testing.T) { t.Log("two ran") }
func TestSkip(t *testing.T) { t.Skip("intentional fixture skip") }
func ExampleSocket() { fmt.Println("example ran")
    // Output: example ran
}
func FuzzSocket(f *testing.F) { f.Add("seed"); f.Fuzz(func(t *testing.T, s string) {}) }
func BenchmarkSocket(b *testing.B) { panic("benchmarks must not run") }
'''


class BinaryTest(unittest.TestCase):
    def test_actual_race_binary_discovery_execution_and_testmain_errors(self):
        with tempfile.TemporaryDirectory(prefix="native-binary-") as directory:
            physical = Path(directory) / "physical"
            physical.mkdir()
            alias = Path(directory) / "alias"
            alias.symlink_to(physical, target_is_directory=True)
            self.check_binary(alias)

    def check_binary(self, root):
        root = root.resolve()
        package = root / "internal/cli"
        package.mkdir(parents=True)
        (root / "go.mod").write_text("module fixture\n\ngo 1.27.4\n")
        (package / "fixture_test.go").write_text(GO_FIXTURE)
        binaries = root / "binaries"
        binaries.mkdir()
        env = os.environ | {"NATIVE_FIXTURE_CWD": str(package)}
        with patch.object(runner, "command", wraps=runner.command) as commands:
            shards = asyncio.run(runner.prepare(root, binaries, env, ("cli",)))
        builds = [call.args[0] for call in commands.call_args_list if call.args[0][:2] == ("go", "test")]
        self.assertEqual(len(builds), 1)
        self.assertIn("-race", builds[0])
        self.assertIn("-c", builds[0])
        self.assertEqual(len(shards), 4)
        self.assertEqual(sum(len(shard["tests"]) for shard in shards), 5)
        output = io.StringIO()
        with contextlib.redirect_stdout(io.StringIO()):
            asyncio.run(runner.run_shards(shards, env, output))
        events = [json.loads(line) for line in output.getvalue().splitlines()]
        self.assertTrue(any(event.get("Test") == "FuzzSocket/seed#0" for event in events))
        self.assertTrue(any(event.get("Test") == "TestOne/subcase" for event in events))
        for mode in ("FAIL", "EXIT", "OMIT", "HIDE_FAILURE", "HANG"):
            self.check_failure(shards, env, mode)

    def check_failure(self, shards, env, mode):
        environment = env | {f"NATIVE_FIXTURE_{mode}": "1"}
        if mode == "HIDE_FAILURE":
            environment["NATIVE_FIXTURE_FAIL"] = "1"
        alarm = "50ms" if mode == "HANG" else runner.ALARM
        with patch.object(runner, "ALARM", alarm), contextlib.redirect_stdout(io.StringIO()), self.assertRaisesRegex(RuntimeError, "failed"):
            asyncio.run(runner.run_shards(shards, environment, io.StringIO()))


if __name__ == "__main__":
    unittest.main()
