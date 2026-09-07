#!/usr/bin/env bash
# Build each Mesh variant once, then run integrations with bounded concurrency.
# Each integration owns its own temporary state directory.
set -uo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
test_timeout=${MESH_INTEGRATION_TIMEOUT:-30s}
# packaging_contract.sh builds real release archives, so its cost tracks the
# runner rather than the code and it cannot share a session-test budget.
slow_test_timeout=${MESH_INTEGRATION_SLOW_TIMEOUT:-600s}
slow_tests=" packaging_contract.sh "
integration_jobs=${MESH_INTEGRATION_JOBS:-}
if [[ -z $integration_jobs ]]; then
  integration_jobs=$(nproc 2>/dev/null || getconf _NPROCESSORS_ONLN 2>/dev/null || echo 2)
  (( integration_jobs <= 4 )) || integration_jobs=4
fi
if [[ ! $integration_jobs =~ ^[1-9][0-9]?$ ]] || (( integration_jobs > 64 )); then
  echo 'FAIL: MESH_INTEGRATION_JOBS must be between 1 and 64' >&2
  exit 1
fi
run_root=$(mktemp -d "${TMPDIR:-/tmp}/mesh-verify.XXXXXX") || exit 1

cleanup() {
  rm -rf -- "$run_root"
}
trap cleanup EXIT

binary="$run_root/mesh"
if ! (cd "$repo_root" && go build -o "$binary" ./cmd/mesh); then
  echo "FAIL: build" >&2
  exit 1
fi
integration_binary="$run_root/mesh-integration"
if ! (cd "$repo_root" && go build -tags mesh_integration -o "$integration_binary" ./cmd/mesh); then
  echo "FAIL: integration build" >&2
  exit 1
fi
printf 'Running integration tests with %s concurrent jobs\n' "$integration_jobs"

tests=("$repo_root"/integration/*.sh)
if [ ! -e "${tests[0]}" ]; then
  echo "FAIL: no integration scripts found" >&2
  exit 1
fi

declare -a names=()
declare -a logs=()
declare -a pids=()
failed=0
next_wait=0

wait_for_test() {
  local i=$1 status
  if wait "${pids[$i]}"; then
    printf 'PASS: %s\n' "${names[$i]}"
    return
  else
    status=$?
  fi
  failed=1
  printf 'FAIL: %s (exit %d)\n' "${names[$i]}" "$status" >&2
  sed 's/^/  /' "${logs[$i]}" >&2
}

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
      MESH="$binary" MESH_INTEGRATION_BINARY="$integration_binary" \
      MESH_CONFIG_DIR="$config_dir" bash "$test_path"
  ) >"$log" 2>&1 &
  pids+=("$!")
  if (( ${#pids[@]} - next_wait >= integration_jobs )); then
    wait_for_test "$next_wait"
    next_wait=$((next_wait + 1))
  fi
done

for (( i=next_wait; i<${#pids[@]}; i++ )); do
  wait_for_test "$i"
done

exit "$failed"
