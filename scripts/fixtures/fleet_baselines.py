"""Acquire the observed legacy pairs from their actual published archives."""
import json
from pathlib import Path

from fixtures import published_releases

FIXTURE = Path(__file__).with_name("fleet-baselines.json")


def load():
    data = json.loads(FIXTURE.read_text())
    if data.get("schema") != 1:
        raise RuntimeError("unsupported fleet baseline fixture")
    hosts = data["hosts"]
    if len({host["name"] for host in hosts}) != len(hosts):
        raise RuntimeError("duplicate fleet baseline host")
    return data


def acquire(root, version, platform, expected=None, fetcher=published_releases.download):
    root = Path(root) / version
    root.mkdir(parents=True, exist_ok=True)
    raw = published_releases.fetch(root.parent, version, "mesh-release.json", 1 << 20, fetcher)
    manifest = json.loads(raw)
    if manifest.get("schema") != 1 or manifest.get("version") != version:
        raise RuntimeError("fleet baseline manifest schema or version differs")
    artifacts = [entry for entry in manifest["artifacts"] if entry["platform"] == platform]
    if len(artifacts) != 1:
        raise RuntimeError("fleet baseline requires one exact native archive")
    artifact = artifacts[0]
    archive = published_releases.fetch(root.parent, version, artifact["archive"], 128 << 20, fetcher)
    binary = published_releases.archive_binary(archive, artifact)
    if expected and artifact["binarySha256"] != expected:
        raise RuntimeError("published binary differs from observed installed fleet image")
    executable = root / "mesh"
    # Write executable inputs only after both archive and executable digests verify.
    (root / "mesh-release.json").write_bytes(raw)
    (root / artifact["archive"]).write_bytes(archive)
    executable.write_bytes(binary)
    executable.chmod(0o755)
    return {"version": version, "manifest": manifest, "executable": executable,
            "manifestSha256": published_releases.digest(raw), "artifact": artifact}


def acquire_pair(root, host, fetcher=published_releases.download):
    system, architecture = host["platform"].split("/")
    platform = {"os": system, "arch": architecture}
    return {role: acquire(root, host[role], platform, host.get(role + "Sha256"), fetcher)
            for role in ("daemon", "helper")}


def first_updater(fixture):
    version = fixture.get("firstPublishedUpdater")
    if not version:
        raise RuntimeError("first published updater baseline is unavailable; a PR-source label cannot replace it")
    return version
