#!/usr/bin/env python3
"""Standalone integration entry points must not read the caller's address book."""

import ctypes
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest


class StandaloneIsolationTest(unittest.TestCase):
    def test_scripts_do_not_read_decoy_hosts(self):
        binary = os.environ["MESH"]
        repo = Path(__file__).resolve().parents[2]
        failures = []
        for name in ("kill_waits.sh", "logs_does_not_attach.sh", "survives_client_death.sh"):
            with tempfile.TemporaryDirectory(prefix="m-decoy-") as temporary:
                root = Path(temporary)
                home = root / "home"
                config = home / ".config" / "mesh"
                config.mkdir(parents=True)
                hosts = config / "hosts.json"
                hosts.write_text(json.dumps({"version": 1, "hosts": []}))
                os.utime(hosts, ns=(1, hosts.stat().st_mtime_ns))
                watcher = None
                if sys.platform == "linux":
                    libc = ctypes.CDLL(None, use_errno=True)
                    watcher = libc.inotify_init1(os.O_NONBLOCK | os.O_CLOEXEC)
                    self.assertGreaterEqual(watcher, 0)
                    self.assertGreaterEqual(libc.inotify_add_watch(watcher, os.fsencode(hosts), 0x1), 0)
                    self.addCleanup(os.close, watcher)
                    hosts.read_bytes()
                    self.assertTrue(os.read(watcher, 4096), "inotify must detect a control read")
                environment = {key: os.environ[key] for key in ("PATH", "TERM", "LANG") if key in os.environ}
                environment.update(HOME=str(home), TMPDIR=str(root), MESH=binary)
                result = subprocess.run(["bash", str(repo / "integration" / name)],
                                        env=environment, cwd=repo, capture_output=True, text=True, timeout=30)
                if watcher is None:
                    read = hosts.stat().st_atime_ns != 1
                else:
                    try:
                        read = bool(os.read(watcher, 4096))
                    except BlockingIOError:
                        read = False
                print(f"DECOY {name}: exit={result.returncode}, hosts_read={read}", flush=True)
                if result.returncode or read:
                    failures.append(f"{name}: exit={result.returncode}, hosts_read={read}\n{result.stdout}{result.stderr}")
        self.assertFalse(failures, "\n".join(failures))


if __name__ == "__main__":
    unittest.main()
