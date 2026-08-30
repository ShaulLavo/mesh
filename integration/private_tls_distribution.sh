#!/usr/bin/env bash
# Signed private-name bundles bind the name to the certificate transcript.
# Staging stays non-serving, and host.info withholds the name without ingress.
set -uo pipefail

if [ -z "${MESH:-}" ]; then
  MESH=$PWD/mesh
  go build -o "$MESH" ./cmd/mesh || { echo "FAIL: build" >&2; exit 1; }
fi
for tool in openssl python3 curl; do
  command -v "$tool" >/dev/null 2>&1 || { echo "FAIL: $tool is required" >&2; exit 1; }
done

TEST_ROOT=$(mktemp -d)
export MESH_STATE_DIR="$TEST_ROOT/state"
PRIVATE_NAME=pc.mesh.shaulavo.dev
DAEMON_PID=""

cleanup() {
  if [ -n "$DAEMON_PID" ]; then
    kill "$DAEMON_PID" 2>/dev/null || true
    wait "$DAEMON_PID" 2>/dev/null || true
  fi
  rm -rf -- "$TEST_ROOT"
}
trap cleanup EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }

identity_id() {
  openssl pkey -in "$1" -pubout -outform DER 2>/dev/null | python3 -c '
import base64, sys
public_der = sys.stdin.buffer.read()
if len(public_der) < 32:
    raise SystemExit("short Ed25519 public key")
print(base64.urlsafe_b64encode(public_der[-32:]).decode().rstrip("="))
'
}

wait_for_socket() {
  for _ in $(seq 100); do
    kill -0 "$DAEMON_PID" 2>/dev/null || return 1
    [ -S "$MESH_STATE_DIR/daemon.sock" ] && return 0
    sleep 0.05
  done
  return 1
}

create_certificate() {
  local serial=$1
  local certificate=$2
  local private_key=$3
  openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -sha256 -nodes -days 30 \
    -set_serial "$serial" -subj '/CN=*.mesh.shaulavo.dev' \
    -addext 'subjectAltName=DNS:*.mesh.shaulavo.dev' \
    -keyout "$private_key" -out "$certificate" >/dev/null 2>&1 || fail "generate certificate $serial"
}

install_bundle() {
  local environment=$1
  local certificate=$2
  local private_key=$3
  local profile=private-origin
  local digest="$TEST_ROOT/$environment.digest"
  local signature="$TEST_ROOT/$environment.signature"
  python3 - "$profile" "$environment" "$ORIGIN_ID" "$RENEWER_ID" "$PRIVATE_NAME" \
    "$certificate" "$private_key" "$digest" <<'PY'
import hashlib
import struct
import sys

(
    profile, environment, target_id, signer_id, private_name,
    certificate_path, key_path, output_path,
) = sys.argv[1:]
fields = [
    b"mesh/certificate-bundle/v3",
    profile.encode(),
    environment.encode(),
    target_id.encode(),
    signer_id.encode(),
    private_name.encode(),
    open(certificate_path, "rb").read(),
    open(key_path, "rb").read(),
]
digest = hashlib.sha256()
for field in fields:
    digest.update(struct.pack(">Q", len(field)))
    digest.update(field)
open(output_path, "wb").write(digest.digest())
PY
  openssl pkeyutl -sign -rawin -inkey "$RENEWER_KEY" -in "$digest" -out "$signature" >/dev/null 2>&1 || fail "sign $environment bundle"
  python3 - "$MESH_STATE_DIR/daemon.sock" "$profile" "$environment" "$ORIGIN_ID" "$RENEWER_ID" \
    "$PRIVATE_NAME" "$certificate" "$private_key" "$signature" <<'PY'
import base64
import json
import socket
import struct
import sys

(
    socket_path, profile, environment, target_id, signer_id, private_name,
    certificate_path, key_path, signature_path,
) = sys.argv[1:]
request = json.dumps({
    "type": "certificate.install",
    "requestId": f"integration-{environment}",
    "certificate": {
        "profile": profile,
        "environment": environment,
        "targetId": target_id,
        "signerId": signer_id,
        "privateName": private_name,
        "certificatePem": base64.b64encode(open(certificate_path, "rb").read()).decode(),
        "privateKeyPem": base64.b64encode(open(key_path, "rb").read()).decode(),
        "signature": base64.b64encode(open(signature_path, "rb").read()).decode(),
    },
}).encode()

def receive(connection, length):
    result = b""
    while len(result) < length:
        chunk = connection.recv(length - len(result))
        if not chunk:
            raise SystemExit("daemon closed an incomplete response")
        result += chunk
    return result

with socket.socket(socket.AF_UNIX) as connection:
    connection.connect(socket_path)
    connection.sendall(b"\x01" + struct.pack(">I", len(request)) + request)
    header = receive(connection, 5)
    if header[0] != 1:
        raise SystemExit(f"unexpected response kind {header[0]}")
    response_length = struct.unpack(">I", header[1:])[0]
    if response_length > 4 << 20:
        raise SystemExit(f"oversized response: {response_length}")
    response = json.loads(receive(connection, response_length))
if (
    response.get("type") != "certificate.installed"
    or response.get("certificateProfile") != profile
    or response.get("certificateEnvironment") != environment
    or response.get("certificatePrivateName") != private_name
    or not response.get("certificateFingerprint")
):
    raise SystemExit(f"certificate install failed: {response!r}")
PY
}

