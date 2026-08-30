#!/usr/bin/env python3
import argparse
import base64
import json
import socket
import struct
import sys
import uuid


def receive(connection, length):
    result = b""
    while len(result) < length:
        chunk = connection.recv(length - len(result))
        if not chunk:
            raise RuntimeError("daemon closed an incomplete response")
        result += chunk
    return result


def round_trip(socket_path, request):
    payload = json.dumps(request, separators=(",", ":")).encode()
    with socket.socket(socket.AF_UNIX) as connection:
        connection.settimeout(10)
        connection.connect(socket_path)
        connection.sendall(b"\x01" + struct.pack(">I", len(payload)) + payload)
        header = receive(connection, 5)
        if header[0] != 1:
            raise RuntimeError(f"unexpected response kind {header[0]}")
        length = struct.unpack(">I", header[1:])[0]
        if length > 4 << 20:
            raise RuntimeError(f"oversized response: {length}")
        return json.loads(receive(connection, length))


def request_id(operation):
    return f"integration-{operation}-{uuid.uuid4().hex}"


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--expect-type")
    parser.add_argument("--expect-code")
    subparsers = parser.add_subparsers(dest="operation", required=True)

    upsert = subparsers.add_parser("upsert")
    upsert.add_argument("socket")
    upsert.add_argument("name")
    upsert.add_argument("kind", choices=("static", "files", "proxy"))
    upsert.add_argument("target")
    upsert.add_argument("public_name")
    upsert.add_argument("--wake", action="store_true")

    delete = subparsers.add_parser("delete")
    delete.add_argument("socket")
    delete.add_argument("name")

    install = subparsers.add_parser("install")
    install.add_argument("socket")
    install.add_argument("profile")
    install.add_argument("environment")
    install.add_argument("target_id")
    install.add_argument("signer_id")
    install.add_argument("certificate")
    install.add_argument("private_key")
    install.add_argument("signature")

    args = parser.parse_args()
    if args.operation == "upsert":
        request = {
            "type": "service.upsert",
            "requestId": request_id("upsert"),
            "service": {
                "name": args.name,
                "kind": args.kind,
                "target": args.target,
                "publicName": args.public_name,
                "wakeOnRequest": args.wake,
            },
        }
        socket_path = args.socket
    elif args.operation == "delete":
        request = {
            "type": "service.delete",
            "requestId": request_id("delete"),
            "serviceName": args.name,
        }
        socket_path = args.socket
    else:
        request = {
            "type": "certificate.install",
            "requestId": request_id("install"),
            "certificate": {
                "profile": args.profile,
                "environment": args.environment,
                "targetId": args.target_id,
                "signerId": args.signer_id,
                "certificatePem": base64.b64encode(open(args.certificate, "rb").read()).decode(),
                "privateKeyPem": base64.b64encode(open(args.private_key, "rb").read()).decode(),
                "signature": base64.b64encode(open(args.signature, "rb").read()).decode(),
            },
        }
        socket_path = args.socket

    response = round_trip(socket_path, request)
    if args.expect_type and response.get("type") != args.expect_type:
        raise RuntimeError(f"response type {response.get('type')!r}, want {args.expect_type!r}: {response!r}")
    if args.expect_code and response.get("errorCode") != args.expect_code:
        raise RuntimeError(f"error code {response.get('errorCode')!r}, want {args.expect_code!r}: {response!r}")
    print(json.dumps(response, sort_keys=True, separators=(",", ":")))


if __name__ == "__main__":
    try:
        main()
    except Exception as error:
        print(f"mesh control fixture: {error}", file=sys.stderr)
        raise SystemExit(1)
