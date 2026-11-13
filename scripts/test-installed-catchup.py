"""Installed proof path boundaries and real native-service failure cleanup."""
import json
import ast
import http.server
import io
import importlib.util
import os
import shutil
import socket
import ssl
import socketserver
import http.client
import zipfile
import tempfile
import tarfile
import unittest
import threading
import subprocess
from types import SimpleNamespace
from pathlib import Path
from unittest.mock import patch

from fixtures import fleet_baselines, installed_paths as paths, native_services, published_releases, installed_cohort, installed_artifacts, installed_accounts, installed_routes, installed_chain, installed_native, publication_origin, installed_publication, installed_storage
from fixtures import installed_review_controls, installed_listener_controls
from fixtures import installed_releases, release_transition

PROOF_PATH = Path(__file__).with_name("prove-installed-catchup.py")
SPEC = importlib.util.spec_from_file_location("installed_catchup_proof", PROOF_PATH)
PROOF = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(PROOF)


def maximum_depth(node, depth):
    if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)):
        depth = 1
    elif isinstance(node, (ast.If, ast.For, ast.While, ast.With, ast.Try, ast.AsyncWith, ast.AsyncFor)):
        depth += 1
    return max((maximum_depth(child, depth) for child in ast.iter_child_nodes(node)), default=depth)


class SocketPathsTest(unittest.TestCase):
    def setUp(self):
        base = os.environ.get("MESH_SHORT_TMP", "/tmp")
        self.temp = tempfile.TemporaryDirectory(prefix="ip-", dir=base)
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name).resolve()

    def test_every_generated_address_binds(self):
        evidence = paths.validate(self.root)
        for row in evidence["paths"]:
            address = Path(row["path"])
            address.parent.mkdir(parents=True, exist_ok=True)
            with socket.socket(socket.AF_UNIX) as listener:
                listener.bind(str(address))

    def test_platform_exact_byte_limits(self):
        for system, maximum in (("Darwin", 103), ("Linux", 107)):
            suffix = max(len(os.fsencode(path)) for path in paths.socket_paths(self.root))
            root = self.root / ("x" * (maximum - suffix - 1))
            self.assertEqual(paths.validate(root, system)["longestSocketBytes"], maximum)
            with self.assertRaisesRegex(ValueError, "socket requires"):
                paths.validate(root.with_name(root.name + "x"), system)

    def test_long_tmpdir_and_symlink_cannot_hide_physical_length(self):
        long = self.root / ("long-tmp-" * 12)
        long.mkdir()
        alias = self.root / "link"
        alias.symlink_to(long, target_is_directory=True)
        before = set(long.iterdir())
        with patch.dict(os.environ, {"MESH_SHORT_TMP": str(alias)}):
            with self.assertRaisesRegex(ValueError, "socket requires"):
                paths.create()
        self.assertEqual(set(long.iterdir()), before)

    def test_filesystem_bytes_are_counted(self):
        with self.assertRaisesRegex(ValueError, "socket requires"):
            paths.validate(self.root / ("é" * 40))

    def test_missing_account_cleanup_retains_actual_order_scratch(self):
        root, output = self.root / "scratch", self.root / "evidence"
        root.mkdir()
        output.mkdir()
        (root / "ordered-input.json").write_text("{}")
        (output / "native-manager.json").write_text(json.dumps({"cleanup": {"serviceAbsent": True, "processAbsent": True}}))
        with self.assertRaisesRegex(RuntimeError, "missing"):
            PROOF.clean_scratch(root, output)
        self.assertTrue(root.exists())
        self.assertFalse((output / "scratch-cleanup.json").exists())

    def test_unmounted_work_drive_cannot_receive_fixture_data(self):
        with (patch.dict(os.environ, {"MESH_SHORT_TMP": "/work/tmp"}), patch.object(Path, "is_mount", return_value=False)):
            with self.assertRaisesRegex(RuntimeError, "not mounted"):
                paths.create()

    def test_python_fixture_blocks_remain_at_most_three_deep(self):
        scripts = PROOF_PATH.parent
        files = (PROOF_PATH, Path(__file__), *(scripts / "fixtures" / name for name in
            ("release_worker_groups.py", "release_worker_index.py", "release_worker_completion.py", "noindex_paths.py", "installed_paths.py", "native_services.py", "installed_listener.py", "installed_listener_evidence.py", "installed_listener_controls.py", "installed_native.py", "fleet_baselines.py",
             "installed_origin.py", "installed_releases.py", "installed_adoption.py", "installed_cohort.py", "installed_chain.py", "installed_waypoint.py", "installed_artifacts.py", "installed_accounts.py", "installed_order.py", "installed_order_host.py", "installed_fresh.py", "installed_routes.py", "installed_storage.py", "publication_origin.py", "installed_publication.py", "installed_account_run.py", "installed_review_controls.py")))
        for path in files:
            self.assertLessEqual(maximum_depth(ast.parse(path.read_text()), 0), 3, str(path))

    def test_native_tmpdir_is_ignored(self):
        environment = dict(os.environ)
        environment.pop("MESH_SHORT_TMP", None)
        environment["TMPDIR"] = str(self.root / ("long-" * 30))
        with patch.dict(os.environ, environment, clear=True):
            root = paths.create()
        self.addCleanup(shutil.rmtree, root)
        self.assertEqual(root.parent, Path("/tmp").resolve())

    def test_cleanup_retains_root_when_native_stop_is_unconfirmed(self):
        output = self.root / "output"
        output.mkdir()
        retained = self.root / "retained"
        retained.mkdir()
        (retained / "sentinel").write_text("test owner retains this until native cleanup is known")
        (output / "native-manager.json").write_text(json.dumps({"cleanupError": "stop failed"}))
        with self.assertRaisesRegex(RuntimeError, "cleanup unconfirmed"):
            PROOF.clean_scratch(retained, output)
        self.assertFalse((output / "scratch-cleanup.json").exists())
        self.assertTrue((retained / "sentinel").exists())


class FleetBaselinesTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="fb-", dir=os.environ.get("MESH_SHORT_TMP", "/tmp"))
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.platform = {"os": "linux", "arch": "amd64"}
        # Checksum fixtures are deliberately not executable release history.
        self.binary = b"checksum unit-test data; never executed"
        buffer = io.BytesIO()
        with tarfile.open(fileobj=buffer, mode="w:gz") as archive:
            member = tarfile.TarInfo("mesh")
            member.size = len(self.binary)
            archive.addfile(member, io.BytesIO(self.binary))
        self.archive = buffer.getvalue()
        self.artifact = {"platform": self.platform, "archive": "mesh_linux_amd64.tar.gz",
                         "sha256": published_releases.digest(self.archive), "binarySha256": published_releases.digest(self.binary)}
        self.manifest = {"schema": 1, "version": "v0.1.249", "artifacts": [self.artifact]}

    def fetch(self, address, _maximum):
        return json.dumps(self.manifest).encode() if address.endswith("mesh-release.json") else self.archive

    def test_fixture_records_exact_observed_pairs_without_inventing_first_updater(self):
        fixture = fleet_baselines.load()
        observed = [(host["name"], host["daemon"], host["helper"], host["helperRunning"]) for host in fixture["hosts"]]
        self.assertEqual(observed, [("pc", "v0.1.249", "v0.1.196", False), ("pi", "v0.1.255", "v0.1.230", True),
                                    ("vps", "v0.1.255", "v0.1.196", True), ("mac", "v0.1.257", "v0.1.196", True)])
        with self.assertRaisesRegex(RuntimeError, "PR-source label cannot replace"):
            fleet_baselines.first_updater(fixture | {"firstPublishedUpdater": None})
        self.assertEqual(fleet_baselines.first_updater(fixture | {"firstPublishedUpdater": "v0.1.999"}), "v0.1.999")

    def test_acquisition_checks_both_hashes_and_inventory_binding(self):
        result = fleet_baselines.acquire(self.root, "v0.1.249", self.platform, self.artifact["binarySha256"], self.fetch)
        self.assertEqual(result["executable"].read_bytes(), self.binary)
        with self.assertRaisesRegex(RuntimeError, "observed installed fleet"):
            fleet_baselines.acquire(self.root, "v0.1.249", self.platform, "a" * 64, self.fetch)

    def test_bad_binary_never_creates_an_executable(self):
        self.artifact["binarySha256"] = "a" * 64
        with self.assertRaisesRegex(RuntimeError, "executable digest mismatch"):
            fleet_baselines.acquire(self.root, "v0.1.249", self.platform, fetcher=self.fetch)
        self.assertFalse((self.root / "v0.1.249" / "mesh").exists())

    def test_cached_archive_is_rechecked(self):
        fleet_baselines.acquire(self.root, "v0.1.249", self.platform, fetcher=self.fetch)
        (self.root / "v0.1.249" / self.artifact["archive"]).write_bytes(b"corrupted cache")
        with self.assertRaisesRegex(RuntimeError, "archive digest mismatch"):
            fleet_baselines.acquire(self.root, "v0.1.249", self.platform, fetcher=self.fetch)

    def test_wrong_native_platform_and_manifest_version_reject(self):
        with self.assertRaisesRegex(RuntimeError, "one exact native archive"):
            fleet_baselines.acquire(self.root, "v0.1.249", {"os": "darwin", "arch": "arm64"}, fetcher=self.fetch)
        self.manifest["version"] = "v0.1.196"
        with self.assertRaisesRegex(RuntimeError, "schema or version differs"):
            fleet_baselines.acquire(self.root, "v0.1.249", self.platform, fetcher=self.fetch)


