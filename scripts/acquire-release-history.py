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

from fixtures import published_releases

REPOSITORY = "ShaulLavo/mesh"
TAG = re.compile(r"v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\Z")
PROOF = re.compile(r"[0-9a-f]{64}(?:\.rollback)?\.json\Z")
LIMITS = {"mesh-release.json": 512 << 10, "mesh-update.json": 64 << 10,
          "mesh-update.sig.json": 256 << 10, "mesh-update-root.json": 4 << 10,
          "mesh-retention.json": 16 << 20}


def api(endpoint):
    result = subprocess.run(["gh", "api", "--paginate", "--slurp", endpoint],
                            check=True, capture_output=True, timeout=120)
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
    return name in LIMITS or PROOF.fullmatch(name) or name.startswith("mesh-anchors-")


def api_digest(asset, cache):
    expected = asset.get("digest")
    if expected:
        if not re.fullmatch(r"sha256:[0-9a-f]{64}", expected):
            raise ValueError(f"unsupported asset digest: {asset['name']}")
        return expected.removeprefix("sha256:"), False
    _, hit = cache_asset(asset, cache, 128 << 20)
    return asset["digest"].removeprefix("sha256:"), hit


def acquire_release(item, cache, documents, cancelled):
    assets = item["assets"]
    if len(assets) >= 100:
        assets = api(f"repos/{REPOSITORY}/releases/{item['id']}/assets?per_page=100")
    names = [asset["name"] for asset in assets]
    if len(names) != len(set(names)):
        raise ValueError(f"duplicate release assets: {item['tag_name']}")
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
    return {"tag": item["tag_name"], "publishedAt": item["published_at"], "assets": retained}, hits


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
