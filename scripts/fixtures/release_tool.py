"""Explicit reviewed release-tool provenance and the single Go codec boundary."""
import hashlib
import json
import os
from pathlib import Path
import platform
import re
import stat
import subprocess
import tempfile
import zipfile

from fixtures import installed_artifacts, installed_storage, release_documents


def digest(path):
    with path.open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def native_target():
    system = {"Linux": "linux", "Darwin": "darwin"}.get(platform.system())
    architecture = {"x86_64": "amd64", "aarch64": "arm64", "arm64": "arm64"}.get(platform.machine())
    if system is None or architecture is None:
        raise ValueError("unsupported release-tool execution target")
    return {"os": system, "arch": architecture}


def identity(tool):
    return json.loads(subprocess.check_output([str(tool), "identity"], timeout=30))


def validate_identity(value, source, target, expected_digest):
    if (value.get("schema") != 1 or not re.fullmatch(r"[0-9a-f]{40}", source)
            or value.get("source") != source or value.get("target") != target
            or value.get("sha256") != expected_digest or value.get("modified") is not False):
        raise ValueError("release tool differs from its reviewed source, target or executable digest")


def record(directory, source, run, role, target):
    tool = directory / "mesh-release-sign"
    value = build_identity(tool, source, target)
    validate_identity(value, source, target, digest(tool))
    if int(run) <= 0 or role not in ("source", "harness"):
        raise ValueError("release tool requires its explicit writer run and source role")
    value.update(run=int(run), role=role, mode=0o755)
    value["companions"] = {}
    for name in ("native-helper", "publication-reader", "mesh-release-routes"):
        helper = directory / name
        if helper.exists():
            value["companions"][name] = {"sha256": digest(helper), "mode": 0o755}
    tool.chmod(0o755)
    (directory / "release-tool.json").write_text(json.dumps(value, sort_keys=True) + "\n")
    return value


def build_identity(tool, source, target):
    if target == native_target():
        return identity(tool)
    raw = subprocess.check_output(["go", "version", "-m", str(tool)], text=True, timeout=30)
    required = ("GOOS=" + target["os"], "GOARCH=" + target["arch"], "vcs.revision=" + source, "vcs.modified=false")
    settings = {line.strip().removeprefix("build\t") for line in raw.splitlines() if line.strip().startswith("build\t")}
    if not set(required) <= settings:
        raise ValueError("cross-built SOURCE tool lacks exact clean VCS and target metadata")
    return {"schema": 1, "source": source, "target": target, "sha256": digest(tool), "modified": False}


def verify(directory, source, run, role):
    value = json.loads((directory / "release-tool.json").read_bytes())
    tool = directory / "mesh-release-sign"
    if tool.is_symlink() or not tool.is_file() or stat.S_IMODE(tool.stat().st_mode) != 0o755:
        raise ValueError("release tool must be an explicitly provisioned executable regular file")
    validate_identity(value, source, native_target(), digest(tool))
    if value.get("run") != int(run) or value.get("role") != role or value.get("mode") != 0o755:
        raise ValueError("release tool differs from its artifact run or SOURCE/H role")
    validate_identity(identity(tool), source, native_target(), value["sha256"])
    for name, companion in value["companions"].items():
        if name not in ("native-helper", "publication-reader", "mesh-release-routes") or companion.get("mode") != 0o755:
            raise ValueError("release tool artifact contains an undeclared companion")
        helper = directory / name
        if helper.is_symlink() or digest(helper) != companion["sha256"] or stat.S_IMODE(helper.stat().st_mode) != 0o755:
            raise ValueError("native helper differs from its once-built source tool artifact")
    return tool


def configured():
    path = Path(os.environ["MESH_RELEASE_TOOL"])
    if not path.is_absolute():
        raise ValueError("release tool path must be explicit and absolute")
    verified = verify(path.parent, os.environ["MESH_RELEASE_TOOL_SOURCE"], os.environ["MESH_RELEASE_TOOL_RUN"],
                      os.environ["MESH_RELEASE_TOOL_ROLE"])
    if verified != path:
        raise ValueError("release tool path differs from its verified artifact")
    return verified


def binding(release):
    return {"version": release["tag_name"], "source": release["target_commitish"], "releaseId": release["id"],
            "writerRun": release["intake_run"], "platforms": sorted(release_documents.release_worker_index.PLATFORMS,
                key=lambda item: (item["os"], item["arch"]))}


def materialize(snapshot, release, destination, inventory_path):
    return materialize_operation("materialize-documents", snapshot, binding(release), destination, inventory_path)


def materialize_history(snapshot, release, destination, inventory_path):
    expected = {"version": release["tag_name"], "source": release["target_commitish"], "releaseId": release["id"]}
    return materialize_operation("materialize-history", snapshot, expected, destination, inventory_path)


def materialize_operation(operation, snapshot, expected, destination, inventory_path):
    tool = configured()
    trust = Path(os.environ["MESH_RELEASE_TRUST_CACHE"])
    if not trust.is_absolute() or not trust.is_dir():
        raise ValueError("materialization requires the explicitly provisioned trust directory")
    with tempfile.TemporaryDirectory(prefix="bundle-binding-", dir=destination.parent) as temporary:
        input_path = Path(temporary) / "binding.json"
        input_path.write_text(json.dumps(expected))
        output = subprocess.check_output([str(tool), operation, str(snapshot), str(input_path),
                                          str(destination)], timeout=180)
    value = json.loads(output)
    observed = value.get("binding", {})
    if (value.get("schema") != 1 or any(observed.get(key) != expected[key] for key in expected)
            or type(observed.get("writerRun")) is not int or observed["writerRun"] <= 0
            or observed.get("platforms") != sorted(release_documents.release_worker_index.PLATFORMS,
                key=lambda item: (item["os"], item["arch"]))):
        raise ValueError("materialized logical inventory differs from the explicit release binding")
    validate_logical_inventory(destination, value["documents"])
    inventory_path.write_text(json.dumps(value, sort_keys=True) + "\n")
    return value