class ExactCohortInputTest(unittest.TestCase):
    def test_copy_controls_obey_darwin_exclusion(self):
        with patch("fixtures.noindex_paths.platform.system", return_value="Darwin"):
            self.test_owned_copy_preserves_exact_digest_and_borrowed_permissions()
            self.test_changed_bytes_and_escaping_paths_fail_closed()

    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="ci-", suffix=".noindex", dir=os.environ.get("MESH_SHORT_TMP", "/tmp"))
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.source = self.root / "source"
        self.source.mkdir()
        self.binary = self.source / "mesh"
        self.binary.write_bytes(b"checksum fixture only; never executed")
        self.binary.chmod(0o640)
        self.platform = {"os": "linux", "arch": "amd64"}
        self.record = {"version": "v0.1.249", "protocol": 1, "binary": "mesh",
            "artifact": {"platform": self.platform, "binarySha256": published_releases.digest(self.binary.read_bytes())}}
        self.registry = self.source / "workers.json"
        self.registry.write_text(json.dumps({"schema": 1, "platform": self.platform, "workers": [self.record]}))

    def test_owned_copy_preserves_exact_digest_and_borrowed_permissions(self):
        staged = installed_cohort.stage(self.registry, self.root / "owned", self.platform)
        selected = json.loads(staged.read_text())["workers"]
        binary = staged.parent / selected[0]["binary"]
        self.assertEqual(binary.read_bytes(), self.binary.read_bytes())
        binary.chmod(0o755)
        self.assertEqual(self.binary.stat().st_mode & 0o777, 0o640)

    def test_changed_bytes_and_escaping_paths_fail_closed(self):
        self.binary.write_bytes(b"changed fixture bytes")
        with self.assertRaisesRegex(RuntimeError, "published identity"):
            installed_cohort.stage(self.registry, self.root / "changed", self.platform)
        with self.assertRaisesRegex(RuntimeError, "relative beneath"):
            installed_cohort.copy_record(self.registry, self.root, self.record | {"binary": "../outside"})

    def test_wrong_platform_does_not_stage_binaries(self):
        with self.assertRaisesRegex(RuntimeError, "schema/platform"):
            installed_cohort.stage(self.registry, self.root / "wrong", {"os": "darwin", "arch": "arm64"})
        self.assertFalse((self.root / "wrong").exists())


@unittest.skipUnless(os.environ.get("MESH_TEST_NATIVE_MANAGER") == "1", "native control explicitly selected")
class NativeServiceTest(unittest.TestCase):
    def setUp(self):
        self.root = paths.create()
        self.addCleanup(self.clean)
        self.report = self.root / "evidence.json"

    def clean(self):
        if not self.report.exists():
            raise RuntimeError(f"native cleanup record missing; test owner retains {self.root}")
        cleanup = json.loads(self.report.read_text()).get("cleanup", {})
        if not cleanup.get("serviceAbsent") or not cleanup.get("processAbsent"):
            raise RuntimeError(f"native cleanup unconfirmed; test owner retains {self.root}")
        shutil.rmtree(self.root)

    def test_real_manager_pid_and_stop(self):
        evidence = native_services.positive_control(self.root, self.report)
        self.assertTrue(evidence["passed"])
        self.assertTrue(evidence["cleanup"]["serviceAbsent"])
        self.assertTrue(evidence["cleanup"]["processAbsent"])

    def test_exception_after_launch_stops_real_service(self):
        with patch.object(native_services.json, "loads", side_effect=RuntimeError("injected after launch")):
            with self.assertRaisesRegex(RuntimeError, "injected after launch"):
                native_services.positive_control(self.root, self.report)
        evidence = json.loads(self.report.read_text())
        self.assertFalse(evidence["passed"])
        self.assertTrue(evidence["cleanup"]["serviceAbsent"])
        self.assertTrue(evidence["cleanup"]["processAbsent"])


class RouteHandler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200)
        self.end_headers()
        try:
            while not self.server.stopping.is_set():
                self.wfile.write((str(self.server.listener_pid) + "\n").encode())
                self.wfile.flush()
                self.server.stopping.wait(0.02)
        except OSError:
            pass

    def log_message(self, *_arguments):
        pass


