#!/usr/bin/env bash
source "$(dirname -- "${BASH_SOURCE[0]}")/helpers/isolate.sh" || exit 1
set -euo pipefail
previous_binary=${1:-}
if [[ -z $previous_binary ]]; then
  echo 'SKIP: pass a previous installed binary as the first argument for rollback verification'
  exit 0
fi
: "${MESH:?candidate Mesh binary is required}"
root=$(mktemp -d "${TMPDIR:-/tmp}/mesh-private-reader.XXXXXX")
pid=''
cleanup() {
  if [[ -n $pid ]]; then
    kill -TERM "$pid" 2>/dev/null || true
    wait "$pid" 2>/dev/null || true
  fi
  rm -rf -- "$root"
}
trap cleanup EXIT
export MESH_STATE_DIR="$root/state"
"$MESH" daemon --tailnet-port=0 --ssh-port=0 >"$root/candidate.log" 2>&1 &
pid=$!
for _ in $(seq 100); do
  [[ -S $MESH_STATE_DIR/daemon.sock ]] && break
  kill -0 "$pid" 2>/dev/null || { cat "$root/candidate.log" >&2; exit 1; }
  sleep 0.05
done
[[ -S $MESH_STATE_DIR/daemon.sock ]] || { echo 'FAIL: candidate did not start' >&2; exit 1; }
kill -TERM "$pid"
wait "$pid"
pid=''
python3 - "$MESH_STATE_DIR/mesh.db" <<'PY'
import sqlite3
import sys
with sqlite3.connect(sys.argv[1]) as db:
    assert db.execute('SELECT max(version_id) FROM goose_db_version WHERE is_applied=1').fetchone()[0] == 12
    assert db.execute("SELECT count(*) FROM sqlite_master WHERE type='table' AND name='app_state'").fetchone()[0] == 0
    db.execute('INSERT INTO private_app_state(key,data) VALUES (?,?)', ('apps.edge', b'{"apps":{},"owners":{}}'))
PY
mkdir -p "$root/bin"
ln -s "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/helpers" && pwd)/fake_tailscale" "$root/bin/tailscale"
python3 - "$root" <<'PYDATA'
import base64
import json
import os
import sys
root = sys.argv[1]
identity = base64.urlsafe_b64encode(os.urandom(32)).decode().rstrip('=')
with open(os.path.join(root, 'tailscale.json'), 'w') as out:
    json.dump({'BackendState': 'Running', 'Self': {'DNSName': 'fixture.app.test.', 'TailscaleIPs': ['100.64.0.1'], 'Online': True}, 'Peer': {}}, out)
with open(os.path.join(root, 'target.json'), 'w') as out:
    json.dump({'identity': identity, 'tailscaleName': 'fixture.app.test', 'controlPort': 17337, 'websocketPath': '/mesh'}, out)
PYDATA
set +e
MESH_FAKE_TAILSCALE_STATUS="$root/tailscale.json" PATH="$root/bin:$PATH" \
  timeout 10s "$previous_binary" daemon --tailnet-port=17337 --ssh-port=0 --public-edge-target="$root/target.json" >"$root/previous.log" 2>&1
status=$?
set -e
if [[ $status == 0 || $status == 124 ]]; then
  echo 'FAIL: previous daemon accepted private-only schema' >&2
  cat "$root/previous.log" >&2
  exit 1
fi
if ! rg -qi 'migration|app_state' "$root/previous.log"; then
  echo 'FAIL: previous daemon stopped for an unrelated reason' >&2
  cat "$root/previous.log" >&2
  exit 1
fi
python3 - "$MESH_STATE_DIR/mesh.db" <<'PY'
import sqlite3
import sys
with sqlite3.connect(sys.argv[1]) as db:
    assert db.execute('SELECT data FROM private_app_state WHERE key=?', ('apps.edge',)).fetchone()[0] == b'{"apps":{},"owners":{}}'
PY
echo 'PASS: previous installed daemon refuses private-only state without changing app data'
