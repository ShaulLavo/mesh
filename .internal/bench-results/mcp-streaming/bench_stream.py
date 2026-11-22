"""PLAN section 11 streaming measurement for the native MCP server (#318).

Usage (repository root, built binary):

    python3 -B .internal/bench-results/mcp-streaming/bench_stream.py MESH_BINARY ROOT OUT_JSON [RUNS]

Uses the mcp_native fixture: `mesh mcp --identity bench` on the client state,
jobs on a second daemon reached over its authenticated WebSocket. Each run:

1. Tail: a paced producer writes 100 MiB of 'a' to stdout in 1 MiB chunks.
   The reader starts at the job's first cursor and pages mesh_job_output
   (1 MiB pages) until EOF. Records time to first byte, bytes read, gaps and
   end-to-end throughput, and verifies every byte read is 'a' and that the
   byte offsets are contiguous unless a gap is reported.
2. Retained read: after the tail, rereads the retained suffix from the
   earliest cursor as fast as paging allows (read throughput without waiting
   on the producer).
3. Stopped reader: an unpaced 100 MiB job runs to completion with nobody
   reading; a read from the first cursor must report the gap and an earliest
   cursor at 100 MiB - 8 MiB, and reading from there returns exactly 8 MiB.

CPU seconds and peak RSS (VmHWM) are read from /proc for the frontend, the
remote daemon and the job's worker.
"""
import json
import os
import subprocess
import sys
import time
from pathlib import Path

HERE = Path(__file__).resolve().parent
REPO = HERE.parents[2]
sys.path.insert(0, str(REPO / "integration" / "helpers"))

from mcp_native import MCPClient, MCPFixture, run_cli  # noqa: E402

MIB = 1 << 20
TOTAL = 100 * MIB
RETAINED = 8 * MIB
PAGE = MIB
PACED = "i=0; while [ $i -lt 100 ]; do head -c 1048576 /dev/zero | tr '\\0' a; sleep 0.02; i=$((i+1)); done"
UNPACED = "head -c 104857600 /dev/zero | tr '\\0' a"
TICKS = os.sysconf("SC_CLK_TCK")
# This measures throughput, not deadlines; the 5 s default fails on a loaded host.
DEADLINE_MS = 30000


def require(condition, message):
    if not condition:
        raise SystemExit(f"FAIL: {message}")


def proc_usage(pid):
    try:
        stat = Path(f"/proc/{pid}/stat").read_text().rsplit(")", 1)[1].split()
        status = Path(f"/proc/{pid}/status").read_text()
    except FileNotFoundError:
        return None
    hwm = next((int(line.split()[1]) for line in status.splitlines() if line.startswith("VmHWM:")), 0)
    return {"cpuSeconds": (int(stat[11]) + int(stat[12])) / TICKS, "peakRssKiB": hwm}


def worker_pid(session_dir):
    found = subprocess.run(["pgrep", "-f", f"session-worker.*{session_dir}"], capture_output=True, text=True)
    pids = [int(pid) for pid in found.stdout.split()]
    return pids[0] if pids else None


def data(reply):
    return reply["result"]["structuredContent"].get("data") or {}


RETRIES = {"count": 0, "codes": {}}
CALL_LOG = None


def retryable(reply):
    errors = reply["result"].get("structuredContent", {}).get("errors") or []
    return reply["result"]["isError"] and errors and errors[0].get("retryable")


def note_retry(reply):
    code = reply["result"]["structuredContent"]["errors"][0]["code"]
    RETRIES["count"] += 1
    RETRIES["codes"][code] = RETRIES["codes"].get(code, 0) + 1


def output(client, job, cursor):
    args = {"job": job, "stream": "stdout", "max_bytes": PAGE, "timeout_ms": DEADLINE_MS}
    if cursor:
        args["cursor"] = cursor
    while True:
        started = time.monotonic()
        reply = client.call_tool("mesh_job_output", args, timeout=60)
        page = data(reply)
        errors = reply["result"].get("structuredContent", {}).get("errors") or [{}]
        CALL_LOG.write(json.dumps({"t": round(time.monotonic() - started, 3), "start": page.get("start"),
                                   "bytes": len(page.get("data") or ""), "eof": page.get("eof"),
                                   "error": errors[0].get("code"), "message": errors[0].get("message")}) + "\n")
        CALL_LOG.flush()
        if not retryable(reply):
            return reply
        note_retry(reply)


