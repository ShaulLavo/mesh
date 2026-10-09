#!/usr/bin/env bash
source "$(dirname -- "${BASH_SOURCE[0]}")/helpers/isolate.sh" || exit 1
set -uo pipefail
export PYTHONDONTWRITEBYTECODE=1
source "$(dirname -- "${BASH_SOURCE[0]}")/helpers/private_app_fixture.sh" || exit 1
if [[ $(uname -s) != Linux ]]; then
  echo "SKIP: private app PROXY ingress requires Linux socket UID authentication"
  exit 0
fi

REPO_ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
TEST_ROOT=$(mktemp -d "${TMPDIR:-/tmp}/au.XXXXXX")
EDGE_STATE="$TEST_ROOT/e"
ORIGIN_STATE="$TEST_ROOT/o"
WORKLOAD="$TEST_ROOT/w"
SOURCE="$TEST_ROOT/src"
MESH_APP=${MESH_INTEGRATION_BINARY:-$TEST_ROOT/mesh}
EDGE_PID=""
ORIGIN_PID=""
APP_ID=""
UPDATE_CLIENT=""

cleanup() {
  if [ -n "$APP_ID" ] && [ -n "$ORIGIN_PID" ]; then
    MESH_STATE_DIR="$ORIGIN_STATE" timeout 5s "$MESH_APP" app delete local "$APP_ID" >/dev/null 2>&1 || true
  fi
  for pid in "$UPDATE_CLIENT" "$ORIGIN_PID" "$EDGE_PID"; do
    [ -z "$pid" ] || kill -TERM "$pid" 2>/dev/null || true
  done
  for pid in "$ORIGIN_PID" "$EDGE_PID"; do
    [ -z "$pid" ] || wait "$pid" 2>/dev/null || true
  done
  python3 - "$ORIGIN_STATE/s" <<'PY'
import glob, json, os, signal, sys
for path in glob.glob(sys.argv[1] + '/*/meta.json'):
    try:
        with open(path) as source:
            meta = json.load(source)
        pid = meta.get('pid', 0)
        if meta.get('label', '').startswith(('app ', 'app-setup ')) and pid > 1 and os.getpgid(pid) == pid:
            os.killpg(pid, signal.SIGTERM)
    except (OSError, ValueError):
        pass
PY
  rm -rf -- "$TEST_ROOT"
}
trap cleanup EXIT

fail() {
  echo "FAIL: $*" >&2
  for log in "$TEST_ROOT/edge.log" "$TEST_ROOT/origin.log"; do
    [ ! -f "$log" ] || tail -20 "$log" >&2
  done
  exit 1
}

mkdir -p "$TEST_ROOT/bin" "$EDGE_STATE" "$ORIGIN_STATE" "$SOURCE"
ln -s "$REPO_ROOT/integration/helpers/fake_tailscale" "$TEST_ROOT/bin/tailscale"
if [ -z "${MESH_INTEGRATION_BINARY:-}" ]; then
  (cd "$REPO_ROOT" && go build -tags mesh_integration -o "$MESH_APP" ./cmd/mesh) || fail 'build integration Mesh'
fi
(cd "$REPO_ROOT" && go build -o "$SOURCE/server" ./integration/helpers/temporary_app_server.go) || fail 'build HTTP server fixture'

for state in "$EDGE_STATE" "$ORIGIN_STATE"; do
  ssh-keygen -q -t ed25519 -N '' -C '' -f "$state/identity.key" || fail 'create fixture identity'
done
cat "$ORIGIN_STATE/identity.key.pub" >"$EDGE_STATE/authorized_keys"
cat "$EDGE_STATE/identity.key.pub" >"$ORIGIN_STATE/authorized_keys"
chmod 0600 "$EDGE_STATE/authorized_keys" "$ORIGIN_STATE/authorized_keys"

private_app_fixture configure "$TEST_ROOT" "$$" || fail 'fixture configuration'
mapfile -t PORTS <"$TEST_ROOT/ports"
CONTROL_PORT=${PORTS[0]}
PROXY_PORT=${PORTS[1]}
BACKEND_PORT=${PORTS[2]}

wait_for_socket() {
  local pid=$1 path=$2
  for _ in $(seq 100); do
    kill -0 "$pid" 2>/dev/null || return 1
    [ -S "$path" ] && return 0
    sleep 0.05
  done
  return 1
}

start_origin() {
  env MESH_STATE_DIR="$ORIGIN_STATE" MESH_FAKE_TAILSCALE_STATUS="$TEST_ROOT/o-status.json" PATH="$TEST_ROOT/bin:$PATH" \
    "$MESH_APP" daemon --tailnet-port "$CONTROL_PORT" --public-edge-target "$TEST_ROOT/target.json" --app-data-root "$WORKLOAD" >"$TEST_ROOT/origin.log" 2>&1 &
  ORIGIN_PID=$!
  wait_for_socket "$ORIGIN_PID" "$ORIGIN_STATE/daemon.sock" || fail 'origin startup'
}

start_edge() {
  env MESH_STATE_DIR="$EDGE_STATE" MESH_FAKE_TAILSCALE_STATUS="$TEST_ROOT/e-status.json" PATH="$TEST_ROOT/bin:$PATH" \
    "$MESH_APP" daemon --tailnet-port "$CONTROL_PORT" --edge "$TEST_ROOT/edge.json" >>"$TEST_ROOT/edge.log" 2>&1 &
  EDGE_PID=$!
  wait_for_socket "$EDGE_PID" "$EDGE_STATE/daemon.sock" || fail 'edge startup'
  private_app_fixture install "$TEST_ROOT" || fail 'install private app HTTPS certificate'
}

