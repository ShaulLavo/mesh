"""Prepare signed releases once; consume complete installed groups independently."""
import hashlib
import json
from pathlib import Path
import shutil
import subprocess

from fixtures import fleet_baselines, installed_adoption, installed_chain, installed_clock
from fixtures import installed_cohort, installed_origin, installed_releases, installed_slices
from fixtures import installed_waypoint, release_worker_groups, release_workers

DOCUMENT_LIMIT = 4 << 20


def file_digest(path):
    with Path(path).open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def source_pin():
    return subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=installed_releases.REPO, text=True, timeout=10).strip()


def local_file(root, name):
    path = Path(name)
    if path.is_absolute() or not path.parts or ".." in path.parts:
        raise ValueError("prepared asset is not a relative local file")
    target = Path(root) / path
    if target.is_symlink() or not target.resolve(strict=True).is_relative_to(Path(root).resolve()):
        raise ValueError("prepared asset escapes its bundle")
    if not target.is_file():
        raise ValueError("prepared asset is not a regular file")
    return target


def read_descriptor(bundle):
    path = local_file(bundle, "prepared.json")
    if path.stat().st_size > DOCUMENT_LIMIT:
        raise ValueError("prepared descriptor exceeds its document bound")
    raw = path.read_bytes()
    value = json.loads(raw)
    if value.get("schema") != 1 or value.get("mode") not in ("chain", "waypoint"):
        raise ValueError("prepared release schema or mode differs")
    if not isinstance(value.get("files"), dict) or not 1 <= len(value["files"]) <= 32768:
        raise ValueError("prepared assets exceed their count bound")
    if not isinstance(value.get("groups"), list) or not 0 < len(value["groups"]) <= installed_slices.MAX_GROUPS:
        raise ValueError("prepared partition exceeds its group bound")
    return value, hashlib.sha256(raw).hexdigest()


def clock_workload(bundle_path, slice_path):
    value, identity = read_descriptor(bundle_path)
    selection = installed_slices.parse_slice(slice_path)
    installed_slices.validate_selection(selection, len(value["groups"]), value["mode"] == "waypoint")
    return {"preparedRole": "consume", "preparedId": identity, "installedSlice": selection}


@installed_clock.timed("prepared-export")
def export(releases, host, pair, previous, groups, destination, *, waypoint=False, preparer=None):
    destination = Path(destination)
    if destination.resolve().is_relative_to(releases.root.resolve()):
        raise ValueError("prepared export must survive outside disposable preparation scratch")
    destination.mkdir(parents=True, exist_ok=False)
    packets = prepare_reader_packets(releases, groups) if waypoint else {}
    installed_origin.request(releases.root, {"export": "prepared-origin.json"})
    files = {}
    for name in ("signer-test", "prepared-origin.json", "fixture-ca.pem", "bootstrap-root.json", "trusted-root.json", "provenance.json"):
        copy_asset(releases.root, Path(name), destination, files)
    for directory in (releases.directory, releases.root / "public"):
        copy_tree(releases.root, directory, destination, files)
    builds = {version: build | {"path": copy_asset(releases.root, Path(build["path"]).relative_to(releases.root), destination, files)}
              for version, build in releases.builds.items()}
    registry = export_registry(releases, destination, files)
    probe = copy_external(releases.probe, "worker-probe", destination, files)
    helper = copy_external(preparer, "native-helper", destination, files) if preparer is not None else None
    for name in packets.values():
        copy_asset(releases.root, Path(name), destination, files)
    value = {"schema": 1, "harnessSource": source_pin(), "native": releases.native, "host": host,
        "mode": "waypoint" if waypoint else "chain", "builds": builds, "manifests": releases.manifests,
        "metadata": releases.metadata, "registry": registry, "probe": probe, "preparer": helper, "readerPackets": packets,
        "pair": encode_pair(pair, releases.root), "previous": encode_image(previous, releases.root),
        "groups": [release_worker_groups.public_group(group) for group in groups], "files": files,
        "publishedRegistrySha256": releases.published_registry_sha256,
        "exporter": json.loads((releases.output / "fixture-exporter.json").read_bytes())}
    installed_releases.write_json(destination / "prepared.json", value)
    identity = file_digest(destination / "prepared.json")
    installed_releases.write_json(releases.output / "prepared-export.json", {"schema": 1, "preparedId": identity,
        "harnessSource": value["harnessSource"], "mode": value["mode"], "groups": value["groups"]})
    return identity


def prepare_reader_packets(releases, groups):
    packets = {}
    for group in groups:
        name = "reader-packets/" + str(group["index"]) + ".json"
        path = releases.root / name
        path.parent.mkdir(exist_ok=True)
        installed_releases.write_json(path, installed_waypoint.prepare_negative_packet(releases, group))
        packets[str(group["index"])] = name
    return packets


