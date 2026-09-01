#!/usr/bin/env bash
# The product CLI publishes services through identity-pinned real daemons.
# This fixture has no T12 certificate or Tailscale Serve, so private services
# use the verified control endpoint while public URLs remain canonical HTTPS.
set -uo pipefail
export PYTHONDONTWRITEBYTECODE=1
export NO_COLOR=1

REPO_ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
HTTP_FIXTURE="$REPO_ROOT/integration/helpers/public_http_fixture.py"
CONFIRM_FIXTURE="$REPO_ROOT/integration/helpers/confirm_public_serve.py"
TEST_ROOT=$(mktemp -d)
MESH_INTEGRATION="$TEST_ROOT/mesh-integration"
EDGE_STATE="$TEST_ROOT/edge-state"
ORIGIN_STATE="$TEST_ROOT/origin-state"
ORIGIN_HOME="$TEST_ROOT/origin-home"
CLIENT_STATE="$TEST_ROOT/client-state"
CLIENT_CONFIG="$TEST_ROOT/client-config"
EDGE_PID=""
ORIGIN_PID=""
BACKEND_PID=""

cleanup() {
  for pid in "$ORIGIN_PID" "$EDGE_PID" "$BACKEND_PID"; do
    [ -z "$pid" ] || kill "$pid" 2>/dev/null || true
  done
  for pid in "$ORIGIN_PID" "$EDGE_PID" "$BACKEND_PID"; do
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
  local path=$2
  local log=$3
  for _ in $(seq 100); do
    process_alive "$pid" || return 1
    [ -S "$path" ] && return 0
    sleep 0.05
  done
  sed 's/^/  /' "$log" >&2
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
    sleep 0.02
  done
  if process_alive "$pid"; then
    kill -9 "$pid" 2>/dev/null || true
  fi
  wait "$pid" 2>/dev/null || true
}

start_edge() {
  local log=$1
  env MESH_STATE_DIR="$EDGE_STATE" MESH_FAKE_TAILSCALE_STATUS="$EDGE_STATUS" PATH="$TEST_ROOT/bin:$PATH" \
    "$MESH_INTEGRATION" daemon --tailnet-port "$CONTROL_PORT" --websocket-path /mesh \
    --edge "$EDGE_CONFIG" >"$log" 2>&1 &
  EDGE_PID=$!
  wait_for_socket "$EDGE_PID" "$EDGE_STATE/daemon.sock" "$log" || fail "edge daemon did not start"
  wait_for_tcp "$EDGE_PID" 127.0.0.1 "$EDGE_PROXY_PORT" || fail "edge proxy listener did not start"
}

start_origin() {
  local log=$1
  env HOME="$ORIGIN_HOME" MESH_STATE_DIR="$ORIGIN_STATE" MESH_FAKE_TAILSCALE_STATUS="$ORIGIN_STATUS" \
    PATH="$TEST_ROOT/bin:$PATH" \
    "$MESH_INTEGRATION" daemon --tailnet-port "$CONTROL_PORT" --websocket-path /mesh \
    --public-edge-target "$ORIGIN_TARGET" >"$log" 2>&1 &
  ORIGIN_PID=$!
  wait_for_socket "$ORIGIN_PID" "$ORIGIN_STATE/daemon.sock" "$log" || fail "origin daemon did not start"
  wait_for_tcp "$ORIGIN_PID" 127.0.0.11 "$CONTROL_PORT" || fail "origin Tailnet listener did not start"
}

edge_request() {
  local host=$1
  local path=$2
  shift 2
  curl --noproxy '*' --silent --max-time 2 \
    --header "Host: $host" --header 'X-Forwarded-For: 203.0.113.77' \
    --header 'X-Forwarded-Proto: https' "$@" "http://127.0.0.1:$EDGE_PROXY_PORT$path"
}

wait_for_public_body() {
  local host=$1
  local path=$2
  local expected=$3
  local body
  for _ in $(seq 60); do
    body=$(edge_request "$host" "$path" 2>/dev/null) && [ "$body" = "$expected" ] && return 0
    sleep 0.05
  done
  return 1
}

