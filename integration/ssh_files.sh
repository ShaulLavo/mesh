#!/usr/bin/env bash
# Exercise SFTP and both OpenSSH SCP protocols against live service updates.
set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
test_root=$(mktemp -d "${TMPDIR:-/tmp}/mesh-ssh-files.XXXXXX")
mesh=${MESH_INTEGRATION_BINARY:-$test_root/mesh}
state=$test_root/state
daemon_pid=

cleanup() {
  [[ -z $daemon_pid ]] || kill "$daemon_pid" 2>/dev/null || true
  [[ -z $daemon_pid ]] || wait "$daemon_pid" 2>/dev/null || true
  chmod -R u+rwX "$test_root"
  rm -rf -- "$test_root"
}
trap cleanup EXIT

fail() {
  echo "FAIL: $*" >&2
  [[ ! -f $test_root/daemon.log ]] || cat "$test_root/daemon.log" >&2
  exit 1
}

for tool in python3 ssh-keygen sftp scp; do
  command -v "$tool" >/dev/null 2>&1 || fail "$tool is required"
done
if [[ -z ${MESH_INTEGRATION_BINARY:-} ]]; then
  (cd "$repo_root" && go build -tags mesh_integration -o "$mesh" ./cmd/mesh)
fi
mkdir -p "$state" "$test_root/bin" "$test_root/files/nested" "$test_root/site"
ln -s "$repo_root/integration/helpers/fake_tailscale" "$test_root/bin/tailscale"
ssh-keygen -q -t ed25519 -N "" -f "$test_root/client-key"
cp "$test_root/client-key.pub" "$state/authorized_keys"
chmod 0600 "$state/authorized_keys"
printf 'file transfer bytes\000\377\n' >"$test_root/files/nested/value.txt"
printf '<h1>static service</h1>\n' >"$test_root/site/index.html"
printf 'outside secret\n' >"$test_root/secret.txt"
for directory in a ab ab/child abc; do
  mkdir -p "$test_root/files/nested/$directory"
  printf 'belongs to %s\n' "$directory" >"$test_root/files/nested/$directory/value.txt"
done
python3 - "$test_root/files/nested/large.bin" <<'PY'
import sys

with open(sys.argv[1], "wb") as output:
    output.write(bytes(range(256)) * 1024 + b"last transfer bytes")
PY

cat >"$test_root/tailscale.json" <<'JSON'
{"BackendState":"Running","Self":{"HostName":"mesh-ssh-files","DNSName":"mesh-ssh-files.example.ts.net.","TailscaleIPs":["127.0.0.2"],"Online":true},"Peer":{}}
JSON
ssh_port=$(python3 - <<'PY'
import socket

with socket.socket() as reservation:
    reservation.bind(("127.0.0.2", 0))
    print(reservation.getsockname()[1])
PY
)
env MESH_STATE_DIR="$state" MESH_FAKE_TAILSCALE_STATUS="$test_root/tailscale.json" \
  PATH="$test_root/bin:$PATH" "$mesh" daemon --ssh-port "$ssh_port" >"$test_root/daemon.log" 2>&1 &
daemon_pid=$!
python3 - "$ssh_port" "$state/daemon.sock" <<'PY' || fail 'daemon did not start'
import os
import socket
import sys
import time

for _ in range(100):
    try:
        with socket.create_connection(("127.0.0.2", int(sys.argv[1])), timeout=0.1):
            if os.path.exists(sys.argv[2]):
                break
    except OSError:
        pass
    time.sleep(0.05)
else:
    raise SystemExit("SSH or control listener missing")
PY

options=(
  -F /dev/null -P "$ssh_port" -i "$test_root/client-key"
  -o BatchMode=yes -o IdentitiesOnly=yes -o ConnectTimeout=2
  -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR
)
remote=mesh@127.0.0.2
control=(python3 "$repo_root/integration/helpers/mesh_control.py")
for service in files site proxy; do
  kind=$service
  target=$test_root/$service
  [[ $service != site ]] || kind=static
  [[ $service != proxy ]] || target=12345
  "${control[@]}" --expect-type service.upserted upsert -- \
    "$state/daemon.sock" "$service" "$kind" "$target" "" >/dev/null
done

cat >"$test_root/download.batch" <<BATCH
pwd
ls -1 /
get /files/nested/value.txt $test_root/sftp-value.txt
get /site/index.html $test_root/sftp-index.html
get -R /files/nested $test_root/sftp-nested
BATCH
sftp "${options[@]}" -b "$test_root/download.batch" "$remote" >"$test_root/sftp.log" 2>&1 ||
  fail "stock SFTP download failed: $(cat "$test_root/sftp.log")"
