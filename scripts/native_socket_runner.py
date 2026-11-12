#!/usr/bin/env python3
"""Run the complete native socket suites with bounded package-test partitions."""
import argparse
import asyncio
from collections import Counter
from contextlib import ExitStack
import json
import os
from pathlib import Path
import re
import signal
import sys
import tempfile
import time

PACKAGES = ("daemon", "cli", "worker", "updatebootstrap", "paths", "testenv")
CLI_PARTITIONS = 4
CONCURRENCY = 4
ALARM = "350s"


def partition(names, count):
    ordered = sorted(names)
    if len(set(ordered)) != len(ordered):
        raise ValueError("duplicate test names in binary inventory")
    return [ordered[index::count] for index in range(min(count, len(ordered)))] or [[]]


def inventory(output):
    names = output.decode().splitlines()
    if any(not name.startswith(("Test", "Example", "Fuzz", "Benchmark")) for name in names):
        raise ValueError(f"unexpected test inventory output: {names!r}")
    return [name for name in names if not name.startswith("Benchmark")]


def check_coverage(names, events):
    wanted = Counter(names)
    started = Counter(event["Test"] for event in events if event["Action"] == "run")
    finished = Counter(event["Test"] for event in events if event["Action"] in ("pass", "skip", "fail"))
    if started != wanted or finished != wanted:
        raise ValueError(f"test coverage differs: wanted={wanted}, started={started}, finished={finished}")
    if any(event["Action"] == "fail" for event in events):
        raise ValueError("a selected test failed")


async def stop_group(process):
    try:
        await terminate_group(process.pid)
    except ProcessLookupError:
        await process.wait()
        return
    try:
        os.killpg(process.pid, signal.SIGKILL)
    except ProcessLookupError:
        pass
    await process.wait()


async def terminate_group(pid):
    try:
        os.killpg(pid, signal.SIGTERM)
    except PermissionError:
        # Darwin can deny a group containing only an unreaped zombie. Give the
        # child watcher the existing grace, then require proof the group is gone.
        await asyncio.sleep(0.1)
        os.killpg(pid, 0)
        raise
    await asyncio.sleep(0.1)


async def command(args, cwd, env, consume=None):
    creation = asyncio.create_task(asyncio.create_subprocess_exec(
        *map(str, args), cwd=cwd, env=env, start_new_session=True,
        stdout=asyncio.subprocess.PIPE, stderr=asyncio.subprocess.STDOUT,
        # A test can log a large JSON line, matching go test -json's streaming behavior.
        limit=16 * 1024 * 1024,
    ))
    try:
        # Cancellation must not lose a process created before asyncio hands it back.
        process = await asyncio.shield(creation)
        return await collect(process, consume)
    finally:
        await join_cleanup(asyncio.create_task(stop_created_group(creation)))


async def stop_created_group(creation):
    process = await creation
    await stop_group(process)


async def join_cleanup(task):
    cancelled = None
    while not task.done():
        try:
            await asyncio.shield(task)
        except asyncio.CancelledError as error:
            # A second cancellation must not interrupt acquisition or group teardown.
            cancelled = error
    task.result()
    if cancelled is not None:
        raise cancelled


async def collect(process, consume):
    if consume is None:
        output, _ = await process.communicate()
        if process.returncode:
            raise RuntimeError(f"command exited {process.returncode}: {output.decode(errors='replace')}")
        return output
    async for line in process.stdout:
        consume(line)
    return await process.wait()


def record(line, shard, output, events):
    event = json.loads(line)
    event["Shard"] = shard["id"]
    encoded = json.dumps(event) + "\n"
    output.write(encoded)
    output.flush()
    sys.stdout.write(encoded)
    sys.stdout.flush()
    if event.get("Test") and "/" not in event["Test"] and event["Action"] in ("run", "pass", "skip", "fail"):
        events.append({"Test": event["Test"], "Action": event["Action"]})


async def execute(shard, semaphore, env, output):
    async with semaphore:
        env = isolated_environment(env, Path(shard["environmentRoot"]))
        started = time.monotonic()
        events = []
        pattern = "^(" + "|".join(re.escape(name) for name in shard["tests"]) + ")$"
        args = (shard["test2json"], "-t", "-p", shard["package"], shard["binary"],
                "-test.v=test2json", "-test.count=1", f"-test.timeout={ALARM}", f"-test.run={pattern}")
        try:
            code = await command(args, shard["cwd"], env, lambda line: record(line, shard, output, events))
            shard["exitCode"] = code
            check_coverage(shard["tests"], events)
            require_success(code)
            shard["status"] = "pass"
        except Exception as error:
            shard["status"] = "fail"
            shard["error"] = str(error)
        finally:
            shard["elapsedSeconds"] = round(time.monotonic() - started, 3)


def require_success(code):
    if code:
        raise RuntimeError(f"test process exited {code}")


def isolated_environment(env, root):
    (root / "home").mkdir(parents=True, exist_ok=True)
    return env | {"HOME": str(root / "home"), "MESH_STATE_DIR": str(root / "state"),
                  "MESH_CONFIG_DIR": str(root / "config"), "XDG_STATE_HOME": str(root / "xdg-state"),
                  "XDG_CONFIG_HOME": str(root / "xdg-config"), "XDG_CACHE_HOME": str(root / "xdg-cache")}


