"""Run an MCP stdio server and log every line in each direction.

Usage: mcp_tee.py LOG -- COMMAND [ARGS...]

The client talks to this process exactly as it would to the server. Each line
is appended to LOG as JSON: {"t": seconds, "dir": "c2s"|"s2c"|"err", "line": ...}.
"""
import json
import os
import subprocess
import sys
import threading
import time


def main():
    log_path = sys.argv[1]
    sep = sys.argv.index("--")
    command = sys.argv[sep + 1:]
    start = time.monotonic()
    lock = threading.Lock()
    log = open(log_path, "a", encoding="utf-8")

    def record(direction, raw):
        text = raw.decode("utf-8", errors="replace").rstrip("\n")
        with lock:
            log.write(json.dumps({"t": round(time.monotonic() - start, 4), "dir": direction,
                                  "pid": os.getpid(), "line": text}) + "\n")
            log.flush()

    child = subprocess.Popen(command, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE)

    def pump_in():
        for raw in sys.stdin.buffer:
            record("c2s", raw)
            try:
                child.stdin.write(raw)
                child.stdin.flush()
            except BrokenPipeError:
                break
        record("c2s", b"<stdin EOF>")
        try:
            child.stdin.close()
        except BrokenPipeError:
            pass

    def pump_out():
        for raw in child.stdout:
            record("s2c", raw)
            sys.stdout.buffer.write(raw)
            sys.stdout.buffer.flush()

    def pump_err():
        for raw in child.stderr:
            record("err", raw)

    threads = [threading.Thread(target=f, daemon=True) for f in (pump_in, pump_out, pump_err)]
    for thread in threads:
        thread.start()
    code = child.wait()
    threads[1].join(5)
    threads[2].join(5)
    record("err", f"<server exited {code}>".encode())
    sys.exit(code)


if __name__ == "__main__":
    main()
