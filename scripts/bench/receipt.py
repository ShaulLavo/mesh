#!/usr/bin/env python3
"""Bind benchmark provenance to the actual Go executable and compilation inputs."""
import argparse
import hashlib
import json
from pathlib import Path
import platform
import re
import subprocess


PROFILE_MARKER = b"main.benchProfile"


def digest(data):
    return hashlib.sha256(data).hexdigest()


def decode_string(data, offset):
    size = 0
    for shift in range(0, 64, 7):
        byte = data[offset]
        offset += 1
        size |= (byte & 127) << shift
        if byte < 128:
            end = offset + size
            if end > len(data):
                raise ValueError("truncated Go build information")
            return data[offset:end], end
    raise ValueError("invalid Go string length")


def inspect_binary(binary):
    data = Path(binary).read_bytes()
    magic = b"\xff Go buildinf:"
    for found in re.finditer(re.escape(magic), data):
        start = found.start()
        if start % 16 or data[start + 14] not in (4, 8) or data[start + 15] & 2 == 0:
            continue
        version, offset = decode_string(data, start + 32)
        module, _ = decode_string(data, offset)
        if not re.fullmatch(rb"go\d+\.\d+(?:\.\d+)?", version):
            continue
        if len(module) < 33 or module[-17] != 10:
            continue
        module = module[16:-16].decode()
        settings = {}
        for line in module.splitlines():
            if line.startswith("build\t"):
                key, value = line[6:].split("=", 1)
                settings[key] = json.loads(value) if value.startswith('"') else value
        paths = [line[5:] for line in module.splitlines() if line.startswith("path\t")]
        if len(paths) != 1 or not re.fullmatch(r"github.com/shaul/mesh/(cmd/mesh|internal/[a-z]+\.test)", paths[0]):
            raise ValueError("executable is not Mesh or a Mesh benchmark")
        if not all(key in settings for key in ("GOOS", "GOARCH", "CGO_ENABLED")):
            raise ValueError("executable lacks target build settings")
        return {"sha256": digest(data), "bytes": len(data), "go_version": version.decode(),
                "build_info": module, "build_settings": settings,
                "profiled": PROFILE_MARKER in data}
    raise ValueError("unsupported or missing inline Go build information")


def git(root, *args):
    return subprocess.check_output(["git", "-C", str(root), *args], stderr=subprocess.PIPE)


def source_snapshot(root, arch, profiled, tags="", package="./cmd/mesh"):
    import os
    env = {**os.environ, "CGO_ENABLED": "0", "GOOS": "linux", "GOARCH": arch}
    command = ["go", "list", "-deps", "-json"]
    if tags:
        command += ["-tags", tags]
    if package != "./cmd/mesh":
        command.append("-test")
    text = subprocess.check_output([*command, package], cwd=root, env=env).decode()
    decoder = json.JSONDecoder()
    files = {"go.mod", "go.sum"}
    benchmarks = set()
    while text.strip():
        item, end = decoder.raw_decode(text.lstrip())
        text = text.lstrip()[end:]
        directory = Path(item.get("Dir", "/"))
        if not directory.is_relative_to(root):
            continue
        for field in ("GoFiles", "CgoFiles", "CFiles", "CXXFiles", "HFiles", "SFiles", "SysoFiles", "EmbedFiles"):
            for name in item.get(field, []):
                path = directory / name
                if not path.is_relative_to(root):
                    continue
                relative = str(path.relative_to(root))
                (benchmarks if path.name.endswith("_test.go") else files).add(relative)
    hashes = {name: digest((root / name).read_bytes()) for name in sorted(files)}
    result = {"files": hashes, "sha256": digest(json.dumps(hashes, sort_keys=True).encode()),
              "target": {"GOOS": "linux", "GOARCH": arch, "CGO_ENABLED": "0"}, "tags": tags,
              "package": package, "benchmark_files": {name: digest((root / name).read_bytes()) for name in sorted(benchmarks)},
              "profile_overlay_sha256": None}
    if profiled:
        # Both the inserted main call and helper are identified, outside production inputs.
        helper = (root / "scripts/bench/profile-main.go.txt").read_bytes()
        result["profile_overlay_sha256"] = digest(b"func main() {\n finish := benchProfile()\n defer finish()" + helper)
    return result


