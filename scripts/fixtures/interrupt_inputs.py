"""A small immutable handoff from an already approved signed fixture producer."""
import hashlib
import json
import shutil
import stat
import tempfile
import zipfile
from pathlib import Path

from fixtures import (
    installed_artifacts,
    installed_origin,
    installed_paths,
    installed_prepared,
    installed_releases,
    installed_storage,
    published_releases,
)
from fixtures.interrupt_capture import ORIGINAL

MAX_ARCHIVE = 256 << 20
MAX_UNPACKED = 512 << 20
MAX_FILES = 128
NAME = "interrupt-inputs.json"
PLATFORM = {"os": "linux", "arch": "amd64"}
HOST = {"name": "pc", "platform": "linux/amd64", "daemon": "v0.1.249", "helper": "v0.1.196", "helperRunning": False}


def digest(path):
    with Path(path).open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def read(bundle, expected):
    descriptor = installed_prepared.local_file(bundle, NAME)
    if descriptor.stat().st_size > 1 << 20 or digest(descriptor) != expected:
        raise ValueError("interrupt input descriptor differs from its reviewed pin")
    value = json.loads(descriptor.read_bytes())
    if value.get("schema") != 1 or value.get("kind") != "pc-interrupt-inputs" or value.get("host") != HOST:
        raise ValueError("interrupt input is not exactly the original pc-interrupt profile")
    files = value.get("files", {})
    if not 1 <= len(files) <= MAX_FILES or sum(row["size"] for row in files.values()) > MAX_UNPACKED:
        raise ValueError("interrupt input closure exceeds its file or byte bound")
    installed_prepared.validate_files(bundle, value)
    verify_images(bundle, value)
    return value


def verify_images(bundle, value):
    images = [(value["pair"]["daemon"], "v0.1.249", ORIGINAL),
        (value["pair"]["helper"], "v0.1.196", "35de97be239b0159bd4bff102d2491e311ae564523c9b5ebfc5860619d161484"),
        (value["previous"], "v0.1.248", "ab917a253d1966675a1020398a859b4e24a1891770b7db529270718267eb92c5")]
    for image, version, expected in images:
        verify_image(bundle, image, version, expected)
    build = value["build"]
    path = installed_prepared.local_file(bundle, build["path"])
    if build["version"] != "v0.1.9001" or build["platform"] != PLATFORM or digest(path) != build["digest"]:
        raise ValueError("signed diagnostic candidate image differs")
    if build["commit"] != value["source"]:
        raise ValueError("candidate source differs from its input source")
    manifest = installed_prepared.local_file(bundle, "releases/v0.1.9001/mesh-release.json")
    if digest(manifest) != value["metadata"]["manifest"]:
        raise ValueError("candidate manifest differs from signed metadata")
    if json.loads(manifest.read_bytes()) != value["manifest"]:
        raise ValueError("candidate context differs from its signed manifest bytes")
    metadata = installed_prepared.local_file(bundle, "releases/v0.1.9001/mesh-update.json")
    if json.loads(metadata.read_bytes()) != value["metadata"]:
        raise ValueError("candidate metadata context differs from its signed bytes")
    artifacts = [row for row in value["manifest"]["artifacts"] if row["platform"] == PLATFORM]
    if len(artifacts) != 1 or artifacts[0]["binarySha256"] != build["digest"]:
        raise ValueError("signed manifest names another native candidate")
    archive = installed_prepared.local_file(bundle, "releases/v0.1.9001/" + artifacts[0]["archive"])
    if published_releases.archive_binary(archive.read_bytes(), artifacts[0]) != path.read_bytes():
        raise ValueError("signed native candidate archive differs from its executable")
    installed_prepared.local_file(bundle, value["preparer"])


def verify_image(bundle, image, version, expected):
    executable = installed_prepared.local_file(bundle, image["executable"])
    if image["version"] != version or image["artifact"]["binarySha256"] != expected or digest(executable) != expected:
        raise ValueError("original published input image differs: " + version)
    manifest = installed_prepared.local_file(bundle, str(Path(image["executable"]).parent / "mesh-release.json"))
    if digest(manifest) != image["manifestSha256"]:
        raise ValueError("original public manifest pin differs")
    archive = installed_prepared.local_file(bundle, str(Path(image["executable"]).parent / image["artifact"]["archive"]))
    if published_releases.archive_binary(archive.read_bytes(), image["artifact"]) != executable.read_bytes():
        raise ValueError("published executable does not match its verified archive")


