#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 4 ]]; then
  echo 'usage: prove-release-transition.sh OLD-BINARY CANDIDATE-BINARY OS/ARCH OUTPUT-DIRECTORY' >&2
  exit 2
fi

old_binary=$(realpath "$1")
candidate_binary=$(realpath "$2")
platform=$3
output_directory=$4
proof_mode=${MESH_TRANSITION_PROOF_MODE:-full}
case $proof_mode in
  full | forward-only) ;;
  *) echo 'transition proof: mode must be full or forward-only' >&2; exit 2 ;;
esac
case $platform in
  linux/amd64 | linux/arm64 | darwin/arm64) ;;
  *) echo "transition proof: unsupported platform $platform" >&2; exit 1 ;;
esac
[[ -x $old_binary && -x $candidate_binary ]] || {
  echo 'transition proof: both inputs must be executable files' >&2
  exit 1
}

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
fixture=$repo_root/scripts/fixtures/release_transition.py
short_tmp=${MESH_SHORT_TMP:-${TMPDIR:-/tmp}}
test_root=$(mktemp -d "$short_tmp/mesh-transition.XXXXXX")
state=$test_root/state
config=$test_root/config
old_daemon=''
candidate_daemon=''
client=''
session=''
shell_pid=''
worker_pid=''
helper_pid=''

cleanup() {
  discover_owned_session_processes
  [[ ! -f $test_root/managed-daemon.pid ]] || old_daemon=$(cat "$test_root/managed-daemon.pid")
  [[ -z $helper_pid ]] || kill -9 "$helper_pid" 2>/dev/null || true
  [[ -z $client ]] || kill -9 "$client" 2>/dev/null || true
  [[ -z $candidate_daemon ]] || kill -9 "$candidate_daemon" 2>/dev/null || true
  [[ -z $old_daemon ]] || kill -9 "$old_daemon" 2>/dev/null || true
  [[ -z $shell_pid ]] || kill -9 "$shell_pid" 2>/dev/null || true
  [[ -z $worker_pid ]] || kill -9 "$worker_pid" 2>/dev/null || true
  rm -rf -- "$test_root"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

discover_owned_session_processes() {
  local pid ppid command
  worker_pid=''
  shell_pid=''
  while read -r pid ppid command; do
    case $command in
      *session-worker*"$state/s/"*) worker_pid=$pid ;;
    esac
  done < <(ps -axww -o pid=,ppid=,command= 2>/dev/null || true)
  [[ -n $worker_pid && -z $shell_pid ]] || return 0
  while read -r pid ppid command; do
    if [[ $ppid == "$worker_pid" ]]; then
      shell_pid=$pid
      return 0
    fi
  done < <(ps -axww -o pid=,ppid=,command= 2>/dev/null || true)
}

dump_diagnostics() {
  local path
  for path in "$test_root/client.err" "$test_root/client.out" "$test_root/old-first.log" \
    "$test_root/candidate.err" "$test_root/candidate.out" "$test_root/candidate.log" \
    "$test_root/restart.err" "$test_root/restart.out" "$test_root/candidate-restart.log" \
    "$test_root"/*.inventory.out "$test_root"/*.inventory.err "$state"/s/*/worker.log; do
    [[ -s $path ]] || continue
    printf '%s (last 4096 bytes):\n' "$path" >&2
    tail -c 4096 "$path" | sed 's/^/  /' >&2
    printf '\n' >&2
  done
}

fail() {
  echo "transition proof: $*" >&2
  dump_diagnostics
  exit 1
}

wait_for_daemon() {
  local binary=$1
  local pid=$2
  for _ in $(seq 120); do
    kill -0 "$pid" 2>/dev/null || return 1
    if [[ -S $state/daemon.sock ]] && mesh "$binary" ls --daemon >/dev/null 2>&1; then
      return 0
    fi
    sleep 0.05
  done
  return 1
}

mesh() {
  env MESH_STATE_DIR="$state" MESH_CONFIG_DIR="$config" "$@"
}

