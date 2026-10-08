from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from enrollment_snapshot import assert_unchanged, snapshot


class EnrollmentSnapshotTest(unittest.TestCase):
    def test_missing_key_differs_from_an_absent_entry(self):
        for before, after in (({"vanished": None}, {}), ({}, {"vanished": None})):
            with self.subTest(before=before):
                with self.assertRaisesRegex(AssertionError, "vanished: .*absent|vanished: .*unlisted"):
                    assert_unchanged(before, after, "enrollment")

    def test_disappearing_listed_entry_is_reported(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            vanished = root / "vanished"
            vanished.touch()

            def disappearing_paths(_root, _pattern):
                vanished.unlink()
                yield vanished

            with patch.object(Path, "rglob", disappearing_paths):
                with self.assertRaisesRegex(AssertionError, "fixture entry disappeared during snapshot: .*vanished"):
                    snapshot(root)

    def test_absent_root_is_retained(self):
        with tempfile.TemporaryDirectory() as directory:
            absent = Path(directory) / "absent"
            self.assertEqual(snapshot(absent), {str(absent): None})

    def test_content_mode_and_directory_changes_are_detected(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            key = root / "identity.key"
            key.write_bytes(b"private-one")
            key.chmod(0o600)
            before = snapshot(root)
            key.write_bytes(b"private-two")
            with self.assertRaisesRegex(AssertionError, "contents changed=True") as result:
                assert_unchanged(before, snapshot(root), "enrollment")
            self.assertNotIn("private", str(result.exception).split("identity.key:")[1])
            key.write_bytes(b"private-one")
            key.chmod(0o644)
            with self.assertRaisesRegex(AssertionError, "mode="):
                assert_unchanged(before, snapshot(root), "enrollment")
            key.chmod(0o600)
            (root / "unexpected").mkdir()
            with self.assertRaisesRegex(AssertionError, "unexpected: unlisted"):
                assert_unchanged(before, snapshot(root), "enrollment")


if __name__ == "__main__":
    unittest.main()
