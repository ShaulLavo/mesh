#!/usr/bin/env bash
# An on-demand route starts its command on the first connection, shares it
# between clients, stops it once no connection has been open for the idle
# window, and keeps its listeners. A daemon restart keeps the session; a
# command that dies at once is reported to the connection that waited for it.
set -uo pipefail
export PYTHONDONTWRITEBYTECODE=1
export NO_COLOR=1

REPO_ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
SERVER="$REPO_ROOT/integration/helpers/demand_server.py"
CLIENT="$REPO_ROOT/integration/helpers/demand_client.py"
TEST_ROOT=$(mktemp -d)
MESH_INTEGRATION=${MESH_INTEGRATION_BINARY:-$TEST_ROOT/mesh-integration}
ORIGIN_STATE="$TEST_ROOT/origin-state"
ORIGIN_HOME="$TEST_ROOT/origin-home"
CLIENT_STATE="$TEST_ROOT/client-state"
CLIENT_CONFIG="$TEST_ROOT/client-config"
WORK="$TEST_ROOT/app"
ORIGIN_PID=""
HOLDER_PID=""
declare -a HELPERS=()

cleanup() {
  for pid in "$ORIGIN_PID" "$HOLDER_PID" "${HELPERS[@]}"; do
    [ -z "$pid" ] || kill -9 "$pid" 2>/dev/null || true
  done
  # The served command belongs to a detached worker, which outlives the
  # daemon by design; end it here so the test leaves nothing running.
  [ -s "$WORK/server.pid" ] && kill -9 "$(<"$WORK/server.pid")" 2>/dev/null
  rm -rf -- "$TEST_ROOT"
}
trap cleanup EXIT

fail() {
  echo "FAIL: $*" >&2
  [ -f "$TEST_ROOT/origin.log" ] && sed 's/^/  origin: /' "$TEST_ROOT/origin.log" >&2
  exit 1
}

identity_id() {
  ssh-keygen -y -f "$1" 2>/dev/null | python3 -c '
import base64, sys
blob = base64.b64decode(sys.stdin.read().split()[1])
print(base64.urlsafe_b64encode(blob[-32:]).decode().rstrip("="))
'
}

wait_until() {
  local tries=$1
  shift
  for _ in $(seq "$tries"); do
    "$@" && return 0
    sleep 0.1
  done
  return 1
}

tcp_accepts() {
  python3 - "$1" <<'PY' >/dev/null 2>&1
import socket, sys
with socket.create_connection(("127.0.0.1", int(sys.argv[1])), timeout=0.2):
    pass
PY
}

start_origin() {
  env HOME="$ORIGIN_HOME" MESH_STATE_DIR="$ORIGIN_STATE" MESH_FAKE_TAILSCALE_STATUS="$ORIGIN_STATUS" \
    PATH="$TEST_ROOT/bin:$PATH" \
    "$MESH_INTEGRATION" daemon --tailnet-port "$CONTROL_PORT" --websocket-path /mesh >>"$TEST_ROOT/origin.log" 2>&1 &
  ORIGIN_PID=$!
  wait_until 100 test -S "$ORIGIN_STATE/daemon.sock" || fail "origin daemon did not start"
  wait_until 100 tcp_accepts_on 127.0.0.11 "$CONTROL_PORT" || fail "origin Tailnet listener did not start"
}

tcp_accepts_on() {
  python3 - "$1" "$2" <<'PY' >/dev/null 2>&1
import socket, sys
with socket.create_connection((sys.argv[1], int(sys.argv[2])), timeout=0.2):
    pass
PY
}

serve_row() {
  "${CLI[@]}" serve ls 2>/dev/null | grep -E "^$1 " || true
}

route_in_state() {
  serve_row "$1" | grep -Fq "$2"
}

server_running() {
  [ -s "$WORK/server.pid" ] && kill -0 "$(<"$WORK/server.pid")" 2>/dev/null
}

server_stopped() { ! server_running; }

hold_connection() {
  local ready=$1
  shift
  rm -f "$ready"
  python3 "$CLIENT" "$@" "$ready" >/dev/null 2>&1 &
  HELPERS+=($!)
  wait_until 150 test -s "$ready" || fail "held connection $* was not answered"
  LAST_HELPER=$!
}