read_state_version() {
  python3 - "$state/mesh.db" <<'PY'
import sqlite3
import sys

with sqlite3.connect(f"file:{sys.argv[1]}?mode=ro", uri=True) as database:
    row = database.execute(
        "SELECT COALESCE(MAX(version_id), 0) FROM goose_db_version WHERE is_applied = 1"
    ).fetchone()
print(row[0])
PY
}

build_field() {
  local binary=$1
  local field=$2
  local fallback=$3
  local metadata
  if metadata=$("$binary" version --json 2>/dev/null); then
    python3 -c 'import json,sys; print(json.load(sys.stdin)[sys.argv[1]])' "$field" <<<"$metadata"
    return
  fi
  printf '%s\n' "$fallback"
}

wait_for_token() {
  local path=$1
  local token=$2
  for _ in $(seq 120); do
    if python3 - "$path" "$token" <<'PY'
import pathlib
import sys

lines = pathlib.Path(sys.argv[1]).read_bytes().replace(b"\r", b"").splitlines()
raise SystemExit(0 if sys.argv[2].encode() in lines else 1)
PY
    then
      return 0
    fi
    sleep 0.05
  done
  return 1
}

wait_for_exit() {
  local pid=$1
  for _ in $(seq 120); do
    kill -0 "$pid" 2>/dev/null || return 0
    sleep 0.05
  done
  return 1
}

send_shell_command() {
  local fd=$1
  local command=$2
  printf '%s\n' "$command" >&"$fd"
}

send_token() {
  local fd=$1
  local token=$2
  send_shell_command "$fd" "printf '\\n%s\\n' '$token'"
}

assert_session_processes() {
  kill -0 "$shell_pid" 2>/dev/null || fail 'retained shell is not alive'
  kill -0 "$worker_pid" 2>/dev/null || fail 'retained worker server is not alive'
  [[ $(ps -o ppid= -p "$shell_pid" | tr -d '[:space:]') == "$worker_pid" ]] ||
    fail 'retained shell is no longer owned by the original worker server'
}

assert_live_recovery() {
  python3 - "$state" "$session" <<'PY'
import json
import pathlib
import socket
import sqlite3
import struct
import sys

state = pathlib.Path(sys.argv[1])
session = sys.argv[2]
with sqlite3.connect(state / "mesh.db") as database:
    host = database.execute("SELECT id FROM hosts ORDER BY id LIMIT 1").fetchone()[0]
record = json.loads((state / "s" / session / "recovery.json").read_bytes())
assert record["version"] == 1 and record["hostId"] == host and record["sessionId"] == session, record
request = {"type": "session.recovery", "sessionId": session, "requestId": "transition-proof-recovery"}
payload = json.dumps(request).encode()
with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as connection:
    connection.settimeout(10)
    connection.connect(str(state / "daemon.sock"))
    connection.sendall(b"\x01" + struct.pack(">I", len(payload)) + payload)
    with connection.makefile("rb") as stream:
        header = stream.read(5)
        assert len(header) == 5 and header[0] == 1, header
        length = struct.unpack(">I", header[1:])[0]
        assert 0 < length <= 4 << 20, length
        response = json.loads(stream.read(length))
assert response["type"] == "session.recovery-record", response
accepted = response["recovery"]
assert accepted["version"] == 1 and accepted["hostId"] == host and accepted["sessionId"] == session, response
assert accepted["checkpointAt"] != "0001-01-01T00:00:00Z", response
PY
}

stop_daemon() {
  local pid=$1
  local binary=$2
  kill -TERM "$pid" 2>/dev/null || true
  for _ in $(seq 120); do
    kill -0 "$pid" 2>/dev/null || break
    sleep 0.05
  done
  if kill -0 "$pid" 2>/dev/null; then
    kill -9 "$pid" 2>/dev/null || true
  fi
  wait "$pid" 2>/dev/null || true
  for _ in $(seq 120); do
    if ! mesh "$binary" ls --daemon >/dev/null 2>&1; then
      rm -f -- "$state/daemon.sock"
      return 0
    fi
    sleep 0.05
  done
  fail 'daemon socket remained live after its process exited'
}


fixture() {
  python3 "$fixture" "$@"
}

