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
case $platform in
  linux/amd64 | linux/arm64 | darwin/arm64) ;;
  *) echo "transition proof: unsupported platform $platform" >&2; exit 1 ;;
esac
[[ -x $old_binary && -x $candidate_binary ]] || {
  echo 'transition proof: both inputs must be executable files' >&2
  exit 1
}

short_tmp=${MESH_SHORT_TMP:-/tmp}
test_root=$(mktemp -d "$short_tmp/mesh-transition.XXXXXX")
state=$test_root/state
config=$test_root/config
old_daemon=''
candidate_daemon=''
client=''
session=''
shell_pid=''
worker_pid=''

cleanup() {
  discover_owned_session_processes
  [[ -z $client ]] || kill -9 "$client" 2>/dev/null || true
  [[ -z $candidate_daemon ]] || kill -9 "$candidate_daemon" 2>/dev/null || true
  [[ -z $old_daemon ]] || kill -9 "$old_daemon" 2>/dev/null || true
  [[ -z $shell_pid ]] || kill -9 "$shell_pid" 2>/dev/null || true
  [[ -z $worker_pid ]] || kill -9 "$worker_pid" 2>/dev/null || true
  rm -rf -- "$test_root"
}
trap cleanup EXIT

discover_owned_session_processes() {
  local pid ppid command
  while read -r pid ppid command; do
    case $command in
      *session-worker*"$state/s/"*) worker_pid=$pid ;;
    esac
  done < <(ps -axo pid=,ppid=,command= 2>/dev/null || true)
  [[ -n $worker_pid && -z $shell_pid ]] || return 0
  while read -r pid ppid command; do
    if [[ $ppid == "$worker_pid" ]]; then
      shell_pid=$pid
      return 0
    fi
  done < <(ps -axo pid=,ppid=,command= 2>/dev/null || true)
}