for tool in curl python3 ssh-keygen; do
  command -v "$tool" >/dev/null 2>&1 || fail "$tool is required"
done
mkdir -p "$TEST_ROOT/bin" "$ORIGIN_STATE" "$ORIGIN_HOME" "$CLIENT_STATE" "$CLIENT_CONFIG" "$WORK"
ln -s "$REPO_ROOT/integration/helpers/fake_tailscale" "$TEST_ROOT/bin/tailscale"
if [[ -z ${MESH_INTEGRATION_BINARY:-} ]]; then
  (cd "$REPO_ROOT" && go build -tags mesh_integration -o "$MESH_INTEGRATION" ./cmd/mesh) ||
    fail "build tagged Mesh binary"
fi
ssh-keygen -q -t ed25519 -N "" -C "" -f "$ORIGIN_STATE/identity.key" >/dev/null 2>&1 || fail "generate daemon identity"
rm -f "$ORIGIN_STATE/identity.key.pub"
ORIGIN_ID=$(identity_id "$ORIGIN_STATE/identity.key") || fail "derive origin identity"

mapfile -t PORTS < <(python3 - "$$" 9 <<'PY'
import socket, sys

# Each concurrent script searches its own band; binding confirms the port is
# free of anything else.
wanted = int(sys.argv[2])
base = 20000 + (int(sys.argv[1]) % 900) * 16
found, held = [], []
for offset in range(16 * 900):
    port = 20000 + (base - 20000 + offset) % (16 * 900)
    listener = socket.socket()
    try:
        listener.bind(("127.0.0.1", port))
    except OSError:
        listener.close()
        continue
    held.append(listener)
    found.append(port)
    if len(found) == wanted:
        break
for port in found:
    print(port)
for listener in held:
    listener.close()
PY
)
[ "${#PORTS[@]}" -eq 9 ] || fail "allocate fixture ports"
CONTROL_PORT=${PORTS[0]}
WEB=${PORTS[1]} WEB_UP=${PORTS[2]} API=${PORTS[3]} API_UP=${PORTS[4]}
BROKEN=${PORTS[5]} BROKEN_UP=${PORTS[6]} HELD=${PORTS[7]} HELD_UP=${PORTS[8]}

ORIGIN_STATUS="$TEST_ROOT/origin-status.json"
python3 - "$ORIGIN_STATUS" "$CLIENT_CONFIG/hosts.json" "$ORIGIN_ID" "$CONTROL_PORT" <<'PY'
import json, os, sys

status_path, hosts_path, origin_id, control_port = sys.argv[1:]
with open(status_path, "w", encoding="utf-8") as output:
    json.dump({
        "BackendState": "Running",
        "Self": {"DNSName": "pc.fixture.test.", "TailscaleIPs": ["127.0.0.11"], "Online": True},
    }, output)
with open(hosts_path, "w", encoding="utf-8") as output:
    json.dump({"version": 1, "hosts": [{
        "alias": "pc", "id": origin_id, "meshIdentity": origin_id, "tailscaleName": "pc.fixture.test",
        "addresses": ["127.0.0.11"], "endpoint": f"ws://127.0.0.11:{control_port}/mesh",
    }]}, output)
os.chmod(hosts_path, 0o600)
PY
CLI=(env "MESH_STATE_DIR=$CLIENT_STATE" "MESH_CONFIG_DIR=$CLIENT_CONFIG" NO_COLOR=1 "$MESH_INTEGRATION")

start_origin

# --- Declare the route. Nothing runs until something connects. -------------
# pc is another machine to this client, so the directory is given, not
# inferred from where the client happens to be.
"${CLI[@]}" serve pc "$WEB" --at /dev \
  --run "exec python3 $SERVER $WEB_UP $API_UP 0.4" --cwd "$WORK" --env FAKE_MARK=recipe-env \
  --listen "$WEB=$WEB_UP" --listen "$API=$API_UP" --idle 1s --ready-timeout 10s \
  >"$TEST_ROOT/serve.out" 2>&1 || fail "declare on-demand route: $(<"$TEST_ROOT/serve.out")"