start_edge
start_origin

json_field() {
  python3 -c 'import json, sys; print(json.load(open(sys.argv[1]))[sys.argv[2]])' "$@"
}
# setup_pid prints the PID of the live setup worker for the app, if any.
setup_pid() {
  python3 - "$ORIGIN_STATE/s" "app-setup $APP_ID" <<'PY'
import glob, json, os, sys
for path in glob.glob(sys.argv[1] + '/*/meta.json'):
    try:
        with open(path) as source:
            meta = json.load(source)
        if meta.get('label') == sys.argv[2] and meta.get('pid', 0) > 1:
            os.kill(meta['pid'], 0)
            print(meta['pid'])
    except (OSError, ValueError):
        pass
PY
}
workspaces() {
  find "$WORKLOAD/apps/$APP_ID" -mindepth 1 -maxdepth 1 -type d -name 'source-*' | wc -l
}

MESH_STATE_DIR="$ORIGIN_STATE" "$MESH_APP" app create local "$SOURCE" --run './server' --port "$BACKEND_PORT" --json >"$TEST_ROOT/create.json" || fail 'create HTTP app'
APP_ID=$(python3 -c 'import json, sys; print(json.load(open(sys.argv[1]))["app"]["id"])' "$TEST_ROOT/create.json") || fail 'creation result'
APP_ENDPOINT="$(app_endpoint)"
private_app_denies_outsiders || fail 'private app owner authorization'
app_curl --fail "$APP_ENDPOINT/api" >"$TEST_ROOT/before.json" || fail 'app before update'
OLD_PID=$(json_field "$TEST_ROOT/before.json" pid)
OLD_CWD=$(json_field "$TEST_ROOT/before.json" cwd)

MESH_STATE_DIR="$ORIGIN_STATE" "$MESH_APP" app update local "$APP_ID" "$SOURCE" --run './server' --port "$BACKEND_PORT" --setup 'sleep 60' \
  >/dev/null 2>"$TEST_ROOT/interrupted.err" &
UPDATE_CLIENT=$!
SETUP_PID=""
for _ in $(seq 100); do
  SETUP_PID=$(setup_pid)
  [ -n "$SETUP_PID" ] && break
  sleep 0.05
done
[ -n "$SETUP_PID" ] || fail 'update setup did not start'
[ "$(workspaces)" = 2 ] || fail 'update setup started without its candidate workspace'

kill -KILL "$ORIGIN_PID"
wait "$ORIGIN_PID" 2>/dev/null
ORIGIN_PID=""
kill -0 "$OLD_PID" 2>/dev/null || fail 'origin crash killed the serving app worker'
kill -0 "$SETUP_PID" 2>/dev/null || fail 'origin crash ended the setup worker before recovery could'
start_origin
wait "$UPDATE_CLIENT" 2>/dev/null && fail 'update reported success although its daemon crashed'
UPDATE_CLIENT=""

for _ in $(seq 100); do
  ! kill -0 "$SETUP_PID" 2>/dev/null && [ "$(workspaces)" = 1 ] && break
  sleep 0.1
done
kill -0 "$SETUP_PID" 2>/dev/null && fail 'recovery left the interrupted setup worker running'
[ "$(workspaces)" = 1 ] || fail 'recovery left the candidate workspace'
app_curl --fail "$APP_ENDPOINT/api" >"$TEST_ROOT/recovered.json" || fail 'app after recovery'
[ "$(json_field "$TEST_ROOT/recovered.json" pid)" = "$OLD_PID" ] || fail 'recovery replaced the serving app worker'
[ "$(json_field "$TEST_ROOT/recovered.json" cwd)" = "$OLD_CWD" ] || fail 'recovery switched workspaces'

MESH_STATE_DIR="$ORIGIN_STATE" "$MESH_APP" app update local "$APP_ID" "$SOURCE" --run './server' --port "$BACKEND_PORT" --setup 'printf updated >setup.txt' \
  --json >"$TEST_ROOT/updated.json" 2>"$TEST_ROOT/updated.err" || fail "update after recovery: $(cat "$TEST_ROOT/updated.err")"
app_curl --fail "$APP_ENDPOINT/api" >"$TEST_ROOT/after.json" || fail 'app after update'
NEW_PID=$(json_field "$TEST_ROOT/after.json" pid)
NEW_CWD=$(json_field "$TEST_ROOT/after.json" cwd)
[ "$NEW_PID" != "$OLD_PID" ] && [ "$NEW_CWD" != "$OLD_CWD" ] || fail 'update did not replace the app worker'
[ "$(cat "$NEW_CWD/setup.txt" 2>/dev/null)" = updated ] || fail 'update served without its setup'
kill -0 "$OLD_PID" 2>/dev/null && fail 'previous app worker survived its replacement'
[ ! -e "$OLD_CWD" ] || fail 'previous workspace survived the update'
[ "$(workspaces)" = 1 ] || fail 'update left extra workspaces'

MESH_STATE_DIR="$ORIGIN_STATE" "$MESH_APP" app delete local "$APP_ID" --json >/dev/null || fail 'delete app'
APP_ID=""
echo 'PASS: an update interrupted by an origin crash during setup is rolled back on restart (setup worker stopped, candidate removed, previous worker still serving), and the re-run update replaces the worker once'
