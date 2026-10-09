#!/usr/bin/env bash
source "$(dirname -- "${BASH_SOURCE[0]}")/helpers/isolate.sh" || exit 1
# The CLI creates private services through an identity-pinned real daemon.
# Without private TLS this fixture uses the verified control endpoint.
set -uo pipefail
export PYTHONDONTWRITEBYTECODE=1
export NO_COLOR=1

REPO_ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
HTTP_FIXTURE="$REPO_ROOT/integration/helpers/http_fixture.py"
TEST_ROOT=$(mktemp -d)
MESH_INTEGRATION=${MESH_INTEGRATION_BINARY:-$TEST_ROOT/mesh-integration}
ORIGIN_STATE="$TEST_ROOT/origin-state"
ORIGIN_HOME="$TEST_ROOT/origin-home"
CLIENT_STATE="$TEST_ROOT/client-state"
CLIENT_CONFIG="$TEST_ROOT/client-config"
ORIGIN_PID=""
BACKEND_PID=""

cleanup() {
  for pid in "$ORIGIN_PID" "$BACKEND_PID"; do
    [ -z "$pid" ] || kill "$pid" 2>/dev/null || true
  done
  for pid in "$ORIGIN_PID" "$BACKEND_PID"; do
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

start_origin() {
  local log=$1
  env HOME="$ORIGIN_HOME" MESH_STATE_DIR="$ORIGIN_STATE" MESH_FAKE_TAILSCALE_STATUS="$ORIGIN_STATUS" \
    PATH="$TEST_ROOT/bin:$PATH" \
    "$MESH_INTEGRATION" daemon --tailnet-port "$CONTROL_PORT" --websocket-path /mesh \
    >"$log" 2>&1 &
  ORIGIN_PID=$!
  wait_for_socket "$ORIGIN_PID" "$ORIGIN_STATE/daemon.sock" "$log" || fail "origin daemon did not start"
  wait_for_tcp "$ORIGIN_PID" 127.0.0.11 "$CONTROL_PORT" || fail "origin Tailnet listener did not start"
}

wait_for_private_body() {
  local path=$1 expected=$2 body
  for _ in $(seq 60); do
    body=$(curl --noproxy '*' --silent --max-time 2 "http://127.0.0.11:$CONTROL_PORT$path") && [ "$body" = "$expected" ] && return 0
    sleep 0.05
  done
  return 1
}

for tool in curl go openssl python3 timeout; do
  command -v "$tool" >/dev/null 2>&1 || fail "$tool is required"
done

mkdir -p "$TEST_ROOT/bin" "$ORIGIN_STATE" "$ORIGIN_HOME/site/assets" \
  "$CLIENT_STATE" "$CLIENT_CONFIG" "$TEST_ROOT/files"
ln -s "$REPO_ROOT/integration/helpers/fake_tailscale" "$TEST_ROOT/bin/tailscale"
printf 'SERVE_CLI_PRIVATE_MARKER' >"$ORIGIN_HOME/site/index.html"
printf 'SECOND_FILE' >"$ORIGIN_HOME/site/assets/data.txt"
printf 'DOWNLOAD_MARKER' >"$TEST_ROOT/files/download.txt"

if [[ -z ${MESH_INTEGRATION_BINARY:-} ]]; then
  (cd "$REPO_ROOT" && go build -tags mesh_integration -o "$MESH_INTEGRATION" ./cmd/mesh) ||
    fail "build tagged Mesh binary"
fi

ssh-keygen -q -t ed25519 -N "" -C "" -f "$ORIGIN_STATE/identity.key" >/dev/null 2>&1 || fail "generate daemon identity"
rm -f "$ORIGIN_STATE/identity.key.pub"
chmod 0600 "$ORIGIN_STATE/identity.key"
ORIGIN_ID=$(identity_id "$ORIGIN_STATE/identity.key") || fail "derive origin identity"

CLIENT_ID=$(env MESH_STATE_DIR="$CLIENT_STATE" "$MESH_INTEGRATION" device identity --json | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])') || fail "create fixture client"
env MESH_STATE_DIR="$ORIGIN_STATE" "$MESH_INTEGRATION" device approve --allow-root -- "$CLIENT_ID" >/dev/null || fail "approve fixture origin client"
mapfile -t PORTS < <(python3 - "$$" "2" <<'PY'
import os, socket, sys

# Each concurrent script searches its own band, so sibling tests cannot pick the
# same port. Binding still confirms the port is free of anything else.
wanted = int(sys.argv[2])
base = 20000 + (int(sys.argv[1]) % 900) * 16
found, held = [], []
for offset in range(16 * 900):
    port = 20000 + (base - 20000 + offset) % (16 * 900)
    listener = socket.socket()
    listener.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    try:
        listener.bind(("127.0.0.1", port))
    except OSError:
        listener.close()
        continue
    held.append(listener)
    found.append(port)
    if len(found) == wanted:
        break
if len(found) != wanted:
    raise SystemExit("no free ports in this band")
for port in found:
    print(port)
for listener in held:
    listener.close()
PY
)
[ "${#PORTS[@]}" -eq 2 ] || fail "allocate fixture ports"
CONTROL_PORT=${PORTS[0]}

