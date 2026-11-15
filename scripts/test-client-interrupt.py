"""Real external socket tracing and pure one-case diagnostic safety controls."""
import ast
import importlib.util
import io
import json
import os
import re
import select
import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import Mock, patch

from fixtures import installed_clock
from fixtures import interrupt_capture as capture
from fixtures import interrupt_prepare as preparation
from fixtures import interrupt_trace as trace

SPEC = importlib.util.spec_from_file_location("interrupt_vehicle", Path(__file__).with_name("diagnose-client-interrupt.py"))
VEHICLE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(VEHICLE)
STRACE = os.environ.get("MESH_TEST_STRACE") or shutil.which("strace")
TOY = """
import ctypes,os,socket,sys
assert ctypes.CDLL(None).prctl(0x59616d61,os.getppid(),0,0,0)==0
s=socket.socket()
s.settimeout(120)
if sys.argv[1]=='server':
 s.bind(('127.0.0.1',0)); s.listen()
 print(s.getsockname()[1],flush=True)
 c,_=s.accept(); c.settimeout(120)
 print('connected',flush=True)
 sys.stdin.readline()
 data=c.recv(1024); c.sendall(data)
 assert c.recv(1024)==b''
 c.close(); s.close()
else:
 s.connect(('127.0.0.1',int(sys.argv[2])))
 print('connected',flush=True)
 sys.stdin.readline()
 s.sendall(b'PRIVATE_PAYLOAD_MUST_NEVER_APPEAR')
 assert s.recv(1024)==b'PRIVATE_PAYLOAD_MUST_NEVER_APPEAR'
 print('round-trip',flush=True)
 sys.stdin.readline()
 s.shutdown(socket.SHUT_RDWR); s.close()
"""


def line(process):
    if not select.select([process.stdout], [], [], 120)[0]:
        raise RuntimeError("toy actor failed to answer before its hang cap")
    return process.stdout.readline().strip()


def stop(process):
    if process.poll() is None:
        process.kill()
    process.wait(timeout=120)
    process.stdin.close()
    process.stdout.close()
    process.stderr.close()