def download(pin, destination, output):
    if set(pin) != {"id", "size", "digest", "run", "runHead", "source", "descriptorSha256"}:
        raise ValueError("interrupt handoff requires an exact artifact, run, source, and descriptor pin")
    installed_artifacts.require_archive_pin(pin)
    if pin["size"] > MAX_ARCHIVE or not installed_prepared.hex_identity(pin["descriptorSha256"], 64):
        raise ValueError("interrupt input archive or descriptor pin is invalid")
    if type(pin["run"]) is not int or pin["run"] <= 0 or any(
            not installed_prepared.hex_identity(pin[key], 40) for key in ("runHead", "source")):
        raise ValueError("interrupt input producer identity is invalid")
    metadata = installed_artifacts.api("repos/ShaulLavo/mesh/actions/artifacts/" + str(pin["id"]))
    expected = {"size_in_bytes": pin["size"], "digest": pin["digest"], "expired": False}
    if any(metadata.get(key) != value for key, value in expected.items()):
        raise ValueError("interrupt input artifact differs from its reviewed metadata")
    if metadata["workflow_run"]["id"] != pin["run"] or metadata["workflow_run"]["head_sha"] != pin["runHead"]:
        raise ValueError("interrupt input artifact belongs to another producer run or source")
    destination = Path(destination)
    installed_paths.require_mounted_drive(destination.resolve())
    if shutil.disk_usage(destination.parent).free < pin["size"] + MAX_UNPACKED + (128 << 20):
        raise ValueError("interrupt handoff destination lacks space for archive and bounded extraction")
    destination.mkdir(parents=True, exist_ok=False)
    try:
        fetch(pin, destination)
        value = read(destination, pin["descriptorSha256"])
        if value["source"] != pin["source"]:
            raise ValueError("interrupt handoff source changed after download")
    except BaseException:
        shutil.rmtree(destination)
        raise
    Path(output).write_text(json.dumps({"diagnosticOnly": True, "accepted": False, "pin": pin,
        "unpackedBytes": sum(row["size"] for row in value["files"].values())}) + "\n")


def fetch(pin, destination):
    with tempfile.TemporaryDirectory(prefix="interrupt-archive-", dir=destination.parent) as temporary:
        archive = Path(temporary) / "input.zip"
        installed_artifacts.fetch_archive(installed_storage.archive_url("ShaulLavo/mesh", pin["id"]), archive, pin)
        with zipfile.ZipFile(archive) as package:
            validate_zip(package)
            extract_package(package, destination)


def extract_package(package, destination):
    package.extractall(destination)
    for member in package.infolist():
        if not member.is_dir():
            (destination / member.filename).chmod(0o700 if member.external_attr >> 16 & 0o111 else 0o600)


def validate_zip(package):
    members = package.infolist()
    if len(members) > MAX_FILES + 32 or sum(row.file_size for row in members) > MAX_UNPACKED:
        raise ValueError("interrupt input ZIP exceeds its bounded closure")
    if len({row.filename for row in members}) != len(members):
        raise ValueError("interrupt input ZIP repeats a member")
    for member in members:
        installed_artifacts.require_local_member(member)
        mode = member.external_attr >> 16
        if stat.S_IFMT(mode) not in (0, stat.S_IFREG, stat.S_IFDIR):
            raise ValueError("interrupt input ZIP contains a link or nonregular member")


def export(releases, host, pair, previous, preparer, destination):
    """Call only inside an already authorized producer, before its teardown."""
    if host != HOST or releases.native != "linux/amd64" or releases.registry:
        raise ValueError("interrupt export requires the original ungrouped PC fixture")
    destination = Path(destination)
    if destination.resolve().is_relative_to(releases.root.resolve()):
        raise ValueError("interrupt export must survive outside its producer scratch")
    destination.mkdir(parents=True, exist_ok=False)
    installed_origin.request(releases.root, {"export": "prepared-origin.json"})
    files = {}
    names = ("signer-test", "prepared-origin.json", "fixture-ca.pem", "bootstrap-root.json", "trusted-root.json", "provenance.json")
    for name in names:
        installed_prepared.copy_asset(releases.root, Path(name), destination, files)
    directories = [releases.directory / "v0.1.9001"] + [Path(image["executable"]).parent
                   for image in (pair["daemon"], pair["helper"], previous)]
    for directory in directories:
        copy_native_tree(releases.root, directory, destination, files)
    build = releases.builds["v0.1.9001"]
    binary = installed_prepared.copy_asset(releases.root, Path(build["path"]).relative_to(releases.root), destination, files)
    helper = installed_prepared.copy_external(preparer, "native-helper", destination, files)
    value = {"schema": 1, "kind": "pc-interrupt-inputs", "source": build["commit"], "host": host,
        "build": build | {"path": binary}, "metadata": releases.metadata["v0.1.9001"],
        "manifest": releases.manifests["v0.1.9001"], "preparer": helper,
        "pair": installed_prepared.encode_pair(pair, releases.root),
        "previous": installed_prepared.encode_image(previous, releases.root), "files": files}
    installed_releases.write_json(destination / NAME, value)
    read(destination, digest(destination / NAME))
    return digest(destination / NAME)


def copy_native_tree(root, directory, destination, files):
    for path in sorted(directory.rglob("*")):
        if path.is_file() and not path.name.endswith(("_arm64.tar.gz", "_amd64.zip")):
            installed_prepared.copy_asset(root, path.relative_to(root), destination, files)


if __name__ == "__main__":
    import os
    import sys
    download(json.loads(os.environ["MESH_INTERRUPT_INPUT_PIN"]), Path(sys.argv[1]), Path(sys.argv[2]))
