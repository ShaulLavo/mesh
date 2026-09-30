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

caller_user_bus=0
if [[ $(uname -s) == Linux ]] && busctl --user status >/dev/null 2>&1; then
  caller_user_bus=1
fi

# Version-manager shims depend on HOME. Resolve Go before replacing it, and
# reuse compiler caches so isolation does not turn warm checks into cold builds.
go_root=$(cd "$repo_root" && go env GOROOT) || exit 1
go_cache=$(cd "$repo_root" && go env GOCACHE) || exit 1
go_modules=$(cd "$repo_root" && go env GOMODCACHE) || exit 1
test_env=(
  "PATH=$go_root/bin:$PATH" "TMPDIR=${TMPDIR:-/tmp}"
  "TERM=${TERM:-dumb}" "LANG=${LANG:-C}"
  "GOCACHE=$go_cache" "GOMODCACHE=$go_modules"
)
for name in XDG_RUNTIME_DIR DBUS_SESSION_BUS_ADDRESS; do
  if [[ ${!name+x} ]]; then
    test_env+=("$name=${!name}")
  fi
done
build_env=("${test_env[@]}")
# Module downloads need the caller's network policy; loopback fixtures do not.
for name in GOPROXY GONOSUMDB GONOPROXY GOPRIVATE GOFLAGS GOINSECURE GOSUMDB; do
  value=$(cd "$repo_root" && go env "$name") || exit 1
  # An exported empty value is still an explicit caller override.
  if [[ ${!name+x} ]]; then
    value=${!name}
  fi
  build_env+=("$name=$value")
done
for name in HTTPS_PROXY HTTP_PROXY NO_PROXY ALL_PROXY https_proxy http_proxy no_proxy all_proxy \
  SSL_CERT_FILE SSL_CERT_DIR; do
  if [[ ${!name+x} ]]; then
    build_env+=("$name=${!name}")
  fi
done
netrc=${NETRC:-"$HOME/.netrc"}
if [[ -n ${NETRC:-} || -f $netrc ]]; then
  netrc=$(cd "$repo_root" && python3 -c 'import os, sys; print(os.path.abspath(sys.argv[1]))' "$netrc") || exit 1
  build_env+=("NETRC=$netrc")
fi
build_home="$run_root/build-home"
mkdir -p "$build_home"

binary="$run_root/mesh"
if ! (cd "$repo_root" && env -i "${build_env[@]}" HOME="$build_home" go build -o "$binary" ./cmd/mesh); then
  echo "FAIL: build" >&2
  exit 1
fi
integration_binary="$run_root/mesh-integration"
if ! (cd "$repo_root" && env -i "${build_env[@]}" HOME="$build_home" go build -tags mesh_integration -o "$integration_binary" ./cmd/mesh); then
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
  home_dir="$run_root/home/$name"
  mkdir -p "$config_dir" "$home_dir"
  names+=("$name")
  logs+=("$log")
  (
    cd "$repo_root" || exit 1
    this_timeout=$test_timeout
    case "$slow_tests" in
      *" $name "*) this_timeout=$slow_test_timeout ;;
    esac
    # Provider routing, proxies and shell startup files must come from fixtures,
    # not the developer's environment.
    script_env=("${test_env[@]}" "HOME=$home_dir" "MESH=$binary"
      "MESH_INTEGRATION_BINARY=$integration_binary" "MESH_CONFIG_DIR=$config_dir")
    # Losing an available bus would turn the scope assertions into a false pass.
    if [[ $name == session_scope.sh ]] && (( caller_user_bus )) &&
      ! env -i "${script_env[@]}" busctl --user status >/dev/null 2>&1; then
      echo "FAIL: session_scope.sh lost the caller's user bus" >&2
      exit 1
    fi
    timeout --kill-after=5s "$this_timeout" \
      env -i "${script_env[@]}" bash "$test_path"
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
