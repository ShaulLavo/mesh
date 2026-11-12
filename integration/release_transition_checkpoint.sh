#!/usr/bin/env bash
source "$(dirname -- "${BASH_SOURCE[0]}")/helpers/isolate.sh" || exit 1
set -euo pipefail
repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
if [[ -z ${MESH:-} ]]; then
  MESH="$repo_root/mesh"
  (cd "$repo_root" && go build -o "$MESH" ./cmd/mesh)
fi
case $(uname -sm) in
  'Linux x86_64') platform=linux/amd64 ;;
  'Linux aarch64') platform=linux/arm64 ;;
  'Darwin arm64') platform=darwin/arm64 ;;
  *) echo 'FAIL: unsupported release proof platform' >&2; exit 1 ;;
esac
scratch=$(mktemp -d "${TMPDIR:-/tmp}/mesh-proof.XXXXXX")
trap 'rm -rf -- "$scratch"' EXIT
export MESH_PROOF_BINARY
MESH_PROOF_BINARY=$(realpath "$MESH")
export MESH_PROOF_STARTS="$scratch/starts"
export MESH_PROOF_CHECKPOINT="$scratch/checkpoint.json"
export MESH_SHORT_TMP=${MESH_SHORT_TMP:-/tmp}
cat >"$scratch/candidate" <<'WRAPPER'
#!/usr/bin/env bash
set -euo pipefail
if [[ ${1:-} == daemon ]]; then
  python3 - <<'PY'
import json
import os
from pathlib import Path
import subprocess

starts = Path(os.environ["MESH_PROOF_STARTS"])
count = int(starts.read_text()) + 1 if starts.exists() else 1
starts.write_text(str(count))
state = Path(os.environ["MESH_STATE_DIR"])
workers = list((state / "s").glob("*/sock"))
assert len(workers) == 1, workers
worker = workers[0]
checkpoint = worker.parent / "recovery.json"
before = checkpoint.read_bytes()
subprocess.run([os.environ["MESH_PROOF_BINARY"], "recovery-command", worker.parent.name,
                "--cwd", str(state), "--", "/bin/true", "checkpoint while daemon is stopped " + str(count)],
               check=True)
after = checkpoint.read_bytes()
assert after != before, "retained worker did not publish a new checkpoint"
record = json.loads(after)
assert record["restart"]["argv"][1] == "checkpoint while daemon is stopped " + str(count), record
Path(os.environ["MESH_PROOF_CHECKPOINT"]).write_bytes(after)
if os.environ.get("MESH_PROOF_CORRUPT_SAVED") == "1" and count == 2:
    saved = [path for path in (state / "s").glob("*/recovery.json")
             if not (path.parent / "sock").exists()]
    assert len(saved) == 1, saved
    saved[0].write_bytes(saved[0].read_bytes() + b"\n")
PY
fi
exec "$MESH_PROOF_BINARY" "$@"
WRAPPER
chmod +x "$scratch/candidate"
"$repo_root/scripts/prove-release-transition.sh" "$MESH_PROOF_BINARY" "$scratch/candidate" \
  "$platform" "$scratch/proofs" >"$scratch/proof.log" 2>&1 || {
    cat "$scratch/proof.log" >&2
    exit 1
  }
[[ -s $MESH_PROOF_CHECKPOINT ]] || { echo 'FAIL: worker checkpoint was not exercised' >&2; exit 1; }
python3 - "$scratch/proofs" <<'PY'
import json
from pathlib import Path
import sys

