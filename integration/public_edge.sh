#!/usr/bin/env bash
# Real tagged daemons exercise public registration, proxy and direct-TLS
# listeners, streaming WebSockets, durable offline claims, collision refusal,
# deletion acknowledgement, and v3 certificates with no private name.
set -uo pipefail
export PYTHONDONTWRITEBYTECODE=1

REPO_ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
CONTROL_FIXTURE="$REPO_ROOT/integration/helpers/mesh_control.py"
HTTP_FIXTURE="$REPO_ROOT/integration/helpers/public_http_fixture.py"
TEST_ROOT=$(mktemp -d)
MESH_INTEGRATION="$TEST_ROOT/mesh-integration"
EDGE_STATE="$TEST_ROOT/edge-state"
ORIGIN_ONE_STATE="$TEST_ROOT/origin-one-state"
ORIGIN_TWO_STATE="$TEST_ROOT/origin-two-state"
EDGE_PID=""
ORIGIN_ONE_PID=""
ORIGIN_TWO_PID=""
BACKEND_PID=""

cleanup() {
  for pid in "$ORIGIN_TWO_PID" "$ORIGIN_ONE_PID" "$EDGE_PID" "$BACKEND_PID"; do
    [ -z "$pid" ] || kill "$pid" 2>/dev/null || true
  done
  for pid in "$ORIGIN_TWO_PID" "$ORIGIN_ONE_PID" "$EDGE_PID" "$BACKEND_PID"; do
    [ -z "$pid" ] || wait "$pid" 2>/dev/null || true
  done
  rm -rf -- "$TEST_ROOT"
}
trap cleanup EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }

process_alive() {
  local pid=$1
  local state
  kill -0 "$pid" 2>/dev/null || return 1
  [ -r "/proc/$pid/stat" ] || return 1
  state=$(awk '{print $3}' "/proc/$pid/stat" 2>/dev/null) || return 1
  [ "$state" != Z ]
}

pkcs8_identity_id() {
  openssl pkey -in "$1" -pubout -outform DER 2>/dev/null | python3 -c '
import base64, sys
public_der = sys.stdin.buffer.read()
if len(public_der) < 32:
    raise SystemExit("short Ed25519 public key")
print(base64.urlsafe_b64encode(public_der[-32:]).decode().rstrip("="))
'
}

identity_id() {
  ssh-keygen -y -f "$1" 2>/dev/null | python3 -c '
import base64, sys
parts = sys.stdin.read().split()
if len(parts) < 2:
    raise SystemExit("no Ed25519 public key")
blob = base64.b64decode(parts[1])
if len(blob) < 32:
    raise SystemExit("short Ed25519 public key")
print(base64.urlsafe_b64encode(blob[-32:]).decode().rstrip("="))
'
}

wait_for_file() {
  local path=$1
  for _ in $(seq 100); do
    [ -s "$path" ] && return 0
    sleep 0.05
  done
  return 1
}

wait_for_socket() {
  local pid=$1
  local socket_path=$2
  local log_path=$3
  for _ in $(seq 120); do
    process_alive "$pid" || return 1
    [ -S "$socket_path" ] && return 0
    sleep 0.05
  done
  echo "daemon log:" >&2
  sed 's/^/  /' "$log_path" >&2
  return 1
}

wait_for_tcp() {
  local pid=$1
  local host=$2
  local port=$3
  for _ in $(seq 100); do
    process_alive "$pid" || return 1
    if python3 - "$host" "$port" <<'PY' >/dev/null 2>&1
import socket, sys
with socket.create_connection((sys.argv[1], int(sys.argv[2])), timeout=0.1):
    pass
PY
    then
      return 0
    fi
    sleep 0.05
  done
  return 1
}

stop_process() {
  local pid=$1
  [ -z "$pid" ] && return 0
  kill "$pid" 2>/dev/null || true
  for _ in $(seq 100); do
    process_alive "$pid" || break
    sleep 0.05
  done
  if process_alive "$pid"; then
    kill -9 "$pid" 2>/dev/null || true
  fi
  wait "$pid" 2>/dev/null || true
}

