"""The original ungrouped PC preparation recipe, private to one hosted job."""
import hashlib
import json
import shutil
import subprocess

from fixtures import (
    fleet_baselines,
    installed_clock,
    installed_origin,
    installed_releases,
)
from fixtures.interrupt_capture import ORIGINAL

SOURCE = "e46caa27a11a534345376df844cf30566d034efa"
PRODUCT = ("cmd", "internal", "third_party", "db", "go.mod", "go.sum", "scripts/install", "scripts/fixtures/native-helper")
TARGET = "v0.1.9001"
PLATFORM = {"os": "linux", "arch": "amd64"}
HOST = {"name": "pc", "platform": "linux/amd64", "daemon": "v0.1.249", "helper": "v0.1.196", "helperRunning": False}
HELPER = "35de97be239b0159bd4bff102d2491e311ae564523c9b5ebfc5860619d161484"
PRIOR = "ab917a253d1966675a1020398a859b4e24a1891770b7db529270718267eb92c5"


def git(*arguments):
    return subprocess.check_output(["git", *arguments], cwd=installed_releases.REPO,
                                   text=True, timeout=installed_clock.timeout(120)).strip()


def source_evidence():
    if git("status", "--porcelain=v1"):
        raise RuntimeError("diagnostic preparation requires the actual clean reviewed checkout")
    roots = {}
    for name in PRODUCT:
        historical, actual = git("rev-parse", SOURCE + ":" + name, "HEAD:" + name).splitlines()
        if actual != historical:
            raise RuntimeError("diagnostic product source differs from the pinned failure source: " + name)
        roots[name] = actual
    return {"checkoutSource": git("rev-parse", "HEAD"), "historicalProductSource": SOURCE,
            "identicalProductObjects": roots, "clean": True,
            "identity": "actual checkout VCS metadata; fresh supplemental fixture, not historical ephemeral bytes"}


def prepare(releases, source):
    root, output = releases.root, releases.output
    free = shutil.disk_usage(root).free
    (output / "storage-preflight.json").write_text(json.dumps({"root": str(root), "freeBytes": free,
                                                             "requiredFreeBytes": 4 << 30}) + "\n")
    if free < 4 << 30:
        raise RuntimeError("native signed fixture needs 4 GiB free before source builds and acquisition")
    preparer = root / "native-helper"
    with (root / "native-helper-build.log").open("wb") as log:
        subprocess.run(["go", "build", "-o", str(preparer), "./scripts/fixtures/native-helper"],
            cwd=installed_releases.REPO, env=releases.environment(), stdout=log, stderr=subprocess.STDOUT,
            check=True, timeout=installed_clock.timeout())
    releases.start_signer()
    origin = installed_origin.Origin(root)
    releases.build(TARGET)
    if releases.builds[TARGET]["commit"] != source["checkoutSource"]:
        raise RuntimeError("candidate VCS identity differs from the actual clean checkout")
    pair = fleet_baselines.acquire_pair(root / "public", HOST | {"daemonSha256": ORIGINAL, "helperSha256": HELPER})
    previous = fleet_baselines.acquire(root / "public", "v0.1.248", PLATFORM, PRIOR)
    inputs = verify_originals(pair, previous)
    proofs = [releases.prove(pair["daemon"]["executable"], TARGET)]
    releases.publish(TARGET, 0, proofs)
    origin.serve_tree(releases.directory)
    origin.serve_tree(root / "public")
    origin.head(releases.directory, TARGET)
    if source_evidence() != source:
        raise RuntimeError("checkout source changed during fixture preparation")
    (output / "preparation.json").write_text(json.dumps({"diagnosticOnly": True, "accepted": False,
        "source": source, "candidate": releases.builds[TARGET], "originals": inputs, "genuineFullProofs": proofs,
        "recipe": "Releases/start_signer/build/acquire_pair/prior/prove/publish; PC only"}, indent=2) + "\n")
    return origin, pair, previous, preparer


def verify_originals(pair, previous):
    images = ((pair["daemon"], "v0.1.249", ORIGINAL), (pair["helper"], "v0.1.196", HELPER),
              (previous, "v0.1.248", PRIOR))
    observed = {}
    for image, version, expected in images:
        with image["executable"].open("rb") as stream:
            digest = hashlib.file_digest(stream, "sha256").hexdigest()
        if image["version"] != version or image["artifact"]["binarySha256"] != expected or digest != expected:
            raise RuntimeError("original published input bytes differ: " + version)
        observed[version] = {"binarySha256": digest, "manifestSha256": image["manifestSha256"],
                             "archiveSha256": image["artifact"]["sha256"]}
    return observed
