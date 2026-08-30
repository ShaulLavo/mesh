#!/usr/bin/env bash
# The product CLI keeps offline hosts useful, and names that look like session
# IDs never enter the host address book.
set -uo pipefail

if [ -z "${MESH:-}" ]; then
  MESH=$PWD/mesh
  go build -o "$MESH" ./cmd/mesh || { echo "FAIL: build" >&2; exit 1; }
fi
T=$(mktemp -d)
export MESH_STATE_DIR="$T/state"
export MESH_CONFIG_DIR="$T/config"
DAEMON=""

cleanup() {
  [ -z "$DAEMON" ] || kill -9 "$DAEMON" 2>/dev/null || true
  rm -rf "$T"
}
trap cleanup EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }

"$MESH" daemon >"$T/daemon.log" 2>&1 &
DAEMON=$!
for _ in $(seq 80); do
  [ -S "$MESH_STATE_DIR/daemon.sock" ] && break
  sleep 0.05
done
[ -S "$MESH_STATE_DIR/daemon.sock" ] || fail "daemon did not create the cache database: $(cat "$T/daemon.log")"
kill -TERM "$DAEMON"
wait "$DAEMON" || fail "daemon did not stop cleanly"
DAEMON=""

python3 - "$MESH_CONFIG_DIR/hosts.json" "$MESH_STATE_DIR/mesh.db" <<'PY'
import json
import os
import sqlite3
import sys
import time

config_path, database_path = sys.argv[1:]
os.makedirs(os.path.dirname(config_path), mode=0o700, exist_ok=True)
host = {
    "alias": "offline",
    "id": "offline-host-id",
    "meshIdentity": "offline-host-key",
    "tailscaleName": "offline.example.ts.net",
    "addresses": ["100.64.0.99"],
    "endpoint": "ws://127.0.0.1:1/mesh",
}
with open(config_path, "w", encoding="utf-8") as destination:
    json.dump({"version": 1, "hosts": [host]}, destination)
    destination.write("\n")
os.chmod(config_path, 0o600)

created_at = int(time.time() * 1000) - 60_000
with sqlite3.connect(database_path) as database:
    database.execute(
        "INSERT INTO hosts (id, alias, mesh_identity, tailscale_name, last_seen_at) VALUES (?, ?, ?, ?, ?)",
        (host["id"], host["alias"], host["meshIdentity"], host["tailscaleName"], created_at),
    )
    database.execute(
        "INSERT INTO sessions (id, host_id, command, cwd, state, created_at, last_output_sequence) VALUES (?, ?, ?, ?, ?, ?, ?)",
        ("7K3D", host["id"], '["bash"]', "/work", "running", created_at, 12),
    )
PY

timeout --kill-after=1s 2s "$MESH" ls --timeout 100ms >"$T/list.out" 2>"$T/list.err" ||
  fail "cross-host list did not return promptly: $(cat "$T/list.err")"
grep -q 'offline.*7K3D.*running.*stale' "$T/list.out" ||
  fail "cached session was not marked stale: $(cat "$T/list.out")"
grep -q 'offline: unavailable' "$T/list.err" ||
  fail "offline host diagnostic is missing: $(cat "$T/list.err")"

if "$MESH" add --alias 7K3D user@pc >"$T/add.out" 2>"$T/add.err"; then
  fail "mesh add accepted a session-shaped host alias"
fi
grep -qi 'session ID' "$T/add.err" || fail "alias rejection did not explain the collision: $(cat "$T/add.err")"

"$MESH" completion bash >"$T/completion"
grep -q '__mesh' "$T/completion" || fail "Bash completion was not generated"
"$MESH" man >"$T/man"
grep -q '^\.TH MESH 1' "$T/man" || fail "manpage was not generated"

echo "PASS: offline catalogs stay visible and the Cobra/Fang CLI artifacts work"