async def prepare(repo, binaries, env, packages, runtime_root=None):
    runtime_root = runtime_root or binaries.parent / "runtime"
    selected = [f"./internal/{name}" for name in packages]
    metadata = await command(("go", "list", "-f", "{{.ImportPath}}\t{{.Dir}}\t{{.Name}}", *selected), repo, env)
    rows = [line.split("\t") for line in metadata.decode().splitlines()]
    if len(rows) != len(packages) or len({row[2] for row in rows}) != len(packages):
        raise ValueError("native test packages must have distinct binary names")
    # One invocation builds every package test binary once and reuses dependency caches.
    await command(("go", "test", "-race", "-c", "-o", str(binaries) + os.sep, *selected), repo, env)
    test2json = (await command(("go", "tool", "-n", "test2json"), repo, env)).decode().strip()
    shards = []
    for package, cwd, name in rows:
        binary = binaries / f"{name}.test"
        listing = await command((binary, "-test.list=.", f"-test.timeout={ALARM}"), cwd,
                                isolated_environment(env, runtime_root / f"{name}-inventory"))
        groups = partition(inventory(listing), CLI_PARTITIONS if name == "cli" else 1)
        for index, tests in enumerate(groups):
            shards.append({"id": f"{name}-{index + 1}", "package": package, "cwd": cwd,
                           "binary": str(binary), "tests": tests, "status": "pending",
                           "test2json": test2json,
                           "environmentRoot": str(runtime_root / f"{name}-{index + 1}")})
    return shards


async def run(repo, output_path, short_base, scratch_base, packages=PACKAGES):
    loop = asyncio.get_running_loop()
    loop.add_signal_handler(signal.SIGTERM, asyncio.current_task().cancel)
    schedule = {"alarm": ALARM, "concurrency": CONCURRENCY, "shards": [], "status": "fail"}
    started = time.monotonic()
    try:
        await run_in_scratch(repo, output_path, short_base, scratch_base, packages, schedule)
        schedule["status"] = "pass"
    except BaseException as error:
        schedule["error"] = str(error) or type(error).__name__
        raise
    finally:
        schedule["elapsedSeconds"] = round(time.monotonic() - started, 3)
        scratch = schedule.get("scratch")
        if scratch:
            scratch["removed"] = not any(Path(scratch[name]).exists() for name in ("short", "long"))
        output_path.with_suffix(".schedule.json").write_text(json.dumps(schedule, indent=2) + "\n")


async def run_in_scratch(repo, output_path, short_base, scratch_base, packages, schedule):
    with ExitStack() as cleanup, output_path.open("w") as output:
        short = Path(cleanup.enter_context(tempfile.TemporaryDirectory(prefix="ns-", dir=short_base)))
        long = Path(cleanup.enter_context(tempfile.TemporaryDirectory(
            prefix="mesh-socket-portability-long-scratch-root.", dir=scratch_base)))
        binaries = long / "binaries"
        binaries.mkdir()
        (short / "alias").symlink_to(long, target_is_directory=True)
        env = os.environ | {"TMPDIR": str(short / "alias") + "/", "MESH_SHORT_TMP": str(short)}
        schedule["scratch"] = {"short": str(short), "long": str(long), "removed": False}
        caches = await command(("go", "env", "-json", "GOCACHE", "GOMODCACHE", "GOPATH", "GOROOT"), repo, env)
        env.update(json.loads(caches))
        # Keep toolchain shims that depend on the caller's HOME out of isolated tests.
        env["PATH"] = str(Path(env["GOROOT"]) / "bin") + os.pathsep + env["PATH"]
        schedule["shards"] = await prepare(repo, binaries, env, packages, short)
        await run_shards(schedule["shards"], env, output)
        # Keep the native long-path rejection and physical short-socket controls blocking.
        code = await command((sys.executable, repo / "scripts/test-helper-recovery-paths.py"), repo, env,
                             lambda line: sys.stdout.buffer.write(line))
        require_success(code)


async def run_shards(shards, env, output):
    semaphore = asyncio.Semaphore(CONCURRENCY)
    tasks = [asyncio.create_task(execute(shard, semaphore, env, output)) for shard in shards]
    try:
        await asyncio.gather(*tasks)
    finally:
        for task in tasks:
            task.cancel()
        await asyncio.gather(*tasks, return_exceptions=True)
    failed = [shard["id"] for shard in shards if shard["status"] != "pass"]
    if failed:
        raise RuntimeError(f"native socket shards failed: {', '.join(failed)}")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--short-base", type=Path, default=Path("/tmp"))
    parser.add_argument("--scratch-base", type=Path, default=Path(os.environ.get("RUNNER_TEMP", tempfile.gettempdir())))
    args = parser.parse_args()
    try:
        asyncio.run(run(Path(__file__).resolve().parents[1], args.output.resolve(), args.short_base, args.scratch_base))
    except (Exception, KeyboardInterrupt, asyncio.CancelledError) as error:
        print(f"native socket runner: {error or type(error).__name__}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
