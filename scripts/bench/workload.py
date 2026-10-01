#!/usr/bin/env python3
"""Deterministic terminal logs, then a quiet input wait; no external services."""
import os
import sys
import tty


def block(size):
    line = b"\x1b[32mINFO\x1b[0m build: compiled workspace package; tests passed; duration=12ms\r\n"
    return (line * (size // len(line) + 1))[:size]


def write_all(data):
    while data:
        data = data[os.write(1, data):]


def main():
    tty.setraw(sys.stdin.fileno())
    write_all(b"\x1b]2;mesh benchmark build log\x07")
    write_all(block(32 << 10))
    write_all(b"\r\nBENCH_READY\r\n")
    for request in sys.stdin.buffer:
        if request.startswith(b"burst "):
            remaining = int(request.split()[1])
            while remaining:
                data = block(min(32 << 10, remaining))
                write_all(data)
                remaining -= len(data)
            write_all(b"\r\nBENCH_DONE\r\n")


if __name__ == "__main__":
    main()
