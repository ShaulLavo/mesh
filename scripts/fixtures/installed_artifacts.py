"""Pin the complete producer artifact set before consuming native inputs."""
import hashlib
import json
import re
import subprocess
import tempfile
import urllib.request
import zipfile
from pathlib import Path

KEYS = ("linux-amd64", "linux-arm64", "darwin-arm64")


def verify_harness(expected, output):
    actual = subprocess.check_output(["git", "rev-parse", "HEAD"], text=True).strip()
    if not re.fullmatch(r"[0-9a-f]{40}", expected) or actual != expected:
        raise RuntimeError("actual harness checkout differs from its explicit source pin")
    Path(output).write_text(json.dumps({"expected": expected, "actual": actual}) + "\n")


def api(endpoint):
    result = subprocess.run(["gh", "api", endpoint], capture_output=True, check=True, timeout=30)
    return json.loads(result.stdout)


def pages(endpoint, field):
    result, page = [], 1
    while True:
        batch = api(f"{endpoint}?per_page=100&page={page}")[field]
        result.extend(batch)
        if len(batch) < 100:
            return result
        page += 1


def verify_preparation(jobs):
    for platform in (key.replace("-", "/") for key in KEYS):
        pattern = r"(?:^| / )prepare \(" + re.escape(platform) + r"(?:,|\))"
        matching = [job for job in jobs if re.search(pattern, job["name"])]
        if len(matching) != 1 or matching[0].get("conclusion") != "success" or matching[0].get("status") != "completed":
            raise RuntimeError("native artifact preparation has not completed successfully for " + platform)


def verify_set(repository, run, source, prefix):
    if run <= 0 or not re.fullmatch(r"[0-9a-f]{40}", source) or not re.fullmatch(r"[A-Za-z0-9_.-]+", prefix):
        raise RuntimeError("native artifact inputs require an exact run, source SHA and artifact prefix")
    metadata = api(f"repos/{repository}/actions/runs/{run}")
    verify_preparation(pages(f"repos/{repository}/actions/runs/{run}/jobs", "jobs"))
    artifacts = pages(f"repos/{repository}/actions/runs/{run}/artifacts", "artifacts")
    result = {}
    for key in KEYS:
        expected = prefix + "-" + key
        matches = [item for item in artifacts if item["name"] == expected and not item["expired"]]
        if len(matches) != 1:
            raise RuntimeError("complete native artifact set requires exactly one live " + expected)
        result[key] = {"id": matches[0]["id"], "name": expected, "size": matches[0]["size_in_bytes"], "digest": matches[0].get("digest")}
        require_archive_pin(result[key])
    return {"run": run, "source": source, "runHead": metadata["head_sha"], "artifacts": result}


def require_archive_pin(artifact):
    if type(artifact.get("id")) is not int or artifact["id"] <= 0 or type(artifact.get("size")) is not int or artifact["size"] <= 0:
        raise RuntimeError("native artifact requires a positive pinned ID and archive size")
    if not isinstance(artifact.get("digest"), str) or not re.fullmatch(r"sha256:[0-9a-f]{64}", artifact["digest"]):
        raise RuntimeError("native artifact requires its pinned archive SHA-256")


def copy_archive(response, target, expected_size):
    size, digest = 0, hashlib.sha256()
    while data := response.read(1 << 20):
        size += len(data)
        if size > expected_size:
            raise RuntimeError("downloaded native artifact exceeds its pinned size")
        digest.update(data)
        target.write(data)
    return size, "sha256:" + digest.hexdigest()


def fetch_archive(url, archive, artifact):
    try:
        with urllib.request.urlopen(url, timeout=30) as response, archive.open("xb") as target:
            size, digest = copy_archive(response, target, artifact["size"])
    except OSError:
        raise RuntimeError("pinned native artifact archive transfer failed") from None
    if size != artifact["size"] or digest != artifact["digest"]:
        raise RuntimeError("downloaded native artifact differs from its pinned size or SHA-256")
    return {"id": artifact["id"], "size": size, "digest": digest}


def extract_archive(archive, directory):
    with zipfile.ZipFile(archive) as package:
        for member in package.infolist():
            require_local_member(member)
        package.extractall(directory)


def require_local_member(member):
    if member.filename.startswith("/") or ".." in Path(member.filename).parts:
        raise RuntimeError("native artifact archive contains a nonlocal path")


def download(repository, pin_path, key, destination, output):
    from fixtures import installed_storage
    pin = json.loads(Path(pin_path).read_text())
    artifact = pin["artifacts"][key]
    require_archive_pin(artifact)
    directory = Path(destination).resolve(strict=True)
    if any(directory.iterdir()):
        raise RuntimeError("pinned native artifact destination is not empty")
    with tempfile.TemporaryDirectory(prefix="native-archive-", dir=directory.parent) as temporary:
        archive = Path(temporary) / "artifact.zip"
        record = fetch_archive(installed_storage.archive_url(repository, artifact["id"]), archive, artifact)
        extract_archive(archive, directory)
    Path(output).write_text(json.dumps(record, indent=2) + "\n")


def verify_native(directory, source, platform):
    directory = Path(directory)
    reader = json.loads((directory / "tools/candidate-reader.json").read_text())
    registry = json.loads((directory / "retained/workers.json").read_text())
    expected = dict(zip(("os", "arch"), platform.split("/")))
    verify_tools(directory, source, expected)
    if reader["build"]["commit"] != source or reader["build"]["platform"] != expected or registry["platform"] != expected:
        raise RuntimeError("downloaded native producer source/platform differs from its pin")
    if reader.get("schema") != 1 or reader["build"].get("modified") is not False:
        raise RuntimeError("native producer did not retain an unmodified actual reader")
    with (directory / "tools/candidate").open("rb") as stream:
        if hashlib.file_digest(stream, "sha256").hexdigest() != reader["build"]["digest"]:
            raise RuntimeError("native producer candidate bytes differ from the retained capability pin")
    if not (directory / "tools/worker-probe").is_file() or not registry["workers"]:
        raise RuntimeError("native producer artifact lacks its exact workers or observer")


def verify_tools(directory, source, platform):
    preparation = json.loads((directory / "tools/preparation.json").read_text())
    if preparation.get("schema") != 1 or preparation.get("source") != {"commit": source, "clean": True} or preparation.get("platform") != platform:
        raise RuntimeError("native tool preparation source, clean checkout or platform differs from its pin")
    for name in ("candidate", "worker-probe", "startup-fault-candidate", "boundary"):
        with (directory / "tools" / name).open("rb") as stream:
            actual = hashlib.file_digest(stream, "sha256").hexdigest()
        if preparation["sha256"].get(name) != actual:
            raise RuntimeError("native prepared tool bytes differ from their recorded source: " + name)


def main(arguments):
    operation, *values = arguments
    if operation == "set":
        repository, run, source, prefix, output = values
        Path(output).write_text(json.dumps(verify_set(repository, int(run), source, prefix), indent=2) + "\n")
        return
    if operation == "native":
        verify_native(*values)
        return
    if operation == "harness":
        verify_harness(*values)
        return
    if operation == "download":
        download(*values)
        return
    raise SystemExit("expected set, native, harness or download")


if __name__ == "__main__":
    import sys
    main(sys.argv[1:])
