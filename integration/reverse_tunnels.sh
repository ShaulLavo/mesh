#!/usr/bin/env bash
# Exercise durable claims and transient HTTP routes with real daemons and OpenSSH.
set -uo pipefail
export PYTHONDONTWRITEBYTECODE=1

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
test_root=$(mktemp -d "${TMPDIR:-/tmp}/mesh-tunnel.XXXXXX")
mesh=${MESH_INTEGRATION_BINARY:-$test_root/mesh-integration}
edge_state=$test_root/edge
client_state=$test_root/client
other_state=$test_root/other
config_dir=$test_root/config
edge_pid=
ssh_pid=
backend_pid=

cleanup() {
  for pid in "$ssh_pid" "$edge_pid" "$backend_pid"; do
    [[ -z $pid ]] || kill "$pid" 2>/dev/null || true
  done
  for pid in "$ssh_pid" "$edge_pid" "$backend_pid"; do
    [[ -z $pid ]] || wait "$pid" 2>/dev/null || true
  done
  rm -rf -- "$test_root"
}
trap cleanup EXIT

fail() {
  echo "FAIL: $*" >&2
  for log in "$test_root/edge.log" "$test_root/ssh.log" "$test_root/command.log"; do
    [[ ! -f $log ]] || tail -n 15 "$log" >&2
  done
  exit 1
}

alive() {
  local state
  kill -0 "$1" 2>/dev/null || return 1
  [[ -r /proc/$1/stat ]] || return 0
  state=$(awk '{print $3}' "/proc/$1/stat" 2>/dev/null) || return 1
  [[ $state != Z ]]
}

stop_process() {
  local pid=$1
  kill "$pid" 2>/dev/null || true
  for _ in {1..100}; do
    alive "$pid" || break
    sleep 0.02
  done
  alive "$pid" && fail "process $pid did not stop"
  wait "$pid" 2>/dev/null || true
}

identity_id() {
  ssh-keygen -y -f "$1" | python3 -c '
import base64, sys
blob = base64.b64decode(sys.stdin.read().split()[1])
print(base64.urlsafe_b64encode(blob[-32:]).decode().rstrip("="))
'
}

cli() {
  env MESH_STATE_DIR="$client_state" MESH_CONFIG_DIR="$config_dir" "$mesh" "$@"
}

request() {
  curl --noproxy '*' --silent --show-error --max-time 1 \
    --header "Host: ${2:-blog.shaulavo.dev}" \
    --header 'X-Forwarded-For: 203.0.113.77' --header 'X-Forwarded-Proto: https' \
    "http://127.0.0.1:$public_port${1:-/}" "${@:3}"
}

expect_404() {
  local status
  status=$(request / "${1:-blog.shaulavo.dev}" --output /dev/null --write-out '%{http_code}') || fail 'inactive route request failed'
  [[ $status == 404 ]] || fail "inactive hostname returned $status, want 404"
}

wait_inactive() {
  for _ in {1..60}; do
    [[ $(request / blog.shaulavo.dev --output /dev/null --write-out '%{http_code}' 2>/dev/null) == 404 ]] && return 0
    sleep 0.02
  done
  fail 'disconnect left a published tunnel route'
}

start_edge() {
  env MESH_STATE_DIR="$edge_state" MESH_FAKE_TAILSCALE_STATUS="$test_root/status.json" PATH="$test_root/bin:$PATH" \
    "$mesh" daemon --tailnet-port "$control_port" --ssh-port "$ssh_port" --edge "$test_root/edge.json" >"$test_root/edge.log" 2>&1 &
  edge_pid=$!
  for _ in {1..100}; do
    alive "$edge_pid" || fail 'edge daemon exited during startup'
    if [[ -S $edge_state/daemon.sock ]] && request / blog.shaulavo.dev --output /dev/null 2>/dev/null; then
      return 0
    fi
    sleep 0.03
  done
  fail 'edge daemon did not become ready'
}

start_forward() {
  ssh "${ssh_options[@]}" -M -S "$test_root/ssh-control" -i "$client_state/identity.key" -N \
    -R "blog.shaulavo.dev:80:localhost:$backend_port" mesh@127.0.0.21 >"$test_root/ssh.out" 2>"$test_root/ssh.log" &
  ssh_pid=$!
  for _ in {1..100}; do
    alive "$ssh_pid" || fail 'stock ssh -N -R exited before activation'
    [[ $(request / 2>/dev/null) == MESH_REVERSE_TUNNEL_BODY ]] && return 0
    sleep 0.02
  done
  fail 'activated forward did not reach the local HTTP server'
}

