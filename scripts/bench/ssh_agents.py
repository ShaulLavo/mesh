"""Separate-process short commands, including an owned OpenSSH control master."""

from concurrent.futures import ThreadPoolExecutor
import hashlib
import os
import resource
import selectors
import shlex
import subprocess
import time

import ssh_command as bench


def timed(argv, environment, expected=b"", expected_error=b"", code=0, payload=b"", allow_denial=False):
    started = time.perf_counter()
    process = subprocess.Popen(argv, env=environment, stdin=subprocess.PIPE,
                               stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    first = None
    output = {"stdout": bytearray(), "stderr": bytearray()}
    try:
        process.stdin.write(payload)
        process.stdin.close()
        first = read_streams(process, output, started)
        process.wait(timeout=bench.TIMEOUT)
        denied = allow_denial and process.returncode == 1 and bytes(output["stdout"]) == b"" and b"rate limit exceeded" in output["stderr"]
        if not denied and (process.returncode, bytes(output["stdout"]), bytes(output["stderr"])) != (code, expected, expected_error):
            raise RuntimeError(f"output/status mismatch: {process.returncode}, {output!r}")
        return {"seconds": time.perf_counter() - started, "first_byte_seconds": first,
                "exit": process.returncode, "stdout_bytes": len(expected),
                "admission_denied": denied,
                "stdout_sha256": hashlib.sha256(expected).hexdigest(),
                "stderr_sha256": hashlib.sha256(expected_error).hexdigest()}
    finally:
        if process.poll() is None:
            process.kill()
        process.wait(timeout=bench.TIMEOUT)
        process.stdout.close()
        process.stderr.close()


def read_streams(process, output, started):
    first = None
    with selectors.DefaultSelector() as reader:
        reader.register(process.stdout, selectors.EVENT_READ, "stdout")
        reader.register(process.stderr, selectors.EVENT_READ, "stderr")
        while reader.get_map():
            first = drain(reader, output, first, started)
    return first


def drain(reader, output, first, started):
    events = reader.select(bench.TIMEOUT)
    if not events:
        raise TimeoutError("command produced no output or EOF before hang cap")
    for key, _ in events:
        data = os.read(key.fd, 65536)
        if not data:
            reader.unregister(key.fileobj)
            continue
        if first is None:
            first = time.perf_counter() - started
        output[key.data].extend(data)
    return first


def multiplexed(ssh, path):
    replacements = {"ControlMaster=no": "ControlMaster=auto", "ControlPath=none": f"ControlPath={path}",
                    "ControlPersist=no": "ControlPersist=60"}
    return [replacements.get(value, value) for value in ssh]


def close_master(ssh, environment):
    bench.checked_run(ssh[:-1] + ["-O", "exit", ssh[-1]], environment)


def resources(pid):
    root = bench.Path(f"/proc/{pid}")
    try:
        return {"pid": pid, "fd_count": len(list((root / "fd").iterdir())),
                "status": (root / "status").read_text(), "stat": (root / "stat").read_text()}
    except FileNotFoundError:
        return {"pid": pid, "exited": True}


def usage(fixture):
    children = resource.getrusage(resource.RUSAGE_CHILDREN)
    direct = bench.Path(f"/proc/{os.getpid()}/task/{os.getpid()}/children").read_text().split()
    processes = [resources(int(pid)) for pid in direct]
    for process in processes:
        process["role"] = process_role(process["pid"], fixture.daemon.pid)
    return {"reaped_child_cpu_seconds": children.ru_utime + children.ru_stime,
            "daemon": resources(fixture.daemon.pid), "children": processes}


def process_role(pid, daemon):
    if pid == daemon:
        return "daemon"
    try:
        argv = bench.Path(f"/proc/{pid}/cmdline").read_bytes().split(b"\0")
        return "helper" if len(argv) > 1 and argv[1] == b"ssh-helper" else "other"
    except FileNotFoundError:
        return "exited"


def cpu(record, child=False):
    if "stat" not in record:
        return 0
    fields = record["stat"].split(") ", 1)[1].split()
    indexes = (13, 14) if child else (11, 12)
    return sum(int(fields[index]) for index in indexes) / os.sysconf("SC_CLK_TCK")


def cpu_accounting(before, after):
    helper_before = {process["pid"]: cpu(process) for process in before["children"] if process["role"] == "helper"}
    helper = sum(cpu(process) - helper_before.get(process["pid"], 0)
                 for process in after["children"] if process["role"] == "helper")
    result = {"cli_seconds": after["reaped_child_cpu_seconds"] - before["reaped_child_cpu_seconds"],
              "helper_seconds": helper,
              "daemon_seconds": cpu(after["daemon"]) - cpu(before["daemon"]),
              "remote_process_seconds": cpu(after["daemon"], True) - cpu(before["daemon"], True)}
    result["total_seconds"] = sum(result.values())
    result["policy"] = "reaped CLI rusage plus live helper CPU plus Mesh daemon and its reaped children; proc ticks are coarse"
    return result


def transport_inodes(fixture):
    rows = bench.Path(f"/proc/{fixture.daemon.pid}/net/tcp").read_text().splitlines()[1:]
    return [row.split()[9] for row in rows if row.split()[1].endswith(f":{fixture.port:04X}") and row.split()[3] == "01"]


def summarize(samples):
    first = [sample["first_byte_seconds"] for sample in samples if sample["first_byte_seconds"] is not None]
    return {"count": len(samples), "wall_seconds": bench.distribution([sample["seconds"] for sample in samples]),
            "first_byte_seconds": bench.distribution(first) if first else None}


def collect(command, environment, count, concurrency):
    samples = []
    for offset in range(0, count, concurrency):
        # Existing listener admission is 10 channels/s with a burst of 20.
        # Sleep outside each measured invocation, never retry a command.
        time.sleep(0.11 * concurrency)
        with ThreadPoolExecutor(max_workers=concurrency) as executor:
            samples.extend(executor.map(lambda _: timed(command, environment), range(min(concurrency, count - offset))))
    return samples


def unpaced(command, environment, count):
    started = time.perf_counter()
    with ThreadPoolExecutor(max_workers=4) as executor:
        samples = list(executor.map(lambda _: timed(command, environment, allow_denial=True), range(count)))
    wall = time.perf_counter() - started
    denied = sum(sample["admission_denied"] for sample in samples)
    return {"samples": samples, "wall_seconds": wall, "attempted": count, "denied": denied,
            "error_fraction": denied / count, "successful_commands_per_second": (count - denied) / wall,
            "policy": "four actual CLI processes at once; no pacing, retries or command replay"}


def workload(prefix, command):
    if prefix[1] == "ssh":
        return prefix + command
    return prefix + [shlex.join(command)]


def exercise(fixture, ssh, args, report, stack):
    host = bench.round_trip(str(fixture.local / "daemon.sock"), {"type": "host.info", "requestId": "bench-agent"})["host"]
    mesh = [fixture.binary, "ssh", "--ssh-port", str(fixture.port), host["id"], "--"]
    mux = multiplexed(ssh, fixture.root / "cm")
    stack.callback(close_master, mux, fixture.environment)
    prefixes = {"mesh": mesh, "openssh_fresh": ssh, "openssh_mux": mux}
    if args.baseline_binary:
        prefixes["mesh_before"] = [str(args.baseline_binary)] + mesh[1:]
    report["agent"] = {"samples": {}, "summary": {}, "resources_before": resources(fixture.daemon.pid),
                       "command": ["/bin/true"], "concurrency": 4}
    for name, prefix in prefixes.items():
        measure_mode(name, prefix, fixture, args, report["agent"])
    if args.baseline_binary:
        paired(prefixes, fixture, args, report["agent"], "mesh_before", "paired_before_after")
    paired(prefixes, fixture, args, report["agent"], "openssh_mux", "promise_vs_mux")
    paired(prefixes, fixture, args, report["agent"], "openssh_fresh", "promise_vs_fresh")
    report["agent"]["resources_after"] = resources(fixture.daemon.pid)
    report["agent"]["correctness"] = "all exits and byte-exact stdout/stderr checked"
    report["status"] = "slowdown" if any(report["agent"][name]["summary"]["clear_slowdown"]
                                         for name in ("promise_vs_mux", "promise_vs_fresh")) else "pass"


def measure_mode(name, prefix, fixture, args, report):
    started = time.perf_counter()
    before = usage(fixture)
    command = workload(prefix, ["/bin/true"])
    cold = [timed(command, fixture.environment)]
    warm, inodes = [], []
    for _ in range(max(100, args.pairs)):
        time.sleep(0.11)
        warm.append(timed(command, fixture.environment))
        inodes.append(transport_inodes(fixture))
    concurrent = collect(command, fixture.environment, args.pairs, 4)
    output = []
    for _ in range(args.pairs):
        time.sleep(0.11)
        output.append(timed(workload(prefix, ["/bin/echo", "first-byte"]), fixture.environment, b"first-byte\n"))
    for phase, samples in {"cold": cold, "warm": warm, "concurrent": concurrent, "output": output}.items():
        key = name + "_" + phase
        report["samples"][key] = samples
        report["summary"][key] = summarize(samples)
    timed(workload(prefix, ["/bin/sh", "-c", "printf out; printf err >&2; exit 7"]),
          fixture.environment, b"out", b"err", 7)
    after = usage(fixture)
    report.setdefault("cases", {})[name] = {
        "argv": command, "connection_policy": "one process per call; " + ("shared authenticated transport" if name in ("mesh", "openssh_mux") else "fresh TCP/SSH connection"),
        "latency_pacing_policy": "sleep110ms per sequential sample,440ms per four-way batch, outside sample timing; no retry",
        "paced_total_wall_seconds": time.perf_counter() - started,
        "resources_before": before, "resources_after_paced": after,
        "mesh_cpu_accounting": cpu_accounting(before, after) if name.startswith("mesh") else None,
        "live_transport_inodes_after_each_warm_call": inodes,
        "unpaced": unpaced(command, fixture.environment, 40)}


def paired(prefixes, fixture, args, report, other, label):
    commands = {"mesh": workload(prefixes["mesh"], ["/bin/true"]),
                "ssh": workload(prefixes[other], ["/bin/true"])}
    samples = []
    started = time.perf_counter()
    for order in bench.orders(40, args.seed):
        pair = {"order": order}
        for name in order:
            time.sleep(0.11)
            pair[name] = timed(commands[name], fixture.environment)
        samples.append(pair)
    report[label] = {"samples": samples, "summary": bench.summarize(samples), "argv": commands,
                                      "total_wall_seconds": time.perf_counter() - started,
                                      "policy": "balanced random order; identical remote listener; 110ms pacing per call"}