def settled_receipt(client, context, key):
    """Poll the receipt of an uncertain launch until the host settles it (hang cap 60 s)."""
    deadline = time.monotonic() + 60
    while time.monotonic() < deadline:
        receipt = client.call_tool("mesh_job_receipt", {"context": context, "request_key": key,
                                                        "timeout_ms": DEADLINE_MS}, timeout=60)
        found = data(receipt)
        if not receipt["result"]["isError"] and found.get("job"):
            return found["job"]
        require(found.get("state") == "uncertain", f"receipt after uncertain exec: {receipt}")
        note_retry(receipt)
        time.sleep(0.2)
    raise SystemExit(f"FAIL: launch {key} stayed uncertain for 60 s")


def exec_job(client, context, key, command):
    reply = client.call_tool("mesh_exec", {"context": context, "request_key": key, "command": command,
                                           "wait_ms": 0, "output_bytes": 1}, timeout=60)
    created = data(reply)
    errors = reply["result"].get("structuredContent", {}).get("errors") or []
    if errors and errors[0]["code"] == "request.outcome_unknown":
        # What the error tells an agent to do: find the job by its key.
        note_retry(reply)
        return {"job": settled_receipt(client, context, key), "stdout": {"nextCursor": ""}}
    # A retryable error after acceptance still carries the job handle; the
    # first output page is read separately below.
    if retryable(reply) and created.get("job"):
        note_retry(reply)
        status = client.call_tool("mesh_job_status", {"job": created["job"], "timeout_ms": DEADLINE_MS}, timeout=60)
        require(not status["result"]["isError"], f"status after retryable exec error: {status}")
        return {"job": created["job"], "stdout": {"nextCursor": ""}}
    require(not reply["result"]["isError"], f"mesh_exec {key}: {reply}")
    return created


def tail(client, fixture, context, run):
    started = time.monotonic()
    created = exec_job(client, context, f"tail-{run}", PACED)
    job = created["job"]
    # No cursor means the start of the retained stream.
    cursor = ""
    session_dir = fixture.remote / "s" / job.split(".")[2]
    first_byte, read, expected, gaps, pages, worker_peak = None, 0, None, [], 0, None
    first_offset = None
    while True:
        reply = output(client, job, cursor)
        page = data(reply)
        if reply["result"]["isError"]:
            gaps.append(reply["result"]["structuredContent"]["errors"])
            require(bool(page.get("earliestCursor")), f"error without an earliest cursor: {reply}")
            cursor = page["earliestCursor"]
            continue
        if page.get("gap"):
            gaps.append({"reason": page.get("reason"), "start": page.get("start")})
        chunk = page.get("data", "")
        if chunk:
            first_byte = first_byte or time.monotonic() - started
            # A read without a cursor starts at the earliest retained byte.
            first_offset = int(page["start"]) if first_offset is None else first_offset
            expected = int(page["start"]) if expected is None else expected
            require(chunk == "a" * len(chunk), f"unexpected bytes at {page.get('start')}")
            require(int(page["start"]) == expected or gaps, f"page at {page.get('start')} after {expected} with no gap")
            expected = int(page["start"]) + len(chunk)
            read += len(chunk)
            pages += 1
        pid = worker_pid(session_dir)
        if pid:
            worker_peak = proc_usage(pid) or worker_peak
        if page.get("eof"):
            break
        cursor = page.get("nextCursor") or cursor
        if not chunk:
            time.sleep(0.005)
    elapsed = time.monotonic() - started
    return job, {"timeToFirstByteMs": round(first_byte * 1000, 1), "firstOffset": first_offset,
                 "bytesRead": read, "pages": pages,
                 "gaps": gaps, "seconds": round(elapsed, 3), "throughputMiBps": round(read / MIB / elapsed, 1),
                 "worker": worker_peak}