expect_forward_refused() {
  local key=$1 tuple=$2 status
  timeout 2 ssh "${ssh_options[@]}" -i "$key" -N -R "$tuple" mesh@127.0.0.21 >"$test_root/refused.out" 2>&1
  status=$?
  [[ $status != 0 && $status != 124 ]] || fail "forward $tuple was not promptly refused, status $status"
}

for tool in go python3 curl ssh ssh-keygen timeout; do
  command -v "$tool" >/dev/null 2>&1 || fail "$tool is required"
done
mkdir -p "$edge_state" "$client_state" "$other_state" "$config_dir" "$test_root/bin" "$test_root/site"
chmod 0700 "$edge_state" "$client_state" "$other_state"
ln -s "$repo_root/integration/helpers/fake_tailscale" "$test_root/bin/tailscale"
if [[ -z ${MESH_INTEGRATION_BINARY:-} ]]; then
  (cd "$repo_root" && go build -tags mesh_integration -o "$mesh" ./cmd/mesh) || fail 'build tagged Mesh binary'
fi

for key in "$edge_state/identity.key" "$client_state/identity.key" "$other_state/identity.key" "$test_root/unauthorized.key"; do
  ssh-keygen -q -t ed25519 -N '' -C '' -f "$key" || fail 'generate fixture identity'
  chmod 0600 "$key"
done
cat "$client_state/identity.key.pub" "$other_state/identity.key.pub" >"$edge_state/authorized_keys"
chmod 0600 "$edge_state/authorized_keys"
edge_id=$(identity_id "$edge_state/identity.key") || fail 'read edge identity'
client_id=$(identity_id "$client_state/identity.key") || fail 'read client identity'

