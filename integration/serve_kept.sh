#!/usr/bin/env bash
source "$(dirname -- "${BASH_SOURCE[0]}")/helpers/isolate.sh" || exit 1
source "$(dirname -- "${BASH_SOURCE[0]}")/helpers/session_cleanup.sh" || exit 1
# A --keep-running route starts its command without traffic, serves clients
# that use the upstream port directly, restarts the command after it exits,
# stays stopped after mesh serve stop until mesh serve start, and is adopted
# by a restarted daemon. Its ended sessions stay listed. A finite route's idle
# stop names the exact session and why in the daemon log.
set -uo pipefail
export PYTHONDONTWRITEBYTECODE=1
export NO_COLOR=1

REPO_ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
SERVER="$REPO_ROOT/integration/helpers/demand_server.py"
TEST_ROOT=$(mktemp -d)
MESH_INTEGRATION=${MESH_INTEGRATION_BINARY:-$TEST_ROOT/mesh-integration}
ORIGIN_STATE="$TEST_ROOT/origin-state"
ORIGIN_HOME="$TEST_ROOT/origin-home"
CLIENT_STATE="$TEST_ROOT/client-state"
CLIENT_CONFIG="$TEST_ROOT/client-config"
KEPT="$TEST_ROOT/kept"
FINITE="$TEST_ROOT/finite"
ORIGIN_PID=""

cleanup() {
  local status=$?
  trap - EXIT
  trap '' INT TERM
  if [[ -n $ORIGIN_PID ]]; then
    kill -9 "$ORIGIN_PID" 2>/dev/null || true
    wait "$ORIGIN_PID" 2>/dev/null || true
  fi
  if ! python3 -B "$REPO_ROOT/integration/helpers/served_cleanup.py" "$TEST_ROOT" "$MESH_INTEGRATION"; then
    printf 'FAIL: served fixture cleanup retained %s\n' "$TEST_ROOT" >&2
    (( status != 0 )) || status=1
    exit "$status"
  fi
  MESH=$MESH_INTEGRATION
  finish_fixture_cleanup "$ORIGIN_STATE" "$TEST_ROOT" "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

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

tcp_accepts_on() {
  python3 - "$1" "$2" <<'PY' >/dev/null 2>&1
import socket, sys
with socket.create_connection((sys.argv[1], int(sys.argv[2])), timeout=0.2):
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

serve_row() {
  "${CLI[@]}" serve ls 2>/dev/null | grep -E "^$1 " || true
}

route_in_state() {
  serve_row "$1" | grep -Fq "$2"
}

server_pid() { [ -s "$1/server.pid" ] && cat "$1/server.pid"; }
server_running() { [ -s "$1/server.pid" ] && kill -0 "$(<"$1/server.pid")" 2>/dev/null; }
server_stopped() { ! server_running "$1"; }
server_replaced() { server_running "$1" && [ "$(server_pid "$1")" != "$2" ]; }
direct_answers() { curl --noproxy '*' --silent --max-time 2 "http://127.0.0.1:$1/$2" | grep -Fq "FAKE_OK port=$1 path=/$2"; }

# logged_session prints the session in the first origin log line matching
# the pattern, whose one group is the session ID.
logged_session() {
  sed -n -E "s#.*daemon: $1.*#\\1#p" "$TEST_ROOT/origin.log" | head -n 1
}

# await_session is logged_session once that line is written: a transition
# logs just after the state it reports becomes visible.
await_session() {
  local id
  for _ in $(seq 100); do
    id=$(logged_session "$1")
    [ -n "$id" ] && { echo "$id"; return 0; }
    sleep 0.1
  done
  return 1
}

logged() { grep -Fq "$1" "$TEST_ROOT/origin.log"; }

for tool in curl python3 ssh-keygen; do
  command -v "$tool" >/dev/null 2>&1 || fail "$tool is required"
done
mkdir -p "$TEST_ROOT/bin" "$ORIGIN_STATE" "$ORIGIN_HOME" "$CLIENT_STATE" "$CLIENT_CONFIG" "$KEPT" "$FINITE"
ln -s "$REPO_ROOT/integration/helpers/fake_tailscale" "$TEST_ROOT/bin/tailscale"
if [[ -z ${MESH_INTEGRATION_BINARY:-} ]]; then
  (cd "$REPO_ROOT" && go build -tags mesh_integration -o "$MESH_INTEGRATION" ./cmd/mesh) ||
    fail "build tagged Mesh binary"
fi
ssh-keygen -q -t ed25519 -N "" -C "" -f "$ORIGIN_STATE/identity.key" >/dev/null 2>&1 || fail "generate daemon identity"
rm -f "$ORIGIN_STATE/identity.key.pub"
ORIGIN_ID=$(identity_id "$ORIGIN_STATE/identity.key") || fail "derive origin identity"

CLIENT_ID=$(env MESH_STATE_DIR="$CLIENT_STATE" "$MESH_INTEGRATION" device identity --json | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])') || fail "create fixture client"
env MESH_STATE_DIR="$ORIGIN_STATE" "$MESH_INTEGRATION" device approve --allow-root -- "$CLIENT_ID" >/dev/null || fail "approve fixture client"
read -ra PORTS <<<"${MESH_INTEGRATION_PORTS:?fixture owner did not reserve ports}"
[ "${#PORTS[@]}" -eq 5 ] || fail "allocate fixture ports"
CONTROL_PORT=${PORTS[0]}
KEEP=${PORTS[1]} KEEP_UP=${PORTS[2]} IDLE=${PORTS[3]} IDLE_UP=${PORTS[4]}

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
        "id": origin_id, "meshIdentity": origin_id, "tailscaleName": "pc.fixture.test",
        "addresses": ["127.0.0.11"], "endpoint": f"ws://127.0.0.11:{control_port}/mesh",
    }]}, output)
