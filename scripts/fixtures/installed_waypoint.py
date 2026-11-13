"""Original executable checks for an actually incompatible future manifest."""
import base64
import json
import os
import subprocess

from fixtures import installed_origin, installed_releases, installed_clock


def prepare(releases, origin, source):
    first, waypoint, future = installed_releases.VERSIONS
    releases.build(first)
    releases.publish(first, 0, [releases.prove(source, first)])
    releases.build(waypoint, epoch=2)
    releases.publish(waypoint, 1, [releases.prove(releases.builds[first]["path"], waypoint)], reader_epoch=2)
    releases.retain_custom_source(first)
    releases.build(future, epoch=2)
    proof = releases.prove(releases.builds[waypoint]["path"], future)
    releases.manifests[future]["schema"] = 2
    prior = releases.metadata[waypoint]
    entries = [{"ordinal": 0, "version": waypoint, "manifest": prior["manifest"],
        "anchorsFrom": 0, "publishedAt": prior["publishedAt"]}]
    releases.publish(future, 1, [proof], epoch=2, reader_epoch=2, entries=entries)
    previous = releases.directory / waypoint / "mesh-update-root.json"
    root = releases.directory / future / "mesh-update-root.json"
    value = json.loads(root.read_text())
    value["previous"] = {"tag": waypoint, "name": previous.name, "sha256": installed_releases.digest(previous.read_bytes())}
    installed_releases.write_json(root, value)
    origin.serve_tree(releases.directory)
    origin.head(releases.directory, future)


def documents(releases, version):
    directory = releases.directory / version
    metadata = json.loads((directory / "mesh-update.json").read_text())
    files = {"metadata": directory / "mesh-update.json", "envelope": directory / "mesh-update.sig.json",
        "manifest": directory / "mesh-release.json", "page": directory / metadata["page"]["name"]}
    return {key: base64.b64encode(path.read_bytes()).decode() for key, path in files.items()}


def packet(releases, source, target, group):
    build = releases.builds[source]
    manifest = releases.manifests[target]
    transition = next(item for item in manifest["compatibility"]["transitions"] if item["fromDigest"] == build["digest"])
    rollback = next(item for item in releases.metadata[target]["rollback"] if item["forward"] == transition["proof"])
    directory = releases.directory / target
    selected = group["members"]
    workers = [{"digest": item["artifact"]["binarySha256"], "protocol": item["protocol"]} for item in selected]
    return {"schema": 1, "source": documents(releases, source), "target": documents(releases, target),
        "forward": base64.b64encode((directory / (transition["proof"] + ".json")).read_bytes()).decode(),
        "rollback": base64.b64encode((directory / (rollback["rollback"] + ".rollback.json")).read_bytes()).decode(), "workers": workers}


def execute(releases, source, value, label):
    path = releases.root / (label + ".packet.json")
    installed_releases.write_json(path, value)
    cache = releases.root / (label + "-trust")
    cache.mkdir()
    blocked = releases.root / "blocked-user-config"
    blocked.write_text("preserve read-only user paths")
    environment = os.environ | {"MESH_STATE_DIR": str(blocked), "MESH_CONFIG_DIR": str(blocked),
        "HTTP_PROXY": "http://127.0.0.1:1", "HTTPS_PROXY": "http://127.0.0.1:1", "NO_PROXY": ""}
    result = subprocess.run([releases.builds[source]["path"], "update-reader", "-input", str(path), "-trust-cache", str(cache)],
        env=environment, capture_output=True, text=True, timeout=15, check=False)
    if list(cache.iterdir()) or blocked.read_text() != "preserve read-only user paths":
        raise RuntimeError("original reader changed offline trust or user configuration")
    return result


@installed_clock.timed("readers", lambda releases, group: str(group["index"]))
def verify_readers(releases, group):
    first, waypoint, future = installed_releases.VERSIONS
    suffix = "-group-" + str(group["index"])
    old_to_waypoint = execute(releases, first, packet(releases, first, waypoint, group), "old-waypoint" + suffix)
    waypoint_to_future = execute(releases, waypoint, packet(releases, waypoint, future, group), "waypoint-future" + suffix)
    if old_to_waypoint.returncode or waypoint_to_future.returncode:
        raise RuntimeError("original executable waypoint verification failed: " + old_to_waypoint.stderr + waypoint_to_future.stderr)
    negative = packet(releases, waypoint, future, group)
    negative["source"] = documents(releases, first)
    directory = releases.root / ("negative-future" + suffix)
    directory.mkdir()
    page = installed_releases.write_json(directory / "page.json", {"schema": 1, "epoch": 1, "page": 0, "entries": [], "back": []})
    metadata = releases.metadata[future] | {"epoch": 1, "updater": {"schema": 1, "epoch": 1, "position": {"anchor": True, "ordinal": 1}},
        "page": {"tag": future, "name": "negative.anchors.json", "sha256": installed_releases.digest(page)}}
    raw = installed_releases.write_json(directory / "metadata.json", metadata)
    installed_origin.sign(releases.root, [directory / "metadata.json"], directory / "envelope.json")
    negative["target"].update(metadata=base64.b64encode(raw).decode(), page=base64.b64encode(page).decode(),
        envelope=base64.b64encode((directory / "envelope.json").read_bytes()).decode())
    rejected = execute(releases, first, negative, "old-incompatible-manifest" + suffix)
    if rejected.returncode == 0 or "manifest schema 2 is unsupported" not in rejected.stderr:
        raise RuntimeError("original reader did not reject actual incompatible manifest bytes: " + rejected.stderr)
    (releases.output / ("original-format-readers" + suffix + ".json")).write_text(json.dumps({"supplementalSourceBuilds": True,
        "oldToWaypoint": json.loads(old_to_waypoint.stdout), "waypointToFuture": json.loads(waypoint_to_future.stdout),
        "oldRejectedManifestSchema2": rejected.stderr.strip()}, indent=2) + "\n")