mapfile -t ports < <(python3 - <<'PY'
import socket
held = []
for address in ["127.0.0.21", "127.0.0.21", "127.0.0.1", "127.0.0.1"]:
    listener = socket.socket()
    listener.bind((address, 0))
    held.append(listener)
    print(listener.getsockname()[1])
PY
)
[[ ${#ports[@]} == 4 ]] || fail 'allocate listener ports'
control_port=${ports[0]}
ssh_port=${ports[1]}
public_port=${ports[2]}
backend_port=${ports[3]}
python3 - "$test_root" "$edge_id" "$client_id" "$control_port" "$public_port" <<'PY'
import json, pathlib, sys
root, edge_id, client_id, control_port, public_port = sys.argv[1:]
root = pathlib.Path(root)
fixtures = {
    "status.json": {
        "BackendState": "Running",
        "Self": {"DNSName": "edge.fixture.test.", "TailscaleIPs": ["127.0.0.21"], "Online": True},
        "Peer": {"client": {"DNSName": "client.fixture.test.", "TailscaleIPs": ["127.0.0.22"], "Online": True}},
    },
    "edge.json": {
        "mode": "proxy", "listenAddress": f"127.0.0.1:{public_port}",
        "origins": [{"identity": client_id, "displayAlias": "client", "tailscaleName": "client.fixture.test",
                     "controlPort": int(control_port), "websocketPath": "/mesh"}],
    },
    "config/hosts.json": {"version": 1, "hosts": [{"alias": "vps", "id": edge_id, "meshIdentity": edge_id,
        "tailscaleName": "127.0.0.21", "endpoint": f"ws://127.0.0.21:{control_port}/mesh"}]},
}
for path, value in fixtures.items():
    (root / path).write_text(json.dumps(value))
PY

printf MESH_REVERSE_TUNNEL_BODY >"$test_root/site/index.html"
python3 -m http.server "$backend_port" --bind 127.0.0.1 --directory "$test_root/site" >"$test_root/backend.log" 2>&1 &
backend_pid=$!
ssh_options=(-F /dev/null -p "$ssh_port" -o BatchMode=yes -o ConnectTimeout=1
  -o ExitOnForwardFailure=yes -o IdentitiesOnly=yes -o StrictHostKeyChecking=no
  -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR)

start_edge
cli serve claim vps blog.shaulavo.dev --yes >"$test_root/command.log" 2>&1 || fail 'CLI create claim'
expect_404
cli serve claim vps blog.shaulavo.dev --yes >"$test_root/command.log" 2>&1 || fail 'same-owner convergent claim'
if env MESH_STATE_DIR="$other_state" MESH_CONFIG_DIR="$config_dir" "$mesh" serve claim vps blog.shaulavo.dev --yes >"$test_root/command.log" 2>&1; then
  fail 'another authorized owner displaced the durable claim'
fi
start_forward
[[ $(request /mesh blog.shaulavo.dev --output /dev/null --write-out '%{http_code}') == 404 ]] || fail 'terminal route crossed the edge'
expect_forward_refused "$client_state/identity.key" "blog.shaulavo.dev:80:localhost:$backend_port"
expect_forward_refused "$client_state/identity.key" "unclaimed.shaulavo.dev:80:localhost:$backend_port"
expect_forward_refused "$other_state/identity.key" "blog.shaulavo.dev:80:localhost:$backend_port"
expect_forward_refused "$test_root/unauthorized.key" "blog.shaulavo.dev:80:localhost:$backend_port"
for bind in blog '*.shaulavo.dev' 127.0.0.1; do
  expect_forward_refused "$client_state/identity.key" "$bind:80:localhost:$backend_port"
done
for port in 0 81; do
  expect_forward_refused "$client_state/identity.key" "blog.shaulavo.dev:$port:localhost:$backend_port"
done
if cli unserve blog.shaulavo.dev --host vps >"$test_root/command.log" 2>&1; then
  fail 'owner release accepted an active tunnel'
fi
[[ $(request /) == MESH_REVERSE_TUNNEL_BODY ]] || fail 'refused mutation disturbed the active forward'
ssh "${ssh_options[@]}" -S "$test_root/ssh-control" -O cancel \
  -R "blog.shaulavo.dev:80:localhost:$backend_port" mesh@127.0.0.21 >"$test_root/command.log" 2>&1 || fail 'stock SSH cancellation'
expect_404
alive "$ssh_pid" || fail 'cancelling a forward closed its SSH connection'
stop_process "$ssh_pid"
ssh_pid=
wait_inactive
start_forward
stop_process "$ssh_pid"
ssh_pid=
wait_inactive
start_forward
stop_process "$edge_pid"
edge_pid=
wait "$ssh_pid" 2>/dev/null || true
ssh_pid=
start_edge
expect_404
start_forward
stop_process "$ssh_pid"
ssh_pid=
wait_inactive

# Revocation preserves reservations but blocks new activation and creation.
cat "$other_state/identity.key.pub" >"$edge_state/authorized_keys"
expect_forward_refused "$client_state/identity.key" "blog.shaulavo.dev:80:localhost:$backend_port"
if cli serve claim vps revoked.shaulavo.dev --yes >"$test_root/command.log" 2>&1; then
  fail 'revoked owner created a reservation'
fi
cli unserve blog.shaulavo.dev --host vps >"$test_root/command.log" 2>&1 || fail 'revoked owner could not release its inactive claim'
expect_404

cat "$client_state/identity.key.pub" "$other_state/identity.key.pub" >"$edge_state/authorized_keys"
cli serve claim vps recovery.shaulavo.dev --yes >"$test_root/command.log" 2>&1 || fail 'create recovery reservation'
rm "$client_state/identity.key"
rm "$edge_state/authorized_keys"
env MESH_STATE_DIR="$edge_state" "$mesh" unserve recovery.shaulavo.dev --local-edge >"$test_root/command.log" 2>&1 || fail 'Unix socket recovery with missing authorization state'
expect_404 recovery.shaulavo.dev
cat "$other_state/identity.key.pub" >"$edge_state/authorized_keys"
chmod 0600 "$edge_state/authorized_keys"
env MESH_STATE_DIR="$other_state" MESH_CONFIG_DIR="$config_dir" "$mesh" serve claim vps recovery.shaulavo.dev --yes >"$test_root/command.log" 2>&1 || fail 'local recovery did not release hostname ownership'

echo 'PASS: stock CLI and OpenSSH reserved, activated, refused invalid forwards, disconnected, restarted, released after revocation, and recovered through the Unix socket'
