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
TEST_ROOT=$(mktemp -d "${TMPDIR:-/tmp}/as.XXXXXX")
EDGE_STATE="$TEST_ROOT/e"
ORIGIN_STATE="$TEST_ROOT/o"
WORKLOAD="$TEST_ROOT/w"
SOURCE="$TEST_ROOT/src"
MESH_APP=${MESH_INTEGRATION_BINARY:-$TEST_ROOT/mesh}
EDGE_PID=""
ORIGIN_PID=""
APP_ID=""
ORDINARY_PID=""

cleanup() {
  if [ -n "$APP_ID" ] && [ -n "$ORIGIN_PID" ]; then
    MESH_STATE_DIR="$ORIGIN_STATE" timeout 5s "$MESH_APP" app delete local "$APP_ID" >/dev/null 2>&1 || true
  fi
  for pid in "$ORIGIN_PID" "$EDGE_PID" "$ORDINARY_PID"; do
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

MESH_STATE_DIR="$ORIGIN_STATE" "$MESH_APP" app create local "$SOURCE" --run './server' --setup 'printf setup-complete >setup.txt' --port "$BACKEND_PORT" --json >"$TEST_ROOT/create.json" || fail 'create HTTP app'
APP_ID=$(python3 - "$TEST_ROOT/create.json" <<'PY'
import json, sys
with open(sys.argv[1]) as source:
    app = json.load(source)['app']
if 'visibility' in app or app['kind'] != 'server' or not app['ready']:
    raise SystemExit(f'invalid new app: {app}')
print(app['id'])
PY
) || fail 'private creation result'
MESH_STATE_DIR="$ORIGIN_STATE" "$MESH_APP" app download local "$APP_ID" "$TEST_ROOT/source.tar.gz" --json >/dev/null || fail 'download app source'
tar -tzf "$TEST_ROOT/source.tar.gz" >"$TEST_ROOT/source.list" || fail 'downloaded archive is not a tarball'
grep -qx server "$TEST_ROOT/source.list" || fail 'downloaded archive misses the app source'
[ -z "$(find "$WORKLOAD/apps/$APP_ID" -mindepth 1 -maxdepth 1 ! -name 'source-*')" ] || fail 'download left an archive in the app directory'

APP_ENDPOINT="$(app_endpoint)"
private_app_denies_outsiders || fail 'private app owner authorization'
private_app_rejects_sharing_commands || fail 'removed sharing commands'
python3 "$REPO_ROOT/integration/helpers/mesh_control.py" --expect-type error upsert \
  "$ORIGIN_STATE/daemon.sock" alias proxy "$BACKEND_PORT" alias.mesh.test >"$TEST_ROOT/alias.out" || fail 'app port alias refusal'
grep -Eq 'app-owned port' "$TEST_ROOT/alias.out" || fail 'ordinary public proxy bypassed private app gate'
python3 "$REPO_ROOT/integration/helpers/mesh_control.py" --expect-type error upsert \
  "$ORIGIN_STATE/daemon.sock" source-alias files "$WORKLOAD" '' >"$TEST_ROOT/source-alias.out" || fail 'app source alias refusal'
grep -Eq 'managed app directories' "$TEST_ROOT/source-alias.out" || fail 'ordinary file route exposed private app source'
app_curl --fail "$APP_ENDPOINT/" >"$TEST_ROOT/index.html" || fail 'private HTML'
grep -Eq 'APP_LABELLED_WORKER' "$TEST_ROOT/index.html" || fail 'server HTML missing'
if grep -Eq '/.mesh-app/' "$TEST_ROOT/index.html"; then fail 'Mesh injected a widget into app HTML'; fi
app_curl --fail "$APP_ENDPOINT/api" >"$TEST_ROOT/api.json" || fail 'private API'
app_curl --fail "$APP_ENDPOINT/mesh" >"$TEST_ROOT/mesh.json" || fail 'whole-host /mesh API'
app_curl --fail --location "$APP_ENDPOINT/redirect" >"$TEST_ROOT/redirect.json" || fail 'root-relative redirect'
private_app_fixture websocket "$TEST_ROOT" "$PROXY_PORT" "$APP_ID" >"$TEST_ROOT/socket.out" || fail 'HTTP app WebSocket'

python3 - "$TEST_ROOT" "$WORKLOAD" <<'PY'
import json, os, sys
root, workload = sys.argv[1:]
with open(root + '/api.json') as source:
    api = json.load(source)
with open(root + '/mesh.json') as source:
    mesh = json.load(source)
with open(root + '/redirect.json') as source:
    redirected = json.load(source)
if api['pid'] != mesh['pid'] or api['pid'] != redirected['pid'] or mesh['path'] != '/mesh':
    raise SystemExit('paths or redirect reached a different worker')
if not api['cwd'].startswith(workload + '/apps/'):
    raise SystemExit('server is outside managed workload')
with open(api['cwd'] + '/setup.txt') as source:
    if source.read() != 'setup-complete':
        raise SystemExit('setup worker did not complete')
PY
[ $? -eq 0 ] || fail 'server process/workspace validation'

kill -TERM "$ORIGIN_PID"
wait "$ORIGIN_PID" || fail 'origin shutdown'
ORIGIN_PID=""
start_origin
# The control socket opens before app maintenance renews the retained worker.
app_curl --fail --retry 5 --retry-delay 1 --retry-max-time 5 "$APP_ENDPOINT/api" >"$TEST_ROOT/restarted.json" || fail 'app after origin restart'
python3 - "$TEST_ROOT/api.json" "$TEST_ROOT/restarted.json" <<'PY'
import json, sys
with open(sys.argv[1]) as source:
    original = json.load(source)
with open(sys.argv[2]) as source:
    restarted = json.load(source)
if original['pid'] != restarted['pid'] or original['cwd'] != restarted['cwd']:
    raise SystemExit('daemon restart replaced the running app worker')
PY
[ $? -eq 0 ] || fail 'running worker adoption'

json_field() {
  python3 -c 'import json, sys; print(json.load(open(sys.argv[1]))[sys.argv[2]])' "$@"
}

# Integration builds lease for 3 s and renew every 500 ms, so a setup that
# outlasts the lease must not delay renewal for the app already serving.
SLOW_SOURCE="$TEST_ROOT/slow"
mkdir -p "$SLOW_SOURCE"
printf 'slow app' >"$SLOW_SOURCE/index.html"
MESH_STATE_DIR="$ORIGIN_STATE" "$MESH_APP" app create local "$SLOW_SOURCE" --setup 'sleep 6' --json >"$TEST_ROOT/slow.json" 2>"$TEST_ROOT/slow.err" &
SLOW_CREATE=$!
for _ in $(seq 100); do
  grep -Eqs '"label": *"app-setup ' "$ORIGIN_STATE"/s/*/meta.json && break
  sleep 0.05
done
grep -Eqs '"label": *"app-setup ' "$ORIGIN_STATE"/s/*/meta.json || fail 'slow setup did not start'
sleep 4
app_curl --fail "$APP_ENDPOINT/api" >"$TEST_ROOT/during-setup.json" || fail "app lease lapsed while another app ran setup"
wait "$SLOW_CREATE" || fail "slow-setup app creation: $(cat "$TEST_ROOT/slow.err")"
SLOW_ID=$(python3 -c 'import json, sys; print(json.load(open(sys.argv[1]))["app"]["id"])' "$TEST_ROOT/slow.json") || fail 'slow app result'
SERVER_APP_ID="$APP_ID"
APP_ID="$SLOW_ID"
app_curl --fail "$(app_endpoint)/" >"$TEST_ROOT/static.html" || fail 'private static HTML'
cmp "$SLOW_SOURCE/index.html" "$TEST_ROOT/static.html" || fail 'static HTML changed or gained a widget'
WIDGET_STATUS=$(app_curl -o "$TEST_ROOT/widget.body" -w '%{http_code}' "$(app_endpoint)/.mesh-app/pill.js") || fail 'retired widget asset request'
[ "$WIDGET_STATUS" = 404 ] || fail "retired widget asset returned $WIDGET_STATUS"
private_app_denies_outsiders / || fail 'static app owner authorization'
MESH_STATE_DIR="$ORIGIN_STATE" "$MESH_APP" app renew local "$SLOW_ID" --json >"$TEST_ROOT/static-renew.json" || fail 'renew private static app'
MESH_STATE_DIR="$ORIGIN_STATE" "$MESH_APP" app download local "$SLOW_ID" "$TEST_ROOT/static-source.tar.gz" --json >/dev/null || fail 'download static source'
tar -xOzf "$TEST_ROOT/static-source.tar.gz" index.html >"$TEST_ROOT/static-source.html" || fail 'read downloaded static source'
cmp "$SLOW_SOURCE/index.html" "$TEST_ROOT/static-source.html" || fail 'download changed static source'
APP_ID="$SERVER_APP_ID"

# With the edge gone nothing renews the lease. Once it lapses the origin stops the
# app's worker, keeps its files, and leaves ordinary sessions alone.
# The session's shell expands these, not this script.
# shellcheck disable=SC2016
MESH_STATE_DIR="$ORIGIN_STATE" "$MESH_APP" local --daemon -- sh -c 'echo $$ >"$1"; exec sleep 600' sh "$TEST_ROOT/ordinary.pid" \
  </dev/null >/dev/null 2>&1 &
ORDINARY_CLIENT=$!
for _ in $(seq 100); do
  [ -s "$TEST_ROOT/ordinary.pid" ] && break
  sleep 0.05
done
[ -s "$TEST_ROOT/ordinary.pid" ] || fail 'ordinary session start'
ORDINARY_PID=$(cat "$TEST_ROOT/ordinary.pid")
kill -KILL "$ORDINARY_CLIENT" 2>/dev/null
wait "$ORDINARY_CLIENT" 2>/dev/null
APP_PID=$(json_field "$TEST_ROOT/restarted.json" pid)
APP_CWD=$(json_field "$TEST_ROOT/restarted.json" cwd)
kill -TERM "$EDGE_PID"
wait "$EDGE_PID" 2>/dev/null
EDGE_PID=""
for _ in $(seq 100); do
  kill -0 "$APP_PID" 2>/dev/null || break
  sleep 0.1
done
kill -0 "$APP_PID" 2>/dev/null && fail 'app worker kept running after its lease lapsed'
[ -f "$APP_CWD/setup.txt" ] || fail 'lapsed lease deleted app files'
kill -0 "$ORDINARY_PID" 2>/dev/null || fail 'lapsed-lease stop killed an ordinary session'

start_edge
RECOVERED=""
for _ in $(seq 100); do
  if app_curl --fail "$APP_ENDPOINT/api" >"$TEST_ROOT/recovered.json" 2>/dev/null; then
    RECOVERED=$(json_field "$TEST_ROOT/recovered.json" pid)
    break
  fi
  sleep 0.1
done
[ -n "$RECOVERED" ] && [ "$RECOVERED" != "$APP_PID" ] || fail 'app did not restart once the edge renewed its lease'
cp "$TEST_ROOT/recovered.json" "$TEST_ROOT/api.json"
APP_ID="$SLOW_ID"
app_curl --fail "$(app_endpoint)/" >"$TEST_ROOT/static-restarted.html" || fail 'static app after registry restart'
cmp "$SLOW_SOURCE/index.html" "$TEST_ROOT/static-restarted.html" || fail 'registry restart changed static HTML'
APP_ID="$SERVER_APP_ID"

MESH_STATE_DIR="$ORIGIN_STATE" "$MESH_APP" app delete local "$APP_ID" --json >"$TEST_ROOT/deleted.json" || fail 'delete running app'
DELETED_STATUS=$(app_curl -o "$TEST_ROOT/deleted.body" -w '%{http_code}' "$APP_ENDPOINT/api") || fail 'deleted host request'
[ "$DELETED_STATUS" = 303 ] || fail "deleted host returned $DELETED_STATUS"
python3 - "$TEST_ROOT/api.json" "$SOURCE/server" "$EDGE_STATE/mesh.db" "$APP_ID" <<'PY'
import json, os, sqlite3, sys
api_path, source_binary, edge_db, app_id = sys.argv[1:]
with open(api_path) as source:
    app = json.load(source)
if os.path.exists(app['cwd']):
    raise SystemExit('deleted app workspace remains')
try:
    os.kill(app['pid'], 0)
except ProcessLookupError:
    pass
else:
    raise SystemExit('deleted app process remains')
if not os.path.isfile(source_binary):
    raise SystemExit('delete removed caller source')
with sqlite3.connect(edge_db) as database:
    retired = database.execute('SELECT active FROM app_names WHERE public_name = ?', (app_id + '.mesh.test',)).fetchone()
    if retired != (0,):
        raise SystemExit(f'used hostname not retained: {retired}')
PY
[ $? -eq 0 ] || fail 'process and payload cleanup'
APP_ID=""
kill -0 "$ORDINARY_PID" 2>/dev/null || fail 'app deletion killed an ordinary session'
# Advance the stored static deadline with the registry stopped, avoiding a 24-hour wait.
kill -TERM "$EDGE_PID"
wait "$EDGE_PID" || fail 'registry shutdown before static expiry'
EDGE_PID=""
private_app_fixture expire "$TEST_ROOT" "$SLOW_ID" || fail 'expire persisted static deadline'
start_edge
APP_ID="$SLOW_ID"
EXPIRED_STATUS=$(app_curl -o "$TEST_ROOT/expired.body" -w '%{http_code}' "$(app_endpoint)/") || fail 'expired static request'
[ "$EXPIRED_STATUS" = 303 ] || fail "expired static app returned $EXPIRED_STATUS"
for _ in $(seq 100); do
  [ ! -e "$WORKLOAD/apps/$SLOW_ID" ] && break
  sleep 0.1
done
[ ! -e "$WORKLOAD/apps/$SLOW_ID" ] || fail 'expired static app kept its managed source'
[ -f "$SLOW_SOURCE/index.html" ] || fail 'static expiry deleted caller source'
APP_ID=""
echo 'PASS: owner-only static app preserves HTML/source, renews, survives registry restart and expires with cleanup; labelled HTTP app worker downloads its source without leaving an archive, serves HTML/API/redirects/WebSockets, survives daemon restart, keeps its lease through another app'"'"'s setup, stops when its lease lapses without touching ordinary sessions, restarts when the edge returns, and deletes its managed payload'
