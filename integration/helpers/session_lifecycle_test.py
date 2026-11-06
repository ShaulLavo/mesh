#!/usr/bin/env python3
"""Keep lifecycle assertions sensitive to missing observations and stale sockets."""
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import Mock, patch

from session_lifecycle import LifecycleFixture, close_owned, exercise_owned, marker
from terminal_window import ACTIVE_CLEANUP, Fixture


class LifecycleTest(unittest.TestCase):
    def test_restart_does_not_accept_a_stale_daemon_socket(self):
        fixture = LifecycleFixture.__new__(LifecycleFixture)
        fixture.daemon = Mock()
        fixture.daemon.poll.return_value = None
        fixture.daemon_listing = Mock(side_effect=[ConnectionRefusedError(), {}])
        with patch.object(Fixture, "start_local_daemon") as start:
            fixture.start_local_daemon()
        start.assert_called_once_with()
        self.assertEqual(fixture.daemon_listing.call_count, 2)

    def test_daemon_readiness_requires_a_live_daemon(self):
        fixture = LifecycleFixture.__new__(LifecycleFixture)
        fixture.daemon = Mock()
        fixture.daemon.poll.return_value = 1
        with self.assertRaisesRegex(RuntimeError, "daemon exited before readiness"):
            fixture.daemon_ready()

    def test_marker_cannot_be_satisfied_by_the_echoed_input(self):
        terminal = Mock()
        terminal.drain.return_value = b"prior output"
        fixture = Mock()
        marker(fixture, terminal, "FRESH_OUTPUT")
        self.assertNotIn("FRESH_OUTPUT", terminal.send.call_args.args[0])
        terminal.expect.assert_called_once_with("FRESH_OUTPUT\r\n", since=len(b"prior output"))
        fixture.shell_identity.assert_called_once_with(terminal)

    def test_fixture_keeps_roots_for_the_existing_owner_even_on_failure(self):
        for fails in (False, True):
            with self.subTest(fails=fails), tempfile.TemporaryDirectory(prefix="u3-owner-") as root:
                self.check_owner_root(Path(root), fails)

    def check_owner_root(self, root, fails):
        home = root / "home"
        home.mkdir()
        fixture = Mock()
        def construct(_binary, directory):
            (directory / "retained-state").write_text("owned")
            return fixture
        scenario = Mock(side_effect=RuntimeError("observer failed") if fails else None)
        environment = {"HOME": str(home), "MESH_INTEGRATION_ENTRY": "registered-entry"}
        with patch.dict(os.environ, environment), patch("session_lifecycle.print_diagnostics"):
            self.run_scenario(construct, scenario, fails)
        fixture.close.assert_called_once_with()
        self.assertEqual((root / "f" / "retained-state").read_text(), "owned")

    def run_scenario(self, construct, scenario, fails):
        if not fails:
            exercise_owned(construct, "owned-binary", scenario)
            return
        with self.assertRaisesRegex(RuntimeError, "observer failed"):
            exercise_owned(construct, "owned-binary", scenario)

    def test_cleanup_shares_one_remaining_time_allowance(self):
        fixture = Mock()
        observed = []
        fixture.close.side_effect = lambda: observed.append(ACTIVE_CLEANUP.get())
        close_owned(fixture)
        self.assertEqual(len(observed), 1)
        self.assertIsNotNone(observed[0])
        self.assertIsNone(ACTIVE_CLEANUP.get())


if __name__ == "__main__":
    unittest.main()
