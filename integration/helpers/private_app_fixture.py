#!/usr/bin/env python3
import argparse
import base64
import hashlib
import json
import os
from pathlib import Path
import socket
import ssl
import sqlite3
import struct
import subprocess

from mesh_control import round_trip
from public_http_fixture import MARKER, receive_frame, receive_headers


def run(*arguments):
    return subprocess.run(arguments, check=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE).stdout


def identity(root, state):
    blob = base64.b64decode((root / state / 'identity.key.pub').read_text().split()[1])
    return base64.urlsafe_b64encode(blob[-32:]).decode().rstrip('=')


def fixture_ports(pid):
    listeners, ports = [], []
    base = 25000 + pid % 700 * 32
    for offset in range(700 * 32):
        listener = socket.socket()
        port = 25000 + (base - 25000 + offset) % (700 * 32)
        try:
            listener.bind(('127.0.0.1', port))
        except OSError:
            listener.close()
            continue
        listeners.append(listener)
        ports.append(port)
        if len(ports) == 3:
            break
    for listener in listeners:
        listener.close()
    if len(ports) != 3:
        raise RuntimeError('no fixture ports')
    return ports


def configure(root, pid):
    ports = fixture_ports(pid)
    control, proxy, _ = ports
    (root / 'ports').write_text('\n'.join(map(str, ports)) + '\n')
    renewer = root / 'renewer.key'
    run('openssl', 'genpkey', '-algorithm', 'ED25519', '-out', str(renewer))
    os.chmod(renewer, 0o600)
    public = run('openssl', 'pkey', '-in', str(renewer), '-pubout', '-outform', 'DER')
    renewer_id = base64.urlsafe_b64encode(public[-32:]).decode().rstrip('=')
    (root / 'renewer.id').write_text(renewer_id)
    run('openssl', 'req', '-x509', '-newkey', 'ec', '-pkeyopt', 'ec_paramgen_curve:P-256',
        '-sha256', '-nodes', '-days', '30', '-subj', '/CN=*.mesh.test',
        '-addext', 'subjectAltName=DNS:*.mesh.test', '-keyout', str(root / 'tls.key'),
        '-out', str(root / 'tls.pem'))
    nodes = {'e': ('edge.app.test', '127.0.0.1'), 'o': ('origin.app.test', '127.0.0.21')}
    for key, (name, address) in nodes.items():
        peers = {peer: {'DNSName': peer_name + '.', 'TailscaleIPs': [peer_address], 'Online': True, 'UserID': 1}
                 for peer, (peer_name, peer_address) in nodes.items() if peer != key}
        peers['owner-client'] = {'DNSName': 'phone.app.test.', 'TailscaleIPs': ['100.64.0.9'], 'Online': True, 'UserID': 1}
        peers['other-user'] = {'DNSName': 'outsider.app.test.', 'TailscaleIPs': ['100.64.0.10'], 'Online': True, 'UserID': 2}
        status = {'BackendState': 'Running', 'Self': {'DNSName': name + '.', 'TailscaleIPs': [address], 'Online': True, 'UserID': 1}, 'Peer': peers}
        (root / (key + '-status.json')).write_text(json.dumps(status))
    origin = {'identity': identity(root, 'o'), 'displayAlias': 'app origin', 'tailscaleName': nodes['o'][0], 'controlPort': control, 'websocketPath': '/mesh'}
    (root / 'edge.json').write_text(json.dumps({'mode': 'direct-tls', 'tailnetOwnerAccess': True,
        'listenAddress': f'127.0.0.1:{proxy}', 'certificateRenewerId': renewer_id, 'origins': [origin]}))
    (root / 'target.json').write_text(json.dumps({'identity': identity(root, 'e'), 'tailscaleName': nodes['e'][0], 'controlPort': control, 'websocketPath': '/mesh'}))


