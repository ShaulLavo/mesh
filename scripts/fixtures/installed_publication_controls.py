"""Direct controls for the published route's release gate.

authenticate_hop is the only check before a published build enters
releases.builds, and the other fixture tests mock it. These tests run the real
function against controlled signed bytes, and run register_running_build's
known-version and rollback rules end to end through validate_published_route."""
import json
from pathlib import Path
import tempfile
import unittest
from types import SimpleNamespace
from unittest.mock import Mock, patch

from fixtures import installed_continuity, installed_native, installed_publication, published_releases

VIA = "v0.1.332"
DRAFT = "v0.1.333"
PLATFORM = {"os": "linux", "arch": "amd64"}
REJECTED_BYTES = "observed intermediate bytes differ from the authenticated route plan"
REJECTED_ARTIFACT = "authenticated intermediate manifest names another native image"
REJECTED_IDENTITY = "executing intermediate differs from its authenticated release identity"


def manifest_bytes(artifacts=None, commit="via-commit"):
    if artifacts is None:
        artifacts = [{"platform": PLATFORM, "binarySha256": "via-image"}, {"platform": {"os": "darwin", "arch": "arm64"}, "binarySha256": "mac-image"}]
    return json.dumps({"version": VIA, "commit": commit, "artifacts": artifacts}).encode()


class SignedOrigin:
    """Serves the published via head and keeps the local logical copies."""

    def __init__(self, test):
        temporary = tempfile.TemporaryDirectory(prefix="published-hop-")
        test.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.served = {}
        self.releases = SimpleNamespace(directory=self.root / "releases", native="linux/amd64", builds={}, verification={"version": DRAFT})
        self.publish(manifest_bytes(), b'{"version": "v0.1.332"}')

    def publish(self, manifest, metadata, *, local=True):
        self.served["/ShaulLavo/mesh/releases/download/" + VIA + "/mesh-release.json"] = manifest
        directory = self.releases.directory / VIA
        directory.mkdir(parents=True, exist_ok=True)
        for name, data in (("mesh-release.json", manifest), ("mesh-update.json", metadata)):
            if local:
                (directory / name).write_bytes(data)
            else:
                (directory / name).unlink(missing_ok=True)
        self.expected = {"version": VIA, "binary": "via-image", "manifest": published_releases.digest(manifest),
                         "metadata": published_releases.digest(metadata)}

    def asset(self, path):
        return SimpleNamespace(read_bytes=lambda: self.served[path])


def running(**changes):
    return {"version": VIA, "digest": "via-image", "commit": "via-commit", "modified": False, "platform": dict(PLATFORM)} | changes


class AuthenticateHopTest(unittest.TestCase):
    def setUp(self):
        self.origin = SignedOrigin(self)
        self.case = SimpleNamespace(origin=self.origin, releases=self.origin.releases)

    def authenticate(self, build=None, expected=None):
        installed_publication.authenticate_hop(self.case, build or running(), expected or self.origin.expected)

    def test_accepts_the_exact_authenticated_release(self):
        self.authenticate()

    def test_accepts_logical_digests_from_the_retention_history(self):
        manifest, metadata = manifest_bytes(), b'{"version": "v0.1.332"}'
        self.origin.publish(manifest, metadata, local=False)
        history = [{"tag": VIA, "assets": [{"name": "mesh-release.json", "sha256": published_releases.digest(manifest)},
                                           {"name": "mesh-update.json", "sha256": published_releases.digest(metadata)}]}]
        (self.origin.releases.directory / DRAFT).mkdir(parents=True)
        (self.origin.releases.directory / DRAFT / "mesh-retention.json").write_text(json.dumps(history))
        self.authenticate()

    def test_rejects_a_release_absent_from_the_retention_history(self):
        self.origin.publish(manifest_bytes(), b"{}", local=False)
        (self.origin.releases.directory / DRAFT).mkdir(parents=True)
        (self.origin.releases.directory / DRAFT / "mesh-retention.json").write_text("[]")
        with self.assertRaisesRegex(ValueError, "logical retention reference"):
            self.authenticate()

    def test_rejects_served_bytes_that_differ_from_the_plan(self):
        self.origin.served["/ShaulLavo/mesh/releases/download/" + VIA + "/mesh-release.json"] = manifest_bytes(commit="other")
        with self.assertRaisesRegex(RuntimeError, REJECTED_BYTES):
            self.authenticate()

    def test_rejects_a_local_manifest_that_differs_from_the_plan(self):
        (self.origin.releases.directory / VIA / "mesh-release.json").write_bytes(manifest_bytes(commit="other"))
        with self.assertRaisesRegex(RuntimeError, REJECTED_BYTES):
            self.authenticate()

    def test_rejects_metadata_that_differs_from_the_plan(self):
        (self.origin.releases.directory / VIA / "mesh-update.json").write_bytes(b'{"version": "other"}')
        with self.assertRaisesRegex(RuntimeError, REJECTED_BYTES):
            self.authenticate()

    def test_rejects_a_manifest_without_exactly_one_matching_artifact(self):
        mac = {"platform": {"os": "darwin", "arch": "arm64"}, "binarySha256": "via-image"}
        linux = {"platform": PLATFORM, "binarySha256": "via-image"}
        cases = {"another image": [linux | {"binarySha256": "other-image"}], "no native artifact": [mac],
                 "two native artifacts": [linux, linux]}
        for name, artifacts in cases.items():
            self.origin.publish(manifest_bytes(artifacts), b"{}")
            with self.subTest(name), self.assertRaisesRegex(RuntimeError, REJECTED_ARTIFACT):
                self.authenticate()

    def test_rejects_a_running_build_that_is_not_the_authenticated_release(self):
        cases = {"version": running(version="v0.1.331"), "digest": running(digest="other-image"),
                 "commit": running(commit="other-commit"), "modified": running(modified=True),
                 "platform": running(platform={"os": "linux", "arch": "arm64"})}
        for name, build in cases.items():
            with self.subTest(name), self.assertRaisesRegex(RuntimeError, REJECTED_IDENTITY):
                self.authenticate(build)


