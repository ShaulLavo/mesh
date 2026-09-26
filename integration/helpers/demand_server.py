#!/usr/bin/env python3
"""A stand-in dev server for on-demand routes.

It waits, then binds its ports one at a time, so a route that proxied before
every port accepted would be caught. Each response names the port, the path,
FAKE_MARK from the environment and the working directory, which is how the
test checks the launch recipe. A WebSocket upgrade is accepted and held open
until the client closes it.
"""
import base64
import hashlib
import os
import socket
import sys
import threading
import time

WEBSOCKET_GUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"


def respond(connection, port):
    buffered = b""
    while True:
        while b"\r\n\r\n" not in buffered:
            chunk = connection.recv(4096)
            if not chunk:
                return
            buffered += chunk
        head, buffered = buffered.split(b"\r\n\r\n", 1)
        lines = head.decode("latin-1").split("\r\n")
        path = lines[0].split()[1]
        headers = {}
        for line in lines[1:]:
            name, _, value = line.partition(":")
            headers[name.strip().lower()] = value.strip()
        if headers.get("upgrade", "").lower() == "websocket":
            digest = hashlib.sha1((headers.get("sec-websocket-key", "") + WEBSOCKET_GUID).encode()).digest()
            connection.sendall(
                b"HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"
                + b"Sec-WebSocket-Accept: " + base64.b64encode(digest) + b"\r\n\r\n"
            )
            while connection.recv(4096):
                pass
            return
        body = "FAKE_OK port={} path={} mark={} cwd={}\n".format(
            port, path, os.environ.get("FAKE_MARK", ""), os.getcwd()
        ).encode()
        connection.sendall(
            b"HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: "
            + str(len(body)).encode() + b"\r\n\r\n" + body
        )


def serve(connection, port):
    try:
        respond(connection, port)
    except OSError:
        pass
    finally:
        connection.close()


def accept(listener, port):
    while True:
        connection, _ = listener.accept()
        threading.Thread(target=serve, args=(connection, port), daemon=True).start()


def main():
    *ports, delay = sys.argv[1:]
    with open("server.pid", "w", encoding="utf-8") as output:
        output.write(str(os.getpid()))
    print("demand server starting", flush=True)
    time.sleep(float(delay))
    for port in ports:
        listener = socket.socket()
        listener.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        listener.bind(("127.0.0.1", int(port)))
        listener.listen(64)
        threading.Thread(target=accept, args=(listener, int(port)), daemon=True).start()
        time.sleep(float(delay) / 2)
    print("demand server ready", flush=True)
    while True:
        time.sleep(60)


if __name__ == "__main__":
    main()