start_edge() {
  local config=$1
  local log=$2
  env MESH_STATE_DIR="$EDGE_STATE" MESH_FAKE_TAILSCALE_STATUS="$EDGE_STATUS" PATH="$TEST_ROOT/bin:$PATH" \
    "$MESH_INTEGRATION" daemon --tailnet-port "$CONTROL_PORT" --websocket-path /mesh --edge "$config" >"$log" 2>&1 &
  EDGE_PID=$!
  wait_for_socket "$EDGE_PID" "$EDGE_STATE/daemon.sock" "$log" || fail "edge daemon did not start"
}

start_origin_one() {
  local log=$1
  env MESH_STATE_DIR="$ORIGIN_ONE_STATE" MESH_FAKE_TAILSCALE_STATUS="$ORIGIN_ONE_STATUS" PATH="$TEST_ROOT/bin:$PATH" \
    "$MESH_INTEGRATION" daemon --tailnet-port "$CONTROL_PORT" --websocket-path /mesh \
    --public-edge-target "$ORIGIN_ONE_TARGET" >"$log" 2>&1 &
  ORIGIN_ONE_PID=$!
  wait_for_socket "$ORIGIN_ONE_PID" "$ORIGIN_ONE_STATE/daemon.sock" "$log" || fail "first origin daemon did not start"
}

start_origin_two() {
  local log=$1
  env MESH_STATE_DIR="$ORIGIN_TWO_STATE" MESH_FAKE_TAILSCALE_STATUS="$ORIGIN_TWO_STATUS" PATH="$TEST_ROOT/bin:$PATH" \
    "$MESH_INTEGRATION" daemon --tailnet-port "$CONTROL_PORT" --websocket-path /mesh \
    --public-edge-target "$ORIGIN_TWO_TARGET" >"$log" 2>&1 &
  ORIGIN_TWO_PID=$!
  wait_for_socket "$ORIGIN_TWO_PID" "$ORIGIN_TWO_STATE/daemon.sock" "$log" || fail "second origin daemon did not start"
}

proxy_curl() {
  curl --noproxy '*' --silent --max-time 4 \
    --header 'Host: app.shaulavo.dev' \
    --header 'X-Forwarded-For: 203.0.113.77' \
    --header 'X-Forwarded-Proto: https' \
    "$@"
}

direct_curl() {
  curl --noproxy '*' --silent --max-time 4 --cacert "$LIVE_CERT" \
    --resolve "app.shaulavo.dev:$DIRECT_PORT:127.0.0.1" "$@"
}

wait_for_proxy_marker() {
  for _ in $(seq 40); do
    body=$(proxy_curl "http://127.0.0.1:$PROXY_PORT/site/" 2>/dev/null) &&
      [ "$body" = PUBLIC_EDGE_ORIGIN_ONE ] && return 0
    sleep 0.05
  done
  return 1
}

wait_for_direct_marker() {
  for _ in $(seq 40); do
    body=$(direct_curl "https://app.shaulavo.dev:$DIRECT_PORT/site/" 2>/dev/null) &&
      [ "$body" = PUBLIC_EDGE_ORIGIN_ONE ] && return 0
    sleep 0.05
  done
  return 1
}

create_certificate() {
  local serial=$1
  local wildcard=$2
  local certificate=$3
  local private_key=$4
  openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -sha256 -nodes -days 30 \
    -set_serial "$serial" -subj "/CN=$wildcard" -addext "subjectAltName=DNS:$wildcard" \
    -keyout "$private_key" -out "$certificate" >/dev/null 2>&1 || fail "generate certificate $serial"
}

