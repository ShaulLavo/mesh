#!/usr/bin/env python3
"""Cache bounded public release evidence and retain the API's asset inventory."""
import argparse
import concurrent.futures
import hashlib
import json
import pathlib
import re
import subprocess
import sys
import threading
import tempfile
import os

from fixtures import published_releases, release_documents, release_tool, release_worker_groups, release_worker_index

REPOSITORY = "ShaulLavo/mesh"
TAG = re.compile(r"v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\Z")
PROOF = re.compile(r"[0-9a-f]{64}(?:\.rollback)?\.json\Z")
LIMITS = {"mesh-release.json": 1 << 20, "mesh-update.json": 64 << 10,
          "mesh-update.sig.json": 256 << 10, "mesh-update-root.json": 4 << 10,
          "mesh-retention.json": 16 << 20}
LIMITS["mesh-workers.json"] = 8 << 20
LIMITS["mesh-worker-groups.json"] = 1 << 20


def api(endpoint):
    try:
        result = subprocess.run(["gh", "api", "--paginate", "--slurp", endpoint],
                                check=True, capture_output=True, timeout=120)
    except (subprocess.CalledProcessError, subprocess.TimeoutExpired) as error:
        for name in ("stdout", "stderr"):
            data = getattr(error, name) or b""
            text = data.decode("utf-8", errors="replace") if isinstance(data, bytes) else data
            error.add_note(f"release API {name}: {text[:32768]}" + (" [truncated]" if len(text) > 32768 else ""))
        raise
    return [item for page in json.loads(result.stdout) for item in page]


def digest(data):
    return hashlib.sha256(data).hexdigest()


def download(url, limit):
    return published_releases.download(url, limit)


def cache_asset(asset, cache, limit):
    if type(asset.get("id")) is not int or asset["id"] <= 0:
        raise ValueError("invalid immutable asset ID")
    expected = asset.get("digest")
    if expected is not None and not re.fullmatch(r"sha256:[0-9a-f]{64}", expected):
        raise ValueError(f"unsupported asset digest: {asset['name']}")
    key = expected.removeprefix("sha256:") if expected else None
    identity = {field: asset.get(field) for field in ("id", "size", "updated_at")}
    mapping = cache / ("asset-" + str(asset["id"]))
    if not key and mapping.is_file():
        recorded = json.loads(mapping.read_bytes())
        if recorded["identity"] == identity:
            key = recorded["sha256"]
    if key and not re.fullmatch(r"[0-9a-f]{64}", key):
        raise ValueError("invalid cached asset digest")
    if key and (cache / key).is_file():
        if (cache / key).stat().st_size > limit:
            raise ValueError(f"corrupt immutable cache: {key}")
        data = (cache / key).read_bytes()
        if len(data) > limit or digest(data) != key:
            raise ValueError(f"corrupt immutable cache: {key}")
        asset["digest"] = "sha256:" + key
        return data, True
    data = download(asset["browser_download_url"], limit)
    actual = digest(data)
    if key and key != actual:
        raise ValueError(f"asset digest changed: {asset['name']}")
    destination = cache / actual
    temporary = cache / f".{actual}.tmp"
    temporary.write_bytes(data)
    temporary.replace(destination)
    asset["digest"] = "sha256:" + actual
    if not expected:
        mapping.write_text(json.dumps({"identity": identity, "sha256": actual}) + "\n")
    return data, False


def route_asset(name):
    return name in LIMITS or PROOF.fullmatch(name) or release_worker_groups.ASSET.fullmatch(name) or name.startswith("mesh-anchors-")


def api_digest(asset, cache):
    expected = asset.get("digest")
    if expected:
        if not re.fullmatch(r"sha256:[0-9a-f]{64}", expected):
            raise ValueError(f"unsupported asset digest: {asset['name']}")
        return expected.removeprefix("sha256:"), False
    _, hit = cache_asset(asset, cache, 128 << 20)
    return asset["digest"].removeprefix("sha256:"), hit


def acquire_release(item, cache, documents, cancelled):
    assets = api(f"repos/{REPOSITORY}/releases/{item['id']}/assets?per_page=100")
    names = [asset["name"] for asset in assets]
    if len(names) != len(set(names)):
        raise ValueError(f"duplicate release assets: {item['tag_name']}")
    if release_documents.BUNDLE in names:
        return acquire_bundled_release(item, assets, cache, documents, cancelled)
    if any(name.startswith("mesh-update") for name in names):
        raise ValueError("signed updater history requires its current authenticated bundle transport")
    directory = documents / item["tag_name"]
    directory.mkdir(parents=True, exist_ok=True)
    hits = 0
    retained = []
    for asset in assets:
        if cancelled.is_set():
            raise RuntimeError("release acquisition cancelled after a required source failed")
        name = asset["name"]
        if "/" in name or "\\" in name or name in (".", ".."):
            raise ValueError(f"invalid release asset name: {name}")
        if not route_asset(name):
            expected, hit = api_digest(asset, cache)
            hits += int(hit)
            retained.append({"name": name, "sha256": expected})
            continue
        data, hit = cache_asset(asset, cache, LIMITS.get(name, 1 << 20))
        hits += int(hit)
        (directory / name).write_bytes(data)
        retained.append({"name": name, "sha256": digest(data)})
    if (directory / "mesh-worker-groups.json").exists():
        release_worker_index.validate_index(directory / "mesh-worker-groups.json", json.loads((directory / "mesh-release.json").read_bytes()))
    return {"tag": item["tag_name"], "publishedAt": item["published_at"], "assets": retained}, hits