for tool in curl go openssl python3 timeout; do
  command -v "$tool" >/dev/null 2>&1 || fail "$tool is required"
done

mkdir -p "$TEST_ROOT/bin" "$EDGE_STATE" "$ORIGIN_STATE" "$ORIGIN_HOME/site/assets" \
  "$CLIENT_STATE" "$CLIENT_CONFIG" "$TEST_ROOT/files" "$TEST_ROOT/secret"
ln -s "$REPO_ROOT/integration/helpers/fake_tailscale" "$TEST_ROOT/bin/tailscale"
printf 'SERVE_CLI_PUBLIC_MARKER' >"$ORIGIN_HOME/site/index.html"
printf 'SECOND_FILE' >"$ORIGIN_HOME/site/assets/data.txt"
printf 'DOWNLOAD_MARKER' >"$TEST_ROOT/files/download.txt"
printf 'SECRET_PUBLIC_MARKER' >"$TEST_ROOT/secret/index.html"
printf 'DO_NOT_PUBLISH' >"$TEST_ROOT/secret/.env"

(cd "$REPO_ROOT" && go build -tags mesh_integration -o "$MESH_INTEGRATION" ./cmd/mesh) ||
  fail "build tagged Mesh binary"

for state in "$EDGE_STATE" "$ORIGIN_STATE"; do
  ssh-keygen -q -t ed25519 -N "" -C "" -f "$state/identity.key" >/dev/null 2>&1 || fail "generate daemon identity"
  rm -f "$state/identity.key.pub"
  chmod 0600 "$state/identity.key"
done
EDGE_ID=$(identity_id "$EDGE_STATE/identity.key") || fail "derive edge identity"
ORIGIN_ID=$(identity_id "$ORIGIN_STATE/identity.key") || fail "derive origin identity"

mapfile -t PORTS < <(python3 - <<'PY'
import socket
sockets = []
for _ in range(2):
    listener = socket.socket()
    listener.bind(("127.0.0.1", 0))
    sockets.append(listener)
for listener in sockets:
    print(listener.getsockname()[1])
for listener in sockets:
    listener.close()
PY
)
[ "${#PORTS[@]}" -eq 2 ] || fail "allocate fixture ports"
CONTROL_PORT=${PORTS[0]}
EDGE_PROXY_PORT=${PORTS[1]}

EDGE_STATUS="$TEST_ROOT/edge-status.json"
ORIGIN_STATUS="$TEST_ROOT/origin-status.json"
EDGE_CONFIG="$TEST_ROOT/edge.json"
ORIGIN_TARGET="$TEST_ROOT/origin-target.json"
python3 - "$EDGE_STATUS" "$ORIGIN_STATUS" "$EDGE_CONFIG" "$ORIGIN_TARGET" \
  "$CLIENT_CONFIG/hosts.json" "$EDGE_ID" "$ORIGIN_ID" "$CONTROL_PORT" "$EDGE_PROXY_PORT" <<'PY'
import json
import os
import sys

(
    edge_status, origin_status, edge_config, origin_target, hosts_path,
    edge_id, origin_id, control_port, proxy_port,
) = sys.argv[1:]
control_port = int(control_port)

edge = ("edge.fixture.test", "127.0.0.1")
origin = ("pc.fixture.test", "127.0.0.11")

def status(self_node, peer_node):
    self_name, self_address = self_node
    peer_name, peer_address = peer_node
    return {
        "BackendState": "Running",
        "Self": {"DNSName": self_name + ".", "TailscaleIPs": [self_address], "Online": True},
        "Peer": {
            "peer": {"DNSName": peer_name + ".", "TailscaleIPs": [peer_address], "Online": True},
        },
    }

with open(edge_status, "w", encoding="utf-8") as output:
    json.dump(status(edge, origin), output, separators=(",", ":"))
with open(origin_status, "w", encoding="utf-8") as output:
    json.dump(status(origin, edge), output, separators=(",", ":"))
with open(edge_config, "w", encoding="utf-8") as output:
    json.dump({
        "mode": "proxy",
        "listenAddress": f"127.0.0.1:{proxy_port}",
        "origins": [{
            "identity": origin_id,
            "displayAlias": "pc",
            "tailscaleName": origin[0],
            "controlPort": control_port,
            "websocketPath": "/mesh",
        }],
    }, output, separators=(",", ":"))