def retained_read(client, job):
    first = output(client, job, "")
    page = data(first)
    cursor = page.get("earliestCursor") if first["result"]["isError"] else page.get("startCursor")
    started, read = time.monotonic(), 0
    while True:
        reply = output(client, job, cursor)
        require(not reply["result"]["isError"], f"retained read: {reply}")
        page = data(reply)
        read += len(page.get("data", ""))
        if page.get("eof") or not page.get("data"):
            break
        cursor = page["nextCursor"]
    elapsed = time.monotonic() - started
    return {"bytesRead": read, "seconds": round(elapsed, 3), "throughputMiBps": round(read / MIB / elapsed, 1)}


def stopped_reader(client, context, run):
    created = exec_job(client, context, f"stopped-{run}", "sleep 1; " + UNPACED)
    job = created["job"]
    # The producer sleeps first, so this page is empty and its cursor is offset 0.
    opening = data(output(client, job, ""))
    require(opening.get("start") in ("0", None) and not opening.get("data"), f"opening page: {opening}")
    first_cursor = opening["nextCursor"]
    done = client.call_tool("mesh_job_wait", {"job": job, "wait_ms": 60000}, timeout=90)
    require(data(done).get("state") == "exited", f"stopped-reader job did not exit: {done}")
    missed = output(client, job, first_cursor)
    missed_data = data(missed)
    errors = missed["result"]["structuredContent"].get("errors") or []
    code = errors[0]["code"] if errors else None
    earliest = missed_data.get("earliestCursor")
    reply = output(client, job, earliest)
    start = int(data(reply).get("start", "-1"))
    total, cursor = 0, earliest
    while True:
        page = data(output(client, job, cursor))
        total += len(page.get("data", ""))
        if page.get("eof") or not page.get("data"):
            break
        cursor = page["nextCursor"]
    require(code == "logs.cursor_expired" or missed_data.get("gap"), f"stopped reader saw no gap: {missed}")
    require(start == TOTAL - RETAINED and total == RETAINED, f"retained suffix start={start} bytes={total}")
    return {"firstCursorResult": code or "gap", "earliestOffset": start, "retainedBytes": total}


def main():
    binary, root, out = str(Path(sys.argv[1]).resolve()), Path(sys.argv[2]), Path(sys.argv[3])
    runs = int(sys.argv[4]) if len(sys.argv) > 4 else 3
    global CALL_LOG
    CALL_LOG = open(root / "calls.jsonl", "w")
    (root / "fleet").mkdir()
    fixture = MCPFixture(binary, root / "fleet")
    result = {"mesh": subprocess.run([binary, "version"], capture_output=True, text=True).stdout.strip(),
              "loadAverageBefore": os.getloadavg(), "runs": []}
    try:
        fixture.start_remote()
        run_cli(fixture.binary, fixture.environment, "mcp", "identity", "create", "bench", "--json")
        client = MCPClient(binary, fixture.environment, args=("--identity", "bench"))
        opened = client.call_tool("mesh_context_open", {"host": "pc", "cwd": str(fixture.root), "shell": "sh",
                                                         "request_key": "bench-ctx", "timeout_ms": DEADLINE_MS},
                                 timeout=60)
        require(not opened["result"]["isError"], f"context open: {opened}")
        context = data(opened)["handle"]
        frontend_before = proc_usage(client.proc.pid)
        daemon_before = proc_usage(fixture.daemon.pid)
        for run in range(runs):
            job, tailed = tail(client, fixture, context, run)
            entry = {"tail": tailed, "retainedRead": retained_read(client, job),
                     "stoppedReader": stopped_reader(client, context, run)}
            result["runs"].append(entry)
            print(json.dumps(entry), flush=True)
        frontend_after = proc_usage(client.proc.pid)
        daemon_after = proc_usage(fixture.daemon.pid)
        result["frontend"] = {"cpuSeconds": round(frontend_after["cpuSeconds"] - frontend_before["cpuSeconds"], 2),
                              "peakRssKiB": frontend_after["peakRssKiB"]}
        result["daemon"] = {"cpuSeconds": round(daemon_after["cpuSeconds"] - daemon_before["cpuSeconds"], 2),
                            "peakRssKiB": daemon_after["peakRssKiB"]}
        client.close_eof()
    finally:
        fixture.close()
    result["loadAverageAfter"] = os.getloadavg()
    result["retryableErrors"] = RETRIES
    out.write_text(json.dumps(result, indent=2) + "\n")
    print(json.dumps(result, indent=2))


if __name__ == "__main__":
    main()