def copy_external(source, name, destination, files):
    target = destination / name
    target.parent.mkdir(parents=True, exist_ok=True)
    shutil.copy2(source, target)
    files[name] = {"sha256": file_digest(target), "size": target.stat().st_size, "executable": bool(target.stat().st_mode & 0o111)}
    return name


def copy_asset(root, name, destination, files):
    label = name.as_posix()
    if label not in files:
        copy_external(local_file(root, label), label, destination, files)
    return label


def copy_tree(root, directory, destination, files):
    for path in sorted(directory.rglob("*")):
        if path.is_file():
            copy_asset(root, path.relative_to(root), destination, files)


def export_registry(releases, destination, files):
    records = release_workers.applicable(releases.registry, installed_releases.VERSIONS[0],
                                         releases.builds[installed_releases.VERSIONS[0]]["path"])
    rows = [row | {"binary": copy_asset(releases.root, Path(row["binary"]).relative_to(releases.root), destination, files)} for row in records]
    name = "prepared-workers.json"
    installed_releases.write_json(releases.root / name, {"schema": 1,
        "platform": dict(zip(("os", "arch"), releases.native.split("/"))), "workers": rows})
    return copy_asset(releases.root, Path(name), destination, files)


def encode_image(image, root):
    return image | {"executable": str(Path(image["executable"]).relative_to(root))}


def encode_pair(pair, root):
    return {role: encode_image(image, root) for role, image in pair.items()}


def run_prepare(root, output, native, host, registry, probe, export_path, *, clock_plan=None, format_waypoint=False, preparer=None):
    if host["platform"] != native or registry is None or probe is None:
        raise ValueError("prepared chain requires its exact native host, registry and observer")
    require_space(root, output, 4 << 30)
    releases = installed_releases.Releases(root, native, output,
        manifest_schemas=(1, 2) if format_waypoint else (1,), clock_plan=clock_plan)
    try:
        releases.published_registry_sha256 = file_digest(registry)
        releases.registry = installed_cohort.stage(registry, root / "workers", dict(zip(("os", "arch"), native.split("/"))))
        releases.probe = probe
        releases.start_signer()
        origin = installed_origin.Origin(root)
        pair, previous = acquire_sources(root, host, native)
        prepare = installed_waypoint.prepare if format_waypoint else installed_chain.prepare
        prepare(releases, origin, pair["daemon"]["executable"])
        groups = installed_adoption.installed_groups(releases.registry, installed_releases.VERSIONS[0],
                  releases.builds[installed_releases.VERSIONS[0]]["path"], pair["daemon"]["executable"])
        installed_clock.installed_partition_resolved()
        identity = export(releases, host, pair, previous, groups, export_path, waypoint=format_waypoint, preparer=preparer)
        releases.clock.provenance["preparedId"] = identity
    finally:
        try:
            releases.close()
        finally:
            installed_adoption.retain_producer_logs(root, output)


def acquire_sources(root, host, native):
    pair = fleet_baselines.acquire_pair(root / "public", host)
    version = "v0.1." + str(int(host["daemon"].split(".")[-1]) - 1)
    previous = fleet_baselines.acquire(root / "public", version, dict(zip(("os", "arch"), native.split("/"))))
    return pair, previous


def validate_files(bundle, value):
    for name, expected in value["files"].items():
        path = local_file(bundle, name)
        if (path.stat().st_size != expected["size"] or file_digest(path) != expected["sha256"]
                or bool(path.stat().st_mode & 0o111) != expected["executable"]):
            raise ValueError("prepared asset changed: " + name)


def require_space(root, output, required):
    free = shutil.disk_usage(root).free
    installed_releases.write_json(output / "storage-preflight.json", {"root": str(root), "freeBytes": free, "requiredFreeBytes": required})
    if free < required:
        raise RuntimeError("prepared fixture lacks space for its immutable inputs and installed scratch")


def decode_image(image, root):
    return image | {"executable": local_file(root, image["executable"])}


class PreparedReleases(installed_releases.Releases):
    def start_prepared_server(self):
        if self.signer_started is not None or self.clock is None or self.clock.plan is not self.clock_plan:
            raise RuntimeError("prepared origin requires the original active command clock")
        self.clock_plan.validate()
        self.server_kind = "prepared-origin"
        self.signer_log = (self.root / "prepared-origin.log").open("wb")
        with installed_clock.operation("prepared-server-start"):
            self.launch_server("TestInstalledPreparedServe", "MESH_INSTALLED_PREPARED_DIR")


