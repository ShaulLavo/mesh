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
TEST_ROOT=$(mktemp -d "${TMPDIR:-/tmp}/az.XXXXXX")
REGISTRY_STATE="$TEST_ROOT/e"
ORIGIN_STATE="$TEST_ROOT/o"
WORKLOAD="$TEST_ROOT/w"
SOURCE="$TEST_ROOT/src"
MESH_APP=${MESH_INTEGRATION_BINARY:-$TEST_ROOT/mesh}
REGISTRY_PID=""
ORIGIN_PID=""
APP_ID=""

cleanup() {
  if [ -n "$APP_ID" ] && [ -n "$ORIGIN_PID" ]; then
    MESH_STATE_DIR="$ORIGIN_STATE" timeout 5s "$MESH_APP" app delete local "$APP_ID" >/dev/null 2>&1 || true
  fi
  for pid in "$ORIGIN_PID" "$REGISTRY_PID"; do
    [ -z "$pid" ] || kill -TERM "$pid" 2>/dev/null || true
  done
  for pid in "$ORIGIN_PID" "$REGISTRY_PID"; do
    [ -z "$pid" ] || wait "$pid" 2>/dev/null || true
  done
  rm -rf -- "$TEST_ROOT"
}
trap cleanup EXIT

fail() {
  echo "FAIL: $*" >&2
  for log in "$TEST_ROOT/registry.log" "$TEST_ROOT/origin.log"; do
    [ ! -f "$log" ] || tail -20 "$log" >&2
  done
  exit 1
}

mkdir -p "$TEST_ROOT/bin" "$REGISTRY_STATE" "$ORIGIN_STATE" "$SOURCE"
ln -s "$REPO_ROOT/integration/helpers/fake_tailscale" "$TEST_ROOT/bin/tailscale"
if [ -z "${MESH_INTEGRATION_BINARY:-}" ]; then
  (cd "$REPO_ROOT" && go build -tags mesh_integration -o "$MESH_APP" ./cmd/mesh) || fail 'build integration Mesh'
fi

for state in "$REGISTRY_STATE" "$ORIGIN_STATE"; do
  ssh-keygen -q -t ed25519 -N '' -C '' -f "$state/identity.key" || fail 'create fixture identity'
done
cat "$ORIGIN_STATE/identity.key.pub" >"$REGISTRY_STATE/authorized_keys"
cat "$REGISTRY_STATE/identity.key.pub" >"$ORIGIN_STATE/authorized_keys"
chmod 0600 "$REGISTRY_STATE/authorized_keys" "$ORIGIN_STATE/authorized_keys"

private_app_fixture configure "$TEST_ROOT" "$$" || fail 'fixture configuration'
mapfile -t PORTS <"$TEST_ROOT/ports"
CONTROL_PORT=${PORTS[0]}
PROXY_PORT=${PORTS[1]}

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
    "$MESH_APP" daemon --tailnet-port "$CONTROL_PORT" --app-registry-target "$TEST_ROOT/target.json" --app-data-root "$WORKLOAD" >"$TEST_ROOT/origin.log" 2>&1 &
  ORIGIN_PID=$!
  wait_for_socket "$ORIGIN_PID" "$ORIGIN_STATE/daemon.sock" || fail 'origin startup'
}

start_registry() {
  env MESH_STATE_DIR="$REGISTRY_STATE" MESH_FAKE_TAILSCALE_STATUS="$TEST_ROOT/e-status.json" PATH="$TEST_ROOT/bin:$PATH" \
    "$MESH_APP" daemon --tailnet-port "$CONTROL_PORT" --app-registry-config "$TEST_ROOT/registry.json" >>"$TEST_ROOT/registry.log" 2>&1 &
  REGISTRY_PID=$!
  wait_for_socket "$REGISTRY_PID" "$REGISTRY_STATE/daemon.sock" || fail 'registry startup'
  private_app_fixture install "$TEST_ROOT" || fail 'install private app HTTPS certificate'
}

start_registry
start_origin

# A source and archive above the old cap must survive creation, an unlimited
# update, a restart, static serving and owner download through the real daemon.
mkdir -p "$TEST_ROOT/large"
printf '%s\n' '<!doctype html><title>Large app</title>' >"$TEST_ROOT/large/index.html"
python3 - "$TEST_ROOT/large/asset.bin" <<'PYDATA'
import os, sys
with open(sys.argv[1], 'wb') as destination:
    for _ in range(65):
        destination.write(os.urandom(1024 * 1024))
PYDATA
if MESH_STATE_DIR="$ORIGIN_STATE" "$MESH_APP" app create local "$TEST_ROOT/large" >"$TEST_ROOT/large.out" 2>"$TEST_ROOT/large.err"; then
  fail 'default limit accepted more than 64 MiB'
fi
grep -q -- '--max-size' "$TEST_ROOT/large.err" || fail 'large source error did not explain the size option'
MESH_STATE_DIR="$ORIGIN_STATE" "$MESH_APP" app create local "$TEST_ROOT/large" --max-size 128MiB --json >"$TEST_ROOT/large.json" || fail 'create above 64 MiB'
APP_ID=$(python3 -c 'import json, sys; print(json.load(open(sys.argv[1]))["app"]["id"])' "$TEST_ROOT/large.json") || fail 'large creation result'
MESH_STATE_DIR="$ORIGIN_STATE" "$MESH_APP" app update local "$APP_ID" "$TEST_ROOT/large" --max-size unlimited --json >/dev/null || fail 'unlimited update'
kill -TERM "$ORIGIN_PID"
wait "$ORIGIN_PID" || fail 'stop origin before size-limit persistence check'
ORIGIN_PID=""
start_origin
app_curl --fail "$(app_endpoint)/asset.bin" >"$TEST_ROOT/served.bin" || fail 'serve large static asset'
cmp "$TEST_ROOT/large/asset.bin" "$TEST_ROOT/served.bin" || fail 'large asset was truncated'
MESH_STATE_DIR="$ORIGIN_STATE" "$MESH_APP" app download local "$APP_ID" "$TEST_ROOT/large.tar.gz" >/dev/null || fail 'download above 64 MiB after restart'
python3 - "$TEST_ROOT/large.tar.gz" "$TEST_ROOT/large/asset.bin" <<'PYDATA' || fail 'large source archive is not intact'
import hashlib, os, sys, tarfile
assert os.path.getsize(sys.argv[1]) > 64 * 1024 * 1024
with tarfile.open(sys.argv[1], 'r:gz') as archive, open(sys.argv[2], 'rb') as original:
    assert hashlib.file_digest(archive.extractfile('asset.bin'), 'sha256').digest() == hashlib.file_digest(original, 'sha256').digest()
PYDATA
MESH_STATE_DIR="$ORIGIN_STATE" "$MESH_APP" app delete local "$APP_ID" --json >/dev/null || fail 'delete large app'
APP_ID=""
echo 'PASS: configured and unlimited size limits support uploads, static assets and downloads above 64 MiB across update and restart'