sign_bundle() {
  local profile=$1
  local environment=$2
  local certificate=$3
  local private_key=$4
  local signature=$5
  local digest="$signature.digest"
  python3 - "$profile" "$environment" "$EDGE_ID" "$RENEWER_ID" "$certificate" "$private_key" "$digest" <<'PY'
import hashlib
import struct
import sys

profile, environment, target_id, signer_id, certificate_path, key_path, output_path = sys.argv[1:]
fields = [
    b"mesh/certificate-bundle/v3",
    profile.encode(),
    environment.encode(),
    target_id.encode(),
    signer_id.encode(),
    b"",
    open(certificate_path, "rb").read(),
    open(key_path, "rb").read(),
]
digest = hashlib.sha256()
for field in fields:
    digest.update(struct.pack(">Q", len(field)))
    digest.update(field)
open(output_path, "wb").write(digest.digest())
PY
  openssl pkeyutl -sign -rawin -inkey "$RENEWER_KEY" -in "$digest" -out "$signature" >/dev/null 2>&1 ||
    fail "sign $profile $environment bundle"
}

served_fingerprint() {
  { openssl s_client -connect "127.0.0.1:$DIRECT_PORT" -servername app.shaulavo.dev -showcerts </dev/null 2>/dev/null || true; } |
    openssl x509 -noout -fingerprint -sha256 2>/dev/null || true
}

for tool in openssl python3 curl go; do
  command -v "$tool" >/dev/null 2>&1 || fail "$tool is required"
done

mkdir -p "$TEST_ROOT/bin" "$EDGE_STATE" "$ORIGIN_ONE_STATE" "$ORIGIN_TWO_STATE" \
  "$TEST_ROOT/origin-one-site" "$TEST_ROOT/origin-two-site"
ln -s "$REPO_ROOT/integration/helpers/fake_tailscale" "$TEST_ROOT/bin/tailscale"
printf PUBLIC_EDGE_ORIGIN_ONE >"$TEST_ROOT/origin-one-site/index.html"
printf PUBLIC_EDGE_ORIGIN_TWO >"$TEST_ROOT/origin-two-site/index.html"

(cd "$REPO_ROOT" && go build -tags mesh_integration -o "$MESH_INTEGRATION" ./cmd/mesh) ||
  fail "build tagged Mesh binary"

for state in "$EDGE_STATE" "$ORIGIN_ONE_STATE" "$ORIGIN_TWO_STATE"; do
  ssh-keygen -q -t ed25519 -N "" -C "" -f "$state/identity.key" >/dev/null 2>&1 || fail "generate daemon identity"
  rm -f "$state/identity.key.pub"
  chmod 0600 "$state/identity.key"
done
RENEWER_KEY="$TEST_ROOT/renewer.key"
# The renewer is an external signer that Mesh never loads, and openssl signs
# with it directly, so it stays PKCS#8 rather than following the daemon keys.
openssl genpkey -algorithm ED25519 -out "$RENEWER_KEY" >/dev/null 2>&1 || fail "generate renewer identity"
chmod 0600 "$RENEWER_KEY"
EDGE_ID=$(identity_id "$EDGE_STATE/identity.key") || fail "derive edge identity"
ORIGIN_ONE_ID=$(identity_id "$ORIGIN_ONE_STATE/identity.key") || fail "derive first origin identity"
ORIGIN_TWO_ID=$(identity_id "$ORIGIN_TWO_STATE/identity.key") || fail "derive second origin identity"
RENEWER_ID=$(pkcs8_identity_id "$RENEWER_KEY") || fail "derive renewer identity"

mapfile -t PORTS < <(python3 - <<'PY'
import socket
sockets = []
for _ in range(3):
    listener = socket.socket()
    listener.bind(("127.0.0.1", 0))
    sockets.append(listener)
for listener in sockets:
    print(listener.getsockname()[1])
for listener in sockets:
    listener.close()
PY
)
[ "${#PORTS[@]}" -eq 3 ] || fail "allocate fixture ports"
CONTROL_PORT=${PORTS[0]}
PROXY_PORT=${PORTS[1]}
DIRECT_PORT=${PORTS[2]}