cmp "$test_root/files/nested/value.txt" "$test_root/sftp-value.txt"
cmp "$test_root/files/nested/value.txt" "$test_root/sftp-nested/value.txt"
cmp "$test_root/files/nested/large.bin" "$test_root/sftp-nested/large.bin"
cmp "$test_root/site/index.html" "$test_root/sftp-index.html"
python3 - "$test_root/sftp.log" <<'PY' || fail 'SFTP synthetic root was incorrect'
import sys

lines = open(sys.argv[1]).read().splitlines()
listing = lines[lines.index("sftp> ls -1 /") + 1:]
listing = listing[:next(i for i, line in enumerate(listing) if line.startswith("sftp>"))]
if sorted(line.strip().strip("/") for line in listing if line.strip()) != ["files", "site"]:
    raise SystemExit(repr(listing))
PY

for protocol in modern legacy; do
  protocol_options=()
  [[ $protocol != legacy ]] || protocol_options=(-O)
  scp -v "${options[@]}" "${protocol_options[@]}" "$remote:/files/nested/value.txt" "$test_root/$protocol-value.txt" \
    >"$test_root/$protocol-download.log" 2>&1 || fail "$protocol SCP download failed: $(cat "$test_root/$protocol-download.log")"
  cmp "$test_root/files/nested/value.txt" "$test_root/$protocol-value.txt"
  scp "${options[@]}" "${protocol_options[@]}" -r "$remote:/files/nested" "$test_root/$protocol-nested" ||
    fail "$protocol recursive SCP download failed"
  cmp "$test_root/files/nested/value.txt" "$test_root/$protocol-nested/value.txt"
  cmp "$test_root/files/nested/large.bin" "$test_root/$protocol-nested/large.bin"
  for directory in a ab ab/child abc; do
    cmp "$test_root/files/nested/$directory/value.txt" "$test_root/$protocol-nested/$directory/value.txt"
  done
  if scp "${options[@]}" "${protocol_options[@]}" "$test_root/secret.txt" "$remote:/files/uploaded" >"$test_root/$protocol-upload.log" 2>&1; then
    fail "$protocol SCP accepted an upload"
  fi
done

printf 'put %s /files/uploaded\n' "$test_root/secret.txt" >"$test_root/upload.batch"
if sftp "${options[@]}" -b "$test_root/upload.batch" "$remote" >"$test_root/upload.log" 2>&1; then
  fail 'SFTP accepted an upload'
fi
[[ ! -e $test_root/files/uploaded ]] || fail 'refused upload created a file'
cmp "$test_root/files/nested/value.txt" "$test_root/sftp-value.txt"

ln -s "$test_root/secret.txt" "$test_root/files/outside-link"
for attack in /etc/passwd /files/../secret.txt /files/outside-link; do
  printf 'get %s %s\n' "$attack" "$test_root/leaked" >"$test_root/attack.batch"
  if sftp "${options[@]}" -b "$test_root/attack.batch" "$remote" >"$test_root/attack.log" 2>&1; then
    fail "SFTP accepted $attack"
  fi
  for protocol in modern legacy; do
    protocol_options=()
    [[ $protocol != legacy ]] || protocol_options=(-O)
    if scp "${options[@]}" "${protocol_options[@]}" "$remote:$attack" "$test_root/leaked" >"$test_root/attack.log" 2>&1; then
      fail "$protocol SCP accepted $attack"
    fi
  done
done

"${control[@]}" --expect-type service.deleted delete -- "$state/daemon.sock" files >/dev/null
if scp "${options[@]}" "$remote:/files/nested/value.txt" "$test_root/removed" >"$test_root/removed.log" 2>&1; then
  fail 'removed live service remained downloadable'
fi
rm -rf -- "$test_root/site"
if scp "${options[@]}" "$remote:/site/index.html" "$test_root/deleted" >"$test_root/deleted.log" 2>&1; then
  fail 'deleted service root remained downloadable'
fi
printf 'ls /\n' >"$test_root/alive.batch"
sftp "${options[@]}" -b "$test_root/alive.batch" "$remote" >"$test_root/alive.log" 2>&1 ||
  fail 'deleted root took down SSH'

echo 'PASS: SFTP, modern SCP and legacy SCP downloaded declared roots, refused uploads and traversal, and tracked live services'