directory = Path(sys.argv[1])
proofs = list(directory.glob("*.json"))
assert len(proofs) == 1, proofs
receipt = json.loads(proofs[0].read_bytes())
assert receipt["candidateOpenedRetainedState"], receipt
assert receipt["sessionsPreserved"] and receipt["recoveryRecordsPreserved"], receipt
rollback_paths = list((directory / "rollback").glob("*.rollback.json"))
assert len(rollback_paths) == 1, rollback_paths
rollback = json.loads(rollback_paths[0].read_bytes())
assert rollback["forward"] == proofs[0].stem, rollback
assert rollback["workers"] == [receipt["fromDigest"]], rollback
assert [boundary["state"] for boundary in rollback["boundaries"]] == list(
    range(receipt["stateReadMin"], receipt["stateWrite"] + 1)
), rollback
PY
rm -f -- "$MESH_PROOF_STARTS"
if MESH_PROOF_CORRUPT_SAVED=1 "$repo_root/scripts/prove-release-transition.sh" \
  "$MESH_PROOF_BINARY" "$scratch/candidate" "$platform" "$scratch/corrupt-proofs" \
  >"$scratch/corrupt.log" 2>&1; then
  echo 'FAIL: release proof accepted a changed saved checkpoint' >&2
  exit 1
fi
grep -Fq 'transition proof: recovery record changed across candidate upgrade and restart' "$scratch/corrupt.log" || {
  cat "$scratch/corrupt.log" >&2
  exit 1
}