def install(root):
    target_id, signer_id = identity(root, 'e'), (root / 'renewer.id').read_text()
    certificate, private_key = (root / 'tls.pem').read_bytes(), (root / 'tls.key').read_bytes()
    fields = [b'mesh/certificate-bundle/v3', b'public-edge', b'live', target_id.encode(), signer_id.encode(), b'', certificate, private_key]
    digest = hashlib.sha256()
    for field in fields:
        digest.update(struct.pack('>Q', len(field)))
        digest.update(field)
    (root / 'tls.digest').write_bytes(digest.digest())
    signature = run('openssl', 'pkeyutl', '-sign', '-rawin', '-inkey', str(root / 'renewer.key'), '-in', str(root / 'tls.digest'))
    response = round_trip(str(root / 'e' / 'daemon.sock'), {
        'type': 'certificate.install', 'requestId': 'integration-private-app-tls',
        'certificate': {'profile': 'public-edge', 'environment': 'live', 'targetId': target_id, 'signerId': signer_id,
            'certificatePem': base64.b64encode(certificate).decode(), 'privateKeyPem': base64.b64encode(private_key).decode(),
            'signature': base64.b64encode(signature).decode()}})
    if response.get('type') != 'certificate.installed' or not response.get('certificateFingerprint'):
        raise RuntimeError(f'certificate installation failed: {response!r}')


def websocket_echo(connection, host):
    key = base64.b64encode(os.urandom(16)).decode()
    headers = ['GET /socket HTTP/1.1', f'Host: {host}', f'Origin: https://{host}',
        'Connection: Upgrade', 'Upgrade: websocket', 'Sec-WebSocket-Version: 13', f'Sec-WebSocket-Key: {key}']
    connection.sendall(('\r\n'.join(headers) + '\r\n\r\n').encode())
    response = receive_headers(connection)
    if not response.startswith(b'HTTP/1.1 101 '):
        raise RuntimeError(f'WebSocket upgrade failed: {response!r}')
    mask = os.urandom(4)
    masked = bytes(value ^ mask[index % 4] for index, value in enumerate(MARKER))
    connection.sendall(bytes((0x81, 0x80 | len(MARKER))) + mask + masked)
    opcode, payload = receive_frame(connection)
    if opcode != 1 or payload != MARKER + b'|xfp=https':
        raise RuntimeError(f'unexpected WebSocket response: {payload!r}')
    print(payload.decode())


def websocket(root, port, app_id):
    host = app_id + '.mesh.test'
    with socket.create_connection(('127.0.0.1', port), timeout=5) as raw:
        raw.sendall(f'PROXY TCP4 100.64.0.9 127.0.0.1 12345 {port}\r\n'.encode())
        context = ssl.create_default_context(cafile=str(root / 'tls.pem'))
        with context.wrap_socket(raw, server_hostname=host) as connection:
            websocket_echo(connection, host)


def expire(root, app_id):
    # Set the persisted deadline while the registry is stopped to exercise its real recovery and cleanup paths.
    with sqlite3.connect(root / 'e' / 'mesh.db') as database:
        raw, = database.execute("SELECT data FROM private_app_state WHERE key = 'apps.edge'").fetchone()
        state = json.loads(raw)
        state['apps'][app_id]['expiresAt'] = '2000-01-01T00:00:00Z'
        database.execute("UPDATE private_app_state SET data = ? WHERE key = 'apps.edge'", (json.dumps(state).encode(),))


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('operation', choices=('configure', 'install', 'websocket', 'expire'))
    parser.add_argument('root', type=Path)
    parser.add_argument('arguments', nargs='*')
    args = parser.parse_args()
    if args.operation == 'configure':
        configure(args.root, int(args.arguments[0]))
    elif args.operation == 'install':
        install(args.root)
    elif args.operation == 'expire':
        expire(args.root, args.arguments[0])
    else:
        websocket(args.root, int(args.arguments[0]), args.arguments[1])


if __name__ == '__main__':
    main()
