#!/usr/bin/env python3
"""Unit tests for the pure logic in scripts/bench/oneshot.py."""

from pathlib import Path
import sys
import unittest

sys.dont_write_bytecode = True
sys.path.insert(0, str(Path(__file__).resolve().parent))
import oneshot  # noqa: E402 - adjacent benchmark module
import ssh_command  # noqa: E402 - adjacent benchmark module


def make_pairs(mesh_seconds, ssh_seconds):
    return [
        {"order": ["mesh", "ssh"], "mesh": {"seconds": m}, "ssh": {"seconds": s}}
        for m, s in zip(mesh_seconds, ssh_seconds)
    ]


class OneshotPureLogicTests(unittest.TestCase):
    def test_parse_hosts_normalizes_pc_and_deduplicates(self):
        self.assertEqual(oneshot.parse_hosts("pc, vps, pi, mac, local"), ["local", "vps", "pi", "mac"])
        self.assertEqual(oneshot.parse_hosts("VPS,mac"), ["vps", "mac"])

    def test_parse_hosts_rejects_unknown_or_empty(self):
        for bad in ("", " , ", "local,bogus"):
            with self.assertRaises(ValueError):
                oneshot.parse_hosts(bad)

    def test_required_platforms_always_includes_pc_client(self):
        self.assertEqual(oneshot.required_platforms(["mac"]), ["darwin/arm64", "linux/amd64"])
        self.assertEqual(oneshot.required_platforms(["pi"]), ["linux/amd64", "linux/arm64"])
        self.assertEqual(oneshot.required_platforms(["local", "vps"]), ["linux/amd64"])
        self.assertEqual(
            oneshot.required_platforms(["local", "vps", "pi", "mac"]),
            ["darwin/arm64", "linux/amd64", "linux/arm64"],
        )

    def test_parse_tailscale_ping_handles_all_units(self):
        stdout = "\n".join([
            "pong from vps (100.84.25.68) via 37.27.17.186:41641 in 80ms",
            "pong from vps (100.84.25.68) via 37.27.17.186:41641 in 79.5ms",
            "pong from pi (100.94.222.118) via 192.168.0.179:41641 in 500µs",
            "pong from slow (100.94.222.119) via derp in 1.2s",
        ])
        parsed = oneshot.parse_tailscale_ping(stdout)
        self.assertTrue(parsed["available"])
        self.assertEqual(parsed["samples_ms"], [80.0, 79.5, 0.5, 1200.0])
        self.assertEqual(parsed["summary_ms"]["min"], 0.5)
        self.assertEqual(parsed["summary_ms"]["p50"], 79.75)
        self.assertEqual(parsed["summary_ms"]["max"], 1200.0)

    def test_parse_tailscale_ping_unavailable_on_empty(self):
        parsed = oneshot.parse_tailscale_ping("timeout waiting for pong\n")
        self.assertFalse(parsed["available"])
        self.assertEqual(parsed["samples_ms"], [])

    def test_parse_prefixed_json(self):
        self.assertIsNone(oneshot.parse_prefixed_json("noise line\n", oneshot.READY_PREFIX))
        self.assertEqual(
            oneshot.parse_prefixed_json('ONESHOT_READY {"port":1234}\n', oneshot.READY_PREFIX),
            {"port": 1234},
        )

    def test_drop_warmups(self):
        warm, measured = oneshot.drop_warmups([1, 2, 3, 4, 5], 3)
        self.assertEqual(warm, [1, 2, 3])
        self.assertEqual(measured, [4, 5])
        with self.assertRaises(ValueError):
            oneshot.drop_warmups([1], -1)

    def test_validate_mcp_exec_reply_accepts_completed_zero(self):
        good = {
            "jsonrpc": "2.0",
            "id": 1,
            "result": {
                "isError": False,
                "structuredContent": {
                    "data": {
                        "state": "exited",
                        "wait": "completed",
                        "result": {"termination": {"kind": "exit", "code": 0}},
                    }
                },
            },
        }
        self.assertEqual(oneshot.validate_mcp_exec_reply(good)["state"], "exited")

    def test_validate_mcp_exec_reply_rejects_failures(self):
        bad_cases = [
            {"error": {"code": -32603, "message": "boom"}},
            {"result": {"isError": True, "structuredContent": {}}},
            {"result": {"isError": False, "structuredContent": {"data": {"state": "running", "wait": "timed_out"}}}},
            {"result": {"isError": False, "structuredContent": {
                "data": {"state": "exited", "wait": "completed", "result": {"termination": {"code": 1}}}
            }}},
        ]
        for case in bad_cases:
            with self.assertRaises(RuntimeError):
                oneshot.validate_mcp_exec_reply(case)

    def test_validate_server_wait_reply(self):
        good = {
            "type": "execution.receipt",
            "executionWait": "completed",
            "executionReceipt": {"result": {"termination": {"kind": "exit", "code": 0}}},
        }
        self.assertEqual(oneshot.validate_server_wait_reply(good)["result"]["termination"]["code"], 0)
        for bad in (
            {"type": "error", "error": "boom"},
            {"type": "execution.receipt", "executionWait": "timed_out", "executionReceipt": {}},
            {"type": "execution.receipt", "executionWait": "completed",
             "executionReceipt": {"result": {"termination": {"kind": "exit", "code": 2}}}},
        ):
            with self.assertRaises(RuntimeError):
                oneshot.validate_server_wait_reply(bad)

    def test_match_scratch_pids_only_matches_exact_scratch_and_skips_ancestors(self):
        rows = [
            (100, "/home/pi/mesh-oneshot-bench-abcd/mesh daemon"),
            (101, "/home/pi/.local/bin/mesh session-worker --dir /home/pi/.local/state/mesh/s/REAL"),
            (102, "/home/pi/mesh-oneshot-bench-abcd/mesh session-worker --dir /home/pi/mesh-oneshot-bench-abcd/state/s/X"),
            (103, "python3 /home/pi/mesh-oneshot-bench-abcd/oneshot.py --remote-agent"),
            (104, "/home/pi/mesh-oneshot-bench-other/mesh daemon"),
        ]
        matched = oneshot.match_scratch_pids(rows, "/home/pi/mesh-oneshot-bench-abcd", exclude_pids={103})
        self.assertEqual(matched, [100, 102])

    def test_summarize_host_slowdown_and_pass_verdicts(self):
        slow = oneshot.summarize_host(
            make_pairs([0.35] * 30, [0.05] * 30),
            [{"create_seconds": 0.1, "wait_seconds": 0.05, "total_seconds": 0.15}] * 30,
            True,
            ssh_command.summarize,
        )
        self.assertEqual(slow["promise_verdict"], "slowdown")
        self.assertFalse(slow["at_or_under_ssh_p50"])
        self.assertAlmostEqual(slow["comparison"]["p50_ratio"], 7.0)
        self.assertAlmostEqual(slow["comparison"]["p50_delta_ms"], 300.0)
        self.assertAlmostEqual(slow["server_ms"]["total_ms"]["p50"], 150.0)

        fast = oneshot.summarize_host(
            make_pairs([0.04] * 30, [0.05] * 30),
            [{"create_seconds": 0.01, "wait_seconds": 0.01, "total_seconds": 0.02}] * 30,
            True,
            ssh_command.summarize,
        )
        self.assertEqual(fast["promise_verdict"], "pass")
        self.assertTrue(fast["at_or_under_ssh_p50"])

        within_margin = oneshot.summarize_host(
            make_pairs([0.04] * 30, [0.02] * 30),
            [],
            True,
            ssh_command.summarize,
        )
        # 2.0x ratio but only +20 ms delta (< 50 ms margin): passes promise policy, but > SSH p50.
        self.assertEqual(within_margin["promise_verdict"], "pass")
        self.assertFalse(within_margin["at_or_under_ssh_p50"])

    def test_summarize_host_when_ssh_unavailable(self):
        summary = oneshot.summarize_host(
            [{"order": ["mesh"], "mesh": {"seconds": 0.12}} for _ in range(5)],
            [{"create_seconds": 0.05, "wait_seconds": 0.03, "total_seconds": 0.08}] * 5,
            False,
            ssh_command.summarize,
        )
        self.assertIsNone(summary["warm_ssh_ms"])
        self.assertIsNone(summary["comparison"])
        self.assertEqual(summary["promise_verdict"], "n/a")
        self.assertIsNone(summary["at_or_under_ssh_p50"])
        self.assertAlmostEqual(summary["mesh_mcp_ms"]["p50"], 120.0)

    def test_build_mac_queue_command_uses_fifo_runner(self):
        cmd = oneshot.build_mac_queue_command(
            ["/usr/bin/python3", "/Users/shaul/mesh-oneshot-bench-1/oneshot.py", "--remote-agent"],
            lane="mesh-oneshot-bench",
            thread="t-1",
            expected_seconds=300,
            wait_seconds=600,
            timeout_seconds=600,
        )
        self.assertIn("mac-queue.ts run", cmd)
        self.assertIn("--lane mesh-oneshot-bench", cmd)
        self.assertIn("-- /usr/bin/python3 /Users/shaul/mesh-oneshot-bench-1/oneshot.py --remote-agent", cmd)

    def test_isolated_env_sets_private_permissions(self):
        import os
        import tempfile
        with tempfile.TemporaryDirectory(prefix="oneshot-env-test-") as directory:
            root = Path(directory)
            old_umask = os.umask(0o002)
            try:
                _env, state, config = oneshot.isolated_env(root)
            finally:
                os.umask(old_umask)
            self.assertEqual(state.stat().st_mode & 0o777, 0o700)
            self.assertEqual(config.stat().st_mode & 0o777, 0o700)

    def test_format_markdown_table_renders_ok_and_error_rows(self):
        summary = oneshot.summarize_host(
            make_pairs([0.20] * 30, [0.05] * 30),
            [{"create_seconds": 0.08, "wait_seconds": 0.04, "total_seconds": 0.12}] * 30,
            True,
            ssh_command.summarize,
        )
        report = {
            "hosts": {
                "vps": {
                    "status": "ok",
                    "rtt": {"available": True, "summary_ms": {"p50": 80.0}},
                    "ssh": {"available": True, "alias": "vps"},
                    "summary": summary,
                },
                "pi": {"status": "error", "error": "unreachable\nsecond line | pipe"},
            }
        }
        table = oneshot.format_markdown_table(report)
        self.assertIn("| vps | 80.0 | 50.0 / 50.0 (50.0–50.0) | 200.0 / 200.0 (200.0–200.0) | 4.00x | +150.0 ms | 80.0 / 40.0 / 120.0 | slowdown | no |", table)
        self.assertIn("| pi | error | error | error | n/a | n/a | n/a | error (unreachable) | n/a |", table)


if __name__ == "__main__":
    unittest.main()
