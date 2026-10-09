#!/usr/bin/env python3
import argparse
import base64
import hashlib
import os
import socketserver
import struct
import sys

MARKER = b"MESH_PRIVATE_WEBSOCKET"


def receive(connection, length):
    result = b""
    while len(result) < length:
        chunk = connection.recv(length - len(result))
        if not chunk:
            raise RuntimeError("peer closed an incomplete message")
        result += chunk
    return result


def receive_headers(connection):
    result = b""
    while b"\r\n\r\n" not in result:
        chunk = connection.recv(4096)
        if not chunk:
            raise RuntimeError("peer closed an incomplete HTTP header")
        result += chunk
        if len(result) > 64 << 10:
            raise RuntimeError("HTTP header exceeded fixture limit")
    return result.split(b"\r\n\r\n", 1)[0]


def parse_request(contents):
    lines = contents.decode("iso-8859-1").split("\r\n")
    method, path, _ = lines[0].split(" ", 2)
    headers = {}
    for line in lines[1:]:
        name, value = line.split(":", 1)
        headers[name.lower()] = value.strip()
    return method, path, headers


def receive_frame(connection):
    first, second = receive(connection, 2)
    length = second & 0x7F
    if length == 126:
        length = struct.unpack(">H", receive(connection, 2))[0]
    elif length == 127:
        length = struct.unpack(">Q", receive(connection, 8))[0]
    if length > 4096:
        raise RuntimeError("WebSocket frame exceeded fixture limit")
    mask = receive(connection, 4) if second & 0x80 else b""
    payload = receive(connection, length)
    if mask:
        payload = bytes(value ^ mask[index % 4] for index, value in enumerate(payload))
    return first & 0x0F, payload


def send_frame(connection, payload):
    if len(payload) > 125:
        raise RuntimeError("fixture only sends short WebSocket frames")
    connection.sendall(bytes((0x81, len(payload))) + payload)


class FixtureHandler(socketserver.BaseRequestHandler):
    def handle(self):
        method, path, headers = parse_request(receive_headers(self.request))
        if path == "/block":
            with open(self.server.block_file, "w", encoding="ascii") as output:
                output.write("ready\n")
                output.flush()
                os.fsync(output.fileno())
            self.request.settimeout(30)
            try:
                while self.request.recv(4096):
                    pass
            except (ConnectionError, TimeoutError):
                pass
            return
        if headers.get("upgrade", "").lower() == "websocket":
            key = headers.get("sec-websocket-key", "")
            accept = base64.b64encode(hashlib.sha1((key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11").encode()).digest()).decode()
            self.request.sendall(
                b"HTTP/1.1 101 Switching Protocols\r\n"
                b"Connection: Upgrade\r\n"
                b"Upgrade: websocket\r\n"
                + f"Sec-WebSocket-Accept: {accept}\r\n\r\n".encode()
            )
            opcode, payload = receive_frame(self.request)
            if opcode != 1:
                raise RuntimeError(f"unexpected WebSocket opcode {opcode}")
            send_frame(self.request, payload + b"|xfp=" + headers.get("x-forwarded-proto", "").encode())
            return
        body = (
            f"method={method}\npath={path}\nhost={headers.get('host', '')}\n"
            f"xff={headers.get('x-forwarded-for', '')}\nxfp={headers.get('x-forwarded-proto', '')}\n"
        ).encode()
        self.request.sendall(
            b"HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nConnection: close\r\n"
            + f"Content-Length: {len(body)}\r\n\r\n".encode()
            + body
        )


class FixtureServer(socketserver.ThreadingMixIn, socketserver.TCPServer):
    allow_reuse_address = True
    daemon_threads = True


def serve(port_file, block_file):
    with FixtureServer(("127.0.0.1", 0), FixtureHandler) as server:
        server.block_file = block_file
        with open(port_file, "w", encoding="ascii") as output:
            output.write(str(server.server_address[1]))
            output.flush()
            os.fsync(output.fileno())
        server.serve_forever()


def main():
    parser = argparse.ArgumentParser()
    subparsers = parser.add_subparsers(dest="operation", required=True)
    server = subparsers.add_parser("serve")
    server.add_argument("port_file")
    server.add_argument("block_file")
    args = parser.parse_args()
    serve(args.port_file, args.block_file)


if __name__ == "__main__":
    try:
        main()
    except Exception as error:
        print(f"HTTP fixture: {error}", file=sys.stderr)
        raise SystemExit(1)