class DiagnosticTest(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="interrupt-controls-")
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)

    def test_real_two_actor_trace_preserves_identity_excludes_payload_and_observes_eof(self):
        self.assertTrue(STRACE, "set MESH_TEST_STRACE to the installed test observer")
        server = self.actor("server")
        port = int(line(server))
        client = self.actor("client", str(port))
        self.assertEqual(line(client), "connected")
        self.assertEqual(line(server), "connected")
        pair = capture.matched_socket(client.pid, server.pid, port)
        observed = [self.attach("client", client), self.attach("daemon", server)]
        before = [capture.traffic(actor, fd["fd"]) for actor, fd in zip(observed, pair)]
        server.stdin.write("go\n")
        server.stdin.flush()
        client.stdin.write("go\n")
        client.stdin.flush()
        self.assertEqual(line(client), "round-trip")
        trace.wait(lambda: all(capture.progressed(actor, fd["fd"], old) for actor, fd, old in zip(observed, pair, before)),
                   "actual socket read/write instrumentation is unarmed")
        for actor in observed:
            actor.require_identity()
        client.stdin.write("close\n")
        client.stdin.flush()
        self.assertEqual(client.wait(timeout=120), 0)
        self.assertEqual(server.wait(timeout=120), 0)
        for actor in observed:
            self.check_trace(actor)

    def actor(self, role, *arguments):
        process = subprocess.Popen([sys.executable, "-u", "-c", TOY, role, *arguments], stdin=subprocess.PIPE,
            stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        self.addCleanup(stop, process)
        return process

    def attach(self, role, process):
        observer = trace.Trace(self.root, role, trace.identity(process.pid), executable=STRACE, privileged=False)
        self.addCleanup(observer.close)
        observer.start()
        trace.wait(observer.armed, "toy tracer did not attach to the actual actor")
        return observer

    def check_trace(self, actor):
        actor.close()
        data = actor.path.read_text()
        self.assertNotIn("PRIVATE_PAYLOAD", data)
        self.assertEqual(actor.summary()["dropped"], 0)
        self.assertEqual(actor.summary()["errors"], [])
        self.assertEqual(actor.status, 0)
        self.assertIn("close(", data)
        self.assertIn("exit_group(", data)
        if actor.role == "daemon":
            rows = [json.loads(row) for row in data.splitlines()]
            self.assertTrue(any(row.get("call") == "recvfrom" and row.get("result") == 0 for row in rows))
        destination = os.environ.get("MESH_INTERRUPT_CONTROL_EVIDENCE")
        if destination:
            self.retain_control(actor, Path(destination))

    def retain_control(self, actor, destination):
        destination.mkdir(parents=True, exist_ok=True)
        shutil.copy2(actor.path, destination / actor.path.name)
        (destination / (actor.role + "-summary.json")).write_text(json.dumps(actor.summary(), indent=2) + "\n")

    def test_parser_keeps_thread_fd_return_errno_and_order(self):
        value = trace.parse_line("[pid 4321] 1794781234.123456 read(0xa, 0xc001, 0x1000) = -1 ECONNRESET (Connection reset by peer) <0.1>", 1234)
        self.assertEqual((value["tid"], value["fd"], value["result"], value["errno"]), (4321, 10, -1, "ECONNRESET"))
        self.assertEqual(value["unixNano"], 1794781234123456000)
        self.assertEqual(trace.parse_line("1234 1794781234.1 write(0x6, 0xc000, 0x4) = 0x4", 9)["result"], 4)

    def test_overflow_is_bounded_and_visible(self):
        observer = trace.Trace(self.root, "small", {"pid": 1})
        observer.stream = io.StringIO()
        observer.rows = trace.MAX_ROWS
        observer.record({"result": 1, "call": "write", "fd": 10})
        self.assertEqual(observer.dropped, 1)
        self.assertEqual(observer.stream.getvalue(), "")

    def test_observer_failure_keeps_draining_without_changing_tracee(self):
        observer = trace.Trace(self.root, "failure", {"pid": 1})
        observer.record = Mock(side_effect=OSError("disk full"))
        observer.consume(b"1.0 read(0x3, 0xc0, 0x1) = 0x1\n")
        observer.consume(b"1.1 read(0x3, 0xc0, 0x1) = 0x1\n")
        self.assertEqual(observer.errors, ["OSError"])
        self.assertEqual(observer.dropped, 1)
        observer.record.assert_called_once()

    def test_capture_refuses_interruption_without_same_actor_control(self):
        observer = capture.Capture(SimpleNamespace(), self.root)
        self.addCleanup(observer.close)
        with self.assertRaisesRegex(RuntimeError, "unarmed"):
            observer.require_armed()

    def test_original_trace_actor_must_keep_birth_owner_and_parent(self):
        expected = {"pid": 123, "uid": 456, "ppid": 789, "birthTicks": 100, "sha256": "a" * 64}
        observer = trace.Trace(self.root, "client", expected)
        with patch.object(trace, "identity", return_value=expected | {"ppid": 987}), self.assertRaisesRegex(RuntimeError, "identity or parent"):
            observer.require_identity()

    def test_native_execution_refuses_local_host_before_manager_commands(self):
        with patch.dict(os.environ, {"GITHUB_ACTIONS": "false"}), patch.object(VEHICLE.subprocess, "run") as command, \
                self.assertRaisesRegex(RuntimeError, "disposable GitHub-hosted"):
            VEHICLE.host_guard()
        command.assert_not_called()

    def test_occupied_canonical_manager_is_refused(self):
        result = SimpleNamespace(stdout="LoadState=loaded\nLoadState=not-found\n")
        with patch.dict(os.environ, {"GITHUB_ACTIONS": "true", "RUNNER_ENVIRONMENT": "github-hosted"}), \
                patch.object(VEHICLE.subprocess, "run", return_value=result), self.assertRaisesRegex(RuntimeError, "must both be absent"):
            VEHICLE.host_guard()

    def test_primary_exception_survives_observer_and_clock_export_failures(self):
        primary = RuntimeError("original strict resume failed")
        owned = SimpleNamespace(finish=Mock(side_effect=ValueError("observer failed")), output=self.root,
                                record={"cleanupVerified": False}, cleanup_error=None)
        clock = SimpleNamespace(export=Mock(side_effect=OSError("disk failed")))
        with patch.object(VEHICLE.subprocess, "check_output", return_value="a" * 40):
            VEHICLE.finish(owned, primary, clock)
        self.assertIn("ValueError", primary.__notes__[0])
        self.assertIn("OSError", primary.__notes__[1])

    def test_cleanup_exception_survives_observer_export_failure(self):
        original = OSError("native service cleanup failed")
        owned = SimpleNamespace(finish=Mock(), output=self.root, record={"cleanupVerified": False}, cleanup_error=original)
        clock = SimpleNamespace(export=Mock(side_effect=ValueError("observer failed")))
        with patch.object(VEHICLE.subprocess, "check_output", return_value="a" * 40):
            VEHICLE.finish(owned, None, clock)
        self.assertIn("ValueError", original.__notes__[0])

    def test_existing_preparation_uses_only_original_pc_pair_prior_and_one_genuine_proof(self):
        self.check_preparation()

    def test_changed_original_bytes_stop_before_proof_or_publication(self):
        self.check_preparation(rejected=True)

    def check_preparation(self, rejected=False):
        source = {"checkoutSource": "a" * 40}
        releases = Mock(root=self.root, output=self.root, builds={preparation.TARGET: {"commit": source["checkoutSource"]}})
        releases.prove.return_value = {"forward": "forward", "rollback": "rollback"}
        pair = {"daemon": {"executable": self.root / "v249"}, "helper": {"executable": self.root / "v196"}}
        prior = {"executable": self.root / "v248"}
        with patch.object(preparation.subprocess, "run"), patch.object(preparation, "source_evidence", return_value=source), \
                patch.object(preparation.installed_origin, "Origin") as origin, \
                patch.object(preparation.fleet_baselines, "acquire_pair", return_value=pair) as acquire_pair, \
                patch.object(preparation.fleet_baselines, "acquire", return_value=prior) as acquire, \
                patch.object(preparation, "verify_originals", return_value={}) as verify:
            self.check_preparation_result(releases, source, verify, rejected)
        releases.start_signer.assert_called_once_with()
        releases.build.assert_called_once_with("v0.1.9001")
        acquire_pair.assert_called_once_with(self.root / "public", preparation.HOST | {
            "daemonSha256": capture.ORIGINAL, "helperSha256": preparation.HELPER})
        acquire.assert_called_once_with(self.root / "public", "v0.1.248", preparation.PLATFORM, preparation.PRIOR)
        if rejected:
            releases.prove.assert_not_called()
            releases.publish.assert_not_called()
            origin.return_value.serve_tree.assert_not_called()
            return
        releases.prove.assert_called_once_with(self.root / "v249", "v0.1.9001")
        releases.publish.assert_called_once_with("v0.1.9001", 0, [releases.prove.return_value])
        origin.return_value.head.assert_called_once_with(releases.directory, "v0.1.9001")
        self.assertFalse(json.loads((self.root / "preparation.json").read_text())["accepted"])

    def check_preparation_result(self, releases, source, verify, rejected):
        if not rejected:
            preparation.prepare(releases, source)
            return
        verify.side_effect = RuntimeError("original published input bytes differ")
        with self.assertRaisesRegex(RuntimeError, "original published input bytes differ"):
            preparation.prepare(releases, source)

    def test_source_metadata_requires_clean_checkout_and_exact_product_objects(self):
        returns = [""] + ["b" * 40 + "\n" + "b" * 40] * len(preparation.PRODUCT) + ["a" * 40]
        with patch.object(preparation, "git", side_effect=returns):
            value = preparation.source_evidence()
        self.assertEqual(value["checkoutSource"], "a" * 40)
        self.assertEqual(value["historicalProductSource"], preparation.SOURCE)
        self.assertEqual(set(value["identicalProductObjects"]), set(preparation.PRODUCT))
        with patch.object(preparation, "git", return_value=" M modified.go"), self.assertRaisesRegex(RuntimeError, "clean reviewed checkout"):
            preparation.source_evidence()
        with patch.object(preparation, "git", side_effect=["", "a" * 40 + "\n" + "b" * 40]), \
                self.assertRaisesRegex(RuntimeError, "differs from the pinned failure source"):
            preparation.source_evidence()

    def test_candidate_vcs_mismatch_stops_before_original_acquisition_or_proof(self):
        releases = Mock(root=self.root, output=self.root, builds={preparation.TARGET: {"commit": "b" * 40}})
        with patch.object(preparation.subprocess, "run"), patch.object(preparation.installed_origin, "Origin"), \
                patch.object(preparation.fleet_baselines, "acquire_pair") as acquire, \
                self.assertRaisesRegex(RuntimeError, "candidate VCS identity differs"):
            preparation.prepare(releases, {"checkoutSource": "a" * 40})
        acquire.assert_not_called()
        releases.prove.assert_not_called()
        releases.publish.assert_not_called()

    def test_unconfirmed_preparation_descendants_keep_their_owned_scratch(self):
        owned = VEHICLE.Run.__new__(VEHICLE.Run)
        owned.case, owned.capture, owned.cleanup_error = None, None, None
        owned.releases = SimpleNamespace(closed=True, transition_clean=False)
        owned.root, owned.record = self.root, {}
        owned.remove_scratch()
        self.assertFalse(owned.record["scratchAbsent"])
        self.assertTrue(self.root.exists())

    def test_observer_caps_use_parent_deadlines_without_renewing_work_or_cleanup(self):
        clock = installed_clock.Clock({"test": "observer-parent"})
        with clock.activate():
            original = clock.deadline_ns
            earlier = clock.origin_ns + 60 * installed_clock.SECOND
            self.assertEqual(trace.deadline(parent=earlier), earlier)
            trace.wait(lambda: True, "armed", parent=earlier)
            self.assertEqual(clock.deadline_ns, original)
            clock.begin_cleanup(final=True)
            cleanup = clock.cleanup_deadline_ns
            self.assertLessEqual(trace.budget(), 120)
            self.assertLessEqual(trace.deadline(), cleanup)
            self.assertEqual(clock.cleanup_deadline_ns, cleanup)

    def test_partial_startup_marker_is_observed_without_starting_another_actor(self):
        marker = self.root / "start.json"
        self.assertIsNone(capture.read_start(marker))
        marker.write_text('{"pid":')
        self.assertIsNone(capture.read_start(marker))
        marker.write_text('{"pid":123}')
        self.assertEqual(capture.read_start(marker), {"pid": 123})

    def test_new_code_nesting_is_at_most_three(self):
        paths = [Path(module.__file__) for module in (capture, preparation, trace, VEHICLE)] + [Path(__file__)]
        for path in paths:
            self.assertLessEqual(depth(ast.parse(path.read_text())), 3, str(path))

    def test_registered_vehicle_runs_only_one_diagnostic_job_and_cannot_publish(self):
        source = (Path(__file__).parents[1] / ".github/workflows/ci.yml").read_text()
        jobs = dict(re.findall(r"^  ([\w-]+):\n(.*?)(?=^  [\w-]+:|\Z)", source.split("jobs:\n", 1)[1], re.MULTILINE | re.DOTALL))
        guarded = {"changes", "race", "integration", "test", "gates", "vulnerability", "packaging", "gate"}
        for name in guarded:
            self.assertIn("!inputs.client_interrupt", jobs[name])
        for name in set(jobs).difference(guarded, {"client-interrupt-diagnostic", "publish"}):
            self.assertRegex(jobs[name], r"(?m)^    needs: (?:changes|\[changes, update-proofs\])$")
            condition = re.findall(r"^    if: (.+)$", jobs[name], re.MULTILINE)
            self.assertEqual(len(condition), 1)
            self.assertNotIn("always()", condition[0])
        self.assertIn("needs.gate.result == 'success'", jobs["publish"])
        diagnostic = jobs["client-interrupt-diagnostic"]
        self.assertIn("github.event_name == 'workflow_dispatch' && inputs.client_interrupt", diagnostic)
        self.assertNotIn("strategy:", diagnostic)
        self.assertNotIn("continue-on-error", diagnostic)
        self.assertEqual(diagnostic.count("scripts/diagnose-client-interrupt.py --output"), 1)
        self.assertIn("uses: ./.github/actions/installed-go", diagnostic)
        self.assertNotIn("download-artifact", diagnostic)
        self.assertNotIn("GH_TOKEN", diagnostic)
        self.assertNotIn("client_interrupt_pin", source)
        self.assertIn("path: ${{ runner.temp }}/client-interrupt-diagnostic", diagnostic)
        self.assertNotIn("path: ${{ runner.temp }}/client-interrupt-input", diagnostic)


def depth(node, level=0):
    if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)):
        level = 1
    elif isinstance(node, (ast.If, ast.For, ast.While, ast.With, ast.Try)):
        level += 1
    return max((depth(child, level) for child in ast.iter_child_nodes(node)), default=level)


if __name__ == "__main__":
    unittest.main()