@installed_clock.timed("prepared-load")
def load(root, output, native, bundle_path, slice_path, clock_plan):
    bundle = Path(bundle_path)
    value, identity = read_descriptor(bundle)
    selection = installed_slices.parse_slice(slice_path)
    installed_slices.validate_selection(selection, len(value["groups"]), value["mode"] == "waypoint")
    if value["native"] != native or value["harnessSource"] != source_pin():
        raise ValueError("prepared source or platform differs from its consumer")
    validate_files(bundle, value)
    require_space(root, output, sum(row["size"] for row in value["files"].values()) + (4 << 30))
    for name in value["files"]:
        destination = root / name
        destination.parent.mkdir(parents=True, exist_ok=True)
        shutil.copy2(local_file(bundle, name), destination)
    releases = PreparedReleases(root, native, output, clock_plan=clock_plan, prepared=True,
        manifest_schemas=(1, 2) if value["mode"] == "waypoint" else (1,))
    initialize_prepared(releases, root, output, value)
    pair = {role: decode_image(image, root) for role, image in value["pair"].items()}
    groups = installed_adoption.installed_groups(releases.registry, installed_releases.VERSIONS[0],
              releases.builds[installed_releases.VERSIONS[0]]["path"], pair["daemon"]["executable"])
    if [release_worker_groups.public_group(group) for group in groups] != value["groups"]:
        raise ValueError("prepared installed partition differs from the actual authoritative registry")
    return releases, value, identity, selection, pair, decode_image(value["previous"], root), groups


def initialize_prepared(releases, root, output, value):
    releases.server_kind = "prepared-origin"
    releases.manifests, releases.metadata = value["manifests"], value["metadata"]
    releases.builds = {version: build | {"path": str(local_file(root, build["path"]))} for version, build in value["builds"].items()}
    releases.registry, releases.probe = local_file(root, value["registry"]), local_file(root, value["probe"])
    releases.reader_packets = value["readerPackets"]
    installed_releases.write_json(output / "fixture-exporter.json", value["exporter"])


def run_slice(root, output, native, preparer, bundle_path, slice_path, *, clock_plan=None):
    releases, value, identity, selection, pair, previous, groups = load(root, output, native, bundle_path, slice_path, clock_plan)
    try:
        if releases.clock is None or releases.clock.provenance.get("preparedId") != identity:
            raise ValueError("prepared bytes changed after clock admission")
        validate_clock_workload(releases.clock, value, selection)
        preparer = resolve_preparer(root, value, preparer, selection)
        releases.start_prepared_server()
        origin = installed_origin.Origin(root)
        origin.serve_tree(releases.directory)
        origin.serve_tree(root / "public")
        installed_clock.installed_partition_resolved()
        run_selected(root, output, preparer, releases, origin, value["host"], pair, previous, groups, selection)
    finally:
        try:
            releases.close()
        finally:
            installed_adoption.retain_producer_logs(root, output)
    installed_releases.write_json(output / "prepared-slice.json", {"schema": 1, "preparedId": identity,
        "harnessSource": value["harnessSource"], "selection": selection, "completed": True,
        "kind": "diagnostic-installed-measurement" if clock_plan.mode == "diagnostic" else "installed-slice"})


def validate_clock_workload(clock, value, selection):
    expected = {"preparedRole": "consume", "installedSlice": selection, "platform": value["native"],
                "host": value["host"]["name"], "mode": value["mode"]}
    if any(clock.plan.workload.get(key) != row for key, row in expected.items()):
        raise ValueError("prepared invocation differs from its admitted clock workload")
    registry = clock.plan.workload.get("publishedRegistrySha256")
    if registry is not None and registry != value["publishedRegistrySha256"]:
        raise ValueError("prepared authoritative registry differs from its admitted workload")


def resolve_preparer(root, value, supplied, selection):
    bundled = local_file(root, value["preparer"]) if value.get("preparer") else None
    if supplied is not None:
        supplied = Path(supplied).resolve(strict=True)
        if bundled is not None and file_digest(supplied) != file_digest(bundled):
            raise ValueError("supplied native preparer differs from the prepared tool")
        return supplied
    if bundled is None and selection["select"]:
        raise ValueError("installed slice requires the prepared or supplied production native helper")
    return bundled


def run_selected(root, output, preparer, releases, origin, host, pair, previous, groups, selection):
    installed_clock.expect_samples("readers:" + str(index) + "#1" for index in selection["readers"])
    for index in selection["readers"]:
        installed_waypoint.verify_readers(releases, groups[index])
    for row in selection["select"]:
        selected = [groups[index] for index in row["group_indices"]]
        installed_chain.run_groups(root, output, host, pair, preparer, origin, releases, previous,
                                   row["profile"].removeprefix("chain-"), selected)