EDGE_STATUS="$TEST_ROOT/edge-status.json"
ORIGIN_ONE_STATUS="$TEST_ROOT/origin-one-status.json"
ORIGIN_TWO_STATUS="$TEST_ROOT/origin-two-status.json"
PROXY_CONFIG="$TEST_ROOT/proxy-edge.json"
DIRECT_CONFIG="$TEST_ROOT/direct-edge.json"
ORIGIN_ONE_TARGET="$TEST_ROOT/origin-one-target.json"
ORIGIN_TWO_TARGET="$TEST_ROOT/origin-two-target.json"
python3 - "$EDGE_STATUS" "$ORIGIN_ONE_STATUS" "$ORIGIN_TWO_STATUS" "$PROXY_CONFIG" "$DIRECT_CONFIG" \
  "$ORIGIN_ONE_TARGET" "$ORIGIN_TWO_TARGET" "$EDGE_ID" "$ORIGIN_ONE_ID" "$ORIGIN_TWO_ID" "$RENEWER_ID" \
  "$CONTROL_PORT" "$PROXY_PORT" "$DIRECT_PORT" <<'PY'
import json
import sys

(
    edge_status, origin_one_status, origin_two_status, proxy_config, direct_config,
    origin_one_target, origin_two_target, edge_id, origin_one_id, origin_two_id,
    renewer_id, control_port, proxy_port, direct_port,
) = sys.argv[1:]
nodes = {
    # Linux chooses 127.0.0.1 for edge-to-origin loopback connections. Publish
    # that exact address so the origin's forwarding trust remains identity-pinned.
    "edge": ("edge.fixture.test", "127.0.0.1"),
    "origin-one": ("origin-one.fixture.test", "127.0.0.11"),
    "origin-two": ("origin-two.fixture.test", "127.0.0.12"),
}


def status_for(self_key):
    name, address = nodes[self_key]
    peers = {}
    for key, (peer_name, peer_address) in nodes.items():
        if key != self_key:
            peers[key] = {"DNSName": peer_name + ".", "TailscaleIPs": [peer_address], "Online": True}
    return {
        "BackendState": "Running",
        "Self": {"DNSName": name + ".", "TailscaleIPs": [address], "Online": True},
        "Peer": peers,
    }


for path, key in ((edge_status, "edge"), (origin_one_status, "origin-one"), (origin_two_status, "origin-two")):
    with open(path, "w", encoding="utf-8") as output:
        json.dump(status_for(key), output, separators=(",", ":"))

origins = [
    {
        "identity": origin_one_id,
        "displayAlias": "origin one",
        "tailscaleName": nodes["origin-one"][0],
        "controlPort": int(control_port),
        "websocketPath": "/mesh",
    },
    {
        "identity": origin_two_id,
        "displayAlias": "origin two",
        "tailscaleName": nodes["origin-two"][0],
        "controlPort": int(control_port),
        "websocketPath": "/mesh",
    },
]
with open(proxy_config, "w", encoding="utf-8") as output:
    json.dump(
        {"mode": "proxy", "listenAddress": f"127.0.0.1:{proxy_port}", "origins": origins},
        output,
        separators=(",", ":"),
    )
with open(direct_config, "w", encoding="utf-8") as output:
    json.dump(
        {
            "mode": "direct-tls",
            "listenAddress": f":{direct_port}",
            "certificateRenewerId": renewer_id,
            "origins": origins,
        },
        output,
        separators=(",", ":"),
    )
target = {
    "identity": edge_id,
    "tailscaleName": nodes["edge"][0],
    "controlPort": int(control_port),
    "websocketPath": "/mesh",
}
for path in (origin_one_target, origin_two_target):
    with open(path, "w", encoding="utf-8") as output:
        json.dump(target, output, separators=(",", ":"))
PY

BACKEND_PORT_FILE="$TEST_ROOT/backend.port"
BLOCK_READY="$TEST_ROOT/backend-block.ready"
python3 "$HTTP_FIXTURE" serve "$BACKEND_PORT_FILE" "$BLOCK_READY" >"$TEST_ROOT/backend.log" 2>&1 &
BACKEND_PID=$!
wait_for_file "$BACKEND_PORT_FILE" || fail "HTTP/WebSocket fixture did not start"
BACKEND_PORT=$(<"$BACKEND_PORT_FILE")

