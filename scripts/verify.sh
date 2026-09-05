#!/usr/bin/env bash
# Build Mesh once, then run every integration test concurrently against that
# immutable binary. Each integration owns its own temporary state directory.
set -uo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
run_root=$(mktemp -d "${TMPDIR:-/tmp}/mesh-verify.XXXXXX")
test_timeout=${MESH_INTEGRATION_TIMEOUT:-30s}
# packaging_contract.sh builds real release archives, so its cost tracks the
# runner rather than the code and it cannot share a session-test budget.
slow_test_timeout=${MESH_INTEGRATION_SLOW_TIMEOUT:-600s}
slow_tests=" packaging_contract.sh "

cleanup() {
  rm -rf -- "$run_root"
}
trap cleanup EXIT

binary="$run_root/mesh"
if ! (cd "$repo_root" && go build -o "$binary" ./cmd/mesh); then
  echo "FAIL: build" >&2
  exit 1
fi

tests=("$repo_root"/integration/*.sh)
if [ ! -e "${tests[0]}" ]; then
  echo "FAIL: no integration scripts found" >&2
  exit 1
fi

declare -a names=()
declare -a logs=()
declare -a pids=()

for test_path in "${tests[@]}"; do
  name=$(basename "$test_path")
  log="$run_root/$name.log"
  config_dir="$run_root/config/$name"
  mkdir -p "$config_dir"
  names+=("$name")
  logs+=("$log")
  (
    cd "$repo_root" || exit 1
    this_timeout=$test_timeout
    case "$slow_tests" in
      *" $name "*) this_timeout=$slow_test_timeout ;;
    esac
    # Integration tests create their own state. Give each one an equally
    # isolated address book and no inherited nesting identity, so running this
    # verifier from inside Mesh cannot change its detach key or catalog shape.
    timeout --kill-after=5s "$this_timeout" \
      env -u MESH_DEPTH -u MESH_HOST_ID -u MESH_SESSION_ID \
      MESH="$binary" MESH_CONFIG_DIR="$config_dir" bash "$test_path"
  ) >"$log" 2>&1 &
  pids+=("$!")
done

failed=0
for i in "${!pids[@]}"; do
  if wait "${pids[$i]}"; then
    printf 'PASS: %s\n' "${names[$i]}"
    continue
  else
    status=$?
  fi
  failed=1
  printf 'FAIL: %s (exit %d)\n' "${names[$i]}" "$status" >&2
  sed 's/^/  /' "${logs[$i]}" >&2
done

exit "$failed"
