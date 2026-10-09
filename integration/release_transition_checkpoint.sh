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
                "--cwd", str(state), "--", "/bin/true", "checkpoint while daemon is stopped"],
               check=True)
after = checkpoint.read_bytes()
assert after != before, "retained worker did not publish a new checkpoint"
record = json.loads(after)
assert record["restart"]["argv"][1] == "checkpoint while daemon is stopped", record
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

proofs = list(Path(sys.argv[1]).glob("*.json"))
assert len(proofs) == 1, proofs
receipt = json.loads(proofs[0].read_bytes())
assert receipt["candidateOpenedRetainedState"], receipt
assert receipt["sessionsPreserved"] and receipt["recoveryRecordsPreserved"], receipt
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
echo 'PASS: forward upgrade and restart proof permits live checkpoints and rejects changed saved recovery'