restore_test_database() {
  rm -f -- "$state/mesh.db" "$state/mesh.db-wal" "$state/mesh.db-shm"
  fixture snapshot "$test_root/source.db" "$state/mesh.db"
}

assert_saved_recovery() {
  cmp -s "$test_root/saved-recovery.json" "$state/s/$saved_session/recovery.json" ||
    fail "rollback changed saved recovery at $boundary/$scenario"
}

rollback_source() {
  local label=$1 candidate_image=${2:-$candidate_binary}
  local retained_worker_pid=$worker_pid retained_shell_pid=$shell_pid
  fixture prepare-rollback "$state" "$old_binary" "$candidate_image" "$label"
  env PATH="$test_root/service-tools:$PATH" MESH_STATE_DIR="$state" MESH_CONFIG_DIR="$config" \
    "$candidate_binary" update-helper --state-dir "$state" >"$test_root/helper-$label.log" 2>&1 &
  helper_pid=$!
  wait_for_helper_rollback "$label"
  old_daemon=$(cat "$test_root/managed-daemon.pid")
  wait_for_daemon "$old_binary" "$old_daemon" || fail "source did not start at $label: $(cat "$test_root/managed-daemon.log")"
  fixture verify-rollback "$state" || fail "helper rollback did not preserve source and worker identity at $label"
  kill -TERM "$helper_pid"
  wait "$helper_pid" 2>/dev/null || true
  helper_pid=''
  mesh "$old_binary" ls --daemon --all >"$test_root/source-$label.inventory.out" || fail "source inventory failed at $label"
  grep -Fq "$session" "$test_root/source-$label.inventory.out" || fail "source lost live session at $label"
  grep -Fq "$saved_session" "$test_root/source-$label.inventory.out" || fail "source lost saved session at $label"
  assert_session_processes
  mkfifo "$test_root/source-$label.input"
  mesh "$old_binary" attach "$session" --daemon --detach-key=ctrl+] <"$test_root/source-$label.input" >"$test_root/source-$label.out" 2>"$test_root/source-$label.err" &
  client=$!
  exec 6>"$test_root/source-$label.input"
  send_token 6 "ROLLBACK_${label}_$session"
  wait_for_token "$test_root/source-$label.out" "ROLLBACK_${label}_$session" || fail "source session I/O failed at $label"
  assert_session_processes
  assert_live_recovery || fail "source did not accept retained checkpoint at $label"
  assert_saved_recovery
  if [[ -f $test_root/$label.persist.json ]]; then
    fixture verify-persistent "$state" "$test_root/$label.persist.json" || fail "source rollback lost candidate writes at $label"
  fi
  printf '\035' >&6
  exec 6>&-
  wait_for_exit "$client" || fail "source client did not detach at $label"
  wait "$client" 2>/dev/null || true
  client=''
  if [[ $label == "$candidate_state_version-candidate-writes" ]]; then
    fixture verify-writes "$state" || fail 'source could not read candidate persistent settings and service'
    mesh "$old_binary" kill "$session" >/dev/null
    shell_pid=''
    worker_pid=''
  fi
  stop_daemon "$old_daemon" "$old_binary"
  old_daemon=''
  rm -f -- "$test_root/managed-daemon.pid"
  printf 'rollback boundary %s passed, worker %s, shell %s\n' "$label" "$retained_worker_pid" "$retained_shell_pid" >&2
}

wait_for_helper_rollback() {
  local label=$1 phase
  for _ in $(seq 120); do
    kill -0 "$helper_pid" 2>/dev/null || fail "helper exited before rollback at $label"
    phase=$(fixture rollback-phase "$state")
    [[ $phase != rollback_failed ]] || fail "source did not start at $label: $(cat "$test_root/managed-daemon.log")"
    [[ $phase != rolled_back ]] || return 0
    sleep 0.05
  done
  fail "helper rollback did not finish at $label: $(cat "$test_root/helper-$label.log")"
}

apply_boundary() {
  local target=$1 fault=$2 expected=$3 status=0
  "$boundary_tool" -database "$state/mesh.db" -version "$target" -fault "$fault" >"$test_root/boundary.log" 2>&1 || status=$?
  [[ $status == "$expected" ]] || fail "boundary driver $target/$fault exited $status, expected $expected: $(cat "$test_root/boundary.log")"
}