with open(origin_target, "w", encoding="utf-8") as output:
    json.dump({
        "identity": edge_id,
        "tailscaleName": edge[0],
        "controlPort": control_port,
        "websocketPath": "/mesh",
    }, output, separators=(",", ":"))
with open(hosts_path, "w", encoding="utf-8") as output:
    json.dump({
        "version": 1,
        "hosts": [{
            "alias": "pc",
            "id": origin_id,
            "meshIdentity": origin_id,
            "tailscaleName": origin[0],
            "addresses": [origin[1]],
            "endpoint": f"ws://{origin[1]}:{control_port}/mesh",
        }],
    }, output, separators=(",", ":"))
    output.write("\n")
os.chmod(hosts_path, 0o600)
PY

CLI=(env "MESH_STATE_DIR=$CLIENT_STATE" "MESH_CONFIG_DIR=$CLIENT_CONFIG" NO_COLOR=1 "$MESH_INTEGRATION")

start_edge "$TEST_ROOT/edge.log"
start_origin "$TEST_ROOT/origin.log"

BACKEND_PORT_FILE="$TEST_ROOT/backend.port"
BLOCK_FILE="$TEST_ROOT/backend-block.ready"
python3 "$HTTP_FIXTURE" serve "$BACKEND_PORT_FILE" "$BLOCK_FILE" >"$TEST_ROOT/backend.log" 2>&1 &
BACKEND_PID=$!
wait_for_file "$BACKEND_PORT_FILE" || fail "proxy backend did not start"
BACKEND_PORT=$(<"$BACKEND_PORT_FILE")

"${CLI[@]}" serve pc ./site --at /blog >"$TEST_ROOT/private-static.out" 2>"$TEST_ROOT/private-static.err" ||
  fail "publish private static directory: $(<"$TEST_ROOT/private-static.err")"
grep -Fq "serving http://127.0.0.11:$CONTROL_PORT/blog on pc (static -> $ORIGIN_HOME/site)" "$TEST_ROOT/private-static.out" ||
  fail "private static success omitted its verified fallback URL: $(<"$TEST_ROOT/private-static.out")"
