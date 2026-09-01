#!/usr/bin/env bash
# Exercise the shipped daemon with stock OpenSSH, including identity selection,
# listener scope, shutdown, and worker survival.
set -uo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
test_root=$(mktemp -d "${TMPDIR:-/tmp}/mesh-ssh.XXXXXX")
MESH=$test_root/mesh-integration
server_state=$test_root/server-state
client_state=$test_root/client-state
unrelated_state=$test_root/unrelated-state
daemon_pid=
ssh_pid=
client_pid=
session_id=
session_pid=

cleanup() {
  [[ -z $session_id ]] || MESH_STATE_DIR="$server_state" "$MESH" kill "$session_id" >/dev/null 2>&1 || true
  for pid in "$ssh_pid" "$client_pid" "$daemon_pid"; do
    [[ -z $pid ]] || kill -9 "$pid" 2>/dev/null || true
  done
  for pid in "$ssh_pid" "$client_pid" "$daemon_pid"; do
    [[ -z $pid ]] || wait "$pid" 2>/dev/null || true
  done
  rm -rf -- "$test_root"
}
trap cleanup EXIT

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

process_alive() {
  local pid=$1
  local state
  kill -0 "$pid" 2>/dev/null || return 1
  [[ -r /proc/$pid/stat ]] || return 0
  state=$(awk '{print $3}' "/proc/$pid/stat" 2>/dev/null) || return 1
  [[ $state != Z ]]
}

wait_for_file() {
  local path=$1
  for _ in $(seq 100); do
    [[ -s $path ]] && return 0
    sleep 0.05
  done
  return 1
}