os.chmod(hosts_path, 0o600)
PY
CLI=(env "MESH_STATE_DIR=$CLIENT_STATE" "MESH_CONFIG_DIR=$CLIENT_CONFIG" NO_COLOR=1 "$MESH_INTEGRATION")

start_origin
"${CLI[@]}" ls --all --timeout 1s >"$TEST_ROOT/name-adoption.out" 2>&1 || fail "adopt origin declaration"

# --- A kept route starts at once, with no traffic at all. ------------------
"${CLI[@]}" serve pc "$KEEP" --at /keep --run "exec python3 $SERVER $KEEP_UP 0.2" --cwd "$KEPT" \
  --listen "$KEEP=$KEEP_UP" --keep-running --ready-timeout 10s >"$TEST_ROOT/serve.out" 2>&1 ||
  fail "declare kept route: $(<"$TEST_ROOT/serve.out")"
grep -Fq 'now and keeps it running' "$TEST_ROOT/serve.out" || fail "serve did not confirm the kept lifetime: $(<"$TEST_ROOT/serve.out")"
wait_until 100 server_running "$KEPT" || fail "a kept route did not start without traffic"
wait_until 100 direct_answers "$KEEP_UP" direct || fail "a direct client of the upstream port got no answer"
wait_until 50 route_in_state /keep running || fail "kept route is not running: $(serve_row /keep)"
first=$(await_session 'route /keep: started session ([0-9A-Z]{4}) \(kept route startup\)')
[ -n "$first" ] || fail "the kept start was not logged"

# --- The command exits; the route restarts it after its delay. --------------
first_pid=$(server_pid "$KEPT")
kill -9 "$first_pid"
wait_until 100 server_replaced "$KEPT" "$first_pid" || fail "the kept route did not restart its command"
wait_until 100 direct_answers "$KEEP_UP" restarted || fail "the restarted command does not answer directly"
grep -Eq "daemon: route /keep: session $first ended \(.+\); restarting in 1s$" "$TEST_ROOT/origin.log" ||
  fail "the exit of $first was not logged with its restart delay"
second=$(await_session 'route /keep: started session ([0-9A-Z]{4}) \(kept route restart\)')
[ -n "$second" ] && [ "$second" != "$first" ] || fail "the restart was not logged as a new session"

