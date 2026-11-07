"""Exercise managed skills only inside the caller's disposable fixture root."""
import json
import os
from pathlib import Path
import subprocess
import sys

binary, scratch = sys.argv[1], Path(sys.argv[2])
config, state = scratch / "config", scratch / "state"
environment = dict(os.environ, MESH_CONFIG_DIR=str(config), MESH_STATE_DIR=str(state))


def invoke(*arguments, code=0):
    result = subprocess.run([binary, *arguments], env=environment,
                            capture_output=True, timeout=15, check=False)
    assert result.returncode == code, (arguments, result.returncode, result.stdout, result.stderr)
    assert not result.stderr, (arguments, result.stderr)
    return result.stdout


def managed(action, root=None, code=0):
    arguments = ["skill", action, "--json"]
    if root is not None:
        arguments.extend(["--dir", str(root)])
    envelope = json.loads(invoke(*arguments, code=code))
    assert envelope["operation"] == "skill." + action, envelope
    assert envelope["schemaVersion"] == 1, envelope
    return envelope["data"]["roots"]


skill = invoke("skill", "show")
reference = invoke("skill", "show", "--file", "references/commands.md")
assert not config.exists() and not state.exists()
config.mkdir(mode=0o700)
state.mkdir(mode=0o700)
for name in ("hosts.json", "domains.json", "skills.json"):
    (config / name).write_text("{", encoding="utf-8")
(state / "key").write_text("invalid key", encoding="utf-8")
assert invoke("skill", "show") == skill
assert invoke("skill", "show", "--file", "references/commands.md") == reference
failure = json.loads(invoke("skill", "show", "--json", code=2))
assert failure["errors"][0]["code"] == "usage.invalid", failure
for path in config.iterdir():
    assert path.read_text() == "{"
    path.unlink()
(state / "key").unlink()

root, other = scratch / "root", scratch / "other"
root.mkdir(mode=0o755)
other.mkdir(mode=0o755)
assert managed("install", root)[0]["state"] == "current"
assert (root / "mesh/SKILL.md").read_bytes() == skill
assert (root / "mesh/references/commands.md").read_bytes() == reference
manifest = (root / "mesh/.mesh-skill.json").read_bytes()
assert managed("install", root)[0]["registered"]
assert (root / "mesh/.mesh-skill.json").read_bytes() == manifest
alias = scratch / "alias"
alias.symlink_to(root, target_is_directory=True)
managed("install", alias)
managed("install", other)
assert len(managed("status")) == 2
customized = root / "mesh/references/safety.md"
customized.write_text("owner customization\n", encoding="utf-8")
(root / "mesh/SKILL.md").unlink()
status = managed("status", root)[0]
assert status["state"] == "modified" and status["missingFiles"] == ["SKILL.md"], status
updated = managed("update", root, code=3)[0]
assert updated["state"] == "modified" and len(updated["proposedFiles"]) == 1, updated
assert customized.read_text() == "owner customization\n"
assert (root / "mesh/SKILL.md").read_bytes() == skill
proposal = Path(updated["proposedFiles"][0])
proposal.write_text("edited proposal\n", encoding="utf-8")
removed = managed("uninstall", root)[0]
assert not removed["registered"] and len(removed["retainedFiles"]) == 2, removed
assert customized.read_text() == "owner customization\n"
assert proposal.read_text() == "edited proposal\n"
managed("uninstall", other)
assert managed("status") == []
assert not (other / "mesh").exists()
assert not (state / "identity.key").exists()
assert not (state / "daemon.sock").exists()
print("skill installation: PASS (isolated roots, exact bytes, two registrations, retained edits)")
