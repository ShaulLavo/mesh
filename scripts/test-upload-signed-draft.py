#!/usr/bin/env python3
"""Offline fake gh controls for a fixed physical upload set and bounded recovery."""
import contextlib
import copy
import hashlib
import importlib
import json
import os
import subprocess
import unittest
from unittest.mock import patch
from urllib.parse import parse_qs, urlsplit

uploader = importlib.import_module("upload-signed-draft")
controls = importlib.import_module("test-prepare-draft-intake")


class FakeCLI:
    def __init__(self, release):
        self.release = copy.deepcopy(release)
        self.calls, self.waits, self.faults = [], [], []
        self.clock = 1000
        self.counter = 100

    def sleep(self, seconds):
        self.waits.append(seconds)
        self.clock += seconds

    def response(self, status, body, headers=None):
        head = f"HTTP/2.0 {status} Fixture\r\n"
        head += "".join(f"{key}: {value}\r\n" for key, value in (headers or {}).items())
        return subprocess.CompletedProcess([], 0 if status < 400 else 1,
            (head + "\r\n" + json.dumps(body)).encode(), b"never expose API error or token")

    def run(self, command, **kwargs):
        method = command[command.index("--method") + 1]
        endpoint = command[command.index("--method") + 2]
        self.calls.append((method, endpoint, kwargs.get("input"), self.clock))
        self.assert_safe_command(command, kwargs)
        if self.faults and method == self.faults[0][0]:
            _, status, headers, body, commit = self.faults.pop(0)
            if commit:
                self.perform(method, endpoint, kwargs.get("input"))
            return self.response(status, body, headers)
        return self.response(204 if method == "DELETE" else 200, self.perform(method, endpoint, kwargs.get("input")))

    @staticmethod
    def assert_safe_command(command, kwargs):
        if "GH_DEBUG" in kwargs["env"] or "sentinel-token" in " ".join(command):
            raise AssertionError("token/debug data entered the command")

    def perform(self, method, endpoint, data):
        if method == "GET":
            if "/assets?" in endpoint:
                query = parse_qs(urlsplit(endpoint).query)
                page = int(query["page"][0])
                return copy.deepcopy(self.release["assets"][(page - 1) * 100:page * 100])
            return copy.deepcopy(self.release)
        if method == "DELETE":
            number = int(endpoint.rsplit("/", 1)[1])
            self.release["assets"] = [item for item in self.release["assets"] if item["id"] != number]
            return None
        name = parse_qs(urlsplit(endpoint).query)["name"][0]
        self.counter += 1
        item = {"name": name, "id": self.counter, "size": len(data), "state": "uploaded",
                "digest": "sha256:" + hashlib.sha256(data).hexdigest()}
        self.release["assets"].append(item)
        return item


