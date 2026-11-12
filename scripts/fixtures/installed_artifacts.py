"""Pin the complete producer artifact set before consuming native inputs."""
import json
import hashlib
import re
import subprocess
from pathlib import Path

KEYS = ("linux-amd64", "linux-arm64", "darwin-arm64")


def api(endpoint):
    result = subprocess.run(["gh", "api", endpoint], capture_output=True, check=True, timeout=30)
    return json.loads(result.stdout)


def verify_set(repository, run, source, prefix):
    if run <= 0 or not re.fullmatch(r"[0-9a-f]{40}", source) or not re.fullmatch(r"[A-Za-z0-9_.-]+", prefix):
        raise RuntimeError("native artifact inputs require an exact run, source SHA and artifact prefix")
    metadata = api(f"repos/{repository}/actions/runs/{run}")
    if metadata["head_sha"] != source:
        raise RuntimeError("native producer run belongs to a different pinned source")
    artifacts, page = [], 1
    while True:
        batch = api(f"repos/{repository}/actions/runs/{run}/artifacts?per_page=100&page={page}")["artifacts"]
        artifacts.extend(batch)
        if len(batch) < 100:
            break
        page += 1
    result = {}
    for key in KEYS:
        expected = prefix + "-" + key
        matches = [item for item in artifacts if item["name"] == expected and not item["expired"]]
        if len(matches) != 1:
            raise RuntimeError("complete native artifact set requires exactly one live " + expected)
        result[key] = {"id": matches[0]["id"], "name": expected, "digest": matches[0].get("digest")}
    return {"run": run, "source": source, "artifacts": result}


def verify_native(directory, source, platform):
    directory = Path(directory)
    reader = json.loads((directory / "tools/candidate-reader.json").read_text())
    registry = json.loads((directory / "retained/workers.json").read_text())
    expected = dict(zip(("os", "arch"), platform.split("/")))
    if reader["build"]["commit"] != source or reader["build"]["platform"] != expected or registry["platform"] != expected:
        raise RuntimeError("downloaded native producer source/platform differs from its pin")
    if reader.get("schema") != 1 or reader["build"].get("modified") is not False:
        raise RuntimeError("native producer did not retain an unmodified actual reader")
    with (directory / "tools/candidate").open("rb") as stream:
        if hashlib.file_digest(stream, "sha256").hexdigest() != reader["build"]["digest"]:
            raise RuntimeError("native producer candidate bytes differ from the retained capability pin")
    if not (directory / "tools/worker-probe").is_file() or not registry["workers"]:
        raise RuntimeError("native producer artifact lacks its exact workers or observer")


if __name__ == "__main__":
    import sys
    if sys.argv[1] == "set":
        value = verify_set(sys.argv[2], int(sys.argv[3]), sys.argv[4], sys.argv[5])
        Path(sys.argv[6]).write_text(json.dumps(value, indent=2) + "\n")
    elif sys.argv[1] == "native":
        verify_native(sys.argv[2], sys.argv[3], sys.argv[4])
    else:
        raise SystemExit("expected set or native")
