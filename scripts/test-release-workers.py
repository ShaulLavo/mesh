#!/usr/bin/env python3
import hashlib
import json
from pathlib import Path
import tempfile
import unittest

from fixtures import release_workers


class ExactWorkerSelectionTests(unittest.TestCase):
    def test_source_applicability_does_not_require_future_worker_binaries(self):
        with tempfile.TemporaryDirectory(prefix="workers-") as directory:
            root = Path(directory)
            source = root / "source"
            source.write_bytes(b"original source worker")
            digest = hashlib.sha256(source.read_bytes()).hexdigest()
            registry = {"schema": 1, "platform": {"os": "linux", "arch": "amd64"}, "workers": [
                {"version": "v0.1.249", "protocol": 1, "binary": "source", "artifact": {"binarySha256": digest}},
                {"version": "v0.1.255", "protocol": 1, "binary": "future-must-not-open", "artifact": {"binarySha256": "a" * 64}},
            ]}
            path = root / "workers.json"
            path.write_text(json.dumps(registry))
            selected = release_workers.applicable(path, "v0.1.249", source)
            self.assertEqual([row["version"] for row in selected], ["v0.1.249"])
            source.write_bytes(b"changed original")
            with self.assertRaisesRegex(ValueError, "executable changed"):
                release_workers.applicable(path, "v0.1.249", source)


if __name__ == "__main__":
    unittest.main()