prove_boundaries() {
  for boundary in $(seq "$old_state_version" "$candidate_state_version"); do
    for scenario in before after-commit failure interrupt; do
      prove_boundary
    done
  done
  restore_test_database
}

prove_boundary() {
  local target=$boundary
  restore_test_database
  apply_boundary "$boundary" none 0
  if [[ $scenario == after-commit ]]; then
    restore_test_database
    apply_boundary "$boundary" after-commit 86
  elif [[ $scenario == failure || $scenario == interrupt ]]; then
    target=$((boundary + 1))
    [[ $target -le $candidate_state_version ]] || return 0
    apply_boundary_fault "$target" "$scenario"
  fi
  [[ $(read_state_version) == "$boundary" ]] || fail "fault left unexpected state at $boundary/$scenario"
  fixture boundary-writes "$state/mesh.db"
  if [[ $boundary == "$candidate_state_version" ]]; then
    fixture schema "$state/mesh.db" "$test_root/boundary-final-schema.json"
  fi
  rollback_source "$boundary-$scenario"
}

apply_boundary_fault() {
  local target=$1 fault=$2 expected=1
  [[ $fault != interrupt ]] || expected=86
  apply_boundary "$target" "$fault" "$expected"
}

prove_actual_startup_faults() {
  [[ -n ${MESH_STARTUP_FAULT_BINARY:-} ]] || return 0
  fixture startup-identity "$old_binary" "$candidate_binary" "$MESH_STARTUP_FAULT_BINARY"
  local version side
  for version in $(seq "$((old_state_version + 1))" "$candidate_state_version"); do
    for side in before after; do
      prove_startup_commit_side "$version" "$side"
    done
  done
  restore_test_database
}

prove_startup_commit_side() {
  local version=$1 side=$2 mode label expected status wanted
  for mode in failure interrupt; do
    restore_test_database
    label="startup-$version-$side-$mode"
    expected=1
    [[ $mode != interrupt ]] || expected=86
    status=0
    env MESH_STATE_DIR="$state" MESH_CONFIG_DIR="$config" MESH_STARTUP_FAULT_VERSION="$version" \
      MESH_STARTUP_FAULT_SIDE="$side" MESH_STARTUP_FAULT_MODE="$mode" \
      "$MESH_STARTUP_FAULT_BINARY" daemon --tailnet-port=0 --ssh-port=0 >"$test_root/$label.log" 2>&1 || status=$?
    [[ $status == "$expected" ]] || fail "actual candidate startup $label exited $status: $(cat "$test_root/$label.log")"
    grep -Fq "event=armed version=$version side=$side mode=$mode " "$test_root/$label.log" || fail "startup hook did not arm at $label"
    grep -Fq "event=fired version=$version side=$side mode=$mode " "$test_root/$label.log" || fail "startup hook did not fire at $label"
    grep -F 'mesh startup commit hook ' "$test_root/$label.log" >&2
    wanted=$version
    [[ $side != before ]] || wanted=$((version - 1))
    boundary=$(read_state_version)
    [[ $boundary == "$wanted" ]] || fail "actual startup $label left state $boundary, expected $wanted"
    scenario=$label
    fixture persistent-snapshot "$state" "$test_root/$label.persist.json"
    rollback_source "$label" "$MESH_STARTUP_FAULT_BINARY"
  done
}

mkdir -p "$state" "$config"
boundary_tool=${MESH_BOUNDARY_TOOL:-$test_root/boundary}
if [[ -z ${MESH_BOUNDARY_TOOL:-} ]]; then
  (cd "$repo_root" && go build -o "$boundary_tool" ./internal/storage/cmd/boundary)
fi
mkfifo "$test_root/input"
env MESH_STATE_DIR="$state" MESH_CONFIG_DIR="$config" "$old_binary" daemon --tailnet-port=0 --ssh-port=0 >"$test_root/old-first.log" 2>&1 &
old_daemon=$!
wait_for_daemon "$old_binary" "$old_daemon" || fail "retained daemon did not start: $(cat "$test_root/old-first.log")"