class RegisterRunningBuildTest(unittest.TestCase):
    """validate_published_route with the real authenticate_hop and the real
    strict inspection, so registration is checked against what inspection reads."""

    def setUp(self):
        self.origin = SignedOrigin(self)
        self.builds = self.origin.releases.builds
        self.builds.update({"v0.1.331": {"version": "v0.1.331", "digest": "first-image", "path": "first"},
                            DRAFT: {"version": DRAFT, "digest": "draft-image", "path": "draft"}})
        self.running = running()
        self.case = SimpleNamespace(origin=self.origin, releases=self.origin.releases, expected_workers=1,
            state=Path("unused-state"), binary=Path("unused-binary"),
            control=Mock(side_effect=lambda _name: {"host": {"build": self.running}}),
            mapped_helper=Mock(return_value={"sha256": "via-image"}),
            route_observer=SimpleNamespace(snapshot=Mock(return_value={})),
            cohort=SimpleNamespace(verify=Mock(return_value=["original-worker"])))
        self.inspected = []
        self.case.inspect_retained = self.inspect
        self.enterContext(patch.object(installed_native, "peer_pid", return_value=201))
        self.enterContext(patch.object(installed_native.native_services, "mapped_image",
                                       return_value={"pid": 201, "sha256": "via-image"}))
        self.authenticated = self.enterContext(patch.object(installed_publication, "authenticate_hop",
                                                            wraps=installed_publication.authenticate_hop))

    def inspect(self):
        self.inspected.append(installed_continuity.current_daemon_image(self.case, self.running))

    def record(self):
        return {"expectedHops": [], "hops": [], "routeKind": "direct", "authenticatedTarget": self.origin.expected,
                "routePlans": [{"target": self.origin.expected, "next": self.origin.expected, "currentDigest": "first-image"}]}

    def forward(self):
        journal = {"phase": "committed", "request": {"manifest": {"version": VIA}}}
        ledger = {"state": {}, "entries": [{"version": VIA, "metadata": self.origin.expected["metadata"], "workersPreserved": 1}]}
        record = self.record()
        installed_publication.validate_published_route(self.case, record, journal, ledger, VIA, None, False, set())
        return record

    def rollback(self):
        fault = SimpleNamespace(bound=True, released=True, error=None)
        journal = {"phase": "rolled_back", "request": {"current": {"digest": "via-image", "platform": PLATFORM}, "manifest": {"version": DRAFT}}}
        edge = {"from": "via-image", "to": "draft-image", "platform": PLATFORM}
        ledger = {"state": {}, "entries": [], "failed": [{"edge": edge, "version": DRAFT, "phase": "rolled_back"}]}
        record = self.record()
        installed_publication.validate_published_route(self.case, record, journal, ledger, DRAFT, fault, True, set())
        return record

    def test_a_mismatched_via_head_is_never_registered_or_inspected(self):
        self.running = running(commit="other-commit")
        with self.assertRaisesRegex(RuntimeError, REJECTED_IDENTITY):
            self.forward()
        self.assertNotIn(VIA, self.builds)
        self.assertEqual(self.inspected, [])

    def test_an_unknown_authenticated_via_head_is_registered_then_inspected(self):
        record = self.forward()
        self.assertEqual(self.builds[VIA], self.running)
        self.assertEqual(len(self.inspected), 1)
        self.assertEqual(record["final"]["build"], self.running)

    def test_a_known_version_with_an_equal_digest_keeps_its_recorded_entry(self):
        recorded = {"version": VIA, "digest": "via-image", "path": "via"}
        self.builds[VIA] = dict(recorded)
        self.forward()
        self.assertEqual(self.builds[VIA], recorded)
        self.assertEqual(len(self.inspected), 1)

    def test_a_known_version_with_another_digest_is_refused_and_kept(self):
        recorded = {"version": VIA, "digest": "recorded-image", "path": "via"}
        self.builds[VIA] = dict(recorded)
        with self.assertRaisesRegex(RuntimeError, "independently recorded release"):
            self.forward()
        self.assertEqual(self.builds[VIA], recorded)
        self.assertEqual(self.inspected, [])

    def test_rollback_to_a_known_build_passes_without_registering_again(self):
        recorded = {"version": VIA, "digest": "via-image", "path": "via"}
        self.builds[VIA] = dict(recorded)
        record = self.rollback()
        self.assertEqual(self.builds[VIA], recorded)
        self.assertEqual(len(self.inspected), 1)
        self.assertEqual(record["final"]["build"], self.running)
        self.authenticated.assert_not_called()

    def test_rollback_to_a_known_version_with_another_digest_fails(self):
        recorded = {"version": VIA, "digest": "recorded-image", "path": "via"}
        self.builds[VIA] = dict(recorded)
        with self.assertRaisesRegex(RuntimeError, "not a previously verified release"):
            self.rollback()
        self.assertEqual(self.builds[VIA], recorded)
        self.assertEqual(self.inspected, [])
        self.authenticated.assert_not_called()
