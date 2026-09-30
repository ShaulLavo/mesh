#!/usr/bin/env bash
source "$(dirname -- "${BASH_SOURCE[0]}")/helpers/isolate.sh"
set -uo pipefail
export PYTHONDONTWRITEBYTECODE=1

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

python3 - "$TEST_ROOT" "$$" <<'PY'
import base64, json, os, socket, sys
root, pid = sys.argv[1:]

def identity(state):
    with open(os.path.join(root, state, 'identity.key.pub')) as source:
        blob = base64.b64decode(source.read().split()[1])
    return base64.urlsafe_b64encode(blob[-32:]).decode().rstrip('=')

held, ports = [], []
base = 25000 + int(pid) % 700 * 32
for offset in range(700 * 32):
    port = 25000 + (base - 25000 + offset) % (700 * 32)
    listener = socket.socket()
    try:
        listener.bind(('127.0.0.1', port))
    except OSError:
        listener.close()
        continue
    held.append(listener)
    ports.append(port)
    if len(ports) == 3:
        break
if len(ports) != 3:
    raise SystemExit('no fixture ports')
control, proxy, backend = ports
with open(os.path.join(root, 'ports'), 'w') as output:
    output.write('\n'.join(map(str, ports)) + '\n')
nodes = {'e': ('edge.app.test', '127.0.0.1'), 'o': ('origin.app.test', '127.0.0.21')}
for key, (name, address) in nodes.items():
    peers = {peer: {'DNSName': peer_name + '.', 'TailscaleIPs': [peer_address], 'Online': True}
             for peer, (peer_name, peer_address) in nodes.items() if peer != key}
    status = {'BackendState': 'Running', 'Self': {'DNSName': name + '.', 'TailscaleIPs': [address], 'Online': True}, 'Peer': peers}
    with open(os.path.join(root, key + '-status.json'), 'w') as output:
        json.dump(status, output)
origin = {'identity': identity('o'), 'displayAlias': 'app origin', 'tailscaleName': nodes['o'][0], 'controlPort': control, 'websocketPath': '/mesh'}
with open(os.path.join(root, 'edge.json'), 'w') as output:
    json.dump({'mode': 'proxy', 'listenAddress': f'127.0.0.1:{proxy}', 'origins': [origin]}, output)
with open(os.path.join(root, 'target.json'), 'w') as output:
    json.dump({'identity': identity('e'), 'tailscaleName': nodes['e'][0], 'controlPort': control, 'websocketPath': '/mesh'}, output)
PY
[ $? -eq 0 ] || fail 'fixture configuration'
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
}

start_edge
start_origin

MESH_STATE_DIR="$ORIGIN_STATE" "$MESH_APP" app create local "$SOURCE" --run './server' --setup 'printf setup-complete >setup.txt' --port "$BACKEND_PORT" --json >"$TEST_ROOT/create.json" || fail 'create HTTP app'
APP_ID=$(python3 - "$TEST_ROOT/create.json" <<'PY'
import json, sys
with open(sys.argv[1]) as source:
    app = json.load(source)['app']
if app['visibility'] != 'private' or app['kind'] != 'server' or not app['ready']:
    raise SystemExit(f'invalid new app: {app}')
print(app['id'])
PY
) || fail 'private creation result'

app_curl() {
  curl --noproxy '*' --silent --show-error --max-time 3 --header "Host: $APP_ID.shaulavo.dev" \
    --header 'X-Forwarded-For: 203.0.113.77' --header 'X-Forwarded-Proto: https' "$@"
}
APP_ENDPOINT="http://127.0.0.1:$PROXY_PORT"
PRIVATE_STATUS=$(app_curl -o "$TEST_ROOT/private.body" -w '%{http_code}' "$APP_ENDPOINT/api") || fail 'private gate request'
[ "$PRIVATE_STATUS" = 303 ] || fail "private app returned $PRIVATE_STATUS"
if grep -Eq 'APP_LABELLED_WORKER|"pid"' "$TEST_ROOT/private.body"; then
  fail 'private backend bytes escaped'
fi
python3 "$REPO_ROOT/integration/helpers/mesh_control.py" --expect-type error upsert \
  "$ORIGIN_STATE/daemon.sock" alias proxy "$BACKEND_PORT" alias.shaulavo.dev >"$TEST_ROOT/alias.out" || fail 'app port alias refusal'
grep -Eq 'app-owned port' "$TEST_ROOT/alias.out" || fail 'ordinary public proxy bypassed private app gate'
python3 "$REPO_ROOT/integration/helpers/mesh_control.py" --expect-type error upsert \
  "$ORIGIN_STATE/daemon.sock" source-alias files "$WORKLOAD" '' >"$TEST_ROOT/source-alias.out" || fail 'app source alias refusal'
grep -Eq 'managed app directories' "$TEST_ROOT/source-alias.out" || fail 'ordinary file route exposed private app source'
MESH_STATE_DIR="$ORIGIN_STATE" "$MESH_APP" app public local "$APP_ID" --json >"$TEST_ROOT/public.json" || fail 'publish app'
app_curl --fail "$APP_ENDPOINT/" >"$TEST_ROOT/index.html" || fail 'public HTML'
grep -Eq 'APP_LABELLED_WORKER' "$TEST_ROOT/index.html" || fail 'server HTML missing'
grep -Eq '/.mesh-app/' "$TEST_ROOT/index.html" || fail 'floating pill missing'
app_curl --fail "$APP_ENDPOINT/api" >"$TEST_ROOT/api.json" || fail 'public API'
app_curl --fail "$APP_ENDPOINT/mesh" >"$TEST_ROOT/mesh.json" || fail 'whole-host /mesh API'
app_curl --fail --location "$APP_ENDPOINT/redirect" >"$TEST_ROOT/redirect.json" || fail 'root-relative redirect'
python3 "$REPO_ROOT/integration/helpers/public_http_fixture.py" client 127.0.0.1 "$PROXY_PORT" "$APP_ID.shaulavo.dev" /socket --proxy >"$TEST_ROOT/socket.out" || fail 'HTTP app WebSocket'

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
app_curl --fail "$APP_ENDPOINT/api" >"$TEST_ROOT/restarted.json" || fail 'app after origin restart'
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
MESH_STATE_DIR="$ORIGIN_STATE" "$MESH_APP" app delete local "$SLOW_ID" --json >/dev/null || fail 'delete slow-setup app'

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

MESH_STATE_DIR="$ORIGIN_STATE" "$MESH_APP" app delete local "$APP_ID" --json >"$TEST_ROOT/deleted.json" || fail 'delete running app'
DELETED_STATUS=$(app_curl -o "$TEST_ROOT/deleted.body" -w '%{http_code}' "$APP_ENDPOINT/api") || fail 'deleted host request'
[ "$DELETED_STATUS" = 410 ] || fail "deleted host returned $DELETED_STATUS"
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
    retired = database.execute('SELECT active FROM app_names WHERE public_name = ?', (app_id + '.shaulavo.dev',)).fetchone()
    if retired != (0,):
        raise SystemExit(f'used hostname not retained: {retired}')
PY
[ $? -eq 0 ] || fail 'process and payload cleanup'
APP_ID=""
kill -0 "$ORDINARY_PID" 2>/dev/null || fail 'app deletion killed an ordinary session'
echo 'PASS: labelled HTTP app worker serves HTML/API/redirects/WebSockets, survives daemon restart, keeps its lease through another app'"'"'s setup, stops when its lease lapses without touching ordinary sessions, restarts when the edge returns, and deletes its managed payload'