# Polling must not depend on the background shell opening its redirections first.
touch "$test_root/client.err"
mesh "$old_binary" local --daemon --detach-key=ctrl+] -- /bin/sh <"$test_root/input" >"$test_root/client.out" 2>"$test_root/client.err" &
client=$!
exec 3>"$test_root/input"
for _ in $(seq 120); do
  session=$(sed -n 's/^new session \([0-9A-Z][0-9A-Z][0-9A-Z][0-9A-Z]\)$/\1/p' "$test_root/client.err" | head -n1)
  [[ -z $session ]] || break
  sleep 0.05
done
[[ -n $session ]] || fail "retained daemon did not create a session: $(cat "$test_root/client.err")"
shell_pid=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["pid"])' "$state/s/$session/meta.json") ||
  fail 'could not identify retained shell'
worker_pid=$(ps -o ppid= -p "$shell_pid" | tr -d '[:space:]') || fail 'could not identify retained worker server'
[[ $worker_pid =~ ^[1-9][0-9]*$ ]] || fail 'could not identify retained worker server'
send_shell_command 3 'PS1= PS2=; stty -echo'
send_token 3 "RETAINED_INITIAL_$session"
wait_for_token "$test_root/client.out" "RETAINED_INITIAL_$session" || fail 'retained worker did not answer'
assert_session_processes
printf '\035' >&3
wait_for_exit "$client" || fail 'initial client did not detach'
wait "$client" 2>/dev/null || true
client=''
exec 3>&-
stop_daemon "$old_daemon" "$old_binary"
old_daemon=''
old_state_version=$(read_state_version)
old_worker_protocol=$(build_field "$old_binary" workerProtocol 1)

# Live checkpoints belong to the worker and must remain free to change.
saved_session=$(python3 - "$state" "$session" <<'PY'
import datetime
import json
import pathlib
import sqlite3
import sys

state = pathlib.Path(sys.argv[1])
session = sys.argv[2]
saved_session = "0000" if session != "0000" else "0001"
saved_dir = state / "s" / saved_session
saved_dir.mkdir(mode=0o700)
checkpoint_at = datetime.datetime.now(datetime.timezone.utc).isoformat().replace("+00:00", "Z")
meta = json.loads((state / "s" / session / "meta.json").read_text())
meta.update(id=saved_session, state="exited", exitedAt=checkpoint_at, exitCode=0)
meta.pop("detachedAt", None)
meta.pop("lastAttachedAt", None)
(saved_dir / "meta.json").write_text(json.dumps(meta, sort_keys=True) + "\n")
with sqlite3.connect(state / "mesh.db") as database:
    host = database.execute("SELECT id FROM hosts ORDER BY id LIMIT 1").fetchone()[0]
record = {
    "version": 1,
    "hostId": host,
    "sessionId": saved_session,
    "checkpointAt": checkpoint_at,
    "shell": "/bin/sh",
    "shellDirectory": str(state),
    "directorySource": "launch",
    "title": "release transition proof",
    "lines": ["READY"],
    "command": ["/bin/sh"],
}
(saved_dir / "recovery.json").write_text(json.dumps(record, sort_keys=True) + "\n")
print(saved_session)
PY
)
cp "$state/s/$saved_session/recovery.json" "$test_root/saved-recovery.json"
fixture snapshot "$state/mesh.db" "$test_root/source.db"
recovery_before=$(sha256sum "$state/s/$saved_session/recovery.json" 2>/dev/null | awk '{print $1}' || shasum -a 256 "$state/s/$saved_session/recovery.json" | awk '{print $1}')

candidate_state_version=$(build_field "$candidate_binary" stateVersion 0)
candidate_worker_protocol=$(build_field "$candidate_binary" workerProtocol 0)
[[ $candidate_state_version =~ ^[1-9][0-9]*$ && $candidate_worker_protocol =~ ^[1-9][0-9]*$ ]] ||
  fail 'candidate did not report release compatibility metadata'
[[ $candidate_state_version -ge $old_state_version ]] || fail 'candidate schema predates source schema'
if [[ $proof_mode == full ]]; then
  prove_boundaries
  prove_actual_startup_faults