dump_diagnostics() {
  local path
  for path in "$test_root/client.err" "$test_root/client.out" "$test_root/old-first.log" \
    "$test_root/candidate.err" "$test_root/candidate.out" "$test_root/candidate.log" \
    "$test_root/rollback.err" "$test_root/rollback.out" "$test_root/old-rollback.log" \
    "$state"/s/*/worker.log; do
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

mkdir -p "$state" "$config"
mkfifo "$test_root/input"
env MESH_STATE_DIR="$state" MESH_CONFIG_DIR="$config" "$old_binary" daemon --tailnet-port=0 --ssh-port=0 >"$test_root/old-first.log" 2>&1 &
old_daemon=$!
wait_for_daemon "$old_binary" "$old_daemon" || fail "retained daemon did not start: $(cat "$test_root/old-first.log")"

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

python3 - "$state" "$session" <<'PY'
import datetime
import json
import pathlib
import sqlite3
import sys

state = pathlib.Path(sys.argv[1])
session = sys.argv[2]
with sqlite3.connect(state / "mesh.db") as database:
    host = database.execute("SELECT id FROM hosts ORDER BY id LIMIT 1").fetchone()[0]
record = {
    "version": 1,
    "hostId": host,
    "sessionId": session,
    "checkpointAt": datetime.datetime.now(datetime.timezone.utc).isoformat().replace("+00:00", "Z"),
    "shell": "/bin/sh",
    "shellDirectory": str(state),
    "directorySource": "launch",
    "title": "release transition proof",
    "lines": ["READY"],
    "command": ["/bin/sh"],
}
(state / "s" / session / "recovery.json").write_text(json.dumps(record, sort_keys=True) + "\n")
PY
recovery_before=$(sha256sum "$state/s/$session/recovery.json" 2>/dev/null | awk '{print $1}' || shasum -a 256 "$state/s/$session/recovery.json" | awk '{print $1}')

candidate_state_version=$(build_field "$candidate_binary" stateVersion 0)
candidate_worker_protocol=$(build_field "$candidate_binary" workerProtocol 0)
[[ $candidate_state_version =~ ^[1-9][0-9]*$ && $candidate_worker_protocol =~ ^[1-9][0-9]*$ ]] ||
  fail 'candidate did not report release compatibility metadata'
env MESH_STATE_DIR="$state" MESH_CONFIG_DIR="$config" "$candidate_binary" daemon --tailnet-port=0 --ssh-port=0 >"$test_root/candidate.log" 2>&1 &
candidate_daemon=$!
wait_for_daemon "$candidate_binary" "$candidate_daemon" || fail "candidate daemon did not open retained state: $(cat "$test_root/candidate.log")"
candidate_written_state=$(read_state_version)
[[ $candidate_written_state == "$candidate_state_version" ]] || fail 'candidate database schema differs from its reported state version'
mesh "$candidate_binary" ls --daemon | grep -Fq "$session" || fail 'candidate did not inventory retained session'
assert_session_processes
mkfifo "$test_root/candidate-input"
mesh "$candidate_binary" attach "$session" --daemon --detach-key=ctrl+] <"$test_root/candidate-input" >"$test_root/candidate.out" 2>"$test_root/candidate.err" &
client=$!
exec 4>"$test_root/candidate-input"
send_token 4 "CANDIDATE_REATTACH_$session"
wait_for_token "$test_root/candidate.out" "CANDIDATE_REATTACH_$session" || fail 'candidate could not exchange data with retained worker'
assert_session_processes
printf '\035' >&4
wait_for_exit "$client" || fail 'candidate client did not detach'
wait "$client" 2>/dev/null || true
client=''
exec 4>&-
stop_daemon "$candidate_daemon" "$candidate_binary"
candidate_daemon=''

env MESH_STATE_DIR="$state" MESH_CONFIG_DIR="$config" "$old_binary" daemon --tailnet-port=0 --ssh-port=0 >"$test_root/old-rollback.log" 2>&1 &
old_daemon=$!
wait_for_daemon "$old_binary" "$old_daemon" || fail "retained daemon could not reopen candidate-written state: $(cat "$test_root/old-rollback.log")"
[[ $(read_state_version) == "$candidate_written_state" ]] || fail 'retained daemon changed candidate-written schema'
mesh "$old_binary" ls --daemon | grep -Fq "$session" || fail 'retained daemon lost the live session after candidate writes'
assert_session_processes
mkfifo "$test_root/rollback-input"
mesh "$old_binary" attach "$session" --daemon --detach-key=ctrl+] <"$test_root/rollback-input" >"$test_root/rollback.out" 2>"$test_root/rollback.err" &
client=$!
exec 5>"$test_root/rollback-input"
send_token 5 "RETAINED_ROLLBACK_$session"
wait_for_token "$test_root/rollback.out" "RETAINED_ROLLBACK_$session" || fail 'retained daemon could not exchange data after rollback'
assert_session_processes
recovery_after=$(sha256sum "$state/s/$session/recovery.json" 2>/dev/null | awk '{print $1}' || shasum -a 256 "$state/s/$session/recovery.json" | awk '{print $1}')
[[ $recovery_after == "$recovery_before" ]] || fail 'recovery record changed across candidate and retained daemon startups'
printf '\035' >&5
exec 5>&-
wait_for_exit "$client" || fail 'rollback client did not detach'
wait "$client" 2>/dev/null || true
client=''
mesh "$old_binary" kill "$session" >/dev/null
shell_pid=''
worker_pid=''
stop_daemon "$old_daemon" "$old_binary"
old_daemon=''

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
    "retainedOpenedCandidateState": True,
    "sessionsPreserved": True,
    "recoveryRecordsPreserved": True,
}
pathlib.Path(sys.argv[1]).write_text(json.dumps(receipt, separators=(",", ":"), sort_keys=True))
PY
proof_digest=$(sha256sum "$temporary" 2>/dev/null | awk '{print $1}' || shasum -a 256 "$temporary" | awk '{print $1}')
mv "$temporary" "$output_directory/$proof_digest.json"
printf '%s\n' "$proof_digest"