class UploadTests(unittest.TestCase):
    def setUp(self):
        self.fixture = controls.DraftIntakeTests()
        self.fixture.setUp()
        self.addCleanup(self.fixture.doCleanups)
        self.root = self.fixture.root
        (self.root / "dist").mkdir()
        self.paths = []
        for name in uploader.release_documents.PHYSICAL:
            path = self.root / "dist" / name
            path.write_bytes(("fixed physical " + name).encode())
            self.paths.append(path)
        self.fake = FakeCLI(self.fixture.release)

    def execute(self):
        environment = {"GITHUB_REPOSITORY": "ShaulLavo/mesh", "GITHUB_RUN_ID": "42", "GH_TOKEN": "sentinel-token", "GH_DEBUG": "api"}
        with patch.dict(os.environ, environment), contextlib.chdir(self.root):
            with patch.object(uploader.subprocess, "run", side_effect=self.fake.run):
                self.clock_run()

    def clock_run(self):
        with patch.object(uploader.time, "monotonic", side_effect=lambda: self.fake.clock), patch.object(uploader.time, "time", side_effect=lambda: self.fake.clock):
            with patch.object(uploader.time, "sleep", side_effect=self.fake.sleep):
                uploader.Publisher(uploader.API(), self.fixture.directory, controls.VERSION, controls.SOURCE, self.paths).run()

    def uploaded(self):
        return [call for call in self.fake.calls if call[0] == "POST"]

    def test_exact_bytes_and_constant_requests_for_one_or_thousand_documents(self):
        counts = []
        for size in (1, 1000):
            self.fake = FakeCLI(self.fixture.release)
            self.paths[0].write_bytes(b"opaque signed bundle document\n" * size)
            self.execute()
            counts.append(len(self.fake.calls))
            self.assertEqual(len(self.uploaded()), 4)
            for call in self.uploaded():
                name = parse_qs(urlsplit(call[1]).query)["name"][0]
                self.assertEqual(call[2], (self.root / "dist" / name).read_bytes())
        self.assertEqual(counts, [12, 12])

    def test_partial_exact_matches_are_noop_and_mismatch_replaces_only_selected_asset(self):
        self.execute()
        original = copy.deepcopy(self.fake.release)
        self.fake.calls.clear()
        self.execute()
        self.assertEqual(self.fake.release, original)
        self.assertFalse(self.uploaded())
        self.assertEqual(len(self.fake.calls), 4)
        self.paths[-1].write_bytes(b"replacement exact bytes")
        self.fake.calls.clear()
        self.execute()
        self.assertEqual(len(self.uploaded()), 1)
        self.assertEqual(sum(call[0] == "DELETE" for call in self.fake.calls), 1)
        mutations = [call[3] for call in self.fake.calls if call[0] != "GET"]
        self.assertGreaterEqual(mutations[-1] - mutations[-2], 1)

    def test_committed_upload_failure_is_reconciled_without_delete_or_duplicate(self):
        self.fake.faults = [("POST", 503, {"retry-after": "90"}, {}, True)]
        self.execute()
        self.assertEqual(len(self.uploaded()), 4)
        self.assertNotIn("DELETE", [call[0] for call in self.fake.calls])
        self.assertIn(90, self.fake.waits)

    def test_502_starter_is_replaced_after_wait_and_inventory_recheck(self):
        name = self.paths[0].name
        self.fake.release["assets"].append({"id": 99, "name": name, "size": 0, "digest": None, "state": "starter"})
        self.fake.faults = [("DELETE", 502, {}, {}, False)]
        self.execute()
        self.assertEqual(sum(call[0] == "DELETE" for call in self.fake.calls), 2)
        self.assertIn(60, self.fake.waits)

    def test_rate_limits_honor_headers_and_exponential_wait_without_real_sleep(self):
        self.fake.faults = [("POST", 403, {"retry-after": "120"}, {"message": "You exceeded a secondary rate limit"}, False),
                            ("POST", 429, {"retry-after": "240"}, {}, False)]
        self.execute()
        self.assertIn(120, self.fake.waits)
        self.assertIn(240, self.fake.waits)
        self.assertEqual(len(self.uploaded()), 6)

    def test_primary_reset_and_permanent_permission_errors(self):
        self.fake.faults = [("GET", 403, {"x-ratelimit-remaining": "0", "x-ratelimit-reset": "1180"}, {}, False)]
        self.execute()
        self.assertIn(181, self.fake.waits)
        self.fake.faults = [("POST", 403, {"retry-after": "90"}, {"message": "Resource not accessible by integration"}, False)]
        self.paths[0].write_bytes(b"changed")
        with self.assertRaisesRegex(ValueError, "HTTP 403"):
            self.execute()
        self.assertEqual(self.fake.faults, [])

    def test_excessive_server_wait_and_repeated_failures_stop_bounded(self):
        self.fake.faults = [("POST", 429, {"retry-after": "3600"}, {}, False)]
        with self.assertRaisesRegex(ValueError, "remaining.*wall clock"):
            self.execute()
        self.assertEqual(len(self.uploaded()), 1)
        self.fake = FakeCLI(self.fixture.release)
        self.fake.faults = [("POST", 503, {}, {}, False)] * 5
        with self.assertRaisesRegex(ValueError, "retry bound"):
            self.execute()
        self.assertEqual(len(self.uploaded()), 3)

    def test_wrong_source_version_release_draft_run_or_candidate_fails_before_mutation(self):
        for key, value in (("id", 10), ("draft", False), ("tag_name", "v0.1.1"), ("target_commitish", "b" * 40)):
            self.fake = FakeCLI(dict(self.fixture.release, **{key: value}))
            with self.subTest(key=key), self.assertRaises(ValueError):
                self.execute()
            self.assertFalse(self.uploaded())
        self.fake = FakeCLI(self.fixture.release)
        self.fake.release["assets"][0]["digest"] = "sha256:" + "0" * 64
        with self.assertRaisesRegex(ValueError, "transition candidate"):
            self.execute()
        self.assertFalse(self.uploaded())


if __name__ == "__main__":
    unittest.main()