# A direct-TLS certificate source would reject this pointer at startup. Proxy
# mode must leave it unread and unchanged because it owns no public TLS state.
mkdir -p "$EDGE_STATE/certificates/public-edge/live"
printf 'POISONED_PROXY_CERTIFICATE_STATE\n' >"$EDGE_STATE/certificates/public-edge/live/current"
start_edge "$PROXY_CONFIG" "$TEST_ROOT/edge-proxy.log"
wait_for_tcp "$EDGE_PID" 127.0.0.1 "$PROXY_PORT" || fail "proxy listener did not start"
[ "$(<"$EDGE_STATE/certificates/public-edge/live/current")" = POISONED_PROXY_CERTIFICATE_STATE ] ||
  fail "proxy mode read or changed public certificate state"
[ ! -e "$EDGE_STATE/certificates/public-edge/staging" ] || fail "proxy mode constructed public staging state"
start_origin_one "$TEST_ROOT/origin-one.log"
start_origin_two "$TEST_ROOT/origin-two.log"

python3 "$CONTROL_FIXTURE" --expect-type service.upserted upsert "$ORIGIN_ONE_STATE/daemon.sock" site static \
  "$TEST_ROOT/origin-one-site" app.shaulavo.dev >/dev/null || fail "publish static service"
python3 "$CONTROL_FIXTURE" --expect-type service.upserted upsert "$ORIGIN_ONE_STATE/daemon.sock" bridge proxy \
  "$BACKEND_PORT" app.shaulavo.dev >/dev/null || fail "publish proxy service"
wait_for_proxy_marker || fail "proxy edge did not reach the first origin: $(cat "$TEST_ROOT/edge-proxy.log")"

python3 "$CONTROL_FIXTURE" --expect-type error --expect-code edge.wake_unavailable upsert \
  "$ORIGIN_ONE_STATE/daemon.sock" wake static "$TEST_ROOT/origin-one-site" wake.shaulavo.dev --wake >/dev/null ||
  fail "refuse unavailable public wake route"
WAKE_STATUS=$(curl --noproxy '*' --silent --max-time 2 --output /dev/null --write-out '%{http_code}' \
  --header 'Host: wake.shaulavo.dev' --header 'X-Forwarded-For: 203.0.113.77' \
  --header 'X-Forwarded-Proto: https' "http://127.0.0.1:$PROXY_PORT/wake/") ||
  fail "wake rollback request"
[ "$WAKE_STATUS" = 404 ] || fail "rejected wake route remained published with status $WAKE_STATUS"

HEADER_BODY=$(proxy_curl "http://127.0.0.1:$PROXY_PORT/bridge/headers") || fail "proxy header request"
printf '%s\n' "$HEADER_BODY" | grep -q '^xfp=https$' ||
  fail "trusted X-Forwarded-Proto did not reach the service: $HEADER_BODY"
printf '%s\n' "$HEADER_BODY" | grep -q '^xff=203\.0\.113\.77' ||
  fail "trusted client address did not reach the service: $HEADER_BODY"
python3 "$HTTP_FIXTURE" client 127.0.0.1 "$PROXY_PORT" app.shaulavo.dev /bridge/socket --proxy >/dev/null ||
  fail "proxy WebSocket stream"

MALFORMED_STATUS=$(curl --noproxy '*' --silent --max-time 2 --output /dev/null --write-out '%{http_code}' \
  --header 'Host: app.shaulavo.dev' --header 'X-Forwarded-For: 203.0.113.1' \
  --header 'X-Forwarded-For: 203.0.113.2' --header 'X-Forwarded-Proto: https' \
  "http://127.0.0.1:$PROXY_PORT/site/") || fail "malformed forwarded-header request"