def acquire_bundled_release(item, assets, cache, documents, cancelled):
    selected = [asset for asset in assets if asset["name"] in release_documents.PHYSICAL]
    if len(selected) != len(release_documents.PHYSICAL) or item.get("draft") is not False:
        raise ValueError("signed history requires every current physical metadata asset and explicit publication state")
    documents.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix=".history-", dir=documents.parent) as temporary:
        staging = pathlib.Path(temporary)
        capture = staging / "physical"
        capture.mkdir()
        hits = capture_history(item, selected, cache, capture, cancelled)
        value = release_tool.materialize_history(capture, item, staging / "logical", staging / "logical-inventory.json")
        manifest = json.loads((staging / "logical/mesh-release.json").read_bytes())
        release_worker_index.validate_index(staging / "logical/mesh-worker-groups.json", manifest)
        retained = history_references(value["documents"], manifest, assets)
        destination = documents / item["tag_name"]
        publish_logical(staging / "logical", destination, value["documents"])
    (documents / (item["tag_name"] + ".physical.json")).write_text(json.dumps(dict(item, repository=REPOSITORY, assets=assets), sort_keys=True) + "\n")
    return {"tag": item["tag_name"], "publishedAt": item["published_at"], "assets": retained}, hits


def capture_history(item, assets, cache, destination, cancelled):
    hits = 0
    limits = {release_documents.BUNDLE: 32 << 20, release_documents.ENVELOPE: 256 << 10,
              "mesh-release.json": LIMITS["mesh-release.json"], "mesh-update-root.json": 4 << 10}
    for asset in assets:
        if cancelled.is_set():
            raise RuntimeError("release acquisition cancelled after a required source failed")
        require_history_asset(asset, limits[asset["name"]])
        raw, hit = cache_asset(asset, cache, limits[asset["name"]])
        if len(raw) != asset["size"]:
            raise ValueError("historical metadata differs from its actual API asset size")
        hits += int(hit)
        (destination / asset["name"]).write_bytes(raw)
    snapshot = dict(item, repository=REPOSITORY, assets=assets)
    (destination / "history.json").write_text(json.dumps(snapshot, sort_keys=True) + "\n")
    return hits


def require_history_asset(asset, limit):
    if (type(asset.get("id")) is not int or asset["id"] <= 0 or asset.get("state") != "uploaded"
            or type(asset.get("size")) is not int or not 0 < asset["size"] <= limit
            or not re.fullmatch(r"sha256:[0-9a-f]{64}", asset.get("digest") or "")):
        raise ValueError("signed historical metadata requires its actual uploaded API ID, size and digest")


def history_references(documents, manifest, assets):
    retained = [{"name": item["name"], "sha256": item["sha256"]} for item in documents]
    for artifact in manifest["artifacts"]:
        matches = [item for item in assets if item["name"] == artifact["archive"]]
        if (len(matches) != 1 or matches[0].get("state") != "uploaded" or type(matches[0].get("id")) is not int
                or matches[0]["id"] <= 0 or matches[0].get("digest") not in (None, "sha256:" + artifact["sha256"])):
            raise ValueError("historical native inventory differs from its authenticated manifest")
        retained.append({"name": artifact["archive"], "sha256": artifact["sha256"]})
    if len({item["name"] for item in retained}) != len(retained):
        raise ValueError("logical history repeats an authenticated document or native artifact")
    return retained


def publish_logical(source, destination, entries):
    if destination.exists():
        release_tool.validate_logical_inventory(destination, entries)
        return
    os.replace(source, destination)


def collect_releases(pool, history, cache, documents):
    cancelled = threading.Event()
    futures = [pool.submit(acquire_release, item, cache, documents, cancelled) for item in history]
    try:
        return [future.result() for future in concurrent.futures.as_completed(futures)]
    except BaseException:
        cancelled.set()
        for future in futures:
            future.cancel()
        raise


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("documents", type=pathlib.Path)
    parser.add_argument("cache", type=pathlib.Path)
    parser.add_argument("inventory", type=pathlib.Path)
    args = parser.parse_args()
    args.cache.mkdir(parents=True, exist_ok=True)
    args.documents.mkdir(parents=True, exist_ok=True)
    history = api(f"repos/{REPOSITORY}/releases?per_page=100")
    history = [item for item in history if not item["draft"] and not item["prerelease"]
               and TAG.fullmatch(item["tag_name"])]
    with concurrent.futures.ThreadPoolExecutor(max_workers=6) as pool:
        results = collect_releases(pool, history, args.cache, args.documents)
    releases = sorted((item for item, _ in results), key=lambda item: tuple(map(int, item["tag"][1:].split("."))))
    args.inventory.write_text(json.dumps(releases, indent=2) + "\n")
    print(f"acquired {len(releases)} published releases; verified immutable cache hits: {sum(hits for _, hits in results)}", file=sys.stderr)


if __name__ == "__main__":
    main()
