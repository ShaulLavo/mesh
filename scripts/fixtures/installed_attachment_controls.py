"""Pure and mock controls for original attachment failure evidence."""
import base64
import hashlib
import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import Mock, patch

from fixtures import installed_attachment_evidence as evidence, installed_native
from fixtures import installed_continuity as continuity, installed_fresh, installed_chain, installed_releases, installed_order_host
from fixtures import installed_publication


class ContinuityPolicyTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="continuity-control-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.first, self.target = installed_releases.VERSIONS[:2]
        self.source = self.root / "source-client"
        self.source.write_bytes(b"current client and daemon code")
        self.digest = hashlib.sha256(self.source.read_bytes()).hexdigest()
        self.build = {"version": self.first, "digest": self.digest, "commit": "current-source", "path": str(self.source)}
        self.target_build = self.build | {"version": self.target, "digest": "b" * 64}
        self.terminal = SimpleNamespace(pid=101, poll=Mock(return_value=None), close=Mock(), send=Mock(), expect_exit=Mock())
        self.case = SimpleNamespace(root=self.root, state=self.root / "state", binary=self.source,
            pair={"daemon": {"artifact": {"binarySha256": self.digest}}}, session="ABCD", terminal=self.terminal,
            releases=SimpleNamespace(builds={self.first: self.build, self.target: self.target_build}),
            control=Mock(return_value={"host": {"build": self.build}}), wait_attachment_resume=Mock(), io=Mock(),
            attachment_observations=[], client_environment={"MESH_CONFIG_DIR": "same-client-config"})
        self.mapped = self.enterContext(patch.object(installed_native.native_services, "mapped_image",
            side_effect=lambda pid, _path: {"pid": pid, "sha256": self.digest if pid == 101 else self.case.control.return_value["host"]["build"]["digest"]}))
        self.peer = self.enterContext(patch.object(installed_native, "peer_pid", return_value=201))

    def seed(self, legacy=False):
        continuity.seed(self.case, legacy=legacy)

    def current_target(self):
        self.case.control.return_value = {"host": {"build": self.target_build}}

    def test_current_to_current_keeps_actual_source_client_and_checks_target_daemon(self):
        self.seed()
        self.current_target()
        result = continuity.inspect(self.case)
        self.assertTrue(result["strictContinuityPassed"])
        self.assertEqual(result["client"], {"pid": 101, "sha256": self.digest})
        self.assertEqual(result["sourceDaemon"], self.build)
        self.assertEqual(result["sourceDaemonImage"], {"pid": 201, "sha256": self.digest})
        self.assertEqual(result["runningDaemon"], self.target_build)
        self.assertEqual(result["daemonImage"], {"pid": 201, "sha256": "b" * 64})
        self.assertIs(self.case.terminal, self.terminal)
        self.case.wait_attachment_resume.assert_called_once_with()
        self.case.io.assert_called_once_with()
        self.terminal.close.assert_not_called()
        self.assertGreaterEqual(result["elapsedSeconds"], 0)

    def test_lost_current_client_fails_even_with_exit_zero(self):
        self.seed()
        self.terminal.poll.return_value = 0
        with self.assertRaisesRegex(RuntimeError, "strict current attachment client exited"):
            continuity.inspect(self.case)
        self.case.wait_attachment_resume.assert_not_called()
        self.terminal.close.assert_not_called()
        self.assertEqual(self.case.attachment_observations, [])

    def test_ordered_polling_coalesces_identical_checks_without_growing_export(self):
        self.seed()
        self.current_target()
        for _ in range(120):
            result = continuity.inspect(self.case)
        self.assertEqual(len(self.case.attachment_observations), 1)
        self.assertEqual(result["checks"], 120)
        self.assertTrue(result["strictContinuityPassed"])
        self.assertLess((self.root / continuity.RECORD).stat().st_size, 8192)

    def test_current_client_lost_after_resume_fails(self):
        self.seed()
        self.current_target()
        self.terminal.poll.side_effect = [None, 0]
        with self.assertRaisesRegex(RuntimeError, "exited after resume"):
            continuity.inspect(self.case)
        self.assertEqual(self.case.attachment_observations, [])

    def test_replaced_current_terminal_fails_even_if_pid_is_reused(self):
        self.seed()
        self.case.terminal = SimpleNamespace(pid=101)
        with self.assertRaisesRegex(RuntimeError, "client was replaced"):
            continuity.inspect(self.case)

    def test_current_marker_failure_is_never_downgraded(self):
        self.seed()
        self.current_target()
        self.case.wait_attachment_resume.side_effect = RuntimeError("strict marker missing")
        with self.assertRaisesRegex(RuntimeError, "strict marker missing"):
            continuity.inspect(self.case)
        self.assertEqual(self.case.attachment_observations, [])

    def test_legacy_exit_zero_records_owner_reconnect_without_continuity_pass(self):
        self.seed(legacy=True)
        self.terminal.poll.return_value = 0
        result = continuity.inspect(self.case)
        self.assertEqual(result["scope"], "legacy-adoption")
        self.assertIsNone(result["strictContinuityPassed"])
        self.assertFalse(result["observedResume"])
        self.assertTrue(result["ownerReconnectRequired"])
        self.assertEqual(result["exitStatus"], 0)
        self.assertIn("owner reconnects", result["reconnectAction"])
        self.case.wait_attachment_resume.assert_not_called()
        self.case.io.assert_not_called()
        self.assertIs(self.case.terminal, self.terminal)
        self.assertEqual(json.loads((self.root / continuity.RECORD).read_text()), [result])

    def test_legacy_drop_after_live_observation_records_the_reconnect_requirement(self):
        self.seed(legacy=True)
        live = continuity.inspect(self.case)
        self.assertIsNone(live["observedResume"])
        self.terminal.poll.return_value = 0
        result = continuity.inspect(self.case)
        self.assertIsNone(result["strictContinuityPassed"])
        self.assertTrue(result["ownerReconnectRequired"])
        self.assertFalse(result["observedResume"])

    def test_legacy_live_client_does_not_create_a_continuity_assertion(self):
        self.seed(legacy=True)
        self.case.wait_attachment_resume.side_effect = RuntimeError("legacy resume is outside the strict gate")
        self.case.io.side_effect = RuntimeError("legacy terminal I/O is outside the strict gate")
        result = continuity.inspect(self.case)
        self.assertIsNone(result["observedResume"])
        self.assertIsNone(result["strictContinuityPassed"])
        self.assertFalse(result["ownerReconnectRequired"])
        self.case.wait_attachment_resume.assert_not_called()
        self.case.io.assert_not_called()

    def test_legacy_drop_keeps_preservation_worker_shell_and_listener_checks(self):
        self.seed(legacy=True)
        self.terminal.poll.return_value = 0
        case = self.case
        case.snapshots, case.retained = {"identity.key": "original"}, Mock(return_value={"identity.key": "original"})
        case.worker, case.shell = 201, 301
        case.open_stream, case.route_observer = Mock(), SimpleNamespace(recovered=Mock())
        session = case.state / "s" / case.session
        session.mkdir(parents=True)
        (session / "meta.json").write_text(json.dumps({"pid": 301}))
        result = installed_fresh.installed_adoption.AdoptionCase.inspect_retained(case)
        self.assertTrue(result["ownerReconnectRequired"])
        case.open_stream.assert_called_once_with()
        case.route_observer.recovered.assert_called_once_with()
        self.peer.return_value = 999
        with self.assertRaisesRegex(RuntimeError, "replaced retained worker"):
            installed_fresh.installed_adoption.AdoptionCase.inspect_retained(case)
        self.peer.return_value = 201
        (session / "meta.json").write_text(json.dumps({"pid": 999}))
        with self.assertRaisesRegex(RuntimeError, "replaced retained shell"):
            installed_fresh.installed_adoption.AdoptionCase.inspect_retained(case)
        case.retained.return_value = {"identity.key": "changed"}
        with self.assertRaisesRegex(RuntimeError, "changed preserved components"):
            installed_fresh.installed_adoption.AdoptionCase.inspect_retained(case)

    def test_current_seed_rejects_client_or_server_bytes_outside_current_build(self):
        self.mapped.return_value = {"pid": 101, "sha256": "old-client"}
        self.mapped.side_effect = None
        with self.assertRaisesRegex(RuntimeError, "client differs"):
            self.seed()
        self.mapped.return_value = {"pid": 101, "sha256": self.digest}
        self.case.releases.builds[self.first] = self.build | {"digest": "old-server"}
        with self.assertRaisesRegex(RuntimeError, "verified release image"):
            self.seed()

    def test_current_source_rejects_old_daemon_even_if_it_reports_the_fixture_version(self):
        self.case.control.return_value = {"host": {"build": self.build | {"digest": "old-server"}}}
        with self.assertRaisesRegex(RuntimeError, "current source differs"):
            self.seed()

    def test_current_source_checks_mapped_daemon_bytes_before_the_hop(self):
        self.mapped.side_effect = lambda pid, _path: {"pid": pid, "sha256": self.digest if pid == 101 else "old-server"}
        with self.assertRaisesRegex(RuntimeError, "maps another image"):
            self.seed()

    def test_current_target_rejects_unverified_server_image(self):
        self.seed()
        self.current_target()
        self.mapped.side_effect = None
        self.mapped.return_value = {"pid": 201, "sha256": "old-server"}
        with self.assertRaisesRegex(RuntimeError, "maps another image"):
            continuity.inspect(self.case)

    def test_explicit_reconnect_uses_current_bytes_and_same_session_before_strict_hops(self):
        self.seed(legacy=True)
        self.terminal.poll.return_value = 0
        continuity.inspect(self.case)
        current = SimpleNamespace(pid=102, poll=Mock(return_value=None))
        self.mapped.side_effect = lambda pid, _path: {"pid": pid, "sha256": self.digest}
        with patch.object(installed_native.terminal_module, "Terminal", return_value=current) as launch:
            result = continuity.reconnect_current(self.case, self.first)
        self.assertEqual(launch.call_args.args[0], [str(self.root / "current-client"), "ABCD"])
        self.assertEqual(launch.call_args.args[1], self.case.client_environment)
        self.assertEqual((self.root / "current-client").read_bytes(), self.source.read_bytes())
        self.assertTrue(result["strictContinuityPassed"])
        self.assertEqual(result["client"]["pid"], 102)
        self.assertEqual(self.case.attachment_observations[1]["scope"], "explicit-reconnect-after-legacy-adoption")
        self.assertIsNone(self.case.attachment_observations[0]["strictContinuityPassed"])
        self.terminal.close.assert_called_once_with()
        self.terminal.send.assert_not_called()
        with self.assertRaisesRegex(RuntimeError, "cannot replace a client"):
            continuity.reconnect_current(self.case, self.first)

    def test_live_legacy_reconnect_detaches_normally_before_launching_current_client(self):
        self.seed(legacy=True)
        current = SimpleNamespace(pid=102, poll=Mock(return_value=None))
        self.mapped.side_effect = lambda pid, _path: {"pid": pid, "sha256": self.digest}
        self.terminal.expect_exit.side_effect = lambda **_: setattr(self.terminal.poll, "return_value", 0)
        with patch.object(installed_native.terminal_module, "Terminal", return_value=current):
            continuity.reconnect_current(self.case, self.first)
        self.terminal.send.assert_called_once_with(b"\x1d")
        self.terminal.expect_exit.assert_called_once()

    def test_fresh_case_selects_strict_policy_for_its_built_source_pair(self):
        releases = SimpleNamespace(builds={self.first: self.build})
        with patch.object(installed_fresh.installed_adoption.AdoptionCase, "__init__", return_value=None) as initialize:
            case = installed_fresh.FreshCase(self.root, None, releases)
        self.assertFalse(case.legacy_attachment)
        pair = initialize.call_args.args[2]
        self.assertEqual(pair["daemon"]["executable"], self.build["path"])
        self.assertEqual(pair["daemon"]["artifact"]["binarySha256"], self.build["digest"])

    def test_fresh_producer_builds_both_current_releases_and_proves_their_edge(self):
        releases, origin = Mock(), Mock()
        releases.builds = {self.first: self.build}
        with patch.object(installed_fresh.fleet_baselines, "load", return_value={"hosts": [{"platform": "linux/amd64", "daemon": "old"}]}), \
                patch.object(installed_fresh.fleet_baselines, "acquire", return_value={"executable": "old-image"}), \
                patch.object(installed_fresh, "run_fresh_case") as consume:
            installed_fresh.run(self.root, self.root, "linux/amd64", releases, origin)
        self.assertEqual(releases.build.call_args_list, [unittest.mock.call(self.first), unittest.mock.call(self.target)])
        self.assertEqual(releases.prove.call_args_list[1], unittest.mock.call(self.build["path"], self.target))
        consume.assert_called_once_with(self.root, self.root, releases, origin, self.target)

    def test_chain_bootstrap_explicitly_reconnects_before_current_hops(self):
        case = Mock()
        case.validate_result.return_value = {"running": self.build}
        case.start_adoption.return_value = {"pin": "metadata-pin"}
        releases = SimpleNamespace(registry="registry", probe="probe", directory=self.root, native_only=False)
        origin = Mock()
        with patch.object(installed_chain.installed_adoption, "AdoptionCase", return_value=case), \
                patch.object(continuity, "reconnect_current", return_value={"strictContinuityPassed": True}) as reconnect, \
                patch.object(installed_chain, "await_chain") as hop:
            self.run_chain(case, releases, origin)
        reconnect.assert_called_once_with(case, self.first)
        case.initialize_preserved_baseline.assert_not_called()
        hop.assert_called_once()

    def test_ordered_host_explicitly_reconnects_after_adoption(self):
        host = object.__new__(installed_order_host.Host)
        host.case = Mock()
        host.case.start_adoption.return_value = {"pin": "metadata-pin"}
        helper = {"pid": 1234, "inode": 22, "device": 11, "sha256": "d" * 64}
        host.case.validate_result.return_value = {"attachment": {"strictContinuityPassed": None}, "helper": helper}
        with patch.object(continuity, "reconnect_current", return_value={"scope": "current-to-current"}) as reconnect:
            result = host.dispatch_operation("adopt", {"version": self.first})
        reconnect.assert_called_once_with(host.case, self.first)
        self.assertEqual(result["explicitReconnect"], {"scope": "current-to-current"})
        self.assertIsNone(result["attachment"]["strictContinuityPassed"])
        self.assertEqual(host.helper_image, helper)

    def test_public_intermediate_is_authenticated_before_strict_image_inspection(self):
        case = self.case
        case.ready = Mock(return_value=True)
        case.releases.builds = {}
        case.units = SimpleNamespace(managers={"helper": SimpleNamespace(manager_pid=lambda: 201)})
        case.cohort = SimpleNamespace(verify=Mock(return_value=["original-worker"]))
        case.inspect_retained = Mock(side_effect=lambda: self.assertEqual(case.releases.builds[self.first], self.build))
        directory = case.state / "update/helper"
        directory.mkdir(parents=True)
        (directory / "installed.json").write_text(json.dumps({"digest": self.digest, "executable": str(self.source)}))
        expected = {"version": self.first, "binary": self.digest}
        record, seen, events = {"hops": []}, set(), []
        self.mapped.side_effect = lambda pid, _path: {"pid": pid, "sha256": self.digest}
        with patch.object(installed_publication, "next_expected_hop", return_value=expected), \
                patch.object(installed_publication, "authenticate_hop", side_effect=lambda *_: events.append("authenticated")), \
                patch.object(installed_publication, "extend_expected_route"), patch.object(installed_publication.os, "kill"), \
                patch.object(installed_publication.release_worker_completion, "hop_label", return_value="native-hop"):
            installed_publication.observe_published_hop(case, record, "baseline", "target", seen)
        self.assertEqual(events, ["authenticated"])
        case.inspect_retained.assert_called_once_with()
        self.assertEqual(seen, {self.first})

    def run_chain(self, case, releases, origin):
        with patch.object(installed_chain, "finish"), patch.object(installed_chain.installed_transport_lifecycle, "prove"):
            installed_chain.run_case(self.root, self.root, {}, {}, None, origin, releases, None, "none")


class AttachmentEvidenceTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="attachment-control-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.case = SimpleNamespace(root=self.root, state=self.root / "state", session="ABCD",
            worker=102, shell=103, listener={"pid": 104}, process_stamps={102: "worker-birth", 103: "shell-birth"},
            terminal=SimpleNamespace(pid=101, snapshot=Mock(return_value=b"disconnected"), poll=Mock(return_value=0), expect=Mock()))

    def test_success_preserves_marker_and_performs_no_capture(self):
        with patch.object(evidence, "capture") as capture:
            evidence.expect(self.case, "exact-token", 42)
        self.case.terminal.expect.assert_called_once_with("__RESUMED__exact-token__", since=42)
        capture.assert_not_called()

    def test_failure_preserves_original_exception_despite_collector_error(self):
        original = RuntimeError("client exited with status 0")
        self.case.terminal.expect.side_effect = original
        with patch.object(evidence, "capture", side_effect=ValueError("capture failed")) as capture:
            with self.assertRaises(RuntimeError) as raised:
                evidence.expect(self.case, "token", 42)
        self.assertIs(raised.exception, original)
        capture.assert_called_once_with(self.case, "token", 42)

    def test_capture_retains_original_client_and_uses_bounded_child(self):
        result = SimpleNamespace(returncode=0, stdout='{"schema":1,"client":{"pid":101,"exitStatus":0}}')
        with patch.object(evidence.subprocess, "run", return_value=result) as run:
            evidence.capture(self.case, "token", 42)
        expected = json.loads(run.call_args.kwargs["input"])
        self.assertEqual(expected["processes"], {"client": 101, "worker": 102, "shell": 103, "listener": 104})
        self.assertEqual(expected["challenge"], {"token": "token", "clientOutputStart": 42})
        self.assertEqual(base64.b64decode(expected["transcript"]), b"disconnected")
        self.assertTrue(any(frame["function"] == "capture" for frame in expected["resumeStack"]))
        self.assertGreater(run.call_args.kwargs["timeout"], 0)
        self.assertLessEqual(run.call_args.kwargs["timeout"], 2)
        self.assertEqual(run.call_args.args[0][1:], ["-m", "fixtures.installed_attachment_evidence"])
        self.assertEqual(run.call_args.kwargs["cwd"], Path(evidence.__file__).resolve().parents[1])
        self.assertEqual(json.loads((self.root / evidence.FAILURE).read_text())["client"]["exitStatus"], 0)
        self.case.terminal.snapshot.assert_called_once_with()
        self.case.terminal.poll.assert_called_once_with()

    def test_expired_clock_skips_child_and_records_unavailable(self):
        with patch.object(evidence.installed_clock, "timeout", side_effect=evidence.installed_clock.ClockExpired("expired")), \
                patch.object(evidence.subprocess, "run") as run:
            evidence.capture(self.case, "token", 42)
        run.assert_not_called()
        self.assertEqual(json.loads((self.root / evidence.FAILURE).read_text())["errorType"], "ClockExpired")

    def test_oversize_child_output_is_not_retained(self):
        result = SimpleNamespace(returncode=0, stdout="x" * (evidence.MAXIMUM + 1))
        with patch.object(evidence.subprocess, "run", return_value=result):
            evidence.capture(self.case, "token", 42)
        self.assertLess((self.root / evidence.FAILURE).stat().st_size, 256)

    def test_collect_retains_live_metadata_and_bounded_pty_without_commands(self):
        session = self.case.state / "s" / self.case.session
        session.mkdir(parents=True)
        (session / "meta.json").write_text(json.dumps({"id": "ABCD", "pid": 103, "state": "detached", "command": ["private"]}))
        (session / "worker.log").write_bytes(b"x" * 10000 + b"__RESUMED__token__")
        update = self.case.state / "update"
        update.mkdir()
        (update / "installation.json").write_text(json.dumps({"phase": "committed", "request": {
            "manifest": {"version": "v0.1.9002"}, "private": "omitted"}}))
        expected = {"state": str(self.case.state), "session": "ABCD", "client": {"pid": 101, "exitStatus": 0},
            "processes": {"client": 101, "worker": 102}, "stamps": {}, "challenge": {"token": "token"},
            "transcript": base64.b64encode(b"client EOF").decode(), "resumeStack": []}
        with patch.object(evidence.evidence, "process", side_effect=lambda pid: {"pid": pid, "status": "live"}):
            record = evidence.collect(expected)
        self.assertEqual(record["workerMeta"], {"id": "ABCD", "pid": 103, "state": "detached"})
        self.assertEqual(record["journal"], {"phase": "committed", "version": "v0.1.9002"})
        self.assertEqual(len(record["workerPtyTail"]), 8192)
        self.assertTrue(record["workerPtyTail"].endswith("__RESUMED__token__"))
        self.assertEqual(record["clientPtyTail"], "client EOF")
        self.assertFalse(record["limits"]["signals"])
        self.assertFalse(record["limits"]["controlRequests"])

    def test_native_resume_replaces_challenge_once_and_keeps_original_terminal(self):
        terminal = self.case.terminal
        self.case.challenge = self.root / "listener.challenge"
        terminal.drain = Mock(return_value=b"old-output")
        with patch.object(evidence, "expect") as expect:
            installed_native.Installation.wait_attachment_resume(self.case)
        self.assertIs(self.case.terminal, terminal)
        expect.assert_called_once_with(self.case, self.case.challenge.read_text(), 10)
        self.assertFalse(self.case.challenge.with_suffix(".tmp").exists())

    def test_retention_uses_existing_native_log_path(self):
        (self.root / evidence.FAILURE).write_text('{"schema":1}')
        output = self.root / "output"
        output.mkdir()
        installed_native.retain_logs(self.root, output, "pc-chain")
        self.assertEqual((output / ("pc-chain-" + evidence.FAILURE)).read_text(), '{"schema":1}')

    def test_client_stderr_is_retained_without_changing_terminal_bytes_or_exit_status(self):
        command = [sys.executable, "-c", "import os; os.write(1, b'valid terminal bytes'); os.write(2, b'transport: stage=peer-acceptance error=EOF\\n')"]
        terminal = installed_native.terminal_module.Terminal(command, {}, self.root, stderr_path=self.root / "client.err")
        self.addCleanup(terminal.close)
        terminal.expect_exit()
        self.assertEqual(terminal.poll(), 0)
        self.assertEqual(terminal.drain(), b"valid terminal bytes")
        output = self.root / "retained"
        output.mkdir()
        (self.root / "daemon.err").write_bytes(b"daemon: stage=client-stream-read error=EOF\n")
        installed_native.retain_logs(self.root, output, "pc-chain")
        self.assertEqual((output / "pc-chain-client.err").read_bytes(), b"transport: stage=peer-acceptance error=EOF\n")
        self.assertEqual((output / "pc-chain-daemon.err").read_bytes(), (self.root / "daemon.err").read_bytes())

    def test_child_entry_point_reads_only_synthetic_fixture_files(self):
        expected = {"state": str(self.root / "absent-state"), "session": "ABCD", "client": {"exitStatus": 0},
            "processes": {}, "stamps": {}, "challenge": {"token": "token"},
            "transcript": base64.b64encode(b"original client transcript").decode(), "resumeStack": []}
        result = subprocess.run([sys.executable, "-m", "fixtures.installed_attachment_evidence"],
            cwd=Path(evidence.__file__).resolve().parents[1], input=json.dumps(expected),
            capture_output=True, text=True, timeout=2, check=True)
        record = json.loads(result.stdout)
        self.assertEqual(record["clientPtyTail"], "original client transcript")
        self.assertEqual(record["processes"], {})
        self.assertEqual(record["workerMeta"]["errorType"], "FileNotFoundError")