host_private_name() {
  python3 - "$MESH_STATE_DIR/daemon.sock" <<'PY'
import json
import socket
import struct
import sys

request = json.dumps({
    "type": "host.info",
    "requestId": "integration-host-info",
}, separators=(",", ":")).encode()


def receive(connection, length):
    result = b""
    while len(result) < length:
        chunk = connection.recv(length - len(result))
        if not chunk:
            raise SystemExit("daemon closed an incomplete host.info response")
        result += chunk
    return result


with socket.socket(socket.AF_UNIX) as connection:
    connection.settimeout(2)
    connection.connect(sys.argv[1])
    connection.sendall(b"\x01" + struct.pack(">I", len(request)) + request)
    header = receive(connection, 5)
    if header[0] != 1:
        raise SystemExit(f"unexpected host.info response kind {header[0]}")
    response_length = struct.unpack(">I", header[1:])[0]
    if response_length > 4 << 20:
        raise SystemExit(f"oversized host.info response: {response_length}")
    response = json.loads(receive(connection, response_length))
if response.get("type") != "host.info.result" or not isinstance(response.get("host"), dict):
    raise SystemExit(f"host.info failed: {response!r}")
print(response["host"].get("privateName", ""))
PY
}

upsert_service() {
  python3 - "$MESH_STATE_DIR/daemon.sock" "$SITE_ROOT" <<'PY'
import json
import socket
import struct
import sys

request = json.dumps({
    "type": "service.upsert",
    "requestId": "integration-service",
    "service": {"name": "site", "kind": "static", "target": sys.argv[2]},
}).encode()

def receive(connection, length):
    result = b""
    while len(result) < length:
        chunk = connection.recv(length - len(result))
        if not chunk:
            raise SystemExit("daemon closed an incomplete response")
        result += chunk
    return result

with socket.socket(socket.AF_UNIX) as connection:
    connection.connect(sys.argv[1])
    connection.sendall(b"\x01" + struct.pack(">I", len(request)) + request)
    header = receive(connection, 5)
    length = struct.unpack(">I", header[1:])[0]
    payload = receive(connection, length)
response = json.loads(payload)
if response.get("type") != "service.upserted" or not response.get("service", {}).get("healthy"):
    raise SystemExit(f"service upsert failed: {response!r}")
PY
}

served_fingerprint() {
  openssl s_client -connect "127.0.0.1:$HTTPS_PORT" -servername "$PRIVATE_NAME" -showcerts </dev/null 2>/dev/null |
    openssl x509 -noout -fingerprint -sha256 2>/dev/null
}

mkdir -p "$MESH_STATE_DIR"
openssl genpkey -algorithm ED25519 -out "$MESH_STATE_DIR/identity.key" >/dev/null 2>&1 || fail "generate origin identity"
chmod 0600 "$MESH_STATE_DIR/identity.key"
RENEWER_KEY="$TEST_ROOT/renewer.key"
openssl genpkey -algorithm ED25519 -out "$RENEWER_KEY" >/dev/null 2>&1 || fail "generate renewer identity"
chmod 0600 "$RENEWER_KEY"
ORIGIN_ID=$(identity_id "$MESH_STATE_DIR/identity.key") || fail "derive origin identity"
RENEWER_ID=$(identity_id "$RENEWER_KEY") || fail "derive renewer identity"
HTTPS_PORT=$(python3 - <<'PY'
import socket
with socket.socket() as listener:
    listener.bind(("127.0.0.1", 0))
    print(listener.getsockname()[1])
PY
)
SITE_ROOT="$TEST_ROOT/site"
mkdir -p "$SITE_ROOT"
printf PRIVATE_TLS_MARKER >"$SITE_ROOT/index.html"