def validate_logical_inventory(directory, entries):
    names = [item["name"] for item in entries]
    if (len(names) != len(set(names)) or not set(release_documents.FIXED + (release_documents.ENVELOPE,)) <= set(names)
            or set(names) != {path.name for path in directory.iterdir()}):
        raise ValueError("authenticated logical inventory is incomplete or repeats documents")
    for item in entries:
        raw = release_documents.read_document(directory, item["name"], item["sha256"])
        if len(raw) != item["size"]:
            raise ValueError("materialized logical bytes differ from the authenticated inventory")


def download(repository, run, source, role, key, destination, artifact=None):
    if repository != "ShaulLavo/mesh" or int(run) <= 0 or role not in ("source", "harness"):
        raise ValueError("release tool acquisition requires its owned repository, run and role")
    metadata = installed_artifacts.api(f"repos/{repository}/actions/runs/{run}")
    if role == "source" and metadata["head_sha"] != source:
        raise ValueError("SOURCE tool artifact run has another source")
    name = f"release-tool-{role}-{key}"
    # Listed by name, and each ID once: a paged listing of the whole run can
    # repeat or skip uploads while other jobs upload or delete artifacts.
    listed = installed_artifacts.named_artifacts(repository, run, name)
    assets = list({item["id"]: item for item in listed}.values())
    item = current_artifact(assets, name, int(run)) if artifact is None else named_artifact(assets, name, int(run), artifact)
    pin = {"id": item["id"], "size": item["size_in_bytes"], "digest": item.get("digest")}
    installed_artifacts.require_archive_pin(pin)
    destination.mkdir(exist_ok=False)
    with tempfile.TemporaryDirectory(prefix="tool-download-", dir=destination.parent) as temporary:
        archive = Path(temporary) / "tool.zip"
        installed_artifacts.fetch_archive(installed_storage.archive_url(repository, item["id"]), archive, pin)
        extract(archive, destination)
    verify(destination, source, run, role)
    (destination / "artifact-pin.json").write_text(json.dumps(pin | {"run": int(run), "source": source, "role": role}) + "\n")


def current_artifact(assets, name, run):
    # A failed-jobs rerun rebuilds the tool under the same name in the same
    # run, and the artifact list carries no attempt number: the newest live
    # upload is the current attempt's. Every candidate must belong to this run.
    matches = [item for item in assets if item["name"] == name and not item["expired"]]
    if not matches or any(artifact_run(item) != run for item in matches):
        raise ValueError("release tool requires one exact live same-run artifact")
    newest = max(item["created_at"] for item in matches)
    current = [item for item in matches if item["created_at"] == newest]
    if len(current) != 1:
        raise ValueError("release tool requires one exact live same-run artifact")
    return current[0]


def named_artifact(assets, name, run, artifact):
    """The exact upload the producing job reported, when the workflow passes its ID."""
    if not str(artifact).isdigit() or int(artifact) <= 0:
        raise ValueError("release tool artifact ID must be a positive integer")
    matches = [item for item in assets if item["id"] == int(artifact)]
    if len(matches) != 1 or matches[0]["name"] != name or matches[0]["expired"] or artifact_run(matches[0]) != run:
        raise ValueError("release tool requires its exact live same-run artifact " + str(artifact))
    return matches[0]


def artifact_run(item):
    workflow_run = item.get("workflow_run")
    if not isinstance(workflow_run, dict) or type(workflow_run.get("id")) is not int:
        raise ValueError("release tool artifact does not name its workflow run")
    return workflow_run["id"]


def extract(archive, destination):
    allowed = {"mesh-release-sign", "release-tool.json", "native-helper", "publication-reader", "mesh-release-routes"}
    with zipfile.ZipFile(archive) as package:
        members = package.infolist()
        if (not {"mesh-release-sign", "release-tool.json"} <= {item.filename for item in members}
                or len({item.filename for item in members}) != len(members)
                or sum(item.file_size for item in members) > 128 << 20):
            raise ValueError("release tool artifact has an invalid file inventory")
        for member in members:
            extract_member(package, member, destination, allowed)


def extract_member(package, member, destination, allowed):
    if member.filename not in allowed or member.is_dir() or stat.S_IFMT(member.external_attr >> 16) not in (0, stat.S_IFREG):
        raise ValueError("release tool artifact contains an undeclared or nonregular file")
    target = destination / member.filename
    with package.open(member) as source, target.open("xb") as output:
        import shutil
        shutil.copyfileobj(source, output)
    target.chmod(0o755 if member.filename != "release-tool.json" else 0o644)


def main():
    import argparse
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("operation", choices=("record", "download", "verify"))
    parser.add_argument("values", nargs="+")
    args = parser.parse_args()
    if args.operation == "download":
        if len(args.values) not in (6, 7):
            parser.error("download takes REPOSITORY RUN SOURCE ROLE KEY DIRECTORY [ARTIFACT-ID]")
        repository, run, source, role, key, directory, *artifact = args.values
        download(repository, run, source, role, key, Path(directory), *artifact)
        return
    directory, source, run, role, *target = args.values
    if args.operation == "record":
        system, architecture = target[0].split("/")
        record(Path(directory), source, run, role, {"os": system, "arch": architecture})
        return
    verify(Path(directory), source, run, role)


if __name__ == "__main__":
    main()