mkdir "$scratch/migrations"
cp "$repo_root"/db/migrations/*.sql "$scratch/migrations/"
export MESH_PROOF_MIGRATIONS="$scratch/migrations"
export MESH_PROOF_BOUNDARY="$scratch/boundary"
(cd "$repo_root" && go build -o "$MESH_PROOF_BOUNDARY" ./internal/storage/cmd/boundary)
source_version=$("$MESH_PROOF_BINARY" version --json | python3 -c 'import json,sys; print(json.load(sys.stdin)["stateVersion"])')
export MESH_PROOF_INTERMEDIATE=$((source_version + 1))
export MESH_PROOF_FINAL=$((source_version + 2))
cat >"$scratch/boundary-tool" <<'WRAPPER'
#!/usr/bin/env bash
exec "$MESH_PROOF_BOUNDARY" -migrations "$MESH_PROOF_MIGRATIONS" "$@"
WRAPPER
cat >"$scratch/migrating-candidate" <<'WRAPPER'
#!/usr/bin/env bash
set -euo pipefail
if [[ ${1:-} == version ]]; then
  "$MESH_PROOF_BINARY" "$@" | python3 -c 'import json,os,sys; value=json.load(sys.stdin); value["stateVersion"]=int(os.environ["MESH_PROOF_FINAL"]); print(json.dumps(value))'
  exit
fi
if [[ ${1:-} == daemon ]]; then
  "$MESH_PROOF_BOUNDARY" -migrations "$MESH_PROOF_MIGRATIONS" -database "$MESH_STATE_DIR/mesh.db" -version "$MESH_PROOF_FINAL"
fi
exec "$MESH_PROOF_BINARY" "$@"
WRAPPER
chmod +x "$scratch/boundary-tool" "$scratch/migrating-candidate"
printf -v intermediate_file '%05d_intermediate.sql' "$MESH_PROOF_INTERMEDIATE"
printf -v final_file '%05d_final.sql' "$MESH_PROOF_FINAL"
printf '%s\n' '-- +goose Up' 'CREATE TABLE rollback_fixture(value TEXT);' >"$scratch/migrations/$intermediate_file"
printf '%s\n' '-- +goose Up' 'INSERT INTO rollback_fixture VALUES ('"'candidate'"');' >"$scratch/migrations/$final_file"
python3 "$repo_root/scripts/fixtures/release_transition.py" startup-overlay "$repo_root" "$scratch/startup-overlay" "$scratch/migrations"
(cd "$repo_root" && go build -overlay "$scratch/startup-overlay/overlay.json" -o "$scratch/startup-candidate" ./cmd/mesh)
if env -u MESH_STARTUP_FAULT_BINARY MESH_BOUNDARY_TOOL="$scratch/boundary-tool" "$repo_root/scripts/prove-release-transition.sh" \
  "$MESH_PROOF_BINARY" "$scratch/migrating-candidate" "$platform" "$scratch/missing-input-proofs" \
  >"$scratch/missing-input.log" 2>&1; then
  echo 'FAIL: migrating full proof accepted missing startup-fault input' >&2
  exit 1
fi
grep -Fq 'requires MESH_STARTUP_FAULT_BINARY' "$scratch/missing-input.log" || {
  cat "$scratch/missing-input.log" >&2
  exit 1
}
[[ ! -e $scratch/missing-input-proofs ]] || { echo 'FAIL: missing startup input emitted proof assets' >&2; exit 1; }
MESH_STARTUP_FAULT_BINARY="$scratch/startup-candidate" MESH_BOUNDARY_TOOL="$scratch/boundary-tool" "$repo_root/scripts/prove-release-transition.sh" \
  "$MESH_PROOF_BINARY" "$scratch/migrating-candidate" "$platform" "$scratch/boundary-proofs" \
  >"$scratch/boundaries.log" 2>&1 || { cat "$scratch/boundaries.log" >&2; exit 1; }
for boundary in $(seq "$source_version" "$MESH_PROOF_FINAL"); do
  grep -Fq "rollback boundary $boundary-before passed" "$scratch/boundaries.log"
  grep -Fq "rollback boundary $boundary-after-commit passed" "$scratch/boundaries.log"
done
for boundary in $(seq "$source_version" "$MESH_PROOF_INTERMEDIATE"); do
  grep -Fq "rollback boundary $boundary-failure passed" "$scratch/boundaries.log"
  grep -Fq "rollback boundary $boundary-interrupt passed" "$scratch/boundaries.log"
done
for version in "$MESH_PROOF_INTERMEDIATE" "$MESH_PROOF_FINAL"; do
  for side in before after; do
    grep -Fq "rollback boundary startup-$version-$side-failure passed" "$scratch/boundaries.log"
    grep -Fq "rollback boundary startup-$version-$side-interrupt passed" "$scratch/boundaries.log"
  done
done
cat "$scratch/startup-overlay/identity.json"
printf '\n'
grep -E 'mesh startup commit hook|rollback boundary startup-' "$scratch/boundaries.log"

printf '%s\n' '-- +goose Up' 'ALTER TABLE sessions RENAME TO rollback_hidden_sessions;' >"$scratch/migrations/$intermediate_file"
printf '%s\n' '-- +goose Up' 'ALTER TABLE rollback_hidden_sessions RENAME TO sessions;' >"$scratch/migrations/$final_file"
python3 "$repo_root/scripts/fixtures/release_transition.py" startup-overlay "$repo_root" "$scratch/incompatible-overlay" "$scratch/migrations"
(cd "$repo_root" && go build -overlay "$scratch/incompatible-overlay/overlay.json" -o "$scratch/incompatible-startup" ./cmd/mesh)
if MESH_STARTUP_FAULT_BINARY="$scratch/incompatible-startup" MESH_BOUNDARY_TOOL="$scratch/boundary-tool" "$repo_root/scripts/prove-release-transition.sh" \
  "$MESH_PROOF_BINARY" "$scratch/migrating-candidate" "$platform" "$scratch/incompatible-proofs" \
  >"$scratch/incompatible.log" 2>&1; then
  echo 'FAIL: release proof accepted an incompatible intermediate committed state' >&2
  exit 1
fi
grep -Fq "source did not start at $MESH_PROOF_INTERMEDIATE-before" "$scratch/incompatible.log" || {
  cat "$scratch/incompatible.log" >&2
  exit 1
}
[[ ! -e $scratch/incompatible-proofs ]] || { echo 'FAIL: failed boundary emitted proof assets' >&2; exit 1; }
echo 'PASS: forward proof and complete rollback boundaries retain live workers, I/O and recovery; incompatible intermediate states fail'
