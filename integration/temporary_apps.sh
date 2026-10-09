#!/usr/bin/env bash
source "$(dirname -- "${BASH_SOURCE[0]}")/helpers/isolate.sh" || exit 1
set -uo pipefail

if [ -z "${MESH:-}" ]; then
  MESH=$PWD/mesh
  go build -o "$MESH" ./cmd/mesh || exit 1
fi
TEST_ROOT=$(mktemp -d "${TMPDIR:-/tmp}/mesh-app-contract.XXXXXX")
export MESH_STATE_DIR="$TEST_ROOT/state"
mkdir -p "$TEST_ROOT/config"
cp "$MESH_CONFIG_DIR/domains.json" "$TEST_ROOT/config/domains.json"
export MESH_CONFIG_DIR="$TEST_ROOT/config"
DAEMON=""

cleanup() {
  if [ -n "$DAEMON" ]; then
    kill -TERM "$DAEMON" 2>/dev/null || true
    wait "$DAEMON" 2>/dev/null || true
  fi
  rm -rf -- "$TEST_ROOT"
}
trap cleanup EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }

mkdir -p "$TEST_ROOT/source"
printf '%s\n' '<!doctype html><title>Temporary app</title>' >"$TEST_ROOT/source/index.html"
"$MESH" daemon --app-data-root "$TEST_ROOT/workload" >"$TEST_ROOT/daemon.log" 2>&1 &
DAEMON=$!
for _ in $(seq 80); do
  [ -S "$MESH_STATE_DIR/daemon.sock" ] && break
  sleep 0.05
done
[ -S "$MESH_STATE_DIR/daemon.sock" ] || fail "daemon startup: $(cat "$TEST_ROOT/daemon.log")"

"$MESH" app --help >"$TEST_ROOT/help" || fail 'app help failed'
grep -Eq 'create|Create' "$TEST_ROOT/help" || fail 'app create absent from help'

if "$MESH" app create local "$TEST_ROOT/source" --run 'printf should-not-run' >"$TEST_ROOT/invalid.out" 2>"$TEST_ROOT/invalid.err"; then
  fail 'server recipe accepted without explicit port'
fi
grep -Eq -- '--run requires --port' "$TEST_ROOT/invalid.err" || fail "port error unclear: $(cat "$TEST_ROOT/invalid.err")"

if "$MESH" app create local "$TEST_ROOT/source" >"$TEST_ROOT/create.out" 2>"$TEST_ROOT/create.err"; then
  fail 'app accepted without a configured private app registry'
fi
grep -Eq -- '--public-edge-target' "$TEST_ROOT/create.err" || fail "edge configuration error unclear: $(cat "$TEST_ROOT/create.err")"

if "$MESH" app list local --json >"$TEST_ROOT/list.out" 2>"$TEST_ROOT/list.err"; then
  fail 'app list accepted without a configured private app registry'
fi
grep -Eq -- '--public-edge-target' "$TEST_ROOT/list.err" || fail "list error unclear: $(cat "$TEST_ROOT/list.err")"

"$MESH" ls >"$TEST_ROOT/sessions.out" || fail 'ordinary daemon stopped accepting controls'
python3 - "$MESH_STATE_DIR/mesh.db" "$TEST_ROOT/source/index.html" "$TEST_ROOT/workload" <<'PY'
import os
import sqlite3
import sys

database_path, source_path, workload_path = sys.argv[1:]
with sqlite3.connect(database_path) as database:
    for table in ('private_app_state', 'app_names', 'sessions'):
        count = database.execute(f'SELECT count(*) FROM {table}').fetchone()[0]
        if count:
            raise SystemExit(f'failed app creation left {count} records in {table}')
with open(source_path, encoding='utf-8') as source:
    if source.read() != '<!doctype html><title>Temporary app</title>\n':
        raise SystemExit('caller source changed')
if os.path.exists(workload_path):
    raise SystemExit('unconfigured daemon created app workload data')
PY
[ $? -eq 0 ] || fail 'failed app requests left state or files'

kill -TERM "$DAEMON"
wait "$DAEMON" || fail 'daemon did not stop cleanly'
DAEMON=""
echo 'PASS: temporary app CLI requires explicit recipes and an enabled private app registry without creating workloads'
