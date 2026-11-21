"""Focused signed updates using the tested source and one retained native pair."""
import shutil
import subprocess

from fixtures import (
    fleet_baselines,
    installed_adoption,
    installed_chain,
    installed_clock,
    installed_fresh,
    installed_origin,
    installed_releases,
    installed_waypoint,
)


ORDER_PLATFORM = "linux/amd64"
ORDER_LATEST = "v0.1.9004"


def run(root, output, native):
    free = shutil.disk_usage(root).free
    installed_releases.write_json(output / "storage-preflight.json", {"root": str(root), "freeBytes": free})
    if free < 2 << 30:
        raise RuntimeError("installed PR fixture needs 2 GiB free for the native images and retained pair")
    host = next(host for host in fleet_baselines.load()["hosts"] if host["platform"] == native)
    ordered = native == ORDER_PLATFORM
    schemas = (1, 2) if ordered else (1,)
    releases = installed_releases.Releases(root, native, output, manifest_schemas=schemas, native_only=True)
    try:
        releases.start_signer()
        preparer = build_tool(releases, "native-helper", "./scripts/fixtures/native-helper", "build")
        origin = installed_origin.Origin(root)
        pair = fleet_baselines.acquire_pair(root / "public", host)
        prepare(releases, origin, pair, ordered)
        verify_cases(releases, origin, host, pair, preparer, ordered)
    finally:
        try:
            releases.close()
        finally:
            installed_adoption.retain_producer_logs(root, output)


def build_tool(releases, name, package, *command):
    path = releases.root / name
    with (releases.root / (name + ".log")).open("wb") as log:
        subprocess.run(["go", *command, "-o", str(path), package], cwd=installed_releases.REPO,
                       env=releases.environment(), check=True, timeout=installed_clock.timeout(),
                       stdout=log, stderr=subprocess.STDOUT)
    return path


def prepare(releases, origin, pair, ordered):
    first, intermediate, final = installed_releases.VERSIONS
    retained = pair["daemon"]
    releases.build(first)
    releases.publish(first, 0, [releases.prove(retained["executable"], first)])
    bind_retained_worker(releases, retained)
    # The original legacy worker and the first current worker both survive the
    # later hops. Every subsequent proof must cover both executing digests.
    releases.retain_custom_source(first)

    epoch = 2 if ordered else 1
    releases.build(intermediate, epoch=epoch)
    edge = releases.prove(releases.builds[first]["path"], intermediate)
    releases.publish(intermediate, 1, [edge], reader_epoch=epoch)
    if ordered:
        # The ordering case needs a direct successor after a failed canary.
        # Give it a separate tag so the waypoint keeps its own signed history.
        releases.build(ORDER_LATEST)
        edge = releases.prove(releases.builds[first]["path"], ORDER_LATEST)
        releases.publish(ORDER_LATEST, 2, [edge])
        installed_waypoint.publish_future(releases)
    else:
        releases.build(final)
        edge = releases.prove(releases.builds[intermediate]["path"], final)
        releases.publish(final, 2, [edge], anchors_from=1)
    origin.serve_tree(releases.directory)
    origin.serve_tree(releases.root / "public")
    origin.head(releases.directory, first)


def bind_retained_worker(releases, retained):
    executable = retained["executable"]
    build = installed_releases.source_build(executable, releases.environment(), releases.native)
    worker = {"version": retained["version"], "protocol": build["workerProtocol"], "artifact": retained["artifact"],
              "binary": str(executable.relative_to(releases.root))}
    releases.registry = releases.root / "pr-workers.json"
    installed_releases.write_json(releases.registry, {"schema": 1, "platform": build["platform"], "workers": [worker]})
    releases.probe = build_tool(releases, "worker-probe", "./internal/updatebootstrap", "test", "-c")


def verify_cases(releases, origin, host, pair, preparer, ordered):
    first, intermediate, _ = installed_releases.VERSIONS
    root, output = releases.root, releases.output
    # The legacy baseline starts without seeded policy or a prior journal.
    installed_adoption.run_case(root, output, host, pair, preparer, origin, releases, "helper")
    if releases.native != "linux/arm64":
        installed_adoption.run_case(root, output, host, pair, preparer, origin, releases, "interrupt")
    installed_fresh.run_fresh_case(root, output, releases, origin, intermediate)

    groups = installed_adoption.installed_groups(releases.registry, first, releases.builds[first]["path"], pair["daemon"]["executable"])
    if len(groups) != 1:
        raise RuntimeError("focused installed cases require exactly one retained worker group")
    if ordered:
        installed_waypoint.verify_readers(releases, groups[0])
    for failure in chain_profiles(releases.native):
        installed_chain.run_groups(root, output, host, pair, preparer, origin, releases, None, failure, groups)
    if ordered:
        origin.head(releases.directory, first)
        installed_clock.expect_samples(("ordered-owner:#1",))
        installed_adoption.execute_order(root, output, releases.native, preparer, host, pair, releases,
                                        (first, intermediate, ORDER_LATEST))


def chain_profiles(native):
    # Exercise rollback on both native managers. The AMD64 ordered test also
    # exercises rollback and failed-edge retention in a separate systemd domain.
    return {ORDER_PLATFORM: ("none", "first"), "linux/arm64": ("none", "final"),
            "darwin/arm64": ("none", "final")}[native]