wait_for_tcp() {
  local host=$1
  local port=$2
  for _ in $(seq 100); do
    process_alive "$daemon_pid" || return 1
    if python3 - "$host" "$port" <<'PY' >/dev/null 2>&1
import socket
import sys

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

for tool in openssl python3 ssh ssh-keygen ssh-keyscan; do
  command -v "$tool" >/dev/null 2>&1 || fail "$tool is required"
done

(cd "$repo_root" && go build -tags mesh_integration -o "$MESH" ./cmd/mesh) || fail 'build integration binary'

mkdir -p "$server_state" "$client_state" "$unrelated_state" "$test_root/bin"
ln -s "$repo_root/integration/helpers/fake_tailscale" "$test_root/bin/tailscale"
for key in "$server_state/identity.key" "$client_state/identity.key" "$unrelated_state/identity.key"; do
  ssh-keygen -q -t ed25519 -N "" -C "" -f "$key" <<<y >/dev/null 2>&1 || fail "generate $key"
  rm -f "$key.pub"
  chmod 0600 "$key"
done
ssh-keygen -y -f "$client_state/identity.key" >"$server_state/authorized_keys" || fail 'export authorized client key'
chmod 0600 "$server_state/authorized_keys"

status=$test_root/tailscale-status.json
cat >"$status" <<'JSON'
{
  "BackendState": "Running",
  "Self": {
    "HostName": "mesh-ssh-test",
    "DNSName": "mesh-ssh-test.example.ts.net.",
    "TailscaleIPs": ["127.0.0.2"],
    "Online": true
  },
  "Peer": {}
}
JSON

ssh_port=$(python3 - <<'PY'
import socket

for _ in range(100):
    first = socket.socket()
    first.bind(("127.0.0.2", 0))
    port = first.getsockname()[1]
    second = socket.socket()
    try:
        second.bind(("127.0.0.1", port))
    except OSError:
        first.close()
        second.close()
        continue
    first.close()
    second.close()
    print(port)
    break
else:
    raise SystemExit("cannot reserve a loopback test port")
PY
) || fail 'reserve SSH port'

env MESH_STATE_DIR="$server_state" MESH_FAKE_TAILSCALE_STATUS="$status" PATH="$test_root/bin:$PATH" \
  "$MESH" daemon --ssh-port "$ssh_port" >"$test_root/daemon.log" 2>&1 &
daemon_pid=$!
wait_for_tcp 127.0.0.2 "$ssh_port" || fail "SSH listener did not start: $(cat "$test_root/daemon.log")"

if python3 - "$ssh_port" <<'PY' >/dev/null 2>&1
import socket
import sys

with socket.create_connection(("127.0.0.1", int(sys.argv[1])), timeout=0.2):
    pass
PY
then
  fail 'SSH listener also bound 127.0.0.1'
fi

ssh_options=(
  -F /dev/null
  -p "$ssh_port"
  -o BatchMode=yes
  -o ConnectTimeout=2
  -o IdentitiesOnly=yes
  -o StrictHostKeyChecking=no
  -o UserKnownHostsFile=/dev/null
  -o LogLevel=ERROR
)
hello=$(ssh "${ssh_options[@]}" -i "$client_state/identity.key" mesh@127.0.0.2 hello) ||
  fail "authorized OpenSSH client was refused: $(cat "$test_root/daemon.log")"
[[ $hello == 'mesh ssh ready' ]] || fail "SSH hello was $hello"

interactive=$(ssh "${ssh_options[@]}" -i "$client_state/identity.key" mesh@127.0.0.2) ||
  fail 'authorized shell request was refused'
[[ $interactive == 'mesh ssh ready' ]] || fail "interactive SSH hello was $interactive"

if ssh "${ssh_options[@]}" -i "$unrelated_state/identity.key" mesh@127.0.0.2 hello >/dev/null 2>&1; then
  fail 'unrelated identity authenticated'
fi
if ssh "${ssh_options[@]}" -o IdentityFile=none mesh@127.0.0.2 hello >/dev/null 2>&1; then
  fail 'client without a key authenticated'
fi

expected_fingerprint=$(ssh-keygen -y -f "$server_state/identity.key" | ssh-keygen -lf - | awk '{print $2}') ||
  fail 'derive Mesh identity fingerprint'
observed_fingerprint=$(ssh-keyscan -T 2 -p "$ssh_port" -t ed25519 127.0.0.2 2>/dev/null | ssh-keygen -lf - | awk '{print $2}') ||
  fail 'read SSH host fingerprint'
[[ $observed_fingerprint == "$expected_fingerprint" ]] ||
  fail "SSH host fingerprint $observed_fingerprint differs from Mesh identity $expected_fingerprint"

mkfifo "$test_root/session-input"
MESH_STATE_DIR="$server_state" "$MESH" local --daemon -- sh -c \
  "echo \$\$ > '$test_root/session.pid'; exec sleep 60" \
  <"$test_root/session-input" >"$test_root/session.out" 2>"$test_root/session.err" &
client_pid=$!
exec 3>"$test_root/session-input"
wait_for_file "$test_root/session.pid" || fail "session did not start: $(cat "$test_root/session.err")"
session_pid=$(cat "$test_root/session.pid")
for _ in $(seq 100); do
  session_id=$(sed -n 's/^session \([0-9A-Z][0-9A-Z][0-9A-Z][0-9A-Z]\)$/\1/p' "$test_root/session.err" | head -n1)
  [[ -n $session_id ]] && break
  sleep 0.02
done
[[ -n $session_id ]] || fail 'session ID was not reported'
printf '\035' >&3
exec 3>&-
for _ in $(seq 100); do
  process_alive "$client_pid" || break
  sleep 0.02
done
process_alive "$client_pid" && fail 'local client did not detach'
wait "$client_pid" 2>/dev/null || fail 'local client failed while detaching'
client_pid=

ssh "${ssh_options[@]}" -i "$client_state/identity.key" -N mesh@127.0.0.2 2>"$test_root/persistent-ssh.err" &
ssh_pid=$!
sleep 0.2
process_alive "$ssh_pid" || fail 'persistent SSH connection did not stay open'

kill "$daemon_pid" 2>/dev/null || fail 'stop daemon'
for _ in $(seq 100); do
  process_alive "$daemon_pid" || break
  sleep 0.02
done
process_alive "$daemon_pid" && fail 'daemon did not stop'
wait "$daemon_pid" 2>/dev/null || fail 'daemon exited with an error'
daemon_pid=
for _ in $(seq 100); do
  process_alive "$ssh_pid" || break
  sleep 0.02
done
process_alive "$ssh_pid" && fail 'daemon shutdown left an SSH connection open'
wait "$ssh_pid" 2>/dev/null || true
ssh_pid=
process_alive "$session_pid" || fail 'daemon shutdown killed a running session'

echo "PASS: OpenSSH used the Mesh identity on 127.0.0.2:$ssh_port and daemon shutdown preserved session $session_id"
