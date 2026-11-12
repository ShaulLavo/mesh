"""Build real unpublished checkpoint and migration candidates for one integration."""
import hashlib
import json
import os
import platform
import runpy
import subprocess
import sys
import tarfile
from pathlib import Path

from release_transition import migration_overlay

PLATFORMS = ("darwin/arm64", "linux/amd64", "linux/arm64")


def checkpoint():
    starts = Path(os.environ["MESH_PROOF_STARTS"])
    count = int(starts.read_text()) + 1 if starts.exists() else 1
    starts.write_text(str(count))
    state = Path(os.environ["MESH_STATE_DIR"])
    workers = list((state / "s").glob("*/sock"))
    assert len(workers) == 1, workers
    worker = workers[0]
    path = worker.parent / "recovery.json"
    before = path.read_bytes()
    subprocess.run([os.environ["MESH_PROOF_BINARY"], "recovery-command", worker.parent.name,
                    "--cwd", str(state), "--", "/bin/true", "checkpoint while daemon is stopped " + str(count)],
                   check=True)
    after = path.read_bytes()
    assert after != before, "retained worker did not publish a new checkpoint"
    record = json.loads(after)
    assert record["restart"]["argv"][1] == "checkpoint while daemon is stopped " + str(count), record
    Path(os.environ["MESH_PROOF_CHECKPOINT"]).write_bytes(after)
    if os.environ.get("MESH_PROOF_CORRUPT_SAVED") != "1" or count != 2:
        return
    saved = [path for path in (state / "s").glob("*/recovery.json") if not (path.parent / "sock").exists()]
    assert len(saved) == 1, saved
    saved[0].write_bytes(saved[0].read_bytes() + b"\n")


def checkpoint_overlay(repo, output):
    source = repo / "cmd/mesh/main.go"
    contents = source.read_text().replace('"context"', '"context"\n\tfixtureexec "os/exec"', 1)
    hook = '''func main() {
	if len(os.Args) > 1 && os.Args[1] == "daemon" && os.Getenv("MESH_PROOF_CHECKPOINT_TOOL") != "" {
		command := fixtureexec.Command("python3", os.Getenv("MESH_PROOF_CHECKPOINT_TOOL"), "checkpoint")
		command.Stdout, command.Stderr = os.Stdout, os.Stderr
		if err := command.Run(); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
'''
    assert contents.count("func main() {\n") == 1
    contents = contents.replace("func main() {\n", hook, 1)
    target = output / "main.go"
    target.write_text(contents)
    return {str(source): str(target)}


def digest(path):
    with path.open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def native_platform():
    arch = {"x86_64": "amd64", "aarch64": "arm64", "arm64": "arm64"}[platform.machine()]
    return ("darwin" if sys.platform == "darwin" else "linux") + "/" + arch


def build_image(repo, binary, target, version, overlay=None):
    binary.parent.mkdir(parents=True, exist_ok=True)
    goos, goarch = target.split("/")
    # Go omits linker settings with -trimpath. These fixture builds retain them
    # so foreign images can bind the same version without executing abroad.
    arguments = ["go", "build", "-ldflags",
                 "-s -w -buildid= -X github.com/shaul/mesh/internal/release.Version=" + version]
    if overlay:
        arguments.extend(["-overlay", str(overlay)])
    subprocess.run(arguments + ["-o", str(binary), "./cmd/mesh"], cwd=repo,
                   env=dict(os.environ, CGO_ENABLED="0", GOOS=goos, GOARCH=goarch), check=True)


def artifact_record(binary, target, build, source):
    info = subprocess.check_output(["go", "version", "-m", str(binary)], text=True)
    settings = dict(line.strip().removeprefix("build\t").split("=", 1)
                    for line in info.splitlines() if line.strip().startswith("build\t") and "=" in line)
    goos, goarch = target.split("/")
    if settings.get("GOOS") != goos or settings.get("GOARCH") != goarch:
        raise ValueError("fixture executable has another platform")
    if settings.get("vcs.revision") != build["commit"] or settings.get("vcs.modified") != "false":
        raise ValueError("fixture executable must report the actual clean source commit")
    if "github.com/shaul/mesh/internal/release.Version=" + build["version"] not in settings.get("-ldflags", ""):
        raise ValueError("fixture executable has another release version")
    image_digest = digest(binary)
    image_platform = {"os": goos, "arch": goarch}
    if image_platform == build["platform"] and image_digest != build["digest"]:
        raise ValueError("native fixture report differs from its actual image")
    archive = binary.parent / ("mesh_" + goos + "_" + goarch + ".tar.gz")
    with tarfile.open(archive, "w:gz", compresslevel=1) as package:
        package.add(binary, arcname="mesh")
    return {"schema": 1, "build": dict(build, platform=image_platform, digest=image_digest),
            "sources": [source], "artifact": {"platform": image_platform, "archive": archive.name,
                "sha256": digest(archive), "binarySha256": image_digest}}


def context(repo, output, source):
    candidate = output / native_platform().replace("/", "-") / "mesh"
    build = json.loads(subprocess.check_output([str(candidate), "version", "--json"], timeout=10))
    baseline = json.loads(subprocess.check_output([str(source), "version", "--json"], timeout=10))
    if baseline["digest"] != digest(source) or baseline["platform"] != build["platform"]:
        raise ValueError("fixture source report differs from its actual native image")
    for target in PLATFORMS:
        directory = output / target.replace("/", "-")
        record = artifact_record(directory / "mesh", target, build, baseline)
        (directory / "rollback-artifact.json").write_text(json.dumps(record) + "\n")
    producer = runpy.run_path(str(repo / "scripts/prepare-rollback-context.py"))
    producer["assemble"](output, output / "manifest.json")
    identity = {"fixtureOnly": True, "candidate": build,
        "source": baseline, "crossPlatforms": list(PLATFORMS),
        "manifestSha256": digest(output / "manifest.json"),
        "note": "Cross-platform files are measured build artifacts; only the native image executes here."}
    (output / "identity.json").write_text(json.dumps(identity, sort_keys=True) + "\n")
    print(json.dumps(identity, sort_keys=True), flush=True)


def prepare(repo, output, source, mode, migrations=None):
    output.mkdir()
    replacements = checkpoint_overlay(repo, output) if mode == "checkpoint" else {}
    if mode == "migration":
        migration_overlay(repo, output, migrations, replacements)
    overlay = output / "overlay.json"
    overlay.write_text(json.dumps({"Replace": replacements}))
    for target in PLATFORMS:
        build_image(repo, output / target.replace("/", "-") / "mesh", target, "v0.1.9001", overlay)
    context(repo, output, source)


if __name__ == "__main__":
    mode, *values = sys.argv[1:]
    if mode == "checkpoint":
        checkpoint()
    elif mode == "source":
        build_image(Path(values[0]), Path(values[1]), native_platform(), "v0.1.9000")
    elif mode == "prepare":
        prepare(Path(values[0]), Path(values[1]), Path(values[2]), values[3], Path(values[4]) if len(values) > 4 else None)
    else:
        raise ValueError(mode)
