#!/usr/bin/env python3
"""Hold one connection to an on-demand route open until killed.

`http PORT READY` makes one keep-alive request and keeps the connection.
`websocket PORT PATH READY` upgrades and keeps the socket. Either writes the
first response line to READY once the route has answered.
"""
import base64
import os
import socket
import sys
import time


def main():
    mode, host, port = sys.argv[1], sys.argv[2], int(sys.argv[3])
    connection = socket.create_connection((host, port), timeout=15)
    if mode == "http":
        ready = sys.argv[4]
        connection.sendall(b"GET / HTTP/1.1\r\nHost: localhost\r\n\r\n")
    else:
        path, ready = sys.argv[4], sys.argv[5]
        key = base64.b64encode(os.urandom(16))
        connection.sendall(
            b"GET " + path.encode() + b" HTTP/1.1\r\nHost: localhost\r\nUpgrade: websocket\r\n"
            + b"Connection: Upgrade\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: " + key + b"\r\n\r\n"
        )
    response = b""
    while b"\r\n" not in response:
        chunk = connection.recv(4096)
        if not chunk:
            raise SystemExit("connection closed before a response")
        response += chunk
    with open(ready + ".tmp", "w", encoding="utf-8") as output:
        output.write(response.split(b"\r\n", 1)[0].decode("latin-1"))
    os.rename(ready + ".tmp", ready)
    while True:
        time.sleep(60)


if __name__ == "__main__":
    main()