def create_receipt(binary, root, before):
    observed = inspect_binary(binary)
    settings = observed["build_settings"]
    after = source_snapshot(root, settings["GOARCH"], observed["profiled"], settings.get("-tags", ""), before["package"])
    if before != after:
        raise ValueError("compilation inputs changed during build")
    if any(settings[key] != value for key, value in before["target"].items()):
        raise ValueError("binary target differs from captured compilation inputs")
    source_revision = git(root, "rev-parse", "HEAD").decode().strip()
    revision = settings.get("vcs.revision")
    differences = []
    vcs_files = {}
    for name, expected in before["files"].items():
        try:
            blob = git(root, "show", f"{revision}:{name}") if revision else b""
        except subprocess.CalledProcessError:
            blob = b""
        if blob:
            vcs_files[name] = digest(blob)
        if vcs_files.get(name) != expected:
            differences.append(name)
    return {"schema_version": 1, "binary": observed, "production_source": before,
            "embedded_vcs_revision": revision, "source_vcs_head": source_revision,
            "source_vcs_dirty": bool(git(root, "status", "--porcelain")),
            "production_equivalent_to_vcs_revision": bool(revision) and not differences,
            "production_differences_from_vcs": differences, "vcs_production_files": vcs_files,
            "harness_revision": git(root, "rev-parse", "HEAD").decode().strip(),
            "harness_dirty": bool(git(root, "status", "--porcelain", "--", "scripts/bench", "integration/bench_harness.sh")),
            "source_capture": "identical before and after build"}


def verify_receipt(binary, receipt, *, profiled=None, native=True, commit=None, go_version=None):
    observed = inspect_binary(binary)
    if receipt.get("schema_version") != 1 or receipt.get("binary") != observed:
        raise ValueError("binary differs from build receipt")
    if receipt.get("embedded_vcs_revision") != observed["build_settings"].get("vcs.revision"):
        raise ValueError("receipt VCS revision differs from binary")
    source = receipt.get("production_source", {})
    required = {"files", "sha256", "target", "tags", "package", "benchmark_files", "profile_overlay_sha256"}
    if not required.issubset(source):
        raise ValueError("incomplete compilation-input receipt")
    files = source["files"]
    if not files or source.get("sha256") != digest(json.dumps(files, sort_keys=True).encode()):
        raise ValueError("invalid production-source receipt")
    if receipt.get("source_capture") != "identical before and after build":
        raise ValueError("receipt lacks bounded compilation-input capture")
    expected_target = {key: observed["build_settings"][key] for key in ("GOOS", "GOARCH", "CGO_ENABLED")}
    if source["target"] != expected_target:
        raise ValueError("receipt source target differs from binary")
    vcs_files = receipt.get("vcs_production_files")
    if not isinstance(vcs_files, dict):
        raise ValueError("receipt lacks production VCS comparison")
    differences = sorted(name for name, sha in files.items() if vcs_files.get(name) != sha)
    equivalent = bool(receipt["embedded_vcs_revision"]) and not differences
    if receipt.get("production_differences_from_vcs") != differences or receipt.get("production_equivalent_to_vcs_revision") != equivalent:
        raise ValueError("receipt production VCS comparison is inconsistent")
    if source["tags"] != observed["build_settings"].get("-tags", ""):
        raise ValueError("receipt build tags differ from binary")
    if bool(source.get("profile_overlay_sha256")) != observed["profiled"]:
        raise ValueError("receipt profiling inputs differ from binary")
    if profiled is not None and observed["profiled"] != profiled:
        raise ValueError("profiling mode differs from binary")
    arch = {"x86_64": "amd64", "aarch64": "arm64"}.get(platform.machine())
    if native and (observed["build_settings"]["GOARCH"] != arch or observed["build_settings"]["GOOS"] != "linux"):
        raise ValueError("binary target differs from benchmark host")
    if commit is not None and (commit != receipt.get("embedded_vcs_revision") or not receipt.get("production_equivalent_to_vcs_revision")):
        raise ValueError("source label differs from verified production source")
    if go_version is not None and go_version != observed["go_version"]:
        raise ValueError("toolchain label differs from embedded compiler")
    return receipt


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=("snapshot", "create", "verify"))
    parser.add_argument("--root", type=Path)
    parser.add_argument("--binary", type=Path)
    parser.add_argument("--receipt", type=Path)
    parser.add_argument("--sources", type=Path)
    parser.add_argument("--arch")
    parser.add_argument("--package", default="./cmd/mesh")
    parser.add_argument("--profile", action="store_true")
    parser.add_argument("--cross-target", action="store_true")
    args = parser.parse_args()
    if args.action == "snapshot":
        value = source_snapshot(args.root.resolve(), args.arch, args.profile, package=args.package)
        args.sources.write_text(json.dumps(value, indent=2) + "\n")
        return
    if args.action == "create":
        value = create_receipt(args.binary, args.root.resolve(), json.loads(args.sources.read_text()))
        args.receipt.write_text(json.dumps(value, indent=2) + "\n")
        return
    value = verify_receipt(args.binary, json.loads(args.receipt.read_text()), native=not args.cross_target)
    print(json.dumps(value, indent=2))


if __name__ == "__main__":
    main()
