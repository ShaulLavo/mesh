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
TEST_ROOT=$(mktemp -d "${TMPDIR:-/tmp}/aq.XXXXXX")
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

mkdir -p "$SOURCE/test/fixtures"
printf '%s\n' '<!doctype html><title>Static query</title>' >"$SOURCE/test/fixtures/mobile-custom-selection.html"
cp "$SOURCE/test/fixtures/mobile-custom-selection.html" "$SOURCE/hello world.html"
cp "$SOURCE/test/fixtures/mobile-custom-selection.html" "$SOURCE/question?.html"
cp "$SOURCE/test/fixtures/mobile-custom-selection.html" "$SOURCE/hash#.html"
MESH_STATE_DIR="$ORIGIN_STATE" "$MESH_APP" app create local "$SOURCE" --json >"$TEST_ROOT/create.json" || fail 'create static app'
APP_ID=$(python3 -c 'import json, sys; print(json.load(open(sys.argv[1]))["app"]["id"])' "$TEST_ROOT/create.json") || fail 'creation result'
for path in '/test/fixtures/mobile-custom-selection.html' '/test/fixtures/mobile-custom-selection.html?' '/test/fixtures/mobile-custom-selection.html?a=1' '/test/fixtures/mobile-custom-selection.html?escape=%zz' '/hello%20world.html?' '/question%3F.html?' '/hash%23.html?a=1'; do
  # --request-target preserves the empty query marker on curl versions that drop it.
  app_curl --fail --request-target "$path" "$(app_endpoint)$path" >"$TEST_ROOT/body" || fail "serve $path"
  cmp "$SOURCE/test/fixtures/mobile-custom-selection.html" "$TEST_ROOT/body" || fail "body for $path"
done
for path in '/%2e%2e/secret?' '/%252e%252e/secret?a=1' '/%00?'; do
  status=$(app_curl --request-target "$path" -o "$TEST_ROOT/body" -w '%{http_code}' "$(app_endpoint)$path") || fail "request $path"
  [[ $status == 400 || $status == 404 ]] || fail "unsafe path $path returned $status"
done
echo 'PASS: real static app serves pathname-only files with empty and nonempty queries and rejects unsafe paths'