fi
env MESH_STATE_DIR="$state" MESH_CONFIG_DIR="$config" "$candidate_binary" daemon --tailnet-port=0 --ssh-port=0 >"$test_root/candidate.log" 2>&1 &
candidate_daemon=$!
wait_for_daemon "$candidate_binary" "$candidate_daemon" || fail "candidate daemon did not open retained state: $(cat "$test_root/candidate.log")"
fixture schema "$state/mesh.db" "$test_root/candidate-final-schema.json"
if [[ $proof_mode == full ]]; then
  cmp -s "$test_root/boundary-final-schema.json" "$test_root/candidate-final-schema.json" || fail 'boundary driver schema/history differs from actual candidate startup'
fi
fixture candidate-writes "$state" || fail 'candidate representative writes failed'
"$boundary_tool" -database "$state/mesh.db" -version "$candidate_state_version" -write-cache || fail 'candidate cache writes failed'
fixture verify-cache "$state/mesh.db"
candidate_written_state=$(read_state_version)
[[ $candidate_written_state == "$candidate_state_version" ]] || fail 'candidate database schema differs from its reported state version'
mesh "$candidate_binary" ls --daemon >"$test_root/candidate-live.inventory.out" 2>"$test_root/candidate-live.inventory.err" || fail 'candidate live inventory command failed'
grep -Fq "$session" "$test_root/candidate-live.inventory.out" || fail 'candidate did not inventory retained session'
mesh "$candidate_binary" ls --daemon --all >"$test_root/candidate-all.inventory.out" 2>"$test_root/candidate-all.inventory.err" || fail 'candidate saved inventory command failed'
grep -Fq "$saved_session" "$test_root/candidate-all.inventory.out" || fail 'candidate did not inventory saved recovery session'
assert_session_processes
mkfifo "$test_root/candidate-input"
mesh "$candidate_binary" attach "$session" --daemon --detach-key=ctrl+] <"$test_root/candidate-input" >"$test_root/candidate.out" 2>"$test_root/candidate.err" &
client=$!
exec 4>"$test_root/candidate-input"
send_token 4 "CANDIDATE_REATTACH_$session"
wait_for_token "$test_root/candidate.out" "CANDIDATE_REATTACH_$session" || fail 'candidate could not exchange data with retained worker'
assert_session_processes
assert_live_recovery || fail 'candidate did not accept retained worker checkpoint'
printf '\035' >&4
wait_for_exit "$client" || fail 'candidate client did not detach'
wait "$client" 2>/dev/null || true
client=''
exec 4>&-
stop_daemon "$candidate_daemon" "$candidate_binary"
candidate_daemon=''

env MESH_STATE_DIR="$state" MESH_CONFIG_DIR="$config" "$candidate_binary" daemon --tailnet-port=0 --ssh-port=0 >"$test_root/candidate-restart.log" 2>&1 &
candidate_daemon=$!
wait_for_daemon "$candidate_binary" "$candidate_daemon" || fail "candidate restart could not reopen candidate-written state: $(cat "$test_root/candidate-restart.log")"
[[ $(read_state_version) == "$candidate_written_state" ]] || fail 'candidate restart changed candidate-written schema'
mesh "$candidate_binary" ls --daemon >"$test_root/restart-live.inventory.out" 2>"$test_root/restart-live.inventory.err" || fail 'candidate restart live inventory command failed'
grep -Fq "$session" "$test_root/restart-live.inventory.out" || fail 'candidate restart lost the live session after candidate writes'
mesh "$candidate_binary" ls --daemon --all >"$test_root/restart-all.inventory.out" 2>"$test_root/restart-all.inventory.err" || fail 'candidate restart saved inventory command failed'
grep -Fq "$saved_session" "$test_root/restart-all.inventory.out" || fail 'candidate restart lost saved recovery session after candidate writes'
assert_session_processes
mkfifo "$test_root/restart-input"
mesh "$candidate_binary" attach "$session" --daemon --detach-key=ctrl+] <"$test_root/restart-input" >"$test_root/restart.out" 2>"$test_root/restart.err" &
client=$!
exec 5>"$test_root/restart-input"
send_token 5 "CANDIDATE_RESTART_$session"
wait_for_token "$test_root/restart.out" "CANDIDATE_RESTART_$session" || fail 'candidate restart could not exchange data after restart'
assert_session_processes
assert_live_recovery || fail 'candidate restart did not accept worker checkpoint after restart'
recovery_after=$(sha256sum "$state/s/$saved_session/recovery.json" 2>/dev/null | awk '{print $1}' || shasum -a 256 "$state/s/$saved_session/recovery.json" | awk '{print $1}')
[[ $recovery_after == "$recovery_before" ]] || fail 'recovery record changed across candidate upgrade and restart'
printf '\035' >&5
exec 5>&-
wait_for_exit "$client" || fail 'restart client did not detach'
wait "$client" 2>/dev/null || true
client=''
kill -KILL "$candidate_daemon"
wait "$candidate_daemon" 2>/dev/null || true
candidate_daemon=''
rm -f -- "$state/daemon.sock"
boundary=$candidate_written_state
scenario=candidate-writes
if [[ $proof_mode == full ]]; then
  rollback_source "$boundary-$scenario"