"$MESH" daemon --https-port "$HTTPS_PORT" --certificate-renewer-id "$RENEWER_ID" >"$TEST_ROOT/daemon.log" 2>&1 &
DAEMON_PID=$!
wait_for_socket || fail "daemon did not start: $(<"$TEST_ROOT/daemon.log")"
upsert_service || fail "service upsert"

create_certificate 101 "$TEST_ROOT/staging.crt" "$TEST_ROOT/staging.key"
install_bundle staging "$TEST_ROOT/staging.crt" "$TEST_ROOT/staging.key" || fail "staging distribution"
[ -f "$MESH_STATE_DIR/private-tls/staging/current" ] || fail "staging bundle was not persisted"
[ ! -e "$MESH_STATE_DIR/private-tls/live/current" ] || fail "staging bundle entered the live slot"
STAGING_PRIVATE_NAME=$(host_private_name) || fail "query host.info after staging distribution"
[ -z "$STAGING_PRIVATE_NAME" ] || fail "staging bundle exposed an unverified private URL"
if served_fingerprint | grep -q .; then
  fail "staging bundle became the serving certificate"
fi

create_certificate 202 "$TEST_ROOT/live-one.crt" "$TEST_ROOT/live-one.key"
install_bundle live "$TEST_ROOT/live-one.crt" "$TEST_ROOT/live-one.key" || fail "first live distribution"
EXPECTED_ONE=$(openssl x509 -in "$TEST_ROOT/live-one.crt" -noout -fingerprint -sha256)
[ "$(served_fingerprint)" = "$EXPECTED_ONE" ] || fail "first live certificate was not served"
LIVE_PRIVATE_NAME=$(host_private_name) || fail "query host.info after live distribution"
[ -z "$LIVE_PRIVATE_NAME" ] || fail "private name was exposed without Tailscale Serve ingress"
BODY=$(curl --noproxy '*' --fail --silent --max-time 2 --cacert "$TEST_ROOT/live-one.crt" \
  --resolve "$PRIVATE_NAME:$HTTPS_PORT:127.0.0.1" "https://$PRIVATE_NAME:$HTTPS_PORT/site/") || fail "HTTPS service request"
[ "$BODY" = PRIVATE_TLS_MARKER ] || fail "HTTPS service body was $BODY"
TERMINAL_STATUS=$(curl --noproxy '*' --silent --max-time 2 --cacert "$TEST_ROOT/live-one.crt" \
  --resolve "$PRIVATE_NAME:$HTTPS_PORT:127.0.0.1" \
  --header 'Connection: Upgrade' --header 'Upgrade: websocket' \
  --output /dev/null --write-out '%{http_code}' "https://$PRIVATE_NAME:$HTTPS_PORT/mesh") || fail "HTTPS terminal isolation request"
[ "$TERMINAL_STATUS" = 404 ] || fail "HTTPS /mesh returned $TERMINAL_STATUS instead of service-only 404"

create_certificate 303 "$TEST_ROOT/live-two.crt" "$TEST_ROOT/live-two.key"
install_bundle live "$TEST_ROOT/live-two.crt" "$TEST_ROOT/live-two.key" || fail "second live distribution"
EXPECTED_TWO=$(openssl x509 -in "$TEST_ROOT/live-two.crt" -noout -fingerprint -sha256)
[ "$EXPECTED_ONE" != "$EXPECTED_TWO" ] || fail "test certificates have the same fingerprint"
[ "$(served_fingerprint)" = "$EXPECTED_TWO" ] || fail "live certificate did not hot-rotate"
kill -0 "$DAEMON_PID" 2>/dev/null || fail "daemon restarted or exited during certificate rotation"

echo "PASS: v3 bound the private name, withheld it without ingress, and hot-rotated live TLS"