[ "$MALFORMED_STATUS" = 404 ] || fail "repeated forwarded header returned $MALFORMED_STATUS, want 404"
MALFORMED_STATUS=$(curl --noproxy '*' --silent --max-time 2 --output /dev/null --write-out '%{http_code}' \
  --header 'Host: app.shaulavo.dev' --header 'X-Forwarded-For: not-an-ip' \
  --header 'X-Forwarded-Proto: https' "http://127.0.0.1:$PROXY_PORT/site/") ||
  fail "invalid forwarded-address request"
[ "$MALFORMED_STATUS" = 404 ] || fail "invalid forwarded address returned $MALFORMED_STATUS, want 404"
TERMINAL_STATUS=$(proxy_curl --output /dev/null --write-out '%{http_code}' \
  "http://127.0.0.1:$PROXY_PORT/%256d%2565%2573%2568") || fail "encoded terminal isolation request"
[ "$TERMINAL_STATUS" = 404 ] || fail "encoded terminal path returned $TERMINAL_STATUS, want 404"

python3 "$CONTROL_FIXTURE" --expect-type error --expect-code edge.route_collision upsert \
  "$ORIGIN_TWO_STATE/daemon.sock" site static "$TEST_ROOT/origin-two-site" app.shaulavo.dev >/dev/null ||
  fail "refuse route collision"