fi

from_digest=$(sha256sum "$old_binary" 2>/dev/null | awk '{print $1}' || shasum -a 256 "$old_binary" | awk '{print $1}')
to_digest=$(sha256sum "$candidate_binary" 2>/dev/null | awk '{print $1}' || shasum -a 256 "$candidate_binary" | awk '{print $1}')
mkdir -p "$output_directory"
temporary=$output_directory/.receipt.$$.json
python3 - "$temporary" "$platform" "$from_digest" "$to_digest" \
  "$old_state_version" "$candidate_state_version" "$candidate_written_state" \
  "$old_worker_protocol" "$candidate_worker_protocol" <<'PY'
import json
import pathlib
import sys

os_name, arch = sys.argv[2].split("/", 1)
receipt = {
    "schema": 1,
    "platform": {"os": os_name, "arch": arch},
    "fromDigest": sys.argv[3],
    "toDigest": sys.argv[4],
    "stateReadMin": int(sys.argv[5]),
    "stateReadMax": int(sys.argv[6]),
    "stateWrite": int(sys.argv[7]),
    "workerMin": int(sys.argv[8]),
    "workerMax": int(sys.argv[9]),
    "workerWrite": int(sys.argv[9]),
    "journalVersion": 1,
    "candidateOpenedRetainedState": True,
    "sessionsPreserved": True,
    "recoveryRecordsPreserved": True,
}
pathlib.Path(sys.argv[1]).write_text(json.dumps(receipt, separators=(",", ":"), sort_keys=True))
PY
proof_digest=$(sha256sum "$temporary" 2>/dev/null | awk '{print $1}' || shasum -a 256 "$temporary" | awk '{print $1}')
mv "$temporary" "$output_directory/$proof_digest.json"
if [[ $proof_mode == forward-only ]]; then
  printf '%s\n' "$proof_digest"
  exit 0
fi
mkdir -p "$output_directory/rollback"
python3 - "$output_directory" "$proof_digest" "$platform" "$from_digest" "$to_digest" "$old_state_version" "$candidate_state_version" <<'PYROLLBACK'
import hashlib
import json
from pathlib import Path
import sys

output, forward, platform, source, target, first, last = sys.argv[1:]
os_name, arch = platform.split("/")
proof = {
    "schema": 1, "platform": {"os": os_name, "arch": arch},
    "fromDigest": source, "toDigest": target, "forward": forward,
    "workers": [source],
    "boundaries": [{"state": state, "candidateWrote": True, "sourceStarted": True,
                    "sessionIO": True, "recoveryPreserved": True}
                   for state in range(int(first), int(last) + 1)],
}
contents = json.dumps(proof, separators=(",", ":"), sort_keys=True).encode()
digest = hashlib.sha256(contents).hexdigest()
(Path(output) / "rollback" / (digest + ".rollback.json")).write_bytes(contents)
print("rollback proof " + digest, file=sys.stderr)
PYROLLBACK
"$boundary_tool" -verify-proofs "$output_directory" || fail 'generated rollback proof did not verify'
printf '%s\n' "$proof_digest"
