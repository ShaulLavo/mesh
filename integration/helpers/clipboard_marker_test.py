#!/usr/bin/env python3
"""U4 emission and ownership contracts; native controls run in the scenario."""

import json
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch

import owned_command
import terminal_window as window


class ClipboardEmissionTest(unittest.TestCase):
    def emission(self, fault):
        with patch.object(window.os, "write") as write:
            window.clipboard_instruction("emit", fault)
        write.assert_called_once()
        self.assertEqual(write.call_args.args[0], 1)
        return write.call_args.args[1]

    def test_exact_harmless_sequence_precedes_visible_completion(self):
        self.assertEqual(self.emission("none"), window.CLIPBOARD_OSC + window.CLIPBOARD_DONE + b"\n")
        self.assertEqual(window.CLIPBOARD_OSC, b"\x1b]52;c;bWVzaC1maXh0dXJlLW9ubHk=\x07")

    def test_permanent_omitted_and_wrong_emissions_keep_the_completion(self):
        for fault in ("omit", "wrong"):
            output = self.emission(fault)
            self.assertNotIn(window.CLIPBOARD_OSC, output)
            self.assertTrue(output.endswith(window.CLIPBOARD_DONE + b"\n"))

    def test_absent_ready_does_not_invent_a_response(self):
        with patch.object(window.os, "write") as write:
            window.clipboard_instruction("fresh", "absent-ready")
        write.assert_not_called()

    def test_identity_reports_fixture_environment_without_changing_it(self):
        environment = {"MESH_SESSION_ID": "7K3D", "SSH_CONNECTION": "unexpected", "DISPLAY": ":9", "OTHER": "kept"}
        with patch.dict(window.os.environ, environment, clear=True), patch.object(window.os, "write") as write:
            window.clipboard_instruction("identity", "none")
            self.assertEqual(dict(window.os.environ), environment)
        output = write.call_args.args[1]
        identity = json.loads(output.removeprefix(b"__IDENTITY__").removesuffix(b"__END__\n"))
        self.assertEqual(identity["sid"], "7K3D")
        self.assertEqual(identity["ssh"], {"SSH_CONNECTION": "unexpected"})
        self.assertEqual(identity["display"], {"DISPLAY": ":9"})


class ClipboardReadinessTest(unittest.TestCase):
    def run_failed_case(self, error):
        with tempfile.TemporaryDirectory(prefix="m-clipboard-ready-") as directory, \
                patch.object(window, "ClipboardFixture") as fixture, \
                patch.object(window, "clipboard_marker", side_effect=error), \
                patch.object(window, "print_diagnostics"), patch("builtins.print"):
            self.checked_failed_case(fixture.return_value, Path(directory))

    def checked_failed_case(self, fixture, directory):
        try:
            window.clipboard_case("/bin/true", directory, window.CleanupDeadline(25), "absent-ready")
        finally:
            fixture.close.assert_called_once()

    def test_absent_ready_rejects_a_client_exit_even_when_the_error_names_the_marker(self):
        error = RuntimeError("terminal client exited with status 17 before printing b'__FRESH_READY__'")
        self.assertRaisesRegex(RuntimeError, "status 17", self.run_failed_case, error)

    def test_absent_ready_rejects_unrelated_marker_diagnostics(self):
        error = RuntimeError("unrelated failure mentioning __FRESH_READY__")
        self.assertRaisesRegex(RuntimeError, "unrelated failure", self.run_failed_case, error)

    def test_missing_ready_timeout_requires_a_fresh_identity_acknowledgement(self):
        fixture, terminal = Mock(), Mock(timeout=0)
        self.assertRaises(window.ClipboardReadinessTimeout, window.clipboard_expect_ready, fixture, terminal, 0)
        fixture.identity.assert_called_once_with(terminal)

    def test_failed_identity_acknowledgement_is_not_a_missing_ready_timeout(self):
        fixture, terminal = Mock(), Mock(timeout=0)
        fixture.identity.side_effect = RuntimeError("identity client died")
        self.assertRaisesRegex(RuntimeError, "identity client died", window.clipboard_expect_ready, fixture, terminal, 0)

    def test_nonzero_client_exit_is_not_a_readiness_timeout(self):
        fixture, terminal = Mock(), Mock(timeout=1)
        terminal.poll.return_value = 17
        self.assertRaisesRegex(RuntimeError, "status 17", window.clipboard_expect_ready, fixture, terminal, 0)
        fixture.identity.assert_not_called()

    def test_absent_ready_accepts_only_the_typed_timeout(self):
        self.assertTrue(window.clipboard_expected_failure("absent-ready", window.ClipboardReadinessTimeout("missing")))
        self.assertFalse(window.clipboard_expected_failure("absent-ready", RuntimeError("__FRESH_READY__")))


class ClipboardOwnershipTest(unittest.TestCase):
    def outcome(self, proved, status):
        with tempfile.TemporaryDirectory(prefix="m-clipboard-contract-") as directory:
            root = Path(directory) / "controller"
            root.mkdir()
            controller = Mock()
            controller.proved.return_value = proved
            controller.check.return_value = None
            controller.start.return_value = SimpleNamespace(outcome={"returncode": status, "state": "joined" if proved else "unknown"})
            with patch.object(window.tempfile, "mkdtemp", return_value=str(root)), \
                    patch.object(owned_command, "Controller", return_value=controller), \
                    patch.object(owned_command, "Triggers"), patch("builtins.print"):
                result = window.run_owned_clipboard("/bin/true")
            controller.wait.assert_called_once_with([controller.start.return_value])
            self.assertEqual(root.exists(), not proved)
            admission = controller.start.call_args.kwargs
            self.assertEqual(admission["budget"], 25)
            self.assertEqual(admission["scratch_parent"], str(root))
            self.assertEqual(admission["command"][-2:], ["--clipboard-owned", str(Path("/bin/true").resolve())])
            return result

    def test_unknown_owner_or_launcher_exit_keeps_the_root(self):
        self.assertEqual(self.outcome(False, None), 1)

    def test_joined_failed_case_preserves_its_failure_status(self):
        self.assertEqual(self.outcome(True, 1), 1)

    def test_joined_success_removes_the_controller_root(self):
        self.assertEqual(self.outcome(True, 0), 0)

    def test_native_attach_names_only_the_existing_sid(self):
        fixture = window.ClipboardFixture.__new__(window.ClipboardFixture)
        fixture.binary, fixture.sid, fixture.root = "/fixture/mesh", "7K3D", Path("/fixture")
        fixture.environment, fixture.resources, fixture.terminals = {}, {"clients": []}, []
        fixture.deadline = window.CleanupDeadline(25)
        fixture.record_resources, fixture.identity = Mock(), Mock()
        with patch.object(window, "Terminal") as terminal:
            fixture.attach_destination()
        self.assertEqual(terminal.call_args.args[0], ["/fixture/mesh", "7K3D"])
        self.assertIsNone(fixture.resources["clients"][0]["lastSeq"])
        self.assertIs(window.SCENARIOS["clipboard-marker"], window.clipboard_marker)


if __name__ == "__main__":
    unittest.main()
