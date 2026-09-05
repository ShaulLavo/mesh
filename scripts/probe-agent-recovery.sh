#!/usr/bin/env bash
set -euo pipefail

if [[ ${1:-} == --native ]]; then
  shift
  exec python3 "$(dirname "$0")/probe-agent-recovery-native.py" "$@"
fi
if [[ ${1:-} == --help ]]; then
  printf 'Usage: %s [OUTPUT_PARENT]\nOffline CLI help, schema, and synthetic event probe. No provider sessions are started.\n\nUse --native --help for the separate interactive native probe.\n' "$0"
  exit 0
fi
if (( $# > 1 )); then
  printf 'Expected at most one output parent directory\n' >&2
  exit 2
fi

python3 - "${1:-${TMPDIR:-/tmp}}" <<'PY'
import datetime
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile


def write_json(path, value):
    path.write_text(json.dumps(value, indent=2, sort_keys=True) + "\n")


def capture(root, name, argv, probe_env):
    record = {"argv": argv}
    if argv[0] is None:
        record.update(status="unavailable", exit_code=None)
        write_json(root / (name + ".json"), record)
        return ""
    try:
        result = subprocess.run(argv, cwd=root / "project", env=probe_env,
                                stdin=subprocess.DEVNULL, capture_output=True,
                                text=True, timeout=30, check=False)
        record.update(status="observed" if result.returncode == 0 else "failed",
                      exit_code=result.returncode)
        (root / (name + ".txt")).write_text(result.stdout)
        (root / (name + ".stderr.txt")).write_text(result.stderr)
        output = result.stdout if result.returncode == 0 else ""
    except (OSError, subprocess.TimeoutExpired) as error:
        record.update(status="failed", error=type(error).__name__)
        output = ""
    write_json(root / (name + ".json"), record)
    return output


def schema_summary(root):
    names = ["ThreadStartParams", "ThreadResumeParams", "ThreadReadParams",
             "HookStartedNotification", "HookCompletedNotification"]
    summary = {}
    for name in names:
        path = root / "codex-schema" / "v2" / (name + ".json")
        if not path.is_file():
            summary[name] = {"status": "unavailable"}
            continue
        value = json.loads(path.read_text())
        summary[name] = {"status": "observed", "path": str(path.relative_to(root)),
                         "required": value.get("required", []),
                         "properties": sorted(value.get("properties", {}))}
    return summary


def sanitize_fixture(provider, event):
    if provider not in ("codex", "claude"):
        raise ValueError("unknown provider")
    required = ("hook_event_name", "session_id", "cwd")
    for key in required:
        value = event.get(key)
        if not isinstance(value, str) or not value or len(value.encode()) > 4096:
            raise ValueError("invalid fixture field: " + key)
    allowed = required + ("source", "agent_id")
    clean = {key: event[key] for key in allowed if key in event}
    return {"provider": provider, "evidence": "synthetic_fixture", "event": clean}


def resume_argv(provider, session_id, cwd):
    if provider == "codex":
        return ["codex", "resume", "--cd", cwd, "--", session_id]
    if provider == "claude":
        return ["claude", "--resume", session_id]
    raise ValueError("unknown provider")


def fixtures(root, probe_env):
    records = []
    launches = []
    project = "/fixture/project with spaces"
    for provider in ("codex", "claude"):
        for index in (1, 2):
            session_id = f"00000000-0000-4000-8000-{index:012d}"
            event = {"hook_event_name": "SessionStart", "session_id": session_id,
                     "cwd": project, "source": "resume", "prompt": "discard-me",
                     "transcript_path": "/fixture/private/transcript.jsonl",
                     "environment": {"API_KEY": "fixture-only-discard-me"}}
            records.append(sanitize_fixture(provider, event))
            launches.append({"provider": provider, "cwd": project,
                             "evidence": "constructed_only",
                             "argv": resume_argv(provider, session_id, project)})
    serialized = json.dumps(records)
    assert "discard-me" not in serialized
    assert "transcript" not in serialized
    assert len({record["event"]["session_id"] for record in records}) == 2
    assert all("--last" not in launch["argv"] for launch in launches)
    # A real subprocess proves argv boundaries without executing either provider.
    opaque_id = "opaque id; $(must-not-run) `literal`"
    for provider in ("codex", "claude"):
        argv = resume_argv(provider, opaque_id, project)
        result = subprocess.run([sys.executable, "-c",
                                 "import json,sys; print(json.dumps(sys.argv[1:]))",
                                 *argv], env=probe_env, capture_output=True,
                                text=True, check=True, timeout=5)
        assert json.loads(result.stdout) == argv
    write_json(root / "sanitized-fixture-events.json", records)
    write_json(root / "resume-argv-fixtures.json", launches)
    return {"status": "passed", "events": len(records),
            "scope": "fixture filtering and subprocess argv boundaries only"}


parent = Path(sys.argv[1]).expanduser().resolve()
parent.mkdir(parents=True, exist_ok=True)
os.umask(0o077)
root = Path(tempfile.mkdtemp(prefix="mesh-agent-recovery-", dir=parent))
for directory in ("home", "codex", "claude", "config", "cache", "data", "project", "tmp"):
    (root / directory).mkdir()
executables = {provider: shutil.which(provider) for provider in ("codex", "claude")}
# These settings apply only to child processes; the calling shell is unchanged.
probe_env = {"PATH": os.defpath, "HOME": str(root / "home"),
             "CODEX_HOME": str(root / "codex"), "CLAUDE_CONFIG_DIR": str(root / "claude"),
             "XDG_CONFIG_HOME": str(root / "config"), "XDG_CACHE_HOME": str(root / "cache"),
             "XDG_DATA_HOME": str(root / "data"), "TMPDIR": str(root / "tmp"),
             "TERM": "dumb", "NO_COLOR": "1", "LANG": "C", "LC_ALL": "C"}
commands = {
    "codex-version": [executables["codex"], "--version"],
    "codex-help": [executables["codex"], "--help"],
    "codex-resume-help": [executables["codex"], "resume", "--help"],
    "codex-daemon-help": [executables["codex"], "app-server", "daemon", "--help"],
    "codex-schema-help": [executables["codex"], "app-server", "generate-json-schema", "--help"],
    "claude-version": [executables["claude"], "--version"],
    "claude-help": [executables["claude"], "--help"],
}
observed = {name: capture(root, name, argv, probe_env) for name, argv in commands.items()}
if "--out" in observed["codex-schema-help"]:
    capture(root, "codex-schema-generation",
            [executables["codex"], "app-server", "generate-json-schema", "--experimental",
             "--out", str(root / "codex-schema")], probe_env)

hook_shapes = {
    "evidence": "documentation_summary_not_native_schema_or_observed_event",
    "checked_date": "2026-09-05",
    "providers": {
        "codex": {"source": "https://learn.chatgpt.com/docs/hooks",
                  "fields": {"session_id": "string", "cwd": "string",
                             "hook_event_name": "string", "transcript_path": "string|null"},
                  "SessionStart.source": ["startup", "resume", "clear", "compact"]},
        "claude": {"source": "https://code.claude.com/docs/en/hooks#sessionstart",
                   "fields": {"session_id": "string", "cwd": "string",
                              "hook_event_name": "string", "transcript_path": "string"},
                   "SessionStart.source": ["startup", "resume", "clear", "compact"]},
    },
}
write_json(root / "documented-hook-shapes.json", hook_shapes)
summary = {
    "probe_version": 1,
    "recorded_at": datetime.datetime.now(datetime.timezone.utc).isoformat(),
    "mode": "offline_help_schema_and_synthetic_fixtures",
    "providers": {
        "codex": {"version": observed["codex-version"].strip() or "unavailable",
                  "exact_id_resume_help": "[SESSION_ID]" in observed["codex-resume-help"],
                  "explicit_cwd_help": "--cd" in observed["codex-resume-help"],
                  "daemon_command_help": "daemon" in observed["codex-daemon-help"],
                  "native_schema": schema_summary(root)},
        "claude": {"version": observed["claude-version"].strip() or "unavailable",
                   "exact_id_resume_help": "--resume" in observed["claude-help"],
                   "native_hook_schema": "unavailable_in_inspected_cli_help"},
    },
    "fixtures": fixtures(root, probe_env),
    "live_verification": {
        "two_simultaneous_conversations_in_one_directory": "unverified",
        "codex_preexisting_daemon_invocation_routing": "unverified",
        "hook_session_id_to_native_resume_id_mapping": "unverified",
        "exact_conversation_resume_without_new_prompt": "unverified",
        "pending_work_after_resume": "unverified",
        "codex_hook_trust": "unverified",
        "claude_invocation_routing": "unverified",
    },
    "automatic_recovery_supported": False,
    "supported_version_range": "not_established",
}
write_json(root / "summary.json", summary)
print(root)
for provider, values in summary["providers"].items():
    print(f"{provider}: {values['version']}; exact-ID option in help: {values['exact_id_resume_help']}")
print("PASS: synthetic event filtering and argv boundaries")
print("UNVERIFIED: live exact resume, invocation association, and automatic recovery")
PY