# --- mesh serve stop holds it; only mesh serve start runs it again. --------
"${CLI[@]}" serve stop /keep >"$TEST_ROOT/stop.out" 2>&1 || fail "serve stop: $(<"$TEST_ROOT/stop.out")"
wait_until 50 server_stopped "$KEPT" || fail "serve stop left the command running"
wait_until 30 route_in_state /keep 'stopped (held)' || fail "a stopped kept route is not held: $(serve_row /keep)"
wait_until 50 logged "route /keep: stopped session $second (mesh serve stop)" || fail "the manual stop of $second was not logged"
status=$(curl --noproxy '*' --silent --max-time 5 --output /dev/null --write-out '%{http_code}' "http://127.0.0.1:$KEEP/") || true
[ "$status" = 502 ] || fail "a request to a held route answered $status"
sleep 3
server_stopped "$KEPT" || fail "a held route started again on its own or for a request"
"${CLI[@]}" serve start /keep >"$TEST_ROOT/start.out" 2>&1 || fail "serve start: $(<"$TEST_ROOT/start.out")"
wait_until 100 server_running "$KEPT" || fail "serve start did not start the held route"
wait_until 50 route_in_state /keep running || fail "started route is not running: $(serve_row /keep)"

# --- A replaced daemon adopts the same process without starting another. ---
adopted_pid=$(server_pid "$KEPT")
kill -9 "$ORIGIN_PID"
wait "$ORIGIN_PID" 2>/dev/null
server_running "$KEPT" || fail "the daemon's death took the kept command with it"
start_origin
wait_until 100 route_in_state /keep running || fail "the restarted daemon lost the kept route: $(serve_row /keep)"
sleep 2
[ "$(server_pid "$KEPT")" = "$adopted_pid" ] || fail "the restarted daemon started a second command instead of adopting"
wait_until 50 logged "from an earlier daemon" || fail "the adoption was not logged"
direct_answers "$KEEP_UP" adopted || fail "the adopted command does not answer directly"

# --- Earlier ended sessions stay listed: they explain what happened. -------
"${CLI[@]}" ls --all --timeout 2s >"$TEST_ROOT/history.out" 2>&1 || fail "mesh ls --all: $(<"$TEST_ROOT/history.out")"
for id in "$first" "$second"; do
  grep -Fwq "$id" "$TEST_ROOT/history.out" || fail "ended session $id was erased: $(<"$TEST_ROOT/history.out")"
done

# --- A finite route's idle stop names the exact session and the reason. ----
"${CLI[@]}" serve pc "$IDLE" --at /idle --run "exec python3 $SERVER $IDLE_UP 0.2" --cwd "$FINITE" \
  --listen "$IDLE=$IDLE_UP" --idle 1s --ready-timeout 10s >"$TEST_ROOT/finite.out" 2>&1 ||
  fail "declare finite route: $(<"$TEST_ROOT/finite.out")"
grep -Fq 'on the first connection through Mesh' "$TEST_ROOT/finite.out" || fail "serve did not confirm the finite lifetime: $(<"$TEST_ROOT/finite.out")"
curl --noproxy '*' --silent --max-time 12 "http://127.0.0.1:$IDLE/first" | grep -Fq FAKE_OK || fail "first finite request failed"
idled=$(await_session 'route /idle: started session ([0-9A-Z]{4}) \(a connection to listener port [0-9]+\)')
[ -n "$idled" ] || fail "the finite start was not logged"
wait_until 100 server_stopped "$FINITE" || fail "the finite route did not stop when idle"
curl --noproxy '*' --silent --max-time 12 "http://127.0.0.1:$IDLE/second" | grep -Fq FAKE_OK || fail "second finite request failed"
replacement=$(sed -n -E 's#.*daemon: route /idle: started session ([0-9A-Z]{4}) .*#\1#p' "$TEST_ROOT/origin.log" | tail -n 1)
[ -n "$replacement" ] && [ "$replacement" != "$idled" ] || fail "the finite route did not start a replacement session"
wait_until 50 logged "route /idle: stopped session $idled (idle for 1s with no Mesh connections)" ||
  fail "the idle stop of $idled was not logged with its reason"
echo "PASS: kept services"