[ "$(proxy_curl "http://127.0.0.1:$PROXY_PORT/site/")" = PUBLIC_EDGE_ORIGIN_ONE ] ||
  fail "collision displaced the first origin"
stop_process "$ORIGIN_TWO_PID"
ORIGIN_TWO_PID=""

stop_process "$ORIGIN_ONE_PID"
ORIGIN_ONE_PID=""
stop_process "$EDGE_PID"
EDGE_PID=""
start_edge "$PROXY_CONFIG" "$TEST_ROOT/edge-proxy-restarted.log"
wait_for_tcp "$EDGE_PID" 127.0.0.1 "$PROXY_PORT" || fail "restarted proxy listener did not start"
OFFLINE_STATUS=$(proxy_curl --output "$TEST_ROOT/restart-offline.out" --write-out '%{http_code}' \
  "http://127.0.0.1:$PROXY_PORT/site/") || fail "offline restored-claim request"
[ "$OFFLINE_STATUS" = 502 ] ||
  fail "restored claim returned $OFFLINE_STATUS before a fresh heartbeat, want 502"
grep -q 'origin one is offline' "$TEST_ROOT/restart-offline.out" ||
  fail "offline response omitted the display alias"
if grep -Eq "127\\.0\\.0\\.|fixture\\.test|$ORIGIN_ONE_ID" "$TEST_ROOT/restart-offline.out"; then
  fail "offline response leaked origin routing or identity data"
fi

start_origin_one "$TEST_ROOT/origin-one-restarted.log"
wait_for_proxy_marker || fail "fresh origin heartbeat did not restore liveness"

python3 "$CONTROL_FIXTURE" --expect-type service.deleted delete "$ORIGIN_ONE_STATE/daemon.sock" site >/dev/null ||
  fail "delete public service"
DELETE_STATUS=$(proxy_curl --output /dev/null --write-out '%{http_code}' \
  "http://127.0.0.1:$PROXY_PORT/site/") || fail "deleted route request"
[ "$DELETE_STATUS" = 404 ] || fail "acknowledged delete left route at edge with status $DELETE_STATUS"
python3 "$CONTROL_FIXTURE" --expect-type service.deleted delete "$ORIGIN_ONE_STATE/daemon.sock" site >/dev/null ||
  fail "repeat absent service delete"
python3 "$CONTROL_FIXTURE" --expect-type service.upserted upsert "$ORIGIN_ONE_STATE/daemon.sock" site static \
  "$TEST_ROOT/origin-one-site" app.shaulavo.dev >/dev/null || fail "restore static service for direct TLS"
wait_for_proxy_marker || fail "restored service did not republish"

stop_process "$ORIGIN_ONE_PID"
ORIGIN_ONE_PID=""
stop_process "$EDGE_PID"
EDGE_PID=""
[ "$(<"$EDGE_STATE/certificates/public-edge/live/current")" = POISONED_PROXY_CERTIFICATE_STATE ] ||
  fail "proxy lifetime changed public certificate state"
mv "$EDGE_STATE/certificates/public-edge" "$TEST_ROOT/proxy-public-certificate-poison"
start_edge "$DIRECT_CONFIG" "$TEST_ROOT/edge-direct.log"
wait_for_tcp "$EDGE_PID" 127.0.0.1 "$DIRECT_PORT" || fail "direct-TLS listener did not start"

STAGING_CERT="$TEST_ROOT/public-staging.crt"
STAGING_KEY="$TEST_ROOT/public-staging.key"
STAGING_SIGNATURE="$TEST_ROOT/public-staging.signature"
LIVE_CERT="$TEST_ROOT/public-live.crt"
LIVE_KEY="$TEST_ROOT/public-live.key"
LIVE_SIGNATURE="$TEST_ROOT/public-live.signature"
create_certificate 401 '*.shaulavo.dev' "$STAGING_CERT" "$STAGING_KEY"
sign_bundle public-edge staging "$STAGING_CERT" "$STAGING_KEY" "$STAGING_SIGNATURE"
python3 "$CONTROL_FIXTURE" --expect-type certificate.installed install "$EDGE_STATE/daemon.sock" public-edge staging \
  "$EDGE_ID" "$RENEWER_ID" "$STAGING_CERT" "$STAGING_KEY" "$STAGING_SIGNATURE" >/dev/null ||
  fail "install staging public certificate"
[ -f "$EDGE_STATE/certificates/public-edge/staging/current" ] ||
  fail "staging public certificate was not persisted"
[ ! -e "$EDGE_STATE/certificates/public-edge/live/current" ] ||
  fail "staging public certificate entered the live slot"
[ -z "$(served_fingerprint)" ] || fail "staging public certificate became serving"

create_certificate 402 '*.shaulavo.dev' "$LIVE_CERT" "$LIVE_KEY"
sign_bundle public-edge live "$LIVE_CERT" "$LIVE_KEY" "$LIVE_SIGNATURE"
python3 "$CONTROL_FIXTURE" --expect-type certificate.installed install "$EDGE_STATE/daemon.sock" public-edge live \
  "$EDGE_ID" "$RENEWER_ID" "$LIVE_CERT" "$LIVE_KEY" "$LIVE_SIGNATURE" >/dev/null ||
  fail "install live public certificate"
EXPECTED_FINGERPRINT=$(openssl x509 -in "$LIVE_CERT" -noout -fingerprint -sha256)
[ "$(served_fingerprint)" = "$EXPECTED_FINGERPRINT" ] ||
  fail "direct listener did not serve the live public certificate"

STAGING_TWO_CERT="$TEST_ROOT/public-staging-two.crt"
STAGING_TWO_KEY="$TEST_ROOT/public-staging-two.key"
STAGING_TWO_SIGNATURE="$TEST_ROOT/public-staging-two.signature"
create_certificate 403 '*.shaulavo.dev' "$STAGING_TWO_CERT" "$STAGING_TWO_KEY"
sign_bundle public-edge staging "$STAGING_TWO_CERT" "$STAGING_TWO_KEY" "$STAGING_TWO_SIGNATURE"
python3 "$CONTROL_FIXTURE" --expect-type certificate.installed install "$EDGE_STATE/daemon.sock" public-edge staging \
  "$EDGE_ID" "$RENEWER_ID" "$STAGING_TWO_CERT" "$STAGING_TWO_KEY" "$STAGING_TWO_SIGNATURE" >/dev/null ||
  fail "rotate staging public certificate"
[ "$(served_fingerprint)" = "$EXPECTED_FINGERPRINT" ] ||
  fail "later staging rotation changed the active direct listener"

PRIVATE_CERT="$TEST_ROOT/private.crt"
PRIVATE_KEY="$TEST_ROOT/private.key"
PRIVATE_SIGNATURE="$TEST_ROOT/private.signature"
create_certificate 404 '*.mesh.shaulavo.dev' "$PRIVATE_CERT" "$PRIVATE_KEY"
sign_bundle private-origin live "$PRIVATE_CERT" "$PRIVATE_KEY" "$PRIVATE_SIGNATURE"
python3 "$CONTROL_FIXTURE" --expect-type error install "$EDGE_STATE/daemon.sock" private-origin live \
  "$EDGE_ID" "$RENEWER_ID" "$PRIVATE_CERT" "$PRIVATE_KEY" "$PRIVATE_SIGNATURE" >/dev/null ||
  fail "reject private certificate profile at public edge"
[ ! -e "$EDGE_STATE/private-tls" ] ||
  fail "private certificate profile created an edge slot"
[ "$(served_fingerprint)" = "$EXPECTED_FINGERPRINT" ] ||
  fail "profile mismatch changed the direct listener certificate"

DIRECT_OFFLINE_STATUS=$(direct_curl --output /dev/null --write-out '%{http_code}' \
  "https://app.shaulavo.dev:$DIRECT_PORT/site/") || fail "direct restored-claim request"
[ "$DIRECT_OFFLINE_STATUS" = 502 ] ||
  fail "direct edge restored claim as $DIRECT_OFFLINE_STATUS before a fresh heartbeat"
start_origin_one "$TEST_ROOT/origin-one-direct.log"
wait_for_direct_marker || fail "direct edge did not restore origin liveness"
DIRECT_HEADER_BODY=$(direct_curl "https://app.shaulavo.dev:$DIRECT_PORT/bridge/headers") ||
  fail "direct header request"
printf '%s\n' "$DIRECT_HEADER_BODY" | grep -q '^xfp=https$' ||
  fail "direct TLS scheme did not reach the service: $DIRECT_HEADER_BODY"
python3 "$HTTP_FIXTURE" client 127.0.0.1 "$DIRECT_PORT" app.shaulavo.dev /bridge/socket \
  --ca "$LIVE_CERT" >/dev/null || fail "direct-TLS WebSocket stream"
DIRECT_TERMINAL_STATUS=$(direct_curl --output /dev/null --write-out '%{http_code}' \
  "https://app.shaulavo.dev:$DIRECT_PORT/%256d%2565%2573%2568") ||
  fail "direct terminal isolation request"
[ "$DIRECT_TERMINAL_STATUS" = 404 ] ||
  fail "direct encoded terminal path returned $DIRECT_TERMINAL_STATUS"

rm -f -- "$BLOCK_READY"
direct_curl --output "$TEST_ROOT/killed-origin.out" --write-out '%{http_code}' \
  "https://app.shaulavo.dev:$DIRECT_PORT/bridge/block" >"$TEST_ROOT/killed-origin.status" &
KILLED_CLIENT_PID=$!
wait_for_file "$BLOCK_READY" || fail "mid-request backend was not reached"
kill -9 "$ORIGIN_ONE_PID" 2>/dev/null || fail "kill origin during request"
wait "$ORIGIN_ONE_PID" 2>/dev/null || true
ORIGIN_ONE_PID=""
for _ in $(seq 60); do
  process_alive "$KILLED_CLIENT_PID" || break
  sleep 0.05
done
if process_alive "$KILLED_CLIENT_PID"; then
  kill -9 "$KILLED_CLIENT_PID" 2>/dev/null || true
  fail "mid-request origin death did not produce a prompt response"
fi
wait "$KILLED_CLIENT_PID" || fail "killed-origin request failed transport"
KILLED_STATUS=$(<"$TEST_ROOT/killed-origin.status")
[ "$KILLED_STATUS" = 502 ] || fail "killed origin returned $KILLED_STATUS, want 502"
if grep -Eq "127\\.0\\.0\\.|fixture\\.test|$ORIGIN_ONE_ID" "$TEST_ROOT/killed-origin.out"; then
  fail "killed-origin response leaked origin routing or identity data"
fi

echo "PASS: real daemons registered, proxied HTTP/WebSocket, restored offline claims, rejected collisions, withdrew routes, and served isolated direct TLS"