grep -Fq 'on the first connection' "$TEST_ROOT/serve.out" || fail "serve did not describe the recipe: $(<"$TEST_ROOT/serve.out")"
route_in_state /dev stopped || fail "a new route is not stopped: $(serve_row /dev)"
server_stopped || fail "declaring the route started its command"
tcp_accepts "$WEB" || fail "listener $WEB is not bound"

# --- The first request starts the session, waits for readiness, succeeds. --
body=$(curl --noproxy '*' --silent --max-time 12 "http://127.0.0.1:$WEB/first") || fail "first request failed"
case $body in
  "FAKE_OK port=$WEB_UP path=/first mark=recipe-env cwd=$WORK"*) ;;
  *) fail "first request got: $body" ;;
esac
first_server=$(<"$WORK/server.pid")
"${CLI[@]}" ls >"$TEST_ROOT/ls.out" 2>&1 || fail "mesh ls: $(<"$TEST_ROOT/ls.out")"
grep -Fq 'serve /dev' "$TEST_ROOT/ls.out" || fail "mesh ls does not show the labelled session: $(<"$TEST_ROOT/ls.out")"
api=$(curl --noproxy '*' --silent --max-time 5 "http://127.0.0.1:$API/api") || fail "second listener failed"
[[ $api == "FAKE_OK port=$API_UP path=/api"* ]] || fail "second listener got: $api"
[ "$(<"$WORK/server.pid")" = "$first_server" ] || fail "the second listener started a second server"

# --- Two clients; one closes and the session stays. Both close: it stops. --
hold_connection "$TEST_ROOT/a.ready" http 127.0.0.1 "$WEB"
first_client=$LAST_HELPER
hold_connection "$TEST_ROOT/b.ready" http 127.0.0.1 "$API"
second_client=$LAST_HELPER
wait_until 30 route_in_state /dev 'running (2 conns)' || fail "two held connections not counted: $(serve_row /dev)"
kill "$first_client"
wait_until 30 route_in_state /dev 'running (1 conn)' || fail "closing one client not counted: $(serve_row /dev)"
sleep 1.6
server_running || fail "the session stopped while a connection was open"
kill "$second_client"
wait_until 50 server_stopped || fail "the session outlived the idle window with no connections"
wait_until 30 route_in_state /dev stopped || fail "an idle route is not stopped: $(serve_row /dev)"
serve_row /dev | grep -Fq ' stopped ' || fail "a stopped route should not read unhealthy: $(serve_row /dev)"

# --- A WebSocket through the tailnet path keeps it running past the window. -
hold_connection "$TEST_ROOT/ws.ready" websocket 127.0.0.11 "$CONTROL_PORT" /dev/ws
websocket=$LAST_HELPER
grep -Fq ' 101 ' "$TEST_ROOT/ws.ready" || fail "tailnet WebSocket was not upgraded: $(<"$TEST_ROOT/ws.ready")"
sleep 2
server_running || fail "the session stopped under an open WebSocket"
route_in_state /dev 'running (1 conn)' || fail "the WebSocket is not counted: $(serve_row /dev)"
kill "$websocket"
wait_until 50 server_stopped || fail "the session did not stop after the WebSocket closed"
tcp_accepts "$WEB" && tcp_accepts "$API" || fail "listeners closed when the route stopped"

# --- A daemon restart keeps the session and restarts the idle clock. -------
hold_connection "$TEST_ROOT/c.ready" http 127.0.0.1 "$WEB"
held=$LAST_HELPER
running_server=$(<"$WORK/server.pid")
kill -9 "$ORIGIN_PID"
wait "$ORIGIN_PID" 2>/dev/null
kill "$held"
server_running || fail "the daemon's death took the session with it"
start_origin
restarted=$(date +%s.%N)
body=$(curl --noproxy '*' --silent --max-time 5 "http://127.0.0.1:$WEB/again") || fail "request after restart failed"
[[ $body == "FAKE_OK port=$WEB_UP path=/again"* ]] || fail "request after restart got: $body"
[ "$(<"$WORK/server.pid")" = "$running_server" ] || fail "the restarted daemon started a new server instead of adopting it"
wait_until 60 server_stopped || fail "the adopted session never idled out"
elapsed=$(python3 -c "import sys, time; print(time.time() - float(sys.argv[1]))" "$restarted")
python3 -c "import sys; sys.exit(float(sys.argv[1]) < 0.9)" "$elapsed" ||
  fail "the adopted session stopped ${elapsed}s after the restart, inside the idle window"