[ "$(curl --noproxy '*' --fail --silent --max-time 2 "http://127.0.0.11:$CONTROL_PORT/blog/")" = SERVE_CLI_PUBLIC_MARKER ] ||
  fail "private static service did not serve through the Tailnet listener"

"${CLI[@]}" serve pc "$TEST_ROOT/files" --at /files --files >"$TEST_ROOT/files.out" 2>"$TEST_ROOT/files.err" ||
  fail "publish browsable directory: $(<"$TEST_ROOT/files.err")"
grep -Fq "serving http://127.0.0.11:$CONTROL_PORT/files on pc (files -> $TEST_ROOT/files)" "$TEST_ROOT/files.out" ||
  fail "files success omitted its verified fallback URL: $(<"$TEST_ROOT/files.out")"
curl --noproxy '*' --fail --silent --location --max-time 2 "http://127.0.0.11:$CONTROL_PORT/files/" |
  grep -Fq 'download.txt' || fail "--files did not enable the directory listing"

"${CLI[@]}" serve pc "$BACKEND_PORT" --at /api >"$TEST_ROOT/proxy.out" 2>"$TEST_ROOT/proxy.err" ||
  fail "publish inferred proxy: $(<"$TEST_ROOT/proxy.err")"
grep -Fq "serving http://127.0.0.11:$CONTROL_PORT/api on pc (proxy -> $BACKEND_PORT)" "$TEST_ROOT/proxy.out" ||
  fail "numeric target was not reported as a proxy: $(<"$TEST_ROOT/proxy.out")"
curl --noproxy '*' --fail --silent --max-time 2 "http://127.0.0.11:$CONTROL_PORT/api/headers" |
  grep -Fq 'method=GET' || fail "numeric target did not proxy to the origin-local port"

python3 "$CONFIRM_FIXTURE" -- "${CLI[@]}" serve pc ./site --at /blog \
  --public blog.shaulavo.dev >"$TEST_ROOT/public-prompt.out" 2>&1 ||
  fail "interactive public publication: $(<"$TEST_ROOT/public-prompt.out")"
grep -Fq 'Publish this service to the internet?' "$TEST_ROOT/public-prompt.out" ||
  fail "public mutation did not require explicit confirmation"
grep -Fq "Resolved path: $ORIGIN_HOME/site" "$TEST_ROOT/public-prompt.out" ||
  fail "confirmation omitted the origin-resolved path: $(<"$TEST_ROOT/public-prompt.out")"
grep -Fq 'Files: 2' "$TEST_ROOT/public-prompt.out" ||
  fail "confirmation omitted the origin file count: $(<"$TEST_ROOT/public-prompt.out")"
grep -Fq 'URL: https://blog.shaulavo.dev/blog' "$TEST_ROOT/public-prompt.out" ||
  fail "confirmation omitted the exact public URL: $(<"$TEST_ROOT/public-prompt.out")"
grep -Fq "serving https://blog.shaulavo.dev/blog on pc (static -> $ORIGIN_HOME/site)" "$TEST_ROOT/public-prompt.out" ||
  fail "confirmed publication did not wait for its acknowledgement"
wait_for_public_body blog.shaulavo.dev /blog/ SERVE_CLI_PUBLIC_MARKER ||
  fail "confirmed public route did not reach the real origin"

if "${CLI[@]}" serve pc "$TEST_ROOT/secret" --at /secret --public secret.shaulavo.dev --yes \
  >"$TEST_ROOT/secret-refused.out" 2>"$TEST_ROOT/secret-refused.err"; then
  fail "--yes bypassed the public credential scan"
fi
grep -Fqi 'credential' "$TEST_ROOT/secret-refused.err" ||
  fail "credential refusal was not explained: $(<"$TEST_ROOT/secret-refused.err")"
grep -Fq '.env' "$TEST_ROOT/secret-refused.err" ||
  fail "credential refusal omitted the matched remote path: $(<"$TEST_ROOT/secret-refused.err")"
SECRET_STATUS=$(edge_request secret.shaulavo.dev /secret/ --output /dev/null --write-out '%{http_code}') ||
  fail "query rejected credential route"
[ "$SECRET_STATUS" = 404 ] || fail "rejected credential route remained public with status $SECRET_STATUS"

"${CLI[@]}" serve pc "$TEST_ROOT/secret" --at /secret --public secret.shaulavo.dev --yes \
  --allow-credentials >"$TEST_ROOT/secret-allowed.out" 2>"$TEST_ROOT/secret-allowed.err" ||
  fail "explicit credential override: $(<"$TEST_ROOT/secret-allowed.err")"
grep -Fq 'credential-like entries are explicitly allowed' "$TEST_ROOT/secret-allowed.err" ||
  fail "credential override was not visibly acknowledged"
grep -Fq 'serving https://secret.shaulavo.dev/secret on pc' "$TEST_ROOT/secret-allowed.out" ||
  fail "credential override omitted the public URL: $(<"$TEST_ROOT/secret-allowed.out")"
wait_for_public_body secret.shaulavo.dev /secret/ SECRET_PUBLIC_MARKER ||
  fail "explicitly approved credential directory did not become reachable"

"${CLI[@]}" serve ls --timeout 800ms >"$TEST_ROOT/list-live.out" 2>"$TEST_ROOT/list-live.err" ||
  fail "list live services: $(<"$TEST_ROOT/list-live.err")"
grep -Eq '^ROUTE[[:space:]]+HOST[[:space:]]+KIND[[:space:]]+TARGET[[:space:]]+SCOPE[[:space:]]+HEALTH[[:space:]]+URL$' \
  "$TEST_ROOT/list-live.out" || fail "service list header is incomplete: $(<"$TEST_ROOT/list-live.out")"
grep -Eq "^/blog[[:space:]]+pc[[:space:]]+static[[:space:]]+$ORIGIN_HOME/site[[:space:]]+public[[:space:]]+healthy[[:space:]]+https://blog\.shaulavo\.dev/blog$" \
  "$TEST_ROOT/list-live.out" || fail "live list omitted the public static URL: $(<"$TEST_ROOT/list-live.out")"
grep -Eq "^/files[[:space:]]+pc[[:space:]]+files[[:space:]]+$TEST_ROOT/files[[:space:]]+tailnet[[:space:]]+healthy[[:space:]]+http://127\.0\.0\.11:$CONTROL_PORT/files$" \
  "$TEST_ROOT/list-live.out" || fail "live list omitted the private fallback URL: $(<"$TEST_ROOT/list-live.out")"
grep -Eq "^/api[[:space:]]+pc[[:space:]]+proxy[[:space:]]+$BACKEND_PORT[[:space:]]+tailnet[[:space:]]+healthy[[:space:]]+http://127\.0\.0\.11:$CONTROL_PORT/api$" \
  "$TEST_ROOT/list-live.out" || fail "live list omitted the proxy fallback URL: $(<"$TEST_ROOT/list-live.out")"
grep -Eq "^/secret[[:space:]]+pc[[:space:]]+static[[:space:]]+$TEST_ROOT/secret[[:space:]]+public[[:space:]]+healthy[[:space:]]+https://secret\.shaulavo\.dev/secret$" \
  "$TEST_ROOT/list-live.out" || fail "live list omitted the approved public URL: $(<"$TEST_ROOT/list-live.out")"

stop_process "$ORIGIN_PID"
ORIGIN_PID=""
timeout --kill-after=1s 3s "${CLI[@]}" serve ls --timeout 150ms \
  >"$TEST_ROOT/list-offline.out" 2>"$TEST_ROOT/list-offline.err" ||
  fail "offline service list exceeded its hard deadline: $(<"$TEST_ROOT/list-offline.err")"
grep -Eq '^/blog[[:space:]]+pc[[:space:]]+static.*offline/stale[[:space:]]+https://blog\.shaulavo\.dev/blog$' \
  "$TEST_ROOT/list-offline.out" || fail "offline cache lost the public URL: $(<"$TEST_ROOT/list-offline.out")"
grep -Eq "^/files[[:space:]]+pc[[:space:]]+files.*offline/stale[[:space:]]+http://127\.0\.0\.11:$CONTROL_PORT/files$" \
  "$TEST_ROOT/list-offline.out" || fail "offline cache lost the private fallback URL: $(<"$TEST_ROOT/list-offline.out")"
grep -Fq 'pc: unavailable' "$TEST_ROOT/list-offline.err" ||
  fail "offline service list omitted its host diagnostic: $(<"$TEST_ROOT/list-offline.err")"

start_origin "$TEST_ROOT/origin-restarted.log"
wait_for_public_body blog.shaulavo.dev /blog/ SERVE_CLI_PUBLIC_MARKER ||
  fail "origin restart did not restore public liveness"

"${CLI[@]}" unserve /blog --timeout 800ms >"$TEST_ROOT/unserve.out" 2>"$TEST_ROOT/unserve.err" ||
  fail "unserve acknowledged public withdrawal: $(<"$TEST_ROOT/unserve.err")"
grep -Fq 'unserved /blog on pc' "$TEST_ROOT/unserve.out" ||
  fail "unserve output omitted the route owner: $(<"$TEST_ROOT/unserve.out")"
WITHDRAWN_STATUS=$(edge_request blog.shaulavo.dev /blog/ --output /dev/null --write-out '%{http_code}') ||
  fail "query withdrawn public route"
[ "$WITHDRAWN_STATUS" = 404 ] ||
  fail "unserve returned before edge withdrawal; public status was $WITHDRAWN_STATUS"
PRIVATE_WITHDRAWN_STATUS=$(curl --noproxy '*' --silent --max-time 2 --output /dev/null --write-out '%{http_code}' \
  "http://127.0.0.11:$CONTROL_PORT/blog/") || fail "query withdrawn private route"
[ "$PRIVATE_WITHDRAWN_STATUS" = 404 ] ||
  fail "unserve left the origin route reachable with status $PRIVATE_WITHDRAWN_STATUS"

echo "PASS: serve CLI publishes, confirms, scans, lists offline, and waits for public withdrawal"