class RouteObservationTest(unittest.TestCase):
    def server(self, port=0, pid=None):
        server = http.server.ThreadingHTTPServer(("127.0.0.1", port), RouteHandler)
        server.stopping = threading.Event()
        server.listener_pid = os.getpid() if pid is None else pid
        thread = threading.Thread(target=server.serve_forever, kwargs={"poll_interval": 0.01})
        self.addCleanup(self.stop_server, server, thread)
        thread.start()
        return server, thread

    def stop_server(self, server, thread):
        server.stopping.set()
        server.shutdown()
        server.server_close()
        thread.join(timeout=2)

    def test_live_stream_loss_and_reconnection_is_measured(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        server, thread = self.server()
        port = server.server_port
        observer = installed_routes.Observer(port, {"pid": os.getpid()}, Path(temporary.name) / "route.json", maximum=2)
        self.addCleanup(observer.close)
        observer.start()
        self.stop_server(server, thread)
        native_services.wait_for(lambda: not observer.snapshot()["connected"], "observer missed the actual TCP stream loss")
        self.server(port)
        observer.recovered()
        value = observer.snapshot()
        self.assertGreater(value["pulses"], 1)
        self.assertGreater(len(value["gaps"]), 0)
        self.assertTrue(all(0 < gap["seconds"] < 2 for gap in value["gaps"]))

    def test_another_listener_is_rejected(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        server, _thread = self.server(pid=os.getpid() + 1)
        observer = installed_routes.Observer(server.server_port, {"pid": os.getpid()}, Path(temporary.name) / "route.json")
        self.addCleanup(observer.close)
        with self.assertRaisesRegex(RuntimeError, "another listener"):
            observer.start()


class PublicationConsumerTest(unittest.TestCase):
    def test_non_main_publication_is_rejected_before_acquisition(self):
        with patch.dict(os.environ, {"GITHUB_ACTIONS": "true", "RUNNER_ENVIRONMENT": "github-hosted", "GITHUB_REF": "refs/pull/435/merge"}):
            with self.assertRaisesRegex(RuntimeError, "main-only"):
                installed_publication.require_main()

    def test_transport_serves_the_original_asset_bytes_over_verified_tls(self):
        temporary = tempfile.TemporaryDirectory(prefix="ipub-", dir=os.environ.get("MESH_SHORT_TMP", "/tmp"))
        self.addCleanup(temporary.cleanup)
        root = Path(temporary.name)
        release = root / "releases/v0.1.1"
        release.mkdir(parents=True)
        actual = b"publication transport control; never a signed release proof"
        (release / "asset.json").write_bytes(actual)
        origin = publication_origin.Origin(root, release.parent, root / "unused-reader")
        self.addCleanup(origin.close)
        origin.start()
        data = publication_request(origin, "/ShaulLavo/mesh/releases/download/v0.1.1/asset.json")
        self.assertEqual(data, actual)
        self.assertFalse(origin.close()["testSignatureRootUsed"])

    def test_owned_activation_port_fault_releases_at_actual_rollback_phase(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        state = Path(temporary.name)
        (state / "update").mkdir()
        previous = socket.socket()
        self.addCleanup(previous.close)
        previous.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        previous.bind(("127.0.0.1", 0))
        previous.listen(1)
        case = SimpleNamespace(state=state, port=previous.getsockname()[1])
        journal = state / "update/installation.json"
        journal.write_text(json.dumps({"phase": "activating", "request": {"manifest": {"version": "control-only"}}}))
        fault = installed_publication.PortFault(case, "control-only")
        self.addCleanup(fault.close)
        fault.start()
        self.assertFalse(fault.bound)
        previous.close()
        native_services.wait_for(lambda: fault.bound, "owned activation port was not acquired")
        temporary_journal = journal.with_suffix(".tmp")
        temporary_journal.write_text(json.dumps({"phase": "rolling_back"}))
        temporary_journal.replace(journal)
        native_services.wait_for(lambda: fault.released, "owned activation port was not released for rollback")
        fault.close()
        self.assertEqual(fault.error, "")

    def test_public_trust_tls_is_tunneled_without_proxy_termination(self):
        temporary = tempfile.TemporaryDirectory(prefix="iptuf-", dir=os.environ.get("MESH_SHORT_TMP", "/tmp"))
        self.addCleanup(temporary.cleanup)
        root = Path(temporary.name)
        origin = publication_origin.Origin(root, root / "releases", root / "unused")
        self.addCleanup(origin.close)
        origin.start()
        upstream = root / "upstream"
        upstream.mkdir()
        publication_origin.tls.fixture_certificates(upstream)
        server = socketserver.TCPServer(("127.0.0.1", 0), TrustTLSHandler)
        server.context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        server.context.load_cert_chain(upstream / "server.pem", upstream / "server.key")
        thread = threading.Thread(target=server.serve_forever, kwargs={"poll_interval": 0.01})
        self.addCleanup(stop_trust_server, server, thread)
        thread.start()
        connect = socket.create_connection
        def redirected(address, *args, **kwargs):
            return connect(server.server_address if address == ("tuf-repo-cdn.sigstore.dev", 443) else address, *args, **kwargs)
        with patch.object(publication_origin.socket, "create_connection", side_effect=redirected):
            context = ssl.create_default_context(cafile=str(upstream / "cert.pem"))
            context.verify_flags |= ssl.VERIFY_X509_STRICT
            data = trust_tunnel_request(origin, context)
        self.assertEqual(data, b"public TLS remained end to end")
        self.assertEqual(origin.trust_connections, 1)


class TrustTLSHandler(socketserver.BaseRequestHandler):
    def handle(self):
        with self.server.context.wrap_socket(self.request, server_side=True) as secure:
            secure.sendall(b"public TLS remained end to end")


def stop_trust_server(server, thread):
    server.shutdown()
    server.server_close()
    thread.join(timeout=2)


def trust_tunnel_request(origin, context):
    with socket.create_connection(("127.0.0.1", origin.server.server_port), timeout=5) as connection:
        connection.sendall(b"CONNECT tuf-repo-cdn.sigstore.dev:443 HTTP/1.1\r\nHost: tuf-repo-cdn.sigstore.dev:443\r\n\r\n")
        read_connect_response(connection)
        with context.wrap_socket(connection, server_hostname="github.com") as secure:
            return secure.recv(128)


class NativeZIPPreflightTest(unittest.TestCase):
    def test_inventory_reads_only_bounded_directory_ranges(self):
        buffer = io.BytesIO()
        with zipfile.ZipFile(buffer, "w", compression=zipfile.ZIP_STORED) as package:
            package.writestr("retained/worker", b"x" * (1 << 20))
        raw = buffer.getvalue()
        remote = installed_storage.RemoteZIP("https://unused.fixture.invalid/unit-input", len(raw))
        with patch.object(remote, "fetch", side_effect=lambda _request, size, _end: raw[remote.position:remote.position + size]):
            with zipfile.ZipFile(remote) as package:
                self.assertEqual(sum(item.file_size for item in package.infolist()), 1 << 20)
        self.assertLess(remote.transferred, 128 << 10)


def publication_request(origin, path):
    context = ssl.create_default_context(cafile=str(origin.bundle))
    context.verify_flags |= ssl.VERIFY_X509_STRICT
    with socket.create_connection(("127.0.0.1", origin.server.server_port), timeout=5) as connection:
        connection.sendall(b"CONNECT github.com:443 HTTP/1.1\r\nHost: github.com:443\r\n\r\n")
        read_connect_response(connection)
        with context.wrap_socket(connection, server_hostname="github.com") as secure:
            secure.sendall(("GET " + path + " HTTP/1.0\r\nHost: github.com\r\n\r\n").encode())
            return read_publication_response(secure)


def read_publication_response(secure):
    response = http.client.HTTPResponse(secure)
    response.begin()
    if response.status != 200:
        raise RuntimeError("publication transport did not serve its exact asset")
    return response.read()


def read_connect_response(connection):
    data = b""
    while not data.endswith(b"\r\n\r\n"):
        data += connection.recv(1)
        if len(data) > 8192:
            raise RuntimeError("publication proxy header exceeded its control bound")
    if not data.startswith(b"HTTP/1.0 200"):
        raise RuntimeError("publication proxy rejected the approved release host")


class EmptyLedgerTest(unittest.TestCase):
    def test_before_first_commit_has_no_entries(self):
        empty = json.loads('{"entries":null,"state":{"schema":1,"phase":"idle"},"failed":[],"peers":null,"dependents":null}')
        value = installed_chain.normalize_ledger(empty)
        self.assertEqual(value["entries"], [])
        self.assertEqual(value["failed"], [])
        self.assertEqual(value["state"]["phase"], "idle")

    def test_malformed_collection_does_not_become_an_empty_commit(self):
        with self.assertRaisesRegex(RuntimeError, "malformed"):
            installed_chain.normalize_ledger({"entries": {"version": "invented"}})


class FailingTerminal:
    def close(self):
        raise RuntimeError("injected terminal I/O failure")


@unittest.skipUnless(os.environ.get("MESH_TEST_NATIVE_MANAGER") == "1", "requires actual native user manager")
class InstalledCleanupTest(unittest.TestCase):
    def test_terminal_failure_still_stops_actual_listener_and_both_units(self):
        temporary = tempfile.TemporaryDirectory(prefix="ic-", dir=os.environ.get("MESH_SHORT_TMP", "/tmp"))
        self.addCleanup(temporary.cleanup)
        root = Path(temporary.name)
        managers = [self.start_manager(root, index) for index in range(2)]
        listener = subprocess.Popen(["/bin/sleep", "30"])
        self.addCleanup(self.stop_process, listener)
        case = object.__new__(installed_native.Installation)
        case.route_observer, case.stream, case.response = None, None, None
        case.listener, case.worker, case.shell = {"pid": listener.pid}, None, None
        case.terminal, case.cleaned = FailingTerminal(), False
        case.process_stamps = {listener.pid: installed_native.process_stamp(listener.pid)}
        case.units = SimpleNamespace(close=lambda: installed_native.cleanup_all([manager.close for manager in managers]))
        case.verify_session_stopped = lambda: listener.wait(timeout=5)
        with self.assertRaisesRegex(RuntimeError, "injected terminal I/O"):
            case.close()
        self.assertIsNotNone(listener.poll())
        self.assertTrue(all(not manager.exists() and not native_services.alive(manager.pid) for manager in managers))

    def start_manager(self, root, index):
        directory = root / str(index)
        directory.mkdir()
        manager = native_services.Manager(directory)
        self.addCleanup(manager.close)
        marker = directory / "started.json"
        manager.start(marker)
        native_services.wait_for(marker.exists, "actual cleanup control did not start")
        manager.pid = manager.manager_pid()
        return manager

    def stop_process(self, process):
        if process.poll() is None:
            process.kill()
        process.wait(timeout=5)


class AccountIsolationTest(unittest.TestCase):
    def test_account_creation_is_disabled_outside_disposable_hosted_root(self):
        with patch.dict(os.environ, {"GITHUB_ACTIONS": "false"}), patch.object(installed_accounts, "command") as command:
            with self.assertRaisesRegex(RuntimeError, "disposable GitHub-hosted"):
                installed_accounts.Accounts(Path("/unused"), Path("/unused"))
        command.assert_not_called()

    def test_preexisting_uid_resources_are_never_stopped(self):
        accounts = object.__new__(installed_accounts.Accounts)
        record = {"name": "test-only", "uid": 12345, "home": "/unused", "created": True, "ownershipVerified": False}
        with patch.object(accounts, "owned"), patch.object(accounts, "persist"), patch.object(installed_accounts, "account_exists", side_effect=[SimpleNamespace(), None]):
            with patch.object(installed_accounts, "command") as command, patch.object(accounts, "stop_processes") as stop:
                accounts.remove_account(record)
        command.assert_called_once_with(["/usr/sbin/userdel", "test-only"])
        stop.assert_not_called()
        self.assertTrue(record["absent"])


class ArtifactPinTest(unittest.TestCase):
    def test_archive_controls_obey_darwin_exclusion(self):
        with patch("fixtures.noindex_paths.platform.system", return_value="Darwin"):
            self.test_pinned_id_archive_is_verified_before_extraction()
            self.test_pinned_archive_rejects_nonlocal_members()

    def inputs(self):
        jobs = [{"name": "Native proofs / prepare (" + key.replace("-", "/") + ", runner)",
                 "status": "completed", "conclusion": "success"} for key in installed_artifacts.KEYS]
        return [{"head_sha": "b" * 40}, {"jobs": jobs}, {"artifacts": [
            {"id": index, "name": "update-native-" + key, "expired": False, "size_in_bytes": 1234, "digest": "sha256:" + "b" * 64}
            for index, key in enumerate(installed_artifacts.KEYS, 1)]}]

    def test_complete_three_platform_set_is_pinned(self):
        with patch.object(installed_artifacts, "api", side_effect=self.inputs()):
            result = installed_artifacts.verify_set("ShaulLavo/mesh", 123, "a" * 40, "update-native")
        self.assertEqual(set(result["artifacts"]), set(installed_artifacts.KEYS))
        self.assertEqual(result["run"], 123)
        self.assertEqual(result["source"], "a" * 40)
        self.assertEqual(result["runHead"], "b" * 40)

    def test_pending_preparation_and_missing_or_expired_platform_fail(self):
        values = self.inputs()
        values[1]["jobs"][0].update(status="in_progress", conclusion=None)
        with patch.object(installed_artifacts, "api", side_effect=values):
            with self.assertRaisesRegex(RuntimeError, "not completed"):
                installed_artifacts.verify_set("ShaulLavo/mesh", 123, "a" * 40, "update-native")
        values = self.inputs()
        values[2]["artifacts"][-1]["expired"] = True
        with patch.object(installed_artifacts, "api", side_effect=values):
            with self.assertRaisesRegex(RuntimeError, "darwin-arm64"):
                installed_artifacts.verify_set("ShaulLavo/mesh", 123, "a" * 40, "update-native")

    def test_missing_archive_digest_is_rejected_during_intake(self):
        values = self.inputs()
        values[2]["artifacts"][0]["digest"] = None
        with patch.object(installed_artifacts, "api", side_effect=values):
            with self.assertRaisesRegex(RuntimeError, "pinned archive SHA-256"):
                installed_artifacts.verify_set("ShaulLavo/mesh", 123, "a" * 40, "update-native")

    def archive(self, value, name="tools/control"):
        buffer = io.BytesIO()
        with zipfile.ZipFile(buffer, "w") as package:
            package.writestr(name, value)
        return buffer.getvalue()

    def download_control(self, root, expected, received):
        destination = root / "input"
        destination.mkdir()
        artifact = {"id": 41, "name": "same-mutable-name", "size": len(expected),
                    "digest": "sha256:" + published_releases.digest(expected)}
        pin = root / "pin.json"
        pin.write_text(json.dumps({"artifacts": {"linux-amd64": artifact}}))
        with patch.object(installed_storage, "archive_url", return_value="https://artifact.invalid/pinned-41") as url, patch.object(installed_artifacts.urllib.request, "urlopen", return_value=io.BytesIO(received)):
            try:
                installed_artifacts.download("ShaulLavo/mesh", pin, "linux-amd64", destination, root / "download.json")
            finally:
                url.assert_called_once_with("ShaulLavo/mesh", 41)

    def test_pinned_id_archive_is_verified_before_extraction(self):
        data = self.archive(b"actual pinned bytes")
        with tempfile.TemporaryDirectory(suffix=".noindex") as temporary:
            root = Path(temporary)
            self.download_control(root, data, data)
            self.assertEqual((root / "input/tools/control").read_bytes(), b"actual pinned bytes")
            receipt = json.loads((root / "download.json").read_text())
            self.assertEqual(receipt, {"id": 41, "size": len(data), "digest": "sha256:" + published_releases.digest(data)})
            self.assertEqual(sorted(path.name for path in root.iterdir()), ["download.json", "input", "pin.json"])

    def test_replacement_after_intake_fails_before_extraction(self):
        original, replacement = self.archive(b"artifact A"), self.archive(b"artifact B")
        with tempfile.TemporaryDirectory(suffix=".noindex") as temporary:
            root = Path(temporary)
            with self.assertRaisesRegex(RuntimeError, "pinned size or SHA-256"):
                self.download_control(root, original, replacement)
            self.assertEqual(list((root / "input").iterdir()), [])
            self.assertEqual(sorted(path.name for path in root.iterdir()), ["input", "pin.json"])

    def test_pinned_archive_rejects_nonlocal_members(self):
        data = self.archive(b"not extracted", "../outside")
        with tempfile.TemporaryDirectory(suffix=".noindex") as temporary:
            root = Path(temporary)
            with self.assertRaisesRegex(RuntimeError, "nonlocal path"):
                self.download_control(root, data, data)
            self.assertEqual(list((root / "input").iterdir()), [])
            self.assertFalse((root / "outside").exists())

    def test_truncated_archive_fails_before_extraction(self):
        data = self.archive(b"original pinned archive")
        with tempfile.TemporaryDirectory(suffix=".noindex") as temporary:
            root = Path(temporary)
            with self.assertRaisesRegex(RuntimeError, "pinned size or SHA-256"):
                self.download_control(root, data, data[:-4])
            self.assertEqual(list((root / "input").iterdir()), [])
            self.assertFalse((root / "download.json").exists())

    def test_tool_source_and_actual_bytes_must_agree(self):
        with tempfile.TemporaryDirectory() as temporary:
            self.check_tool_provenance(Path(temporary))

    def check_tool_provenance(self, root):
        platform = {"os": "linux", "arch": "amd64"}
        tools = root / "tools"
        tools.mkdir()
        digests = {}
        for name in ("candidate", "worker-probe", "startup-fault-candidate", "boundary"):
            (tools / name).write_bytes(name.encode())
            digests[name] = published_releases.digest(name.encode())
        path = tools / "preparation.json"
        value = {"schema": 1, "source": {"commit": "a" * 40, "clean": True}, "platform": platform, "sha256": digests}
        path.write_text(json.dumps(value))
        installed_artifacts.verify_tools(root, "a" * 40, platform)
        with self.assertRaisesRegex(RuntimeError, "source"):
            installed_artifacts.verify_tools(root, "b" * 40, platform)
        (tools / "worker-probe").write_bytes(b"another observer")
        with self.assertRaisesRegex(RuntimeError, "worker-probe"):
            installed_artifacts.verify_tools(root, "a" * 40, platform)


class UnpublishedRollbackContextTest(unittest.TestCase):
    def manifest(self):
        return {"artifacts": [], "compatibility": {"stateReadMin": 16, "stateReadMax": 16, "stateWrite": 16,
            "workerMin": 1, "workerMax": 1, "workerWrite": 1, "journalVersion": 1,
            "transitions": [{"proof": "existing-completed-receipt"}]}}

    def test_actual_source_range_is_only_added_to_the_private_zero_claim_context(self):
        manifest = self.manifest()
        before = json.loads(json.dumps(manifest))
        source = {"stateVersion": 13, "workerProtocol": 1}
        with self.assertRaisesRegex(ValueError, "actual source database schema"):
            release_transition.verify_source_range(manifest, source)
        context = installed_releases.rollback_context(manifest, source)
        release_transition.verify_source_range(context, source)
        self.assertEqual(context["compatibility"]["stateReadMin"], 13)
        self.assertEqual(context["compatibility"]["stateWrite"], 16)
        self.assertEqual(context["compatibility"]["transitions"], [])
        self.assertIs(context["artifacts"], manifest["artifacts"])
        self.assertEqual(manifest, before)

    def test_source_above_compiled_capabilities_is_rejected(self):
        for source in ({"stateVersion": 17, "workerProtocol": 1}, {"stateVersion": 13, "workerProtocol": 2}):
            with self.assertRaisesRegex(RuntimeError, "actual candidate context capabilities"):
                installed_releases.rollback_context(self.manifest(), source)

    def test_executing_source_report_must_match_actual_image_and_platform(self):
        with tempfile.TemporaryDirectory() as temporary:
            source = Path(temporary) / "source"
            source.write_bytes(b"source measurement control")
            build = {"digest": published_releases.digest(source.read_bytes()), "modified": False,
                     "platform": {"os": "linux", "arch": "amd64"}, "stateVersion": 13, "workerProtocol": 1}
            self.check_source_report(source, build, True)
            self.check_source_report(source, build | {"digest": "0" * 64}, False)
            self.check_source_report(source, build | {"modified": True}, False)
            self.check_source_report(source, build | {"platform": {"os": "darwin", "arch": "arm64"}}, False)

    def check_source_report(self, source, build, accepted):
        result = subprocess.CompletedProcess([], 0, json.dumps(build), "")
        with patch.object(installed_releases.subprocess, "run", return_value=result):
            if accepted:
                self.assertEqual(installed_releases.source_build(source, {}, "linux/amd64"), build)
                return
            self.reject_source_report(source)

    def reject_source_report(self, source):
        with self.assertRaisesRegex(RuntimeError, "actual unmodified native image"):
            installed_releases.source_build(source, {}, "linux/amd64")


class PublicationModeTest(unittest.TestCase):
    def arguments(self, version, source):
        return SimpleNamespace(publication_version=version, publication_source=source, native_helper=None,
                               fresh_managed=False, installed_adoption=False, acquire_baselines=False,
                               worker_registry=None, worker_probe=None)

    def test_explicit_empty_publication_inputs_fail_without_consumption(self):
        for version, source in (("", ""), ("v0.1.1", ""), ("", "a" * 40), (None, "a" * 40)):
            self.reject_publication(version, source)

    def reject_publication(self, version, source):
        with patch.object(installed_publication, "prove") as consume:
            with self.assertRaisesRegex(RuntimeError, "exact release version, source"):
                PROOF.run_requested(self.arguments(version, source), Path("/unused"), Path("/unused"), "linux/amd64")
        consume.assert_not_called()

    def test_omitted_publication_mode_remains_only_a_manager_preflight(self):
        with patch.object(installed_publication, "prove") as consume:
            PROOF.run_requested(self.arguments(None, None), Path("/unused"), Path("/unused"), "linux/amd64")
        consume.assert_not_called()

    def test_valid_publication_inputs_call_the_actual_draft_consumer(self):
        with tempfile.TemporaryDirectory() as temporary:
            helper = Path(temporary) / "helper"
            helper.touch()
            arguments = self.arguments("v0.1.1", "a" * 40)
            arguments.native_helper = helper
            with patch.object(installed_publication, "prove") as consume:
                PROOF.run_requested(arguments, Path("/unused"), Path("/output"), "linux/amd64")
            consume.assert_called_once_with(Path("/unused"), Path("/output"), "linux/amd64", "v0.1.1", "a" * 40,
                                            helper.resolve(), None, None)


def load_tests(loader, tests, _pattern):
    return unittest.TestSuite((tests, loader.loadTestsFromModule(installed_review_controls),
                               loader.loadTestsFromModule(installed_listener_controls)))


if __name__ == "__main__":
    unittest.main()