# --- A command that exits at once is a 502 naming its status and output. ---
# No TARGET and no --at: this route exists only on its listener.
"${CLI[@]}" serve pc --run 'echo BROKEN_OUTPUT_MARKER; exit 7' --cwd "$WORK" \
  --listen "$BROKEN=$BROKEN_UP" --idle 1s --ready-timeout 5s >"$TEST_ROOT/broken.out" 2>&1 ||
  fail "declare failing route: $(<"$TEST_ROOT/broken.out")"
status=$(curl --noproxy '*' --silent --max-time 10 --output "$TEST_ROOT/broken.body" --write-out '%{http_code}' \
  "http://127.0.0.1:$BROKEN/") || true
[ "$status" = 502 ] || fail "failed start answered $status: $(cat "$TEST_ROOT/broken.body" 2>/dev/null)"
grep -Fq "route :$BROKEN" "$TEST_ROOT/broken.body" || fail "502 does not name the route: $(<"$TEST_ROOT/broken.body")"
grep -Fq 'status 7' "$TEST_ROOT/broken.body" || fail "502 does not name the exit status: $(<"$TEST_ROOT/broken.body")"
grep -Fq BROKEN_OUTPUT_MARKER "$TEST_ROOT/broken.body" || fail "502 does not carry the output: $(<"$TEST_ROOT/broken.body")"
route_in_state ":$BROKEN" failed || fail "failed route is not failed: $(serve_row ":$BROKEN")"

# --- A --listen port someone else holds is refused, naming the holder. -----
python3 -c '
import socket, sys, time
listener = socket.socket()
listener.bind(("127.0.0.1", int(sys.argv[1])))
listener.listen()
time.sleep(60)
' "$HELD" &
HOLDER_PID=$!
wait_until 50 tcp_accepts "$HELD" || fail "port holder did not start"
if "${CLI[@]}" serve pc "$HELD" --at /held --run true --cwd "$WORK" --listen "$HELD=$HELD_UP" >"$TEST_ROOT/held.out" 2>&1; then
  fail "a held --listen port was accepted"
fi
grep -Fq "port $HELD is held by pid $HOLDER_PID" "$TEST_ROOT/held.out" || fail "refusal does not name the holder: $(<"$TEST_ROOT/held.out")"
serve_row /held | grep -q . && fail "the refused route was stored anyway"

# --- Another machine's --run needs its own directory named. ----------------
if "${CLI[@]}" serve pc "$HELD_UP" --at /nocwd --run true >"$TEST_ROOT/nocwd.out" 2>&1; then
  fail "a remote --run without --cwd used this client's directory"
fi
grep -Fq -- '--cwd' "$TEST_ROOT/nocwd.out" || fail "refusal does not ask for --cwd: $(<"$TEST_ROOT/nocwd.out")"

# --- mesh serve start/stop, then unserve releases the ports. ---------------
"${CLI[@]}" serve start /dev >"$TEST_ROOT/start.out" 2>&1 || fail "serve start: $(<"$TEST_ROOT/start.out")"
grep -Fq 'running' "$TEST_ROOT/start.out" || fail "serve start did not report running: $(<"$TEST_ROOT/start.out")"
server_running || fail "serve start did not start the command"
"${CLI[@]}" serve stop /dev >"$TEST_ROOT/stop.out" 2>&1 || fail "serve stop: $(<"$TEST_ROOT/stop.out")"
server_stopped || fail "serve stop did not stop the command"
curl --noproxy '*' --silent --max-time 5 "http://127.0.0.1:$WEB/" >/dev/null || fail "stopped route did not start again"
"${CLI[@]}" unserve /dev >"$TEST_ROOT/unserve.out" 2>&1 || fail "unserve: $(<"$TEST_ROOT/unserve.out")"
wait_until 50 server_stopped || fail "unserve left the command running"
tcp_accepts "$WEB" && fail "unserve left listener $WEB bound"
"${CLI[@]}" unserve ":$BROKEN" >"$TEST_ROOT/unserve-broken.out" 2>&1 || fail "unserve :PORT: $(<"$TEST_ROOT/unserve-broken.out")"
echo "PASS: serve on demand"