ORIGIN_STATUS="$TEST_ROOT/origin-status.json"
cp "$MESH_CONFIG_DIR/domains.json" "$CLIENT_CONFIG/domains.json"
python3 - "$ORIGIN_STATUS" "$CLIENT_CONFIG/hosts.json" "$ORIGIN_ID" "$CONTROL_PORT" <<'PYCONFIG'
import json, os, sys
status_path, hosts_path, origin_id, control_port = sys.argv[1:]
name, address = "pc.fixture.test", "127.0.0.11"
with open(status_path, "w") as output:
    json.dump({"BackendState":"Running", "Self":{"DNSName":name+".","TailscaleIPs":[address],"Online":True},"Peer":{}}, output)
with open(hosts_path, "w") as output:
    json.dump({"version":1,"hosts":[{"id":origin_id,"meshIdentity":origin_id,"tailscaleName":name,"addresses":[address],"endpoint":f"ws://{address}:{control_port}/mesh"}]}, output)
os.chmod(hosts_path, 0o600)
PYCONFIG

CLI=(env "MESH_STATE_DIR=$CLIENT_STATE" "MESH_CONFIG_DIR=$CLIENT_CONFIG" NO_COLOR=1 "$MESH_INTEGRATION")

start_origin "$TEST_ROOT/origin.log"
"${CLI[@]}" ls --all --timeout 1s >"$TEST_ROOT/name-adoption.out" 2>&1 || fail "adopt origin declaration"
[ -s "$CLIENT_CONFIG/machine-names/$ORIGIN_ID.json" ] || fail "origin declaration was not authenticated"


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
[ "$(curl --noproxy '*' --fail --silent --max-time 2 "http://127.0.0.11:$CONTROL_PORT/blog/")" = SERVE_CLI_PRIVATE_MARKER ] ||
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

PRIVATE_POST_STATUS=$(curl --noproxy '*' --silent --max-time 2 --request POST \
  --header 'Origin: https://attacker.example' --header 'Sec-Fetch-Site: cross-site' \
  --output /dev/null --write-out '%{http_code}' "http://127.0.0.11:$CONTROL_PORT/api/block") ||
  fail "query private proxy with a cross-site POST"
[ "$PRIVATE_POST_STATUS" = 403 ] || fail "private cross-site POST returned $PRIVATE_POST_STATUS, want 403"
[ ! -e "$BLOCK_FILE" ] || fail "private cross-site POST reached the upstream"
PRIVATE_FILES_STATUS=$(curl --noproxy '*' --silent --max-time 2 \
  --header 'Sec-Fetch-Site: same-site' --header 'Sec-Fetch-Mode: no-cors' --header 'Sec-Fetch-Dest: image' \
  --output /dev/null --write-out '%{http_code}' "http://127.0.0.11:$CONTROL_PORT/files/download.txt") ||
  fail "query private files from a sibling page"
[ "$PRIVATE_FILES_STATUS" = 403 ] || fail "private sibling-page read returned $PRIVATE_FILES_STATUS, want 403"
curl --noproxy '*' --fail --silent --max-time 2 --request POST \
  --header "Origin: http://127.0.0.11:$CONTROL_PORT" --header 'Sec-Fetch-Site: same-origin' \
  "http://127.0.0.11:$CONTROL_PORT/api/headers" |
  grep -Fq 'method=POST' || fail "private same-origin POST did not reach the upstream"
curl --noproxy '*' --fail --silent --max-time 2 \
  --header 'Sec-Fetch-Site: cross-site' --header 'Sec-Fetch-Mode: navigate' --header 'Sec-Fetch-Dest: document' \
  "http://127.0.0.11:$CONTROL_PORT/files/download.txt" |
  grep -Fq DOWNLOAD_MARKER || fail "private top-level navigation did not reach the files route"

"${CLI[@]}" serve pc ./site --at /blog >"$TEST_ROOT/blog.out" 2>"$TEST_ROOT/blog.err" ||
  fail "register home-relative static directory: $(<"$TEST_ROOT/blog.err")"
grep -Fq "serving http://127.0.0.11:$CONTROL_PORT/blog on pc (static -> $ORIGIN_HOME/site)" "$TEST_ROOT/blog.out" ||
  fail "static success omitted the origin-resolved path or verified URL"
wait_for_private_body /blog/ SERVE_CLI_PRIVATE_MARKER || fail "static route did not reach the origin"
"${CLI[@]}" serve pc "$TEST_ROOT/files" --at /blog/admin --files >"$TEST_ROOT/nested.out" 2>"$TEST_ROOT/nested.err" || fail "register nested private directory"
[ "$(curl --noproxy '*' --fail --silent --max-time 2 "http://127.0.0.11:$CONTROL_PORT/blog/admin/download.txt")" = DOWNLOAD_MARKER ] || fail "nested private route failed"
if "${CLI[@]}" serve pc ./site --public blog.mesh.test >"$TEST_ROOT/removed.out" 2>"$TEST_ROOT/removed.err"; then
  fail "removed public publication flag succeeded"
fi
grep -Fq 'unknown flag: --public' "$TEST_ROOT/removed.err" || fail "removed flag was not rejected by CLI"
UNKNOWN_HOST_STATUS=$(curl --noproxy '*' --silent --max-time 2 --header 'Host: blog.mesh.test' --output /dev/null --write-out '%{http_code}' "http://127.0.0.11:$CONTROL_PORT/blog/") || fail "query unknown Host"
[ "$UNKNOWN_HOST_STATUS" = 421 ] || fail "unknown Host reached a private route"

"${CLI[@]}" serve label /api 'CLI Proxy' --host pc >"$TEST_ROOT/label.out" 2>"$TEST_ROOT/label.err" ||
  fail "label existing service: $(<"$TEST_ROOT/label.err")"

"${CLI[@]}" serve ls --timeout 800ms >"$TEST_ROOT/list-live.out" 2>"$TEST_ROOT/list-live.err" ||
  fail "list live services: $(<"$TEST_ROOT/list-live.err")"
grep -Eq '^ROUTE[[:space:]]+NAME[[:space:]]+HOST[[:space:]]+KIND[[:space:]]+TARGET[[:space:]]+SCOPE[[:space:]]+STATE[[:space:]]+HEALTH[[:space:]]+URL$' \
  "$TEST_ROOT/list-live.out" || fail "service list header is incomplete: $(<"$TEST_ROOT/list-live.out")"
grep -Eq "^/blog[[:space:]]+blog[[:space:]]+pc[[:space:]]+static[[:space:]]+$ORIGIN_HOME/site[[:space:]]+tailnet[[:space:]]+-[[:space:]]+healthy[[:space:]]+http://127\.0\.0\.11:$CONTROL_PORT/blog$" \
  "$TEST_ROOT/list-live.out" || fail "live list omitted the static fallback URL: $(<"$TEST_ROOT/list-live.out")"
grep -Eq "^/files[[:space:]]+files[[:space:]]+pc[[:space:]]+files[[:space:]]+$TEST_ROOT/files[[:space:]]+tailnet[[:space:]]+-[[:space:]]+healthy[[:space:]]+http://127\.0\.0\.11:$CONTROL_PORT/files$" \
  "$TEST_ROOT/list-live.out" || fail "live list omitted the private fallback URL: $(<"$TEST_ROOT/list-live.out")"
grep -Eq "^/api[[:space:]]+CLI Proxy[[:space:]]+pc[[:space:]]+proxy[[:space:]]+${BACKEND_PORT}[[:space:]]+tailnet[[:space:]]+-[[:space:]]+healthy[[:space:]]+http://127\.0\.0\.11:$CONTROL_PORT/api$" \
  "$TEST_ROOT/list-live.out" || fail "live list omitted the proxy fallback URL: $(<"$TEST_ROOT/list-live.out")"

stop_process "$ORIGIN_PID"
ORIGIN_PID=""
timeout --kill-after=1s 3s "${CLI[@]}" serve ls --timeout 150ms \
  >"$TEST_ROOT/list-offline.out" 2>"$TEST_ROOT/list-offline.err" ||
  fail "offline service list exceeded its hard deadline: $(<"$TEST_ROOT/list-offline.err")"
grep -Eq "^/blog[[:space:]]+blog[[:space:]]+pc[[:space:]]+static.*offline/stale[[:space:]]+http://127\.0\.0\.11:$CONTROL_PORT/blog$" \
  "$TEST_ROOT/list-offline.out" || fail "offline cache lost the static fallback URL: $(<"$TEST_ROOT/list-offline.out")"
grep -Eq "^/files[[:space:]]+files[[:space:]]+pc[[:space:]]+files.*offline/stale[[:space:]]+http://127\.0\.0\.11:$CONTROL_PORT/files$" \
  "$TEST_ROOT/list-offline.out" || fail "offline cache lost the private fallback URL: $(<"$TEST_ROOT/list-offline.out")"
grep -Eq "^/api[[:space:]]+CLI Proxy[[:space:]]+pc[[:space:]]+proxy.*offline/stale" \
  "$TEST_ROOT/list-offline.out" || fail "offline cache lost the display name: $(<"$TEST_ROOT/list-offline.out")"
grep -Fq -- "$ORIGIN_ID: unavailable" "$TEST_ROOT/list-offline.err" ||
  fail "offline service list omitted its host diagnostic: $(<"$TEST_ROOT/list-offline.err")"

start_origin "$TEST_ROOT/origin-restarted.log"
wait_for_private_body /blog/ SERVE_CLI_PRIVATE_MARKER ||
  fail "origin restart did not restore the static route"

"${CLI[@]}" unserve /blog --timeout 800ms >"$TEST_ROOT/unserve.out" 2>"$TEST_ROOT/unserve.err" ||
  fail "unserve private service: $(<"$TEST_ROOT/unserve.err")"
grep -Fq 'unserved /blog on pc' "$TEST_ROOT/unserve.out" ||
  fail "unserve output omitted the route owner: $(<"$TEST_ROOT/unserve.out")"
PRIVATE_WITHDRAWN_STATUS=$(curl --noproxy '*' --silent --max-time 2 --output /dev/null --write-out '%{http_code}' \
  "http://127.0.0.11:$CONTROL_PORT/blog/") || fail "query withdrawn private route"
[ "$PRIVATE_WITHDRAWN_STATUS" = 404 ] ||
  fail "unserve left the origin route reachable with status $PRIVATE_WITHDRAWN_STATUS"

echo "PASS: private serve CLI creates, lists offline, restores, removes, and rejects public publication"
